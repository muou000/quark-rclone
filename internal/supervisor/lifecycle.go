package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/muou000/quark-rclone/internal/diag"
	"github.com/muou000/quark-rclone/internal/preflight"
	"github.com/muou000/quark-rclone/internal/redact"
)

// SupervisorConfig is one supervised mount's wiring.
type SupervisorConfig struct {
	// Adapter endpoint session.
	Port    int
	Cookie  string
	Persist func(cookie string)
	RootFID string

	// rclone child process. An empty RclonePath means the bundled
	// RcloneExeName inside BundleDir.
	BundleDir      string
	RclonePath     string
	MountPoint     string
	CacheMode      string
	WriteBack      string
	WriteChunkSize string
	CacheDir       string

	// Behaviour. An empty BackoffSchedule takes the ADR 0004 cadence; a zero
	// ShutdownTimeout takes 15s; an empty StatePath disables persistence.
	BackoffSchedule []time.Duration
	ShutdownTimeout time.Duration
	StatePath       string

	// Diagnostics (issue #22). Recorder is the capability/writeback ledger
	// the adapter endpoint shares; ProbeCaps runs the side-effect-free
	// capability probes after readiness succeeded. Probe failures are
	// recorded, never fatal — diagnostics must not gate the mount.
	Recorder  *diag.Recorder
	ProbeCaps func(ctx context.Context)

	// Injection seams. Production leaves them nil.
	Preflight    func() error
	ReadyProbe   func(ctx context.Context) error
	ProbeDrive   func(mountPoint string) bool
	LaunchRclone func(ctx context.Context, configPath string) (RcloneProcess, error)
}

// RcloneProcess is the slice of the rclone child the supervisor needs. The
// seam exists so lifecycle tests can drive the state machine without a real
// rclone.
type RcloneProcess interface {
	Wait() error
	Kill() error
}

// Supervisor owns one mount's whole lifecycle: the adapter endpoint, the
// rclone child and the drive letter, in the ADR 0001/0004 order — adapter
// healthy, then rclone, then the letter reachable, then Mounted. One
// supervisor runs exactly one mount: a second instance would be a duplicate
// instance, which the contract forbids.
type Supervisor struct {
	cfg    SupervisorConfig
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// Lifecycle-owned state, touched only by the run goroutine. They need no
	// lock: the run goroutine is the single writer, and Stop reads them only
	// after done closes.
	sess     *Session
	rclone   RcloneProcess
	procDone chan error
	// rcloneExitText holds the dead child's captured stderr for the failure
	// classification that follows its exit.
	rcloneExitText string

	mu          sync.Mutex
	status      Status
	subs        map[chan Status]struct{}
	final       Status
	mountedSeen bool
	// pauseReason remembers the paused state the lifecycle parked in, so the
	// caller can tell "stopped cleanly" from "stopped because the user must
	// act" even though both end at Disabled.
	pauseReason State
}

// Supervise starts the lifecycle and returns immediately. The caller reads
// Status/Subscribe for progress and calls Stop (or cancels the context) to
// end it; Wait blocks until the final state is reached.
func Supervise(ctx context.Context, cfg SupervisorConfig) *Supervisor {
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}
	runCtx, cancel := context.WithCancel(ctx)
	s := &Supervisor{
		cfg:    cfg,
		ctx:    runCtx,
		cancel: cancel,
		done:   make(chan struct{}),
		subs:   map[chan Status]struct{}{},
	}
	go s.run()
	return s
}

// Status is the current snapshot.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Subscribe receives every state transition from now on, starting with the
// current one. A slow consumer drops updates rather than blocking the
// lifecycle; Status() always has the truth.
func (s *Supervisor) Subscribe() <-chan Status {
	ch := make(chan Status, 16)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	current := s.status
	s.mu.Unlock()
	select {
	case ch <- current:
	default:
	}
	return ch
}

// Wait blocks until the lifecycle reaches its final state and returns it.
func (s *Supervisor) Wait() Status {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.final
}

// Done closes when the lifecycle reaches its final state; stream consumers
// select on it to stop printing.
func (s *Supervisor) Done() <-chan struct{} { return s.done }

// ReachedMounted reports whether the lifecycle ever held a mounted drive —
// the precondition for recording the last-used letter (ADR 0005).
func (s *Supervisor) ReachedMounted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mountedSeen
}

// PauseReason reports the paused state the lifecycle parked in, or Disabled
// when it stopped cleanly. The tray and the CLI need this distinction at the
// process boundary, where every terminal state is otherwise Disabled.
func (s *Supervisor) PauseReason() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pauseReason.Paused() {
		return s.pauseReason
	}
	return StateDisabled
}

// Stop cancels the lifecycle and blocks until the shutdown sequence —
// unmount, stop rclone, stop adapter — has finished. It returns the final
// state, which is Failed when the shutdown itself timed out.
func (s *Supervisor) Stop() Status {
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.final
}

// transition records one state change, notifies subscribers and persists it.
// The error text is redacted before it lands anywhere.
func (s *Supervisor) transition(state State, phase, errMsg string, nextRetry ...time.Time) {
	st := Status{State: state, Phase: phase, Since: time.Now()}
	if errMsg != "" {
		st.Err = truncate(redact.Text(errMsg), 200)
	}
	if len(nextRetry) > 0 {
		st.NextRetry = nextRetry[0]
	}
	s.mu.Lock()
	s.status = st
	if state == StateMounted {
		s.mountedSeen = true
	}
	for ch := range s.subs {
		select {
		case ch <- st:
		default:
		}
	}
	s.mu.Unlock()
	if s.cfg.StatePath != "" {
		if err := persistStatus(s.cfg.StatePath, st); err != nil {
			fmt.Fprintf(os.Stderr, "supervisor: could not persist state: %v\n", err)
		}
	}
}

// truncate shortens a diagnostic string without cutting a rune in half.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// run is the lifecycle goroutine. On the way out the last observed status
// becomes the final one every Waiter reads.
func (s *Supervisor) run() {
	defer func() {
		s.mu.Lock()
		s.final = s.status
		s.mu.Unlock()
		close(s.done)
	}()

	s.transition(StateStarting, "preflight", "")
	if err := s.runPreflight(); err != nil {
		// The verdict is structured for the production gates; an injected
		// probe's text is classified by the driver's own name. Both pause
		// retrying (ADR 0004).
		s.stopLifecycle(s.preflightVerdict(err), "preflight", err.Error())
		return
	}

	backoff := NewBackoff(s.cfg.BackoffSchedule)
	for {
		state := s.runAttempt()
		switch state {
		case StateMounted:
			if !s.monitor() {
				return // shutdown completed inside monitor
			}
			// Monitor saw a child die: loop into a fresh attempt.
		case StateWaitingForNetwork:
			// The far side was unreachable; wait out the schedule and retry.
			d := backoff.Next()
			s.transition(StateWaitingForNetwork, "retry", "远端暂不可达，稍后重试。", time.Now().Add(d))
			if !s.sleep(d) {
				s.shutdown()
				return
			}
		default:
			// A paused state: NeedsReauth, DriveConflict, WinFspRequired or
			// Failed. Retry would be futile; wait for Stop (ADR 0004).
			s.stopLifecycle(state, s.status.Phase, s.status.Err)
			return
		}
	}
}

// runPreflight runs the runtime gates. The injected probe wins; production
// checks the bundled rclone and WinFsp and classifies from the structured
// check results, not from message text.
func (s *Supervisor) runPreflight() error {
	if s.cfg.Preflight != nil {
		return s.cfg.Preflight()
	}
	if rc := preflight.CheckRclone(s.ctx, s.cfg.BundleDir, nil); !rc.OK {
		return errors.New(rc.Detail)
	}
	if wc := preflight.CheckWinFsp(); !wc.OK {
		return errors.New(wc.Detail)
	}
	return nil
}

// preflightVerdict maps a preflight failure onto its paused state: the WinFsp
// gate's verdict is structured, so an injected probe's text is the only
// heuristic left, and it keys on the driver's own name.
func (s *Supervisor) preflightVerdict(err error) State {
	if strings.Contains(strings.ToLower(errText(err)), "winfsp") {
		return StateWinFspRequired
	}
	return StateFailed
}

// runAttempt builds one mount: adapter session, readiness, rclone, letter.
// It returns the state the attempt ended in — Mounted, or the classified
// failure state for the loop to act on.
//
// The adapter session is reused across attempts: a far-side failure leaves
// the healthy endpoint up (ADR 0003 keeps the adapter through
// NeedsReauth), and a fresh Start on the same fixed port would collide with
// the still-bound listener.
func (s *Supervisor) runAttempt() State {
	if err := s.ensureAdapter(); err != nil {
		return StateFailed
	}

	// Real readiness (ADR 0004): the provider's /config through the session.
	// A failure here is the far side — authentication or network — so the
	// adapter stays up and only the retry loop acts.
	if s.cfg.ReadyProbe != nil {
		s.transition(StateStarting, "ready", "")
		if err := s.cfg.ReadyProbe(s.ctx); err != nil {
			return s.classifyFarSide("ready", err)
		}
	}

	// The capability probes run after readiness (ADR 0010: startup and
	// re-authentication probe without side effects) and never block or fail
	// the mount — their outcomes land in the snapshot, that is all.
	s.runProbes()

	// The letter must be free before rclone is asked to take it: a volume
	// already mounted there would make the visibility probe succeed
	// instantly and fake a healthy mount.
	s.transition(StateStarting, "rclone", "")
	if s.probeDrive(s.cfg.MountPoint) {
		s.transition(StateDriveConflict, "rclone",
			fmt.Sprintf("盘符 %s 已被其它卷占用：请更换盘符后重试。", s.cfg.MountPoint))
		return StateDriveConflict
	}
	proc, err := s.launchRclone(s.ctx, s.sess.ConfigPath())
	if err != nil {
		return s.classifyFarSide("rclone", err)
	}
	s.rclone = proc
	procDone := make(chan error, 1)
	go func() { procDone <- proc.Wait() }()
	s.procDone = procDone
	s.rcloneExitText = ""

	// The letter becomes reachable within seconds of a healthy mount; a
	// driver or letter problem shows up as the process exiting early.
	s.transition(StateStarting, "drive", "")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if s.probeDrive(s.cfg.MountPoint) {
			s.transition(StateMounted, "drive", "")
			return StateMounted
		}
		select {
		case err := <-s.procDone:
			s.rcloneExitText = errText(err)
			return s.classifyMountDeath()
		case <-s.ctx.Done():
			return StateStopping
		default:
		}
		if time.Now().After(deadline) {
			s.rcloneExitText = "mount did not appear within 30s"
			return s.classifyMountDeath()
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ensureAdapter starts the endpoint session when none is running, or keeps
// the live one. It is what makes a retry cheap and a restart collision
// impossible.
func (s *Supervisor) ensureAdapter() error {
	if s.sess != nil {
		return nil
	}
	s.transition(StateStarting, "adapter", "")
	sess, err := Start(s.ctx, Config{
		Port:     s.cfg.Port,
		Cookie:   s.cfg.Cookie,
		Persist:  s.cfg.Persist,
		RootFID:  s.cfg.RootFID,
		Recorder: s.cfg.Recorder,
	})
	if err != nil {
		s.transition(StateFailed, "adapter", fmt.Sprintf("适配器启动失败：%v", err))
		return err
	}
	s.sess = sess
	return nil
}

// runProbes refreshes the capability snapshot after readiness (issue #22,
// ADR 0010). A stopped supervisor records nothing: the lifecycle is on its
// way out, not starting up.
func (s *Supervisor) runProbes() {
	if s.cfg.ProbeCaps == nil || s.ctx.Err() != nil {
		return
	}
	s.cfg.ProbeCaps(s.ctx)
}

// monitor watches a mounted session until a child dies or shutdown begins.
// It returns false when the lifecycle has ended (shutdown ran), true when
// the caller should loop into a fresh attempt.
func (s *Supervisor) monitor() bool {
	select {
	case err := <-s.procDone:
		// rclone died on its own. Clean the letter before restarting so no
		// stale instance survives (issue #21: no leftovers, no duplicates).
		s.rcloneExitText = errText(err)
		s.transition(StateRecovering, "rclone exited", s.rcloneExitText)
		s.teardownRclone()
		return true
	case err := <-s.sess.Done():
		// The endpoint stopped serving; the rclone it fed is now dead weight.
		s.transition(StateRecovering, "adapter exited", errText(err))
		s.teardownRclone()
		s.sess.Stop()
		s.sess = nil
		return true
	case <-s.ctx.Done():
		s.shutdown()
		return false
	}
}

// shutdown is the exit sequence of ADR 0004: unmount the letter, stop
// rclone, stop the adapter. A letter that refuses to disappear inside the
// timeout leaves a diagnosable Failed state instead of pretending. When no
// rclone of ours ever ran — a paused state before the first launch — there
// is nothing to wait for: the letter belongs to another volume.
func (s *Supervisor) shutdown() {
	s.transition(StateStopping, "unmount", "")
	if s.rclone != nil {
		_ = s.rclone.Kill()
		s.rclone = nil
		if !s.waitForDriveGone() {
			s.transition(StateFailed, "shutdown",
				fmt.Sprintf("盘符 %s 在 %s 内未消失：可能有进程仍占用挂载；请检查后手动卸载。", s.cfg.MountPoint, s.cfg.ShutdownTimeout))
			s.stopSession()
			return
		}
	}
	s.stopSession()
	s.transition(StateDisabled, "stopped", "")
}

// stopSession drains the endpoint session, tolerating a nil or already-dead
// one.
func (s *Supervisor) stopSession() {
	if s.sess == nil {
		return
	}
	s.sess.Stop()
	s.sess = nil
}

// teardownRclone kills a running (or half-dead) rclone and waits for the
// letter to disappear before the next attempt may reuse it.
func (s *Supervisor) teardownRclone() {
	if s.rclone != nil {
		_ = s.rclone.Kill()
		s.rclone = nil
	}
	s.waitForDriveGone()
}

// waitForDriveGone polls until the letter is free or the shutdown timeout
// expires.
func (s *Supervisor) waitForDriveGone() bool {
	deadline := time.Now().Add(s.cfg.ShutdownTimeout)
	for s.probeDrive(s.cfg.MountPoint) {
		if time.Now().After(deadline) || s.ctx.Err() != nil {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
	return true
}

// sleep waits d, returning false when the supervisor was stopped first.
func (s *Supervisor) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// probeDrive defaults to the filesystem visibility check.
func (s *Supervisor) probeDrive(mountPoint string) bool {
	if s.cfg.ProbeDrive != nil {
		return s.cfg.ProbeDrive(mountPoint)
	}
	_, err := os.Stat(mountPoint + `\`)
	return err == nil
}

// launchRclone defaults to exec'ing the bundled binary with the profile's
// write-back parameters.
func (s *Supervisor) launchRclone(ctx context.Context, configPath string) (RcloneProcess, error) {
	if s.cfg.LaunchRclone != nil {
		return s.cfg.LaunchRclone(ctx, configPath)
	}
	path := s.cfg.RclonePath
	if path == "" {
		path = filepath.Join(s.cfg.BundleDir, preflight.RcloneExeName)
	}
	cmd := exec.CommandContext(ctx, path, s.rcloneArgs(configPath)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, stderr: &stderr}, nil
}

// rcloneArgs assembles the mount command: the temporary session config
// supplies the credential, the profile's write-back settings drive the
// mount (issue #19).
func (s *Supervisor) rcloneArgs(configPath string) []string {
	args := []string{"mount", "quark:", s.cfg.MountPoint, "--config", configPath}
	if s.cfg.CacheMode != "" {
		args = append(args, "--vfs-cache-mode", s.cfg.CacheMode)
	}
	if s.cfg.WriteBack != "" {
		args = append(args, "--vfs-write-back", s.cfg.WriteBack)
	}
	if s.cfg.WriteChunkSize != "" {
		args = append(args, "--vfs-write-chunk-size", s.cfg.WriteChunkSize)
	}
	if s.cfg.CacheDir != "" {
		args = append(args, "--cache-dir", s.cfg.CacheDir)
	}
	return args
}

// stopLifecycle parks the supervisor in a paused state: the status is
// terminal until Stop, and the adapter session — when one exists — stays up
// for diagnostics (ADR 0003: NeedsReauth keeps the adapter and the profile).
// The reason is remembered so a later Stop reports why, not just that.
func (s *Supervisor) stopLifecycle(state State, phase, errMsg string) {
	s.mu.Lock()
	if state.Paused() {
		s.pauseReason = state
	}
	s.mu.Unlock()
	s.transition(state, phase, errMsg)
	<-s.ctx.Done()
	s.shutdown()
}

// classifyFarSide sorts a readiness or launch failure: the provider's
// authentication signatures (issue #2's 31001/need login, plus an explicit
// HTTP 401) are NeedsReauth and pause; everything else is the network, which
// retries.
func (s *Supervisor) classifyFarSide(phase string, err error) State {
	lower := strings.ToLower(errText(err))
	if strings.Contains(lower, "31001") || strings.Contains(lower, "need login") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "http 401") || strings.Contains(lower, "401 unauthorized") {
		s.transition(StateNeedsReauth, phase, errText(err))
		return StateNeedsReauth
	}
	s.transition(StateWaitingForNetwork, phase, errText(err))
	return StateWaitingForNetwork
}

// classifyMountDeath sorts an early mount death the same way, and always
// tears the dead process down first so no instance survives.
func (s *Supervisor) classifyMountDeath() State {
	s.teardownRclone()
	return s.classifyFarSide("rclone", errors.New(s.rcloneExitText))
}

// errText renders an error for classification and reporting; a nil error is
// the empty string.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// execProcess adapts an os/exec child to RcloneProcess, carrying the
// captured stderr so a failed Wait can be classified.
type execProcess struct {
	cmd    *exec.Cmd
	stderr *strings.Builder
}

// Wait blocks until the child exits. A non-zero exit carries the captured
// stderr, because rclone's diagnosis lives there.
func (p *execProcess) Wait() error {
	err := p.cmd.Wait()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(p.stderr.String()), err)
	}
	return nil
}

// Kill terminates the child.
func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}
