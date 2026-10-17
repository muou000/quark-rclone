// Command quark-adapter is a standalone harness for the Quark adapter core.
//
// It exists so the adapter can be exercised end-to-end without a mount: import
// a cookie, then drive every operation the adapter exposes against the real
// provider. It is deliberately thin — every behaviour lives in the tested
// packages — and deliberately quiet about credentials: the cookie never
// appears in argv, in output, or in errors.
//
// Cookie handling follows ADR 0003's shape as far as a stateless tool can:
// the candidate is read from stdin (piped or typed, never a flag), and the
// effective cookie is written to a file the caller names. DPAPI encryption of
// that file is a separate work item; until then the caller picks a location
// with the same care as an SSH key.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/muou000/quark-rclone/internal/adapter"
	"github.com/muou000/quark-rclone/internal/dav"
	"github.com/muou000/quark-rclone/internal/diag"
	"github.com/muou000/quark-rclone/internal/preflight"
	"github.com/muou000/quark-rclone/internal/profile"
	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/quarkerr"
	"github.com/muou000/quark-rclone/internal/redact"
	"github.com/muou000/quark-rclone/internal/session"
	"github.com/muou000/quark-rclone/internal/supervisor"
)

// defaultLoopbackPort is the fixed port the loopback endpoint binds when the
// caller does not choose one. ADR 0009 fixes the port for supervisor and
// diagnostic predictability; any uncommon free port serves.
const defaultLoopbackPort = 17385

const usage = `quark-adapter — standalone harness for the Quark adapter core

usage:
  quark-adapter <command> [arguments]

commands:
  import        read a candidate cookie on stdin, validate it, and activate it
  list          list a directory by FID (default: the drive root)
  download      mint a download URL for a FID
  mkdir         create a directory
  upload        upload a local file
  rename        rename an object
  move          move an object to a new parent
  delete        delete an object by FID
  refresh       force a session cookie refresh now
  keepalive     run the periodic session refresh until interrupted
  serve         serve the loopback WebDAV endpoint until interrupted
  mount         run one mount session: endpoint + temporary rclone config
  profile       print the mount profile and credential-store state
  doctor        run the WinFsp/rclone runtime pre-check (issue #20)
  capabilities  print the backend capability statement
  probe-write   run the manual write diagnostic and print the capability snapshot (issue #22)

common flags:
  -cookie-file path   file holding the active cookie (import writes it)
  -port number        fixed loopback port for serve/mount (default 17385)
  -profile            import/mount use the profile's DPAPI credential store
  -profile-dir path   override the profile directory (diagnostics)
  -bundle-dir path    override the runtime bundle directory (doctor/mount)
`

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("missing command"), usage)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, cancel := signalContext()
	defer cancel()

	if err := run(ctx, cmd, args); err != nil {
		fail(err, "")
	}
}

// run dispatches one command.
func run(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "import":
		return runImport(ctx, args)
	case "list":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			parent := quarkapi.RootFID
			if len(rest) > 0 {
				parent = rest[0]
			}
			entries, err := svc.List(ctx, parent)
			if err != nil {
				return err
			}
			for _, e := range entries {
				kind := "d"
				if !e.IsDir() {
					kind = "f"
				}
				fmt.Printf("%s %s %12d %s\n", kind, e.FID, e.Size, e.Name)
			}
			return nil
		})
	case "download":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 1 {
				return errors.New("usage: download <fid>")
			}
			res, err := svc.DownloadURL(ctx, rest[0])
			if err != nil {
				return err
			}
			fmt.Println(res.URL)
			return nil
		})
	case "mkdir":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 2 {
				return errors.New("usage: mkdir <parent-fid> <name>")
			}
			fid, err := svc.Mkdir(ctx, rest[0], rest[1])
			if err != nil {
				return err
			}
			fmt.Println(fid)
			return nil
		})
	case "upload":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 3 {
				return errors.New("usage: upload <parent-fid> <name> <local-file>")
			}
			f, err := os.Open(rest[2])
			if err != nil {
				return err
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return err
			}
			fid, err := svc.Upload(ctx, adapter.UploadRequest{
				ParentFID:   rest[0],
				Name:        rest[1],
				Size:        info.Size(),
				ContentType: "application/octet-stream",
			}, f, func(sent int64) {
				fmt.Fprintf(os.Stderr, "\r%12d / %d", sent, info.Size())
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr)
			fmt.Println(fid)
			return nil
		})
	case "rename":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 2 {
				return errors.New("usage: rename <fid> <new-name>")
			}
			return svc.Rename(ctx, rest[0], rest[1])
		})
	case "move":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 2 {
				return errors.New("usage: move <fid> <to-parent-fid>")
			}
			return svc.Move(ctx, []string{rest[0]}, rest[1])
		})
	case "delete":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 1 {
				return errors.New("usage: delete <fid>")
			}
			return svc.Delete(ctx, rest[0])
		})
	case "refresh":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			return svc.RefreshSessionCookie(ctx)
		})
	case "keepalive":
		return runKeepalive(ctx, args)
	case "serve":
		return runServe(ctx, args)
	case "mount":
		return runMount(ctx, args)
	case "profile":
		return runProfileShow(ctx, args)
	case "doctor":
		return runDoctor(ctx, args)
	case "capabilities":
		caps := adapter.CapabilitiesOf()
		fmt.Printf("copy:             %s\n", supportStr(caps.Copy))
		fmt.Printf("overwrite upload: %s\n", supportStr(caps.OverwriteUpload))
		return nil
	case "probe-write":
		return runProbeWrite(ctx, args)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// runDoctor runs the runtime pre-check (issue #20): platform, bundled
// rclone, WinFsp, and — with -self-test and a credential source — one real
// mount attempt. Failures print a Chinese remedy; nothing is ever repaired
// silently. With -install-winfsp the bundled official installer starts after
// the user's explicit confirmation, and the UAC prompt is the installer's
// own.
func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	bundleDir := fs.String("bundle-dir", "", "override the runtime bundle directory (default: <exe>\\bundle)")
	installWinFsp := fs.Bool("install-winfsp", false, "explicitly confirm starting the bundled official WinFsp installer")
	selfTest := fs.Bool("self-test", false, "run one real mount attempt after the static gates")
	cookieFile := fs.String("cookie-file", "", "file holding the active cookie (with -self-test)")
	useProfile := fs.Bool("profile", false, "load the credential from the profile's DPAPI store (with -self-test)")
	profileDir := fs.String("profile-dir", "", "override the profile directory (diagnostics)")
	port := fs.Int("port", defaultLoopbackPort, "fixed loopback port")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *installWinFsp {
		dir := *bundleDir
		if dir == "" {
			dir = preflight.DefaultBundleDir()
		}
		if err := preflight.LaunchWinFspInstaller(dir, true); err != nil {
			return err
		}
		return nil
	}

	var selfTestFn func(ctx context.Context) error
	if *selfTest {
		st, err := buildSelfTest(ctx, selfTestOpts{
			bundleDir:  *bundleDir,
			profileDir: *profileDir,
			cookieFile: *cookieFile,
			useProfile: *useProfile,
			port:       *port,
		})
		if err != nil {
			return err
		}
		selfTestFn = st
	}

	report := preflight.Run(ctx, preflight.Options{
		BundleDir: *bundleDir,
		SelfTest:  selfTestFn,
	})
	for _, f := range report.Findings {
		mark := "通过"
		if !f.OK {
			mark = "未通过"
		}
		fmt.Printf("[%s] %s：%s\n", mark, f.Check, f.Detail)
	}
	fmt.Printf("预检结论：%s\n", report.Status)
	if !report.OK() {
		return fmt.Errorf("预检未通过（%s）", report.Status)
	}
	return nil
}

// loadCredential resolves the session credential from the profile's DPAPI
// store or a plain cookie file, returning the cookie, the matching rotation
// sink, and — in profile mode — the loaded profile for the caller to update.
// A profile load failure surfaces as "state: failed", the supervisor's
// Failed answer (ADR 0005).
func loadCredential(cookieFile string, useProfile bool, profileDir string) (cookie string, persist func(string), store *profile.Store, cfg profile.Config, outcome profile.Outcome, err error) {
	switch {
	case useProfile:
		var st *profile.Store
		if st, err = openProfileStore(profileDir); err != nil {
			return
		}
		store = st
		var loadErr error
		if cfg, outcome, loadErr = st.LoadConfig(); loadErr != nil {
			fmt.Fprintln(os.Stderr, "state: failed")
			err = loadErr
			return
		}
		if cookie, err = st.LoadCookie(); err != nil {
			err = fmt.Errorf("load credential: %w (import -profile first)", err)
			return
		}
		persist = profilePersist(st)
	case cookieFile != "":
		if cookie, err = readCookieFile(cookieFile); err != nil {
			return
		}
		persist = writeCookieFilePersist(cookieFile)
	default:
		err = errors.New("需要凭据来源：-profile 或 -cookie-file")
	}
	return
}

// runImport reads a candidate cookie from stdin, validates it, activates it
// and stores the effective cookie — either in the profile's DPAPI credential
// store (-profile, issue #19) or in the plain cookie file.
func runImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	cookieFile := fs.String("cookie-file", "", "file to write the effective cookie to")
	useProfile := fs.Bool("profile", false, "store the effective credential in the profile's DPAPI store")
	profileDir := fs.String("profile-dir", "", "override the profile directory (diagnostics)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*useProfile && *cookieFile == "" {
		return errors.New("import requires -cookie-file or -profile")
	}

	fmt.Fprintln(os.Stderr, "paste the browser-exported cookie, then press enter:")
	candidate, err := readLine(os.Stdin)
	if err != nil {
		return err
	}

	var persist func(string)
	if *useProfile {
		st, err := openProfileStore(*profileDir)
		if err != nil {
			return err
		}
		persist = profilePersist(st)
	} else {
		persist = writeCookieFilePersist(*cookieFile)
	}

	store := session.NewStore("", persist)
	svc := adapter.New(quarkapi.New(store, quarkapi.Config{}, nil), store)
	if err := svc.ValidateAndActivate(ctx, candidate); err != nil {
		// A rejected import must not create or clobber the stored credential,
		// and the failure must be reported without echoing the candidate.
		return err
	}
	fmt.Fprintln(os.Stderr, "cookie validated and activated")
	return nil
}

// runProfileShow prints the mount profile and the credential-store state
// without ever printing the credential itself (issue #19). Load failures are
// diagnostics, not silent exits: the message names the file and what to do.
func runProfileShow(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	profileDir := fs.String("profile-dir", "", "override the profile directory (diagnostics)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openProfileStore(*profileDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "config: %s\n", st.ConfigPath())
	fmt.Fprintf(os.Stderr, "credential: %s (DPAPI CurrentUser)\n", st.CredPath())

	cfg, outcome, err := st.LoadConfig()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "outcome: %s\n", outcome)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	// Credential presence only: the value never reaches the output.
	fmt.Fprintf(os.Stderr, "credential stored: %t\n", st.CookieExists())
	return nil
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

// withService wires the adapter from a cookie file and runs fn.
func withService(ctx context.Context, args []string, fn func(svc *adapter.Service, rest []string) error) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cookieFile := fs.String("cookie-file", "", "file holding the active cookie (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cookieFile == "" {
		return errors.New("this command requires -cookie-file")
	}
	svc, _, err := wireService(*cookieFile)
	if err != nil {
		return err
	}
	return fn(svc, fs.Args())
}

// runKeepalive runs the periodic session refresh until interrupted. It is the
// standalone shape of the mount's keep-alive (issue #2): every rotation a
// refresh observes lands in the cookie file, and Ctrl+C stops the loop for
// real rather than leaving it sending requests behind the process.
func runKeepalive(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("keepalive", flag.ContinueOnError)
	cookieFile := fs.String("cookie-file", "", "file holding the active cookie (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cookieFile == "" {
		return errors.New("keepalive requires -cookie-file")
	}
	svc, store, err := wireService(*cookieFile)
	if err != nil {
		return err
	}

	r := session.NewRefresher(store, svc, 0, 0)
	r.Start(ctx)
	defer r.Stop()
	fmt.Fprintln(os.Stderr, "keepalive running; press Ctrl+C to stop")
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "keepalive stopped")
	return nil
}

// runProbeWrite runs the manual write diagnostic (issue #22, ADR 0010): a
// small file is uploaded into the drive root and deleted again — the only
// probe with a side effect, and it runs only because the user asked. The
// outcome lands in the shared capability snapshot and is printed, redacted.
func runProbeWrite(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe-write", flag.ContinueOnError)
	cookieFile := fs.String("cookie-file", "", "file holding the active cookie (required)")
	profileDir := fs.String("profile-dir", "", "override the profile directory (diagnostics)")
	diagPath := fs.String("diag-path", "", "override the capability snapshot file (default: <profile dir>\\diag.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cookieFile == "" {
		return errors.New("probe-write requires -cookie-file")
	}
	svc, _, err := wireService(*cookieFile)
	if err != nil {
		return err
	}
	rec := diag.NewRecorder()
	rec.SetPersistPath(resolveDiagPath(*profileDir, *diagPath))

	fmt.Fprintln(os.Stderr, "写入诊断：将在网盘根目录上传并删除一个小文件（quark-write-probe-*.txt）")
	rec.RunProbe(ctx, diag.ProbeWrite, func(ctx context.Context) error {
		return writeDiagProbe(ctx, svc)
	})
	snap := rec.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	failed := false
	for _, p := range snap.Probes {
		mark := "通过"
		switch {
		case p.Skipped:
			mark = "跳过"
		case !p.OK:
			mark = "未通过"
			failed = true
		}
		line := fmt.Sprintf("[%s] 探针 %s（%s）", mark, p.Name, p.At.Format("15:04:05"))
		if p.Err != "" {
			line += " " + p.Err
		}
		fmt.Fprintln(os.Stderr, line)
	}
	for _, wb := range snap.Writeback {
		fmt.Fprintf(os.Stderr, "[回写失败] %s：已重试 %d 次，下次重试 %s\n",
			wb.Path, wb.Retries, wb.NextRetry.Format("15:04:05"))
	}
	if failed {
		return errors.New("写入诊断未通过，详见上方快照")
	}
	return nil
}

// resolveDiagPath resolves the capability snapshot file: the explicit
// override, or diag.json in the profile directory (the default profile dir
// when none was named).
func resolveDiagPath(profileDir, override string) string {
	if override != "" {
		return override
	}
	dir := profileDir
	if dir == "" {
		var err error
		if dir, err = profile.DefaultDir(); err != nil {
			// A resolvable default was already required by credential loading
			// in every caller; a failure here leaves the recorder in memory
			// only, which degrades diagnostics, not the mount.
			return ""
		}
	}
	return filepath.Join(dir, diag.SnapshotFile)
}

// diagProbeContent is the diagnostic file's whole content: self-describing and
// free of anything worth protecting, because a diagnostic that needs redacting
// is a diagnostic that will not get written.
const diagProbeContent = "quark-rclone write diagnostic — uploaded and deleted by probe-write.\n"

// writeDiagProbe uploads the small diagnostic file into the drive root and
// deletes it. A cleanup failure is reported as the probe's failure: an
// orphaned probe file the user cannot see would be worse than an honest
// error.
func writeDiagProbe(ctx context.Context, svc *adapter.Service) error {
	name := fmt.Sprintf("quark-write-probe-%d.txt", time.Now().Unix())
	fid, err := svc.Upload(ctx, adapter.UploadRequest{
		ParentFID:   quarkapi.RootFID,
		Name:        name,
		Size:        int64(len(diagProbeContent)),
		ContentType: "text/plain",
	}, strings.NewReader(diagProbeContent), nil)
	if err != nil {
		return err
	}
	if err := svc.Delete(ctx, fid); err != nil {
		return fmt.Errorf("已上传 %s 但清理失败：%w", name, err)
	}
	return nil
}

// runServe runs the loopback WebDAV endpoint until interrupted. It is the
// diagnostics form of the mount session: everything but rclone.
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cookieFile := fs.String("cookie-file", "", "file holding the active cookie (required)")
	port := fs.Int("port", defaultLoopbackPort, "fixed loopback port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cookieFile == "" {
		return errors.New("serve requires -cookie-file")
	}
	svc, store, err := wireService(*cookieFile)
	if err != nil {
		return err
	}
	server := dav.New(dav.Config{
		Addr:    fmt.Sprintf("127.0.0.1:%d", *port),
		Cookie:  store.CookieSnapshot(),
		Service: svc,
	})
	if err := server.Listen(); err != nil {
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	fmt.Fprintf(os.Stderr, "serving dav on http://%s/dav\n", server.Addr())

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	}
	// Draining is the endpoint's shutdown contract: in-flight transfers
	// finish before the process exits.
	sd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fmt.Fprintln(os.Stderr, "draining in-flight requests...")
	return server.Shutdown(sd)
}

// runMount runs one mount session: the endpoint, a temporary rclone config,
// optionally rclone itself, and the cleanup of all of it on the way out. The
// cookie reaches rclone only through the 0600 config file, never the command
// line (issue #18).
func runMount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	cookieFile := fs.String("cookie-file", "", "file holding the active cookie (or use -profile)")
	useProfile := fs.Bool("profile", false, "load the credential and drive settings from the mount profile (DPAPI, issue #19)")
	profileDir := fs.String("profile-dir", "", "override the profile directory (diagnostics)")
	port := fs.Int("port", defaultLoopbackPort, "fixed loopback port")
	rclone := fs.String("rclone", "", "path to the rclone binary; empty prints the mount command instead of running it")
	rcloneSHA256 := fs.String("rclone-sha256", "", "explicit SHA-256 vouching for a non-bundled -rclone binary (required with -rclone)")
	mountPoint := fs.String("mount-point", "", "drive letter or directory rclone mounts at")
	bundleDir := fs.String("bundle-dir", "", "override the runtime bundle directory for the pre-check (default: <exe>\\bundle)")
	statePathFlag := fs.String("state-path", "", "override the lifecycle state file (default: profile dir\\state.json in -profile mode)")
	diagPathFlag := fs.String("diag-path", "", "override the capability snapshot file (default: <profile dir>\\diag.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cookie, persist, profileStore, profileConfig, outcome, err := loadCredential(*cookieFile, *useProfile, *profileDir)
	if err != nil {
		return err
	}
	if *useProfile {
		fmt.Fprintf(os.Stderr, "profile: %s (%s)\n", profileStore.ConfigPath(), outcome)
		// Auto recovery (ADR 0005): an unset mount point falls back to the
		// recorded preference — the explicit letter, or the last letter a
		// session actually held.
		if *mountPoint == "" {
			*mountPoint = profileConfig.MountPoint()
		}
	}

	// Issue #20: the runtime pre-check gates every mount. A bundled rclone
	// that fails its manifest, or a missing/mismatched WinFsp, blocks startup
	// with a Chinese remedy — nothing is downloaded or repaired automatically.
	bundleDirForGate := *bundleDir
	if bundleDirForGate == "" {
		bundleDirForGate = preflight.DefaultBundleDir()
	}
	if rc := preflight.CheckRclone(ctx, bundleDirForGate, nil); !rc.OK {
		return errors.New(rc.Detail)
	}
	if wc := preflight.CheckWinFsp(); !wc.OK {
		return errors.New(wc.Detail)
	}
	if *rclone != "" {
		// ADR 0006: the mount runs the bundled, manifest-verified rclone. An
		// arbitrary binary is refused unless the caller explicitly vouches
		// for its hash.
		bundleRclone := filepath.Join(bundleDirForGate, preflight.RcloneExeName)
		if filepath.Clean(*rclone) != filepath.Clean(bundleRclone) {
			if *rcloneSHA256 == "" {
				return fmt.Errorf("拒绝使用非随包 rclone %q：该二进制未经 SHA-256 校验。请省略 -rclone 使用随包组件，或用 -rclone-sha256 显式声明其 SHA-256。", *rclone)
			}
			sum, sumErr := fileSHA256Hex(*rclone)
			if sumErr != nil {
				return fmt.Errorf("无法计算 %s 的 SHA-256：%v", *rclone, sumErr)
			}
			if !strings.EqualFold(sum, *rcloneSHA256) {
				return fmt.Errorf("%s 的 SHA-256 校验失败（实际 %s，声明 %s）：拒绝启动。", *rclone, sum, *rcloneSHA256)
			}
			fmt.Fprintln(os.Stderr, "警告：-rclone 指定了非随包 rclone，其哈希与 -rclone-sha256 声明一致；该组合未被发布流程验证。")
		}
	}

	// The supervised lifecycle (issue #21) owns everything from here: the
	// adapter endpoint, the rclone child, the drive letter, restarts and the
	// shutdown order. The CLI subscribes to the state stream and waits.
	statePath := *statePathFlag
	if statePath == "" && profileStore != nil {
		// Persist the lifecycle state beside the profile (ADR 0005).
		statePath = filepath.Join(profileStore.Dir(), "state.json")
	}
	// Real readiness (ADR 0004): the provider's /config through the live
	// credential, probed before rclone is ever asked to mount.
	probeStore := session.NewStore(cookie, nil)
	probeAPI := quarkapi.New(probeStore, quarkapi.Config{}, nil)

	// Issue #22: the capability snapshot and the writeback ledger live beside
	// the profile, so a later probe-write and the tray read the same truth the
	// running mount wrote. The probes are the side-effect-free trio of ADR
	// 0010 — auth, root list, read link — run after readiness succeeds; the
	// write probe never runs here, it is manual only.
	rec := diag.NewRecorder()
	rec.SetPersistPath(resolveDiagPath(*profileDir, *diagPathFlag))
	probeSvc := adapter.New(probeAPI, probeStore)
	probeCaps := func(ctx context.Context) {
		rec.RunProbe(ctx, diag.ProbeAuth, probeAPI.ProbeConfig)
		files, listErr := probeSvc.List(ctx, quarkapi.RootFID)
		rec.RunProbe(ctx, diag.ProbeRootList, func(context.Context) error { return listErr })
		var linkTarget string
		if listErr == nil {
			for _, f := range files {
				if !f.IsDir() {
					linkTarget = f.FID
					break
				}
			}
		}
		if linkTarget == "" {
			// Nothing in the root to mint a read link against: skipped, not
			// failed — the precondition was absent, not the capability.
			rec.RunProbe(ctx, diag.ProbeReadLink, nil)
			return
		}
		rec.RunProbe(ctx, diag.ProbeReadLink, func(ctx context.Context) error {
			_, err := probeSvc.DownloadURL(ctx, linkTarget)
			return err
		})
	}
	sup := supervisor.Supervise(ctx, supervisor.SupervisorConfig{
		Port:           *port,
		Cookie:         cookie,
		Persist:        persist,
		RootFID:        profileConfig.Quark.RootFID,
		BundleDir:      bundleDirForGate,
		RclonePath:     *rclone,
		MountPoint:     *mountPoint,
		CacheMode:      profileConfig.Writes.CacheMode,
		WriteBack:      profileConfig.Writes.WriteBack,
		WriteChunkSize: profileConfig.Writes.WriteChunkSize,
		CacheDir:       profileConfig.CacheDir,
		StatePath:      statePath,
		ReadyProbe:     probeAPI.ProbeConfig,
		Recorder:       rec,
		ProbeCaps:      probeCaps,
	})
	states := sup.Subscribe()
	printDone := make(chan struct{})
	go func() {
		defer close(printDone)
		for {
			select {
			case st := <-states:
				line := fmt.Sprintf("状态: %s（%s）", st.State, st.Phase)
				if st.Err != "" {
					line += " " + st.Err
				}
				if !st.NextRetry.IsZero() {
					line += fmt.Sprintf(" 下次重试: %s", st.NextRetry.Format("15:04:05"))
				}
				fmt.Fprintln(os.Stderr, line)
			case <-sup.Done():
				return
			}
		}
	}()
	final := sup.Wait()
	<-printDone

	// The letter this session actually held is what the next auto recovery
	// tries first (ADR 0005); only a session that reached Mounted records it.
	if profileStore != nil && *mountPoint != "" && sup.ReachedMounted() {
		profileConfig.Drive.LastLetter = profile.NormalizeDriveLetter(*mountPoint)
		if err := profileStore.SaveConfig(profileConfig); err != nil {
			fmt.Fprintf(os.Stderr, "quark-adapter: could not save profile: %v\n", err)
		}
	}
	switch final.State {
	case supervisor.StateDisabled, supervisor.StateMounted:
		fmt.Fprintln(os.Stderr, "mount session ended; temporary config deleted")
		return nil
	case supervisor.StateFailed:
		return fmt.Errorf("挂载会话失败：%s %s", final.Phase, final.Err)
	default:
		// NeedsReauth / DriveConflict / WinFspRequired: the user must act,
		// and the pause reason survives the clean shutdown at this boundary.
		return fmt.Errorf("挂载已暂停（%s）：%s", sup.PauseReason(), final.Err)
	}
}

// profilePersist returns the rotation sink that stores refreshed cookies in
// the profile's DPAPI credential store (issue #19).
func profilePersist(st *profile.Store) func(string) {
	return func(effective string) {
		if err := st.SaveCookie(effective); err != nil {
			fmt.Fprintf(os.Stderr, "quark-adapter: could not persist credential: %v\n", err)
		}
	}
}

// writeCookieFilePersist returns the rotation sink for the plain cookie file.
func writeCookieFilePersist(path string) func(string) {
	return func(effective string) { writeCookieFile(path, effective) }
}

// readCookieFile loads the active cookie from disk.
func readCookieFile(path string) (string, error) {
	cookie, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read cookie file: %w", err)
	}
	return strings.TrimSpace(string(cookie)), nil
}

// fileSHA256Hex hashes a file the way the -rclone-sha256 vouching compares.
func fileSHA256Hex(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// wireService loads the cookie file and builds the adapter on top of a store
// that persists every rotation back to that file.
func wireService(cookieFile string) (*adapter.Service, *session.Store, error) {
	cookie, err := readCookieFile(cookieFile)
	if err != nil {
		return nil, nil, err
	}
	store := session.NewStore(cookie, func(effective string) {
		writeCookieFile(cookieFile, effective)
	})
	return adapter.New(quarkapi.New(store, quarkapi.Config{}, nil), store), store, nil
}

// writeCookieFile persists the effective cookie with owner-only permissions.
// Failures are reported on stderr only: a failed persist must not fail the
// request that caused the rotation.
func writeCookieFile(path, cookie string) {
	if err := os.WriteFile(path, []byte(cookie), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "quark-adapter: could not persist cookie: %v\n", err)
	}
}

// readLine reads one line from r, trimming the newline.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// supportStr renders a capability flag.
func supportStr(b bool) string {
	if b {
		return "supported"
	}
	return "not supported"
}

// fail prints the error, redacted, plus optional usage, and exits nonzero.
func fail(err error, usage string) {
	msg := redact.Error(err)
	cat := quarkerr.CategoryOf(err)
	if cat != "" && cat != quarkerr.CatUnknown {
		msg = fmt.Sprintf("%s (category: %s)", msg, cat)
	}
	fmt.Fprintln(os.Stderr, "quark-adapter: "+msg)
	if usage != "" {
		fmt.Fprintln(os.Stderr)
		fmt.Fprint(os.Stderr, usage)
	}
	os.Exit(1)
}

// signalContext is a context cancelled on the first interrupt or terminate
// signal, so in-flight requests are abandoned cleanly instead of blocking the
// shutdown until their own timeout.
//
// There is deliberately no wall-clock cap: a large upload legitimately runs
// longer than any bound a harness could guess, and every request is already
// cancellable through this context.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
