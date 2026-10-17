// Command quark-tray is the Chinese-first tray front end (issue #24): one
// mount session supervised in-process, a native tray icon and menu, and a
// loopback web panel carrying the main view, the diagnostics view and the
// configuration/reauth wizards (ADR 0012).
//
// The shell is deliberately thin: every screen, label and flow lives in
// internal/trayui, every lifecycle rule in internal/supervisor, and this
// command only wires them together and renders. The tray runs the mount CLI's
// profile mode only — a tray that could not persist a re-validated credential
// could not keep its reauth promise.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"fyne.io/systray"

	"github.com/muou000/quark-rclone/internal/adapter"
	"github.com/muou000/quark-rclone/internal/diag"
	"github.com/muou000/quark-rclone/internal/mountapp"
	"github.com/muou000/quark-rclone/internal/preflight"
	"github.com/muou000/quark-rclone/internal/profile"
	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/session"
	"github.com/muou000/quark-rclone/internal/supervisor"
	"github.com/muou000/quark-rclone/internal/trayui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "quark-tray: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("quark-tray", flag.ContinueOnError)
	useProfile := fs.Bool("profile", true, "使用 profile 的 DPAPI 凭据库（托盘必选）")
	profileDir := fs.String("profile-dir", "", "覆盖 profile 目录（诊断用）")
	port := fs.Int("port", 17385, "适配层固定回环端口")
	panelPort := fs.Int("panel-port", 0, "主面板回环端口（0 为随机）")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if !*useProfile {
		return errors.New("托盘需要 profile 模式（-profile）：重新登录必须能持久化新凭据")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := openProfileStore(*profileDir)
	if err != nil {
		return err
	}
	cfg, outcome, err := store.LoadConfig()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "profile: %s (%s)\n", store.ConfigPath(), outcome)

	cookie, cookieErr := store.LoadCookie()
	hasCredential := cookieErr == nil && cookie != ""
	if cookieErr != nil && !os.IsNotExist(cookieErr) {
		fmt.Fprintf(os.Stderr, "quark-tray: 读取凭据失败：%v（请在设置页重新登录）\n", cookieErr)
		cookie = ""
	}

	log := trayui.NewEventLog()
	log.Add("托盘启动（端口 %d）", *port)

	manager := trayui.NewManager(
		panelSource{},
		startFunc(store, cfg, *port),
		func(effective string) error { return store.SaveCookie(effective) },
		saveConfigFunc(store),
		initialOptions(cfg),
		hasCredential,
		log,
	)
	if err := manager.Start(ctx, cookie); err != nil {
		// A failed first mount is a panel concern, not a tray crash: the
		// supervisor's state machine already classified it.
		fmt.Fprintf(os.Stderr, "quark-tray: 初始挂载未完成：%v\n", err)
	}

	token, err := newPanelToken()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *panelPort))
	if err != nil {
		return fmt.Errorf("主面板监听失败：%w", err)
	}
	panelURL := "http://" + listener.Addr().String() + "/?t=" + token
	srv := &http.Server{Handler: trayui.NewServer(manager, token).Handler()}
	go func() { _ = srv.Serve(listener) }()
	log.Add("主面板就绪：%s", panelURL)

	go watchMount(ctx, manager, store, panelURL)

	systray.Run(func() { onReady(ctx, manager, panelURL) }, func() { onExit(ctx, cancel, manager, srv) })
	return nil
}

// initialOptions seeds the wizard with the profile's current choices.
func initialOptions(cfg profile.Config) trayui.MountOptions {
	opts := trayui.MountOptions{RootFID: cfg.Quark.RootFID, RootPath: cfg.Quark.RootPath}
	if cfg.Drive.Mode == profile.DriveExplicit && cfg.Drive.ExplicitLetter != "" {
		opts.Letter = cfg.Drive.ExplicitLetter + ":"
	}
	return opts
}

// startFunc brings up one supervised mount for the manager. The auto letter
// is resolved from the freshly loaded profile, so a wizard's drive-mode
// change takes effect on the very next restart.
func startFunc(store *profile.Store, cfg profile.Config, port int) func(context.Context, string, trayui.MountOptions) (trayui.MountRunner, *diag.Recorder, error) {
	return func(ctx context.Context, effective string, opts trayui.MountOptions) (trayui.MountRunner, *diag.Recorder, error) {
		// The wizard may have updated the stored config since startup.
		if loaded, _, err := store.LoadConfig(); err == nil {
			cfg = loaded
		}
		mountPoint := opts.Letter
		if mountPoint == "" {
			mountPoint = cfg.MountPoint()
		}
		rootFID := opts.RootFID
		if rootFID == "" {
			rootFID = cfg.Quark.RootFID
		}
		sup, rec := mountapp.Start(ctx, mountapp.Options{
			Port:           port,
			Cookie:         effective,
			Persist:        func(c string) { _ = store.SaveCookie(c) },
			RootFID:        rootFID,
			MountPoint:     mountPoint,
			CacheMode:      cfg.Writes.CacheMode,
			WriteBack:      cfg.Writes.WriteBack,
			WriteChunkSize: cfg.Writes.WriteChunkSize,
			CacheRoot:      mountapp.ResolveCacheRoot(store.Dir(), cfg.CacheDir),
			BundleDir:      preflight.DefaultBundleDir(),
			StatePath:      filepath.Join(store.Dir(), "state.json"),
			ProfileDir:     store.Dir(),
		})
		return runnerAdapter{sup}, rec, nil
	}
}

// runnerAdapter fits *supervisor.Supervisor to trayui.MountRunner: the tray
// reads the final state through Status/Done, so Stop's return value is noise.
type runnerAdapter struct{ *supervisor.Supervisor }

func (r runnerAdapter) Stop() { _ = r.Supervisor.Stop() }

// saveConfigFunc merges the wizard's choices into the stored profile. Letter
// empty means the auto strategy; a letter pins explicit mode.
func saveConfigFunc(store *profile.Store) func(trayui.MountOptions) error {
	return func(opts trayui.MountOptions) error {
		cfg, _, err := store.LoadConfig()
		if err != nil {
			return err
		}
		if opts.Letter == "" {
			cfg.Drive.Mode = profile.DriveAuto
			cfg.Drive.ExplicitLetter = ""
		} else {
			cfg.Drive.Mode = profile.DriveExplicit
			cfg.Drive.ExplicitLetter = strings.TrimSuffix(opts.Letter, ":")
		}
		if opts.RootFID != "" {
			cfg.Quark.RootFID = opts.RootFID
			cfg.Quark.RootPath = opts.RootPath
		}
		return store.SaveConfig(cfg)
	}
}

// watchMount follows the mount's transitions for the tray surface and the
// letter recording, exactly what the mount CLI prints on stderr.
func watchMount(ctx context.Context, manager *trayui.Manager, store *profile.Store, panelURL string) {
	updates := manager.Subscribe()
	for {
		select {
		case st, ok := <-updates:
			if !ok {
				return
			}
			if st.State == supervisor.StateMounted && manager.CurrentOptions().Letter == "" {
				// Auto recovery remembers the letter this session actually
				// held (ADR 0005).
				recordLastLetter(store, manager)
			}
		case <-ctx.Done():
			return
		}
	}
}

// recordLastLetter persists the resolved letter of a session that reached
// Mounted.
func recordLastLetter(store *profile.Store, manager *trayui.Manager) {
	runner := manager.CurrentRunner()
	if runner == nil {
		return
	}
	letter := runner.MountPoint()
	if letter == "" {
		return
	}
	cfg, _, err := store.LoadConfig()
	if err != nil {
		return
	}
	cfg.Drive.LastLetter = profile.NormalizeDriveLetter(letter)
	if err := store.SaveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "quark-tray: could not save profile: %v\n", err)
	}
}

// onReady builds the tray icon and menu: open the panel, reauth (panel),
// recheck (a fresh lifecycle from the stored credential) and quit. The panel
// is the surface for every flow (ADR 0012); the menu carries the high-frequency
// recovery action of ADR 0004.
func onReady(ctx context.Context, manager *trayui.Manager, panelURL string) {
	systray.SetIcon(trayui.Icon(trayui.IconInactive))
	systray.SetTooltip("夸克网盘挂载 — 未运行")

	mOpen := systray.AddMenuItem("打开主面板", "在浏览器中打开主面板")
	mReauth := systray.AddMenuItem("重新登录", "登录失效时从主面板重新登录")
	mRecheck := systray.AddMenuItem("重新检查", "重新运行预检与挂载（安装 WinFsp、释放盘符或手动重试后使用）")
	systray.AddSeparator()
	mStatus := systray.AddMenuItem("状态：未运行", "")
	mStatus.Disable()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "停止挂载并退出")

	openPanel := func() {
		// The system URL handler; rundll32 keeps this free of shell deps.
		if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", panelURL).Start(); err != nil {
			fmt.Fprintf(os.Stderr, "quark-tray: 打开主面板失败：%v（请手动访问 %s）\n", err, panelURL)
		}
	}
	openPanel()

	go func() {
		for range mOpen.ClickedCh {
			openPanel()
		}
	}()
	go func() {
		for range mReauth.ClickedCh {
			openPanel()
		}
	}()
	go func() {
		for range mRecheck.ClickedCh {
			// The ADR 0004 recovery action: a fresh lifecycle re-runs
			// preflight, readiness and the drive probe.
			if err := manager.Recheck(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "quark-tray: 重新检查失败：%v\n", err)
			}
		}
	}()
	go func() {
		for range mQuit.ClickedCh {
			systray.Quit()
		}
	}()

	updates := manager.Subscribe()
	go func() {
		for st := range updates {
			kind := trayui.KindOf(st.State)
			systray.SetIcon(trayui.Icon(trayui.IconForState(kind)))
			line := "夸克网盘挂载 — " + trayui.StateLabel(st.State)
			if mp := manager.CurrentRunnerMountPoint(); mp != "" {
				line += " (" + mp + ")"
			}
			systray.SetTooltip(line)
			mStatus.SetTitle("状态：" + trayui.StateLabel(st.State))
		}
	}()
}

// onExit tears the mount down in the supervisor's own order and stops the
// panel server.
func onExit(ctx context.Context, cancel context.CancelFunc, manager *trayui.Manager, srv *http.Server) {
	manager.Stop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	cancel()
	<-ctx.Done()
}

// panelSource implements the wizard's adapter needs against throwaway
// credentials: validation and root listing never persist anything.
type panelSource struct{}

func (panelSource) Validate(ctx context.Context, candidate string) (string, error) {
	store := session.NewStore("", nil)
	svc := adapter.New(quarkapi.New(store, quarkapi.Config{}, nil), store)
	if err := svc.ValidateAndActivate(ctx, candidate); err != nil {
		return "", err
	}
	return store.CookieSnapshot(), nil
}

func (panelSource) ListDirs(ctx context.Context, cookie string) ([]trayui.RootDir, error) {
	store := session.NewStore(cookie, nil)
	svc := adapter.New(quarkapi.New(store, quarkapi.Config{}, nil), store)
	files, err := svc.List(ctx, quarkapi.RootFID)
	if err != nil {
		return nil, err
	}
	dirs := make([]trayui.RootDir, 0, len(files))
	for _, f := range files {
		if !f.IsDir() {
			continue
		}
		dirs = append(dirs, trayui.RootDir{
			FID:  f.FID,
			Name: f.Name,
			Path: "/" + f.Name,
		})
	}
	return dirs, nil
}

// newPanelToken generates the per-process panel token.
func newPanelToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// openProfileStore resolves the profile directory and opens the store.
func openProfileStore(override string) (*profile.Store, error) {
	dir := override
	if dir == "" {
		var err error
		if dir, err = profile.DefaultDir(); err != nil {
			return nil, err
		}
	}
	return profile.Open(dir), nil
}
