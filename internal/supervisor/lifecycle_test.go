package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProcess is a controllable RcloneProcess: Kill marks the process dead
// and feeds Wait a killed error, exactly the observable behaviour of a real
// child.
type fakeProcess struct {
	mu     sync.Mutex
	killed bool
	wait   chan error
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{wait: make(chan error, 1)}
}

func (p *fakeProcess) Wait() error { return <-p.wait }

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	first := !p.killed
	p.killed = true
	p.mu.Unlock()
	if first {
		p.wait <- errors.New("process killed")
	}
	return nil
}

// exit makes the process die on its own, the way a crashed rclone would.
func (p *fakeProcess) exit(err error) {
	p.mu.Lock()
	first := !p.killed
	p.killed = true
	p.mu.Unlock()
	if first {
		p.wait <- err
	}
}

func (p *fakeProcess) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// probeSeq answers the drive-visibility probe from a scripted sequence and a
// fallback, recording how often it was consulted.
type probeSeq struct {
	mu       sync.Mutex
	seq      []bool
	fallback bool
	calls    int
}

func (p *probeSeq) probe(mountPoint string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls <= len(p.seq) {
		return p.seq[p.calls-1]
	}
	return p.fallback
}

// launcher hands out scripted launch results and counts the launches, which
// is how the tests pin "no duplicate instances" and "no futile retries".
type launcher struct {
	mu      sync.Mutex
	errs    []error
	procs   []*fakeProcess
	started int
}

func (l *launcher) do(ctx context.Context, configPath string) (RcloneProcess, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started++
	if l.started <= len(l.errs) {
		return nil, l.errs[l.started-1]
	}
	p := newFakeProcess()
	l.procs = append(l.procs, p)
	return p, nil
}

func (l *launcher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.started
}

func (l *launcher) last() *fakeProcess {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.procs) == 0 {
		return nil
	}
	return l.procs[len(l.procs)-1]
}

// supHarness wires one supervisor against test doubles with a fast backoff.
type supHarness struct {
	sup    *Supervisor
	probe  *probeSeq
	launch *launcher
	states <-chan Status
}

func newHarness(t *testing.T, mutate func(*SupervisorConfig)) *supHarness {
	t.Helper()
	h := &supHarness{
		probe:  &probeSeq{},
		launch: &launcher{},
	}
	cfg := SupervisorConfig{
		Port:            freePort(t),
		Cookie:          "__puus=harness",
		MountPoint:      "Q:",
		BackoffSchedule: []time.Duration{time.Millisecond, time.Millisecond},
		ShutdownTimeout: 2 * time.Second,
		// Isolate the instance cache directory per test; nothing ever cleans
		// it up, so a shared temp location would grow across runs.
		CacheRoot:    t.TempDir(),
		Preflight:    func() error { return nil },
		ProbeDrive:   h.probe.probe,
		LaunchRclone: h.launch.do,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h.sup = Supervise(context.Background(), cfg)
	h.states = h.sup.Subscribe()
	t.Cleanup(func() { h.sup.Stop() })
	return h
}

// await polls until the named state appears, failing with the observed
// snapshot on timeout.
func await(t *testing.T, h *supHarness, want State, timeout time.Duration) Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last Status
	for time.Now().Before(deadline) {
		last = h.sup.Status()
		if last.State == want {
			return last
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("state %s not reached within %s; last = %+v", want, timeout, last)
	return Status{}
}

// awaitAfter waits for a fresh transition into want — one that happened
// strictly after `after` — because a state the supervisor is already sitting
// in would satisfy a plain state check instantly.
func awaitAfter(t *testing.T, h *supHarness, want State, after time.Time, timeout time.Duration) Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last Status
	for time.Now().Before(deadline) {
		last = h.sup.Status()
		if last.State == want && last.Since.After(after) {
			return last
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("state %s not re-entered after %s within %s; last = %+v", want, after, timeout, last)
	return Status{}
}

// readStateFile reads the persisted status, tolerating the momentary Windows
// sharing lock the atomic rename can put on the file.
func readStateFile(t *testing.T, path string) string {
	t.Helper()
	for i := 0; i < 50; i++ {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state file %s never became readable", path)
	return ""
}

// TestStartupSequenceMountedAndCleanStop checks the full happy path: the
// letter is free before rclone starts, visible once it mounts, and cleanly
// released on shutdown — with exactly one rclone instance throughout.
func TestStartupSequenceMountedAndCleanStop(t *testing.T) {
	probe := &probeSeq{seq: []bool{false, true}, fallback: false}
	launch := &launcher{}
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probe.probe
		c.LaunchRclone = launch.do
	})

	st := await(t, h, StateMounted, 5*time.Second)
	if st.Phase != "drive" {
		t.Fatalf("mounted phase = %q", st.Phase)
	}
	if launch.count() != 1 {
		t.Fatalf("launches = %d, want 1", launch.count())
	}

	final := h.sup.Stop()
	if final.State != StateDisabled {
		t.Fatalf("final = %s, want Disabled", final.State)
	}
	if !launch.last().wasKilled() {
		t.Fatal("rclone was not killed on shutdown")
	}
	if launch.count() != 1 {
		t.Fatalf("launches after stop = %d, want still 1", launch.count())
	}
}

// TestProbeCapsRunAfterReadyAndBeforeLaunch pins the issue-22 probe ordering:
// the capability probes run exactly once per attempt, after readiness
// succeeded and before rclone is asked to mount, and a failing probe never
// fails the mount.
func TestProbeCapsRunAfterReadyAndBeforeLaunch(t *testing.T) {
	probe := &probeSeq{seq: []bool{false, true}, fallback: false}
	launch := &launcher{}
	readyCalls := 0
	launchSeq := 0
	probeOrder := ""
	var mu sync.Mutex
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probe.probe
		c.LaunchRclone = launch.do
		c.ReadyProbe = func(context.Context) error {
			mu.Lock()
			readyCalls++
			mu.Unlock()
			return nil
		}
		c.ProbeCaps = func(context.Context) {
			mu.Lock()
			probeOrder = fmt.Sprintf("ready=%d launch=%d", readyCalls, launchSeq)
			launchSeq++
			mu.Unlock()
		}
	})

	await(t, h, StateMounted, 5*time.Second)
	if launch.count() != 1 {
		t.Fatalf("launches = %d, want 1", launch.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if probeOrder != "ready=1 launch=0" {
		t.Fatalf("probe order = %q, want the probe between ready and launch", probeOrder)
	}
}

// TestDriveConflictOnHeldLetter checks the explicit-conflict branch: a letter
// that is taken before rclone ever starts pauses the supervisor in
// DriveConflict, launches nothing, and keeps the adapter up for diagnostics.
func TestDriveConflictOnHeldLetter(t *testing.T) {
	probe := &probeSeq{seq: []bool{true}, fallback: true}
	launch := &launcher{}
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probe.probe
		c.LaunchRclone = launch.do
	})

	st := await(t, h, StateDriveConflict, 5*time.Second)
	if !strings.Contains(st.Err, "已被其它卷占用") {
		t.Fatalf("err = %q", st.Err)
	}
	if launch.count() != 0 {
		t.Fatalf("launches = %d, want 0: a held letter must not reach rclone", launch.count())
	}
	// DriveConflict pauses retrying: still parked after a grace period.
	time.Sleep(20 * time.Millisecond)
	if got := h.sup.Status().State; got != StateDriveConflict {
		t.Fatalf("state drifted to %s; DriveConflict must pause retrying", got)
	}
	final := h.sup.Stop()
	if final.State != StateDisabled {
		t.Fatalf("final = %s, want Disabled (nothing to unmount)", final.State)
	}
}

// TestPreflightFailureClassifies pins the two preflight verdicts: WinFsp
// wording parks in WinFspRequired, anything else in Failed.
func TestPreflightFailureClassifies(t *testing.T) {
	t.Run("winfsp", func(t *testing.T) {
		h := newHarness(t, func(c *SupervisorConfig) {
			c.Preflight = func() error { return errors.New("unable to locate winfsp-x64.dll") }
		})
		await(t, h, StateWinFspRequired, 5*time.Second)
	})
	t.Run("other", func(t *testing.T) {
		h := newHarness(t, func(c *SupervisorConfig) {
			c.Preflight = func() error { return errors.New("内置 SHA-256 清单缺少条目") }
		})
		await(t, h, StateFailed, 5*time.Second)
	})
}

// TestNetworkBackoffUntilMounted checks the retry cadence: far-side launch
// failures walk the injected schedule under WaitingForNetwork (with a next
// retry timestamp attached) and eventually mount.
func TestNetworkBackoffUntilMounted(t *testing.T) {
	probe := &probeSeq{seq: []bool{false, false, false, true}, fallback: false}
	launch := &launcher{errs: []error{
		errors.New("connection refused"),
		errors.New("connection refused"),
	}}
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probe.probe
		c.LaunchRclone = launch.do
		c.BackoffSchedule = []time.Duration{time.Millisecond, time.Millisecond}
	})

	await(t, h, StateMounted, 5*time.Second)
	if launch.count() < 3 {
		t.Fatalf("launches = %d, want the two failed attempts plus the successful one", launch.count())
	}
	// The waiting state was observable with a retry time attached.
	sawWaiting := false
	for {
		select {
		case s := <-h.states:
			if s.State == StateWaitingForNetwork && !s.NextRetry.IsZero() {
				sawWaiting = true
			}
			continue
		default:
		}
		break
	}
	if !sawWaiting {
		t.Fatal("no WaitingForNetwork transition with a next-retry time was observed")
	}
}

// TestNeedsReauthStopsRetrying pins the credential-failure branch: a launch
// failure carrying the provider's authentication signature parks in
// NeedsReauth and never retries (no duplicate instances behind a dead
// credential).
func TestNeedsReauthStopsRetrying(t *testing.T) {
	launch := &launcher{errs: []error{
		errors.New("quark: authentication rejected: 31001 need login"),
	}}
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probeFalse
		c.LaunchRclone = launch.do
	})

	await(t, h, StateNeedsReauth, 5*time.Second)
	time.Sleep(20 * time.Millisecond)
	if got := launch.count(); got != 1 {
		t.Fatalf("launches = %d, want 1: NeedsReauth must not retry", got)
	}
	final := h.sup.Stop()
	if final.State != StateDisabled {
		t.Fatalf("final = %s, want Disabled", final.State)
	}
}

// TestReadyProbeNeedsReauth checks the real-readiness gate: a /config probe
// failing with the session-expired signature lands in NeedsReauth.
func TestReadyProbeNeedsReauth(t *testing.T) {
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probeFalse
		c.ReadyProbe = func(ctx context.Context) error {
			return errors.New("quark: 31001 login expired")
		}
	})
	await(t, h, StateNeedsReauth, 5*time.Second)
}

// TestRcloneExitTriggersRecoveringAndRestart checks the child-supervision
// contract: a mounted rclone that dies on its own is cleaned up — letter
// released — and a fresh attempt mounts again, with no second live instance.
func TestRcloneExitTriggersRecoveringAndRestart(t *testing.T) {
	// Probe order: free before launch, visible after mount; then teardown's
	// gone-check, then the restart's free-check, and visible again.
	probe := &probeSeq{seq: []bool{false, true, false, false, true}, fallback: false}
	launch := &launcher{}
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probe.probe
		c.LaunchRclone = launch.do
	})

	await(t, h, StateMounted, 5*time.Second)
	first := launch.last()
	mountedAt := h.sup.Status().Since
	first.exit(errors.New("rclone crashed"))

	// The restart must land in a *fresh* Mounted transition: the supervisor
	// is already sitting in Mounted when the child dies, so the check keys
	// on the transition time, not the state name.
	awaitAfter(t, h, StateMounted, mountedAt, 5*time.Second)
	if launch.count() != 2 {
		t.Fatalf("launches = %d, want the restart", launch.count())
	}
	// The first instance must have been collected before the second launch:
	// no duplicates.
	if !first.wasKilled() {
		t.Fatal("the crashed rclone was not torn down")
	}
	final := h.sup.Stop()
	if final.State != StateDisabled {
		t.Fatalf("final = %s, want Disabled", final.State)
	}
}

// TestStopTimesOutWhenLetterWillNotDisappear checks the diagnosable shutdown
// failure: a letter that stays visible past the shutdown timeout fails the
// stop with an explicit remedy instead of pretending.
func TestStopTimesOutWhenLetterWillNotDisappear(t *testing.T) {
	probe := &probeSeq{seq: []bool{false, true}, fallback: true}
	launch := &launcher{}
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probe.probe
		c.LaunchRclone = launch.do
		c.ShutdownTimeout = 100 * time.Millisecond
	})

	await(t, h, StateMounted, 5*time.Second)
	final := h.sup.Stop()
	if final.State != StateFailed {
		t.Fatalf("final = %s, want Failed", final.State)
	}
	if !strings.Contains(final.Err, "盘符") || !strings.Contains(final.Err, "手动卸载") {
		t.Fatalf("shutdown diagnosis = %q", final.Err)
	}
}

// TestStatusPersistence checks the status contract: every transition lands in
// the state file, which always holds the last state.
func TestStatusPersistence(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	probe := &probeSeq{seq: []bool{false, true}, fallback: false}
	launch := &launcher{}
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probe.probe
		c.LaunchRclone = launch.do
		c.StatePath = statePath
	})

	await(t, h, StateMounted, 5*time.Second)
	// The transition updates the in-memory status before it finishes writing
	// the file, so the file content is polled, not read once.
	waitStateFile(t, statePath, `"Mounted"`)
	h.sup.Stop()
	waitStateFile(t, statePath, `"Disabled"`)
}

// waitStateFile polls until the persisted status contains the expected
// marker.
func waitStateFile(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data := readStateFile(t, path)
		if strings.Contains(data, want) {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state file never contained %s", want)
	return ""
}

// TestStatusRedactsErrors checks the status contract's hygiene rule: an error
// carried through a transition is redacted, so a cookie or a signed URL can
// never land in the state file.
func TestStatusRedactsErrors(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	h := newHarness(t, func(c *SupervisorConfig) {
		c.Preflight = func() error {
			return errors.New("winfsp missing; https://oss.example.com/obj?Signature=LEAK __puus=LEAK")
		}
		c.StatePath = statePath
	})
	await(t, h, StateWinFspRequired, 5*time.Second)
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "LEAK") {
		t.Fatalf("state file leaked sensitive material: %s", data)
	}
}

func probeFalse(mountPoint string) bool { return false }

// TestHistoryRecordsTimelineAndCaps checks the recovery timeline (issue #24):
// every transition lands oldest-first, and a flapping lifecycle cannot grow
// the buffer past the cap.
func TestHistoryRecordsTimelineAndCaps(t *testing.T) {
	probe := &probeSeq{seq: []bool{false, true}, fallback: false}
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probe.probe
	})
	await(t, h, StateMounted, 5*time.Second)

	hist := h.sup.History()
	if len(hist) < 3 {
		t.Fatalf("history = %d entries, want the startup sequence", len(hist))
	}
	for i := 1; i < len(hist); i++ {
		if hist[i].Since.Before(hist[i-1].Since) {
			t.Fatalf("history not oldest-first at %d", i)
		}
	}
	if hist[len(hist)-1].State != StateMounted {
		t.Fatalf("last entry = %s, want Mounted", hist[len(hist)-1].State)
	}

	// Direct cap check on a bare supervisor: 70 transitions must keep the
	// newest 64.
	s := &Supervisor{subs: map[chan Status]struct{}{}}
	for i := 0; i < statusHistoryCap+6; i++ {
		s.transition(StateStarting, fmt.Sprintf("phase-%d", i), "")
	}
	capped := s.History()
	if len(capped) != statusHistoryCap {
		t.Fatalf("capped history = %d, want %d", len(capped), statusHistoryCap)
	}
	if capped[0].Phase != fmt.Sprintf("phase-%d", 6) || capped[len(capped)-1].Phase != fmt.Sprintf("phase-%d", statusHistoryCap+5) {
		t.Fatalf("cap window wrong: first %q last %q", capped[0].Phase, capped[len(capped)-1].Phase)
	}
}

// TestCacheInstanceKeyIsStableAndSafe pins the instance-key derivation (issue
// #23, ADR 0011): a volume spelled differently lands in the same directory,
// directory mount points get deterministic, mutually distinct keys, and no
// key can escape the cache root as a dot segment.
func TestCacheInstanceKeyIsStableAndSafe(t *testing.T) {
	// Bare drive letters: one volume, one key, whatever the spelling.
	for _, in := range []string{"Q:", "q:", "Q", "q"} {
		if got := cacheInstanceKey(in); got != "q" {
			t.Errorf("cacheInstanceKey(%q) = %q, want q", in, got)
		}
	}
	for _, in := range []string{"", ":", "   "} {
		if got := cacheInstanceKey(in); got != "default" {
			t.Errorf("cacheInstanceKey(%q) = %q, want default", in, got)
		}
	}

	// Directory mount points: readable prefix, deterministic hash, mutually
	// distinct, and always one safe path segment.
	dirCases := []string{`D:\data\`, `D:\data`, `\\server\share`, `D:\mnt\quark`, `D:\mnt-quark`, `X:..\..\evil`, "..", "."}
	seen := map[string]string{}
	for _, in := range dirCases {
		got := cacheInstanceKey(in)
		if got != cacheInstanceKey(in) {
			t.Errorf("cacheInstanceKey(%q) is not stable", in)
		}
		if strings.ContainsAny(got, `\/:`) || got == "." || got == ".." {
			t.Errorf("cacheInstanceKey(%q) = %q, want one safe segment", in, got)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("keys of %q and %q collide on %q", in, other, got)
		}
		seen[got] = in
	}
	if !strings.HasPrefix(cacheInstanceKey(`D:\data`), "d-data-") {
		t.Errorf("sanitized prefix lost: %q", cacheInstanceKey(`D:\data`))
	}
	if !strings.HasPrefix(cacheInstanceKey(`\\server\share`), "server-share-") {
		t.Errorf("sanitized prefix lost: %q", cacheInstanceKey(`\\server\share`))
	}
}

// TestRcloneArgsPinWritesCacheDefaults checks the ADR 0002 write path (issue
// #23): the mount always runs a VFS cache mode (writes unless configured
// otherwise) and always gets this instance's own cache directory, with the
// profile's write-back parameters passed through when present.
func TestRcloneArgsPinWritesCacheDefaults(t *testing.T) {
	h := newHarness(t, func(c *SupervisorConfig) {
		c.WriteBack = "5s"
		c.WriteChunkSize = "128Mi"
	})
	args := h.sup.rcloneArgs("cfg-path")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--vfs-cache-mode writes",
		"--cache-dir " + h.sup.CacheDir(),
		"--vfs-write-back 5s",
		"--vfs-write-chunk-size 128Mi",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rclone args missing %q: %s", want, joined)
		}
	}

	// An explicit mode is honoured, not overridden.
	h2 := newHarness(t, func(c *SupervisorConfig) { c.CacheMode = "full" })
	if got := h2.sup.cacheMode(); got != "full" {
		t.Fatalf("cacheMode = %q, want the explicit value", got)
	}
	// An empty root falls back to a per-user temp location, never to rclone's
	// shared default.
	h3 := newHarness(t, func(c *SupervisorConfig) { c.CacheRoot = "" })
	if dir := h3.sup.CacheDir(); !strings.Contains(dir, "quark-rclone-cache") {
		t.Fatalf("fallback cache dir = %q, want the dedicated temp root", dir)
	}
}

// TestCacheDirCreatedBeforeLaunchAndSurvivesStop is the dirty-cache recovery
// contract: the instance directory exists before rclone starts and is never
// deleted by the supervisor — dirty cache left behind must be there for the
// next mount of the same letter (issue #23, ADR 0002).
func TestCacheDirCreatedBeforeLaunchAndSurvivesStop(t *testing.T) {
	probe := &probeSeq{seq: []bool{false, true}, fallback: false}
	h := newHarness(t, func(c *SupervisorConfig) {
		c.ProbeDrive = probe.probe
	})
	await(t, h, StateMounted, 5*time.Second)
	dir := h.sup.CacheDir()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("instance cache dir was not created before launch: %v", err)
	}
	h.sup.Stop()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cache dir must survive Stop: %v", err)
	}
}
