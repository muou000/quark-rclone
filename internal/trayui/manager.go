package trayui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/muou000/quark-rclone/internal/diag"
	"github.com/muou000/quark-rclone/internal/quarkerr"
	"github.com/muou000/quark-rclone/internal/supervisor"
)

// MountRunner is the slice of a supervised mount the tray drives and reads.
// *supervisor.Supervisor satisfies it; tests substitute a fake so the reauth
// and restart semantics are testable without a real lifecycle.
type MountRunner interface {
	Status() supervisor.Status
	History() []supervisor.Status
	Subscribe() <-chan supervisor.Status
	Done() <-chan struct{}
	CacheDir() string
	MountPoint() string
	ReachedMounted() bool
	PauseReason() supervisor.State
	Stop()
}

// Source is what the configuration and reauth wizards need from the adapter,
// injected so this package never touches the wire. Both methods run against a
// throwaway credential: a failed validation must leave every durable store —
// the profile's DPAPI blob, the config, the dirty cache — untouched (issue
// #24, ADR 0003).
type Source interface {
	// Validate checks a candidate cookie and returns the effective one (the
	// possibly rotated value a restart should run with).
	Validate(ctx context.Context, candidate string) (effective string, err error)
	// ListDirs lists the drive's root directories so the wizard can offer a
	// root choice without the user knowing FIDs.
	ListDirs(ctx context.Context, cookie string) ([]RootDir, error)
}

// RootDir is one selectable root directory in the first-run wizard.
type RootDir struct {
	FID  string `json:"fid"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// MountOptions is what the wizards may change about a mount. Letter empty
// means the auto strategy; RootFID/RootPath name the exposed drive subtree.
type MountOptions struct {
	Letter   string `json:"letter"`
	RootFID  string `json:"root_fid"`
	RootPath string `json:"root_path"`
}

// merge returns the current options with every non-empty field of in applied.
// A reauth applies no options at all, which is exactly how the wizard keeps
// the profile and the root choice intact (issue #24).
func (m MountOptions) merge(in MountOptions) MountOptions {
	out := m
	if in.Letter != "" {
		out.Letter = in.Letter
	}
	if in.RootFID != "" {
		out.RootFID = in.RootFID
	}
	if in.RootPath != "" {
		out.RootPath = in.RootPath
	}
	return out
}

// normalize validates and canonicalises the wizard's drive choice: a letter
// must be a single A-Z character (colon optional); empty stays auto.
func (m MountOptions) normalize() (MountOptions, error) {
	out := m
	letter := strings.ToUpper(strings.TrimSpace(out.Letter))
	letter = strings.TrimSuffix(letter, ":")
	if letter == "" {
		out.Letter = ""
		return out, nil
	}
	if len(letter) != 1 || letter[0] < 'A' || letter[0] > 'Z' {
		return out, fmt.Errorf("盘符无效：%q（应为 A–Z 的单个字母，如 Q）", out.Letter)
	}
	out.Letter = letter + ":"
	return out, nil
}

// Manager owns the tray's mount across restarts: it starts the supervised
// lifecycle, drives the reauth flow (validate → persist → restart) and
// assembles the panel's view. The mount CLI has no counterpart because a CLI
// process ends with its mount; the tray outlives both.
type Manager struct {
	// src validates candidates and lists root directories (the wizards).
	src Source
	// start brings up one supervised mount; injection seam for tests.
	start func(ctx context.Context, effective string, opts MountOptions) (MountRunner, *diag.Recorder, error)
	// persist stores the effective credential after a successful validation.
	persist func(effective string) error
	// saveConfig stores the wizard's root and drive choices. Nil keeps the
	// choices in memory only (a tray running without a profile).
	saveConfig func(opts MountOptions) error

	mu         sync.Mutex
	runner     MountRunner
	rec        *diag.Recorder
	opts       MountOptions
	configured bool
	// cookie is the effective credential the current (or next) mount runs
	// with; Recheck needs it to re-run without a wizard round-trip.
	cookie  string
	log     *EventLog
	updates chan supervisor.Status
	// applyMu serializes the wizard flows against each other and against
	// Recheck: two concurrent applies must not interleave persistence and
	// restart.
	applyMu sync.Mutex
	stopped bool
}

// NewManager wires the manager. configured says whether a durable credential
// existed at process start (the panel's "已配置凭据" flag).
func NewManager(
	src Source,
	start func(ctx context.Context, effective string, opts MountOptions) (MountRunner, *diag.Recorder, error),
	persist func(effective string) error,
	saveConfig func(opts MountOptions) error,
	opts MountOptions,
	configured bool,
	log *EventLog,
) *Manager {
	return &Manager{
		src:        src,
		start:      start,
		persist:    persist,
		saveConfig: saveConfig,
		opts:       opts,
		configured: configured,
		log:        log,
		updates:    make(chan supervisor.Status, 16),
	}
}

// Subscribe streams the current mount's transitions, across restarts. Updates
// are dropped, never blocking, when the consumer is slow; Panel() always has
// the full truth.
func (m *Manager) Subscribe() <-chan supervisor.Status { return m.updates }

// CurrentOptions are the wizard's live choices.
func (m *Manager) CurrentOptions() MountOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts
}

// CurrentRunner is the running mount, nil when none is up.
func (m *Manager) CurrentRunner() MountRunner {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runner
}

// CurrentRunnerMountPoint is the resolved letter of the running mount, or ""
// when nothing is mounted.
func (m *Manager) CurrentRunnerMountPoint() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runner == nil {
		return ""
	}
	return m.runner.MountPoint()
}

// Start brings up the mount with the credential the profile already holds.
// An empty cookie starts nothing: the Disabled status is pushed so the tray
// reflects the true state, and the wizard takes over.
func (m *Manager) Start(ctx context.Context, cookie string) error {
	if cookie == "" {
		m.log.Add("托盘启动：尚无已保存的凭据，等待首次配置")
		select {
		case m.updates <- supervisor.Status{State: supervisor.StateDisabled, Since: time.Now()}:
		default:
		}
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(ctx, cookie)
}

// startLocked runs one supervised mount and pumps its state stream. The
// caller holds m.mu.
func (m *Manager) startLocked(ctx context.Context, cookie string) error {
	m.cookie = cookie
	runner, rec, err := m.start(ctx, cookie, m.opts)
	if err != nil {
		// The dead mount's snapshot is stale: the panel must fall back to the
		// static matrix instead of showing a stopped session's probes.
		m.rec = nil
		m.log.Add("挂载启动失败：%v", err)
		return err
	}
	m.runner = runner
	m.rec = rec
	m.configured = true
	m.log.Add("挂载已启动（盘符 %s，根目录 %s）", m.opts.displayLetter(), m.opts.RootPath)
	go m.pump(runner)
	return nil
}

// pump forwards one runner's transitions onto the shared stream and the log,
// for exactly as long as that runner lives. A restart replaces the pump; the
// old one retires with its runner's Done.
func (m *Manager) pump(runner MountRunner) {
	ch := runner.Subscribe()
	done := runner.Done()
	for {
		select {
		case st, ok := <-ch:
			if !ok {
				return
			}
			m.log.Add("状态：%s（%s）", StateLabel(st.State), st.Phase)
			select {
			case m.updates <- st:
			default:
			}
		case <-done:
			return
		}
	}
}

// Apply runs the shared configuration/reauth flow (issue #24): validate the
// candidate, and only after success touch the durable stores and restart the
// mount. opts names what the wizard changes; an empty MountOptions is the
// pure reauth, which keeps the root and drive choice intact.
//
// The ordering is the contract: a validation failure returns before anything
// is persisted or stopped; a persistence failure is logged but does not block
// the restart with the validated credential. Wizard flows and Recheck are
// serialized against each other.
func (m *Manager) Apply(ctx context.Context, candidate string, in MountOptions) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	if strings.TrimSpace(candidate) == "" {
		return errors.New("请先粘贴 Cookie")
	}
	effective, err := m.src.Validate(ctx, candidate)
	if err != nil {
		m.log.Add("凭据验证失败（旧凭据保持不变）：%v", err)
		return fmt.Errorf("Cookie 验证未通过：%w", err)
	}

	opts, err := m.opts.merge(in).normalize()
	if err != nil {
		m.log.Add("配置无效：%v", err)
		return err
	}

	if m.persist != nil {
		if err := m.persist(effective); err != nil {
			m.log.Add("凭据保存失败：%v", err)
			return fmt.Errorf("凭据保存失败：%w", err)
		}
	}
	if m.saveConfig != nil {
		if err := m.saveConfig(opts); err != nil {
			// The config write is best-effort, exactly like the mount CLI's
			// letter recording: a failed save must not undo a validated login.
			m.log.Add("配置保存失败（挂载继续）：%v", err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts = opts
	if m.runner != nil {
		// The clean restart order is the supervisor's own: unmount the
		// letter, stop rclone, stop the adapter. The dirty cache directory is
		// keyed by the letter and survives untouched (issue #23).
		m.runner.Stop()
		m.runner = nil
	}
	if err := m.startLocked(ctx, effective); err != nil {
		return err
	}
	m.log.Add("重新登录成功，挂载已重启")
	return nil
}

// Recheck re-runs the mount from its current credential: the tray's recovery
// action for WinFspRequired (after installing), DriveConflict (after freeing
// the letter) and a wedged writeback (ADR 0004's 重新检查). A fresh lifecycle
// re-runs preflight, readiness and the drive probe — that is the recheck. No
// validation happens: the stored credential is already trusted.
func (m *Manager) Recheck(ctx context.Context) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	m.mu.Lock()
	cookie := m.cookie
	m.mu.Unlock()
	if cookie == "" {
		return errors.New("尚无已保存的凭据，请先在设置页完成配置")
	}
	if m.stopRunner() {
		m.log.Add("重新检查：停止当前挂载后重试")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.startLocked(ctx, cookie); err != nil {
		return err
	}
	m.log.Add("重新检查完成，挂载已重启")
	return nil
}

// stopRunner tears the current mount down, if any. Reports whether there was
// one.
func (m *Manager) stopRunner() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runner == nil {
		return false
	}
	m.runner.Stop()
	m.runner = nil
	return true
}

// Stop tears the current mount down; the tray calls it on quit. It is safe
// with no mount running.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	if m.runner != nil {
		m.runner.Stop()
		m.runner = nil
	}
}

// Panel assembles everything the main panel renders (issue #24): the Chinese
// status with its remedy, the recovery timeline, the capability snapshot with
// the writeback ledger, the wizard's current choices and the log tail.
func (m *Manager) Panel() PanelData {
	m.mu.Lock()
	runner, rec, opts, configured := m.runner, m.rec, m.opts, m.configured
	m.mu.Unlock()

	data := PanelData{
		Opts:          opts,
		HasCredential: configured,
	}

	st := supervisor.Status{State: supervisor.StateDisabled}
	if runner != nil {
		st = runner.Status()
		data.CacheDir = runner.CacheDir()
		data.MountPoint = runner.MountPoint()
	}
	data.Status = StatusView{
		State:     string(st.State),
		Label:     StateLabel(st.State),
		Remedy:    StateRemedy(st.State),
		Phase:     st.Phase,
		Err:       st.Err,
		Since:     st.Since,
		NextRetry: st.NextRetry,
		// A paused state waits for the user; everything else recovers on its
		// own. "临时错误"与"永久失败"在这里分家（issue #24, ADR 0010）。
		Permanent:   st.State.Paused(),
		RootPath:    opts.RootPath,
		NeedsReauth: st.State == supervisor.StateNeedsReauth,
	}

	for _, h := range runnerHistory(runner) {
		data.Timeline = append(data.Timeline, TimelineView{
			State: string(h.State), Label: StateLabel(h.State),
			Phase: h.Phase, Err: h.Err, Since: h.Since,
		})
	}

	snap := diag.Snapshot{Static: diag.StaticCapsOf()}
	if rec != nil {
		snap = rec.Snapshot()
	}
	data.Caps = snap
	for _, w := range snap.Writeback {
		cat := quarkerr.Category(w.Category)
		data.Writeback = append(data.Writeback, WritebackView{
			Path: w.Path, Category: w.Category,
			CategoryLabel: CategoryLabel(cat),
			Permanent:     CategoryIsPermanent(cat),
			Retries:       w.Retries, NextRetry: w.NextRetry, Err: w.Err,
		})
	}
	data.Unsupported = UnsupportedNotice

	data.Log = m.log.Entries()
	return data
}

// runnerHistory tolerates a nil runner.
func runnerHistory(r MountRunner) []supervisor.Status {
	if r == nil {
		return nil
	}
	return r.History()
}

// displayLetter renders the wizard's letter choice for the log: auto stays
// readable as 自动 instead of an empty string.
func (o MountOptions) displayLetter() string {
	if o.Letter == "" {
		return "自动"
	}
	return o.Letter
}
