package trayui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muou000/quark-rclone/internal/diag"
	"github.com/muou000/quark-rclone/internal/quarkerr"
	"github.com/muou000/quark-rclone/internal/supervisor"
)

// fakeRunner is a controllable MountRunner.
type fakeRunner struct {
	mu         sync.Mutex
	st         supervisor.Status
	hist       []supervisor.Status
	subs       []chan supervisor.Status
	done       chan struct{}
	stopped    bool
	cacheDir   string
	mountPoint string
}

func newFakeRunner(mountPoint string) *fakeRunner {
	// A real supervisor transitions into Starting the moment it exists, so
	// the fake never reports an empty status either.
	return &fakeRunner{
		done:       make(chan struct{}),
		mountPoint: mountPoint,
		st:         supervisor.Status{State: supervisor.StateStarting, Phase: "boot", Since: time.Now()},
	}
}

func (f *fakeRunner) push(st supervisor.Status) {
	f.mu.Lock()
	f.st = st
	f.hist = append(f.hist, st)
	subs := append([]chan supervisor.Status(nil), f.subs...)
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- st:
		default:
		}
	}
}

func (f *fakeRunner) Status() supervisor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeRunner) History() []supervisor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]supervisor.Status(nil), f.hist...)
}

func (f *fakeRunner) Subscribe() <-chan supervisor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan supervisor.Status, 16)
	f.subs = append(f.subs, ch)
	if f.st.State != "" {
		ch <- f.st
	}
	return ch
}

func (f *fakeRunner) Done() <-chan struct{} { return f.done }

func (f *fakeRunner) CacheDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cacheDir
}

func (f *fakeRunner) MountPoint() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mountPoint
}

func (f *fakeRunner) ReachedMounted() bool {
	for _, h := range f.History() {
		if h.State == supervisor.StateMounted {
			return true
		}
	}
	return false
}

func (f *fakeRunner) PauseReason() supervisor.State {
	if s := f.Status(); s.State.Paused() {
		return s.State
	}
	return supervisor.StateDisabled
}

func (f *fakeRunner) Stop() {
	f.mu.Lock()
	first := !f.stopped
	f.stopped = true
	f.mu.Unlock()
	if first {
		close(f.done)
	}
}

// fakeSource validates a small script of candidates.
type fakeSource struct {
	effective map[string]string
	dirs      []RootDir
}

func (s *fakeSource) Validate(_ context.Context, candidate string) (string, error) {
	if effective, ok := s.effective[candidate]; ok {
		return effective, nil
	}
	return "", quarkerr.New(quarkerr.CatAuthRequired, "validate", "31001 need login")
}

func (s *fakeSource) ListDirs(_ context.Context, _ string) ([]RootDir, error) {
	return s.dirs, nil
}

// managerHarness records what Apply touched.
type managerHarness struct {
	mgr       *Manager
	src       *fakeSource
	started   []string          // effective cookies handed to start
	startCtxs []context.Context // contexts handed to start
	optsSeen  []MountOptions    // options handed to start
	persisted []string          // cookies persisted
	savedCfg  []MountOptions    // configs saved
	resets    int               // full-reset hook calls
	failStart error             // when set, start fails
	runners   []*fakeRunner     // one fresh runner per start, like real lifecycles
	rec       *diag.Recorder
}

func newManagerHarness(t *testing.T, opts MountOptions) *managerHarness {
	t.Helper()
	h := &managerHarness{
		src: &fakeSource{
			effective: map[string]string{"good": "effective-cookie"},
			dirs:      []RootDir{{FID: "d1", Name: "photos", Path: "/photos"}},
		},
		rec: diag.NewRecorder(),
	}
	h.mgr = NewManager(
		h.src,
		func(ctx context.Context, effective string, opts MountOptions) (MountRunner, *diag.Recorder, error) {
			if h.failStart != nil {
				return nil, nil, h.failStart
			}
			h.started = append(h.started, effective)
			h.startCtxs = append(h.startCtxs, ctx)
			h.optsSeen = append(h.optsSeen, opts)
			// A restart must get a fresh lifecycle: reusing one whose Done
			// already closed would retire the new pump instantly.
			runner := newFakeRunner("Q:")
			h.runners = append(h.runners, runner)
			return runner, h.rec, nil
		},
		func(effective string) error {
			h.persisted = append(h.persisted, effective)
			return nil
		},
		func(opts MountOptions) error {
			h.savedCfg = append(h.savedCfg, opts)
			return nil
		},
		func() error {
			h.resets++
			return nil
		},
		opts,
		true,
		NewEventLog(),
	)
	t.Cleanup(func() { h.mgr.Stop() })
	return h
}

// lastRunner is the runner the most recent start returned.
func (h *managerHarness) lastRunner() *fakeRunner {
	return h.runners[len(h.runners)-1]
}

// TestFullResetReturnsToFirstRunState pins the one-way reset: the runner is
// stopped, the instance's VFS cache directory is really deleted, the reset
// hook clears the durable credential, and the manager ends with no runner, no
// cookie and first-run options — the wizard takes over from a fresh install's
// position. No restart happens: remounting is the user's next move.
func TestFullResetReturnsToFirstRunState(t *testing.T) {
	h := newManagerHarness(t, MountOptions{Letter: "Q:", RootFID: "r1", RootPath: "/docs"})
	if err := h.mgr.Start(context.Background(), "seed-cookie"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "dirty.bin"), []byte("pending upload"), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	h.lastRunner().cacheDir = cacheDir

	if err := h.mgr.FullReset(); err != nil {
		t.Fatalf("FullReset: %v", err)
	}

	if h.resets != 1 {
		t.Errorf("reset hook calls = %d, want 1", h.resets)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "dirty.bin")); !os.IsNotExist(err) {
		t.Errorf("cache file survived the reset: %v", err)
	}
	if len(h.started) != 1 {
		t.Errorf("starts = %d, want 1: the reset must not remount by itself", len(h.started))
	}
	if got := h.mgr.Snapshot().State; got != supervisor.StateDisabled {
		t.Errorf("Snapshot after reset = %s, want Disabled", got)
	}
	if h.mgr.Panel().HasCredential {
		t.Error("panel still reports a credential after the reset")
	}
	if opts := h.mgr.CurrentOptions(); opts.RootFID != "" || opts.Letter != "" {
		t.Errorf("options after reset = %+v, want first-run defaults", opts)
	}
}

// TestSnapshotMirrorsCurrentRunner pins the pull model the tray menu lives
// on: with a runner up, Snapshot is its live status; with none, Disabled —
// never a stale transition from a dead stream.
func TestSnapshotMirrorsCurrentRunner(t *testing.T) {
	h := newManagerHarness(t, MountOptions{Letter: "Q:", RootFID: "r1", RootPath: "/docs"})
	if got := h.mgr.Snapshot().State; got != supervisor.StateDisabled {
		t.Fatalf("Snapshot without runner = %s, want Disabled", got)
	}
	if err := h.mgr.Start(context.Background(), "seed-cookie"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := h.mgr.Snapshot().State; got != h.lastRunner().Status().State {
		t.Fatalf("Snapshot = %s, want the live runner's %s", got, h.lastRunner().Status().State)
	}
}

func TestApplyValidationFailureTouchesNothing(t *testing.T) {
	h := newManagerHarness(t, MountOptions{Letter: "Q:", RootFID: "r1", RootPath: "/docs"})
	if err := h.mgr.Start(context.Background(), "seed-cookie"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	old := h.mgr.runnerSnapshot()
	startsBefore := len(h.started)

	err := h.mgr.Apply(context.Background(), "bad-cookie", MountOptions{Letter: "X:"})
	if err == nil {
		t.Fatal("a rejected candidate must fail the apply")
	}
	if len(h.persisted) != 0 {
		t.Errorf("persisted = %v, want nothing on validation failure (AC 3)", h.persisted)
	}
	if len(h.savedCfg) != 0 {
		t.Errorf("savedCfg = %v, want nothing on validation failure", h.savedCfg)
	}
	if len(h.started) != startsBefore {
		t.Errorf("restarts = %v, want none on validation failure", h.started[startsBefore:])
	}
	if old.stopped {
		// Validation failed: the healthy mount must never have been torn down.
		t.Error("the running mount must stay up when validation fails")
	}
	if !strings.Contains(err.Error(), "验证") {
		t.Errorf("err = %v, want a Chinese remedy", err)
	}
}

func TestApplyPersistsThenRestartsWithMergedOptions(t *testing.T) {
	h := newManagerHarness(t, MountOptions{Letter: "Q:", RootFID: "r1", RootPath: "/docs"})

	// A pure reauth applies no options: everything stays as it was (AC 3).
	if err := h.mgr.Apply(context.Background(), "good", MountOptions{}); err != nil {
		t.Fatalf("reauth apply: %v", err)
	}
	if got := h.persisted; len(got) != 1 || got[0] != "effective-cookie" {
		t.Errorf("persisted = %v, want [effective-cookie]", got)
	}
	if len(h.savedCfg) != 1 || h.savedCfg[0].Letter != "Q:" || h.savedCfg[0].RootFID != "r1" {
		t.Errorf("savedCfg = %+v, want the untouched choices", h.savedCfg)
	}
	if len(h.started) != 1 || h.started[0] != "effective-cookie" {
		t.Errorf("started = %v, want the effective cookie", h.started)
	}
	if got := h.optsSeen[0]; got.Letter != "Q:" || got.RootFID != "r1" {
		t.Errorf("restart options = %+v, want the previous ones", got)
	}

	// A wizard apply merges only the provided fields and normalizes the
	// letter.
	if err := h.mgr.Apply(context.Background(), "good", MountOptions{Letter: "x", RootFID: "r2", RootPath: "/r2"}); err != nil {
		t.Fatalf("wizard apply: %v", err)
	}
	if got := h.optsSeen[1]; got.Letter != "X:" || got.RootFID != "r2" {
		t.Errorf("restart options = %+v, want X:/r2", got)
	}
	if got := h.mgr.optsSnapshot(); got.Letter != "X:" || got.RootFID != "r2" {
		t.Errorf("manager opts = %+v, want X:/r2", got)
	}
}

func TestApplyRejectsInvalidLetterBeforePersisting(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	if err := h.mgr.Apply(context.Background(), "good", MountOptions{Letter: "55"}); err == nil {
		t.Fatal("an invalid letter must fail the apply")
	}
	if len(h.persisted) != 0 || len(h.started) != 0 {
		t.Errorf("persisted = %v started = %v, want nothing persisted for an invalid letter", h.persisted, h.started)
	}
}

func TestStartWithoutCredentialWaitsForWizard(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	if err := h.mgr.Start(context.Background(), ""); err != nil {
		t.Fatalf("Start with no cookie: %v", err)
	}
	if h.mgr.runnerSnapshot() != nil {
		t.Error("no runner should exist without a credential")
	}
}

// TestRecheckRestartsFromTheStoredCredential pins the ADR 0004 recovery
// action: a recheck re-runs the lifecycle from the current cookie with no
// validation round-trip and nothing persisted again.
func TestRecheckRestartsFromTheStoredCredential(t *testing.T) {
	h := newManagerHarness(t, MountOptions{Letter: "Q:"})
	if err := h.mgr.Start(context.Background(), "seed-cookie"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	old := h.mgr.runnerSnapshot()
	startsBefore := len(h.started)

	if err := h.mgr.Recheck(context.Background()); err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	if !old.stopped {
		t.Error("recheck must stop the current mount before restarting")
	}
	if len(h.started) != startsBefore+1 || h.started[startsBefore] != "seed-cookie" {
		t.Errorf("started = %v, want the same cookie reused", h.started)
	}
	if len(h.persisted) != 0 {
		t.Errorf("persisted = %v, want nothing on a recheck", h.persisted)
	}
	if got := h.mgr.optsSnapshot(); got.Letter != "Q:" {
		t.Errorf("opts = %+v, want untouched", got)
	}
}

func TestRecheckWithoutCredentialErrors(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	if err := h.mgr.Recheck(context.Background()); err == nil {
		t.Fatal("a recheck without a credential must point at the wizard")
	}
	if len(h.started) != 0 {
		t.Errorf("started = %v, want none", h.started)
	}
}

// TestStartWithoutCredentialPushesDisabled checks the tray surface learns the
// true state when no credential is configured: the disabled status reaches
// the update stream the tray renders.
func TestStartWithoutCredentialPushesDisabled(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	updates := h.mgr.Subscribe()
	if err := h.mgr.Start(context.Background(), ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case st := <-updates:
		if st.State != supervisor.StateDisabled {
			t.Fatalf("pushed status = %s, want Disabled", st.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Disabled status pushed")
	}
}

func TestStopWithoutRunnerIsSafe(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	h.mgr.Stop() // must not panic
}

// TestApplyStartFailureAfterPersist pins the restart ordering: the validated
// credential is already persisted when the new mount fails to come up, so the
// next start uses it, and the panel shows the failure instead of silently
// keeping a dead runner.
func TestApplyStartFailureAfterPersist(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	h.failStart = errors.New("port taken")

	err := h.mgr.Apply(context.Background(), "good", MountOptions{})
	if err == nil {
		t.Fatal("a failed start must surface")
	}
	if len(h.persisted) != 1 {
		t.Errorf("persisted = %v, want the validated credential kept", h.persisted)
	}
	if h.mgr.runnerSnapshot() != nil {
		t.Error("no runner may survive a failed start")
	}
	if !strings.Contains(h.mgr.log.Export(), "挂载启动失败") {
		t.Error("the failure should be in the diagnostic log")
	}
}

func TestPanelAssemblesTheChineseView(t *testing.T) {
	h := newManagerHarness(t, MountOptions{Letter: "Q:", RootFID: "r1", RootPath: "/docs"})
	if err := h.mgr.Start(context.Background(), "seed-cookie"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.lastRunner().cacheDir = `C:\cache\q`
	now := time.Now()
	h.lastRunner().push(supervisor.Status{State: supervisor.StateStarting, Phase: "adapter", Since: now.Add(-time.Second)})
	h.lastRunner().push(supervisor.Status{State: supervisor.StateMounted, Phase: "drive", Since: now})
	h.rec.RecordWritebackFailure("/doc.txt", quarkerr.New(quarkerr.CatUpstreamTransient, "upload", "boom"))

	p := h.mgr.Panel()
	if p.Status.Label != "已挂载" || p.Status.Remedy == "" {
		t.Errorf("status view = %+v, want the Chinese mounted view", p.Status)
	}
	if p.Status.Permanent {
		t.Error("Mounted must not read as permanent")
	}
	if p.Status.RootPath != "/docs" || p.MountPoint != "Q:" || p.CacheDir != `C:\cache\q` {
		t.Errorf("identity = mp %q root %q cache %q", p.MountPoint, p.Status.RootPath, p.CacheDir)
	}
	if len(p.Timeline) != 2 || p.Timeline[0].Label != "启动中" || p.Timeline[1].Label != "已挂载" {
		t.Errorf("timeline = %+v", p.Timeline)
	}
	if !p.Caps.Static.Write || p.Caps.Static.Copy {
		t.Errorf("static caps = %+v, want writes available, copy not", p.Caps.Static)
	}
	if len(p.Writeback) != 1 {
		t.Fatalf("writeback = %+v, want the recorded failure", p.Writeback)
	}
	w := p.Writeback[0]
	if w.CategoryLabel != "上游暂时不可用" || w.Permanent {
		t.Errorf("writeback view = %+v, want the transient Chinese category", w)
	}
	if p.Unsupported == "" {
		t.Error("the unsupported-capability notice must always be present (AC 5)")
	}
	if !p.HasCredential {
		t.Error("a started manager has a credential")
	}

	// A paused mount flips the display: user action needed, not a retry.
	h.lastRunner().push(supervisor.Status{State: supervisor.StateNeedsReauth, Phase: "ready", Err: "31001", Since: now})
	p = h.mgr.Panel()
	if !p.Status.Permanent || !p.Status.NeedsReauth {
		t.Errorf("NeedsReauth view = %+v, want permanent/needs-reauth flags", p.Status)
	}
}

func TestManagerSubscribeStreamsAcrossRestart(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	if err := h.mgr.Start(context.Background(), "seed-cookie"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	updates := h.mgr.Subscribe()

	h.lastRunner().push(supervisor.Status{State: supervisor.StateStarting, Phase: "adapter", Since: time.Now()})
	select {
	case st := <-updates:
		if st.State != supervisor.StateStarting {
			t.Fatalf("first update = %s", st.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no status update forwarded")
	}

	// Restart replaces the pump; the shared stream keeps flowing. The new
	// pump replays its runner's current status first, so drain until Mounted.
	if err := h.mgr.Apply(context.Background(), "good", MountOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	h.lastRunner().push(supervisor.Status{State: supervisor.StateMounted, Phase: "drive", Since: time.Now()})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case st := <-updates:
			if st.State == supervisor.StateMounted {
				return
			}
		case <-deadline:
			t.Fatal("no Mounted update after restart")
		}
	}
}

// runnerSnapshot exposes the manager's runner for the not-touched assertions.
func (m *Manager) runnerSnapshot() *fakeRunner {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, _ := m.runner.(*fakeRunner)
	return r
}

func (m *Manager) optsSnapshot() MountOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts
}
