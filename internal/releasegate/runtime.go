package releasegate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muou000/quark-rclone/internal/autostart"
	"github.com/muou000/quark-rclone/internal/preflight"
	"github.com/muou000/quark-rclone/internal/singleinstance"
	"github.com/muou000/quark-rclone/internal/upgrade"
)

// VerifiedScope is the only combination the first release promises (ADR
// 0007); everything else is reported as 未验证.
const VerifiedScope = "Windows 10/11 x64"

// testSuiteGates are the blocking unit assertions of AC2/AC3: they run the
// packages whose tests pin the release-blocking behaviours. One gate per
// concern, so a failure names the remedy directly.
func testSuiteGates(o Options) []Gate {
	suites := []struct {
		id, title, packages string
	}{
		{"tests.config-security", "配置迁移备份/回滚、更高版本拒绝与 DPAPI 解密失败", "./internal/profile"},
		{"tests.redaction", "日志脱敏", "./internal/redact"},
		{"tests.rclone-manifest", "rclone SHA-256 清单校验", "./internal/preflight"},
		{"tests.upgrade-rollback", "升级 staging 与回滚", "./internal/upgrade"},
		{"tests.lifecycle", "盘符冲突、盘符可见性、读写回写、断网恢复、卸载顺序与错误映射", "./internal/supervisor ./internal/dav ./internal/power ./internal/singleinstance"},
		{"tests.tray", "托盘面板与恢复流程", "./internal/trayui ./internal/adapter ./internal/quarkapi ./internal/session"},
	}
	gates := make([]Gate, 0, len(suites))
	for _, s := range suites {
		suites := s
		gates = append(gates, Gate{
			ID:    suites.id,
			Title: suites.title,
			Kind:  KindTestSuite,
			Run: func(ctx context.Context) Outcome {
				return runGoTests(ctx, o, suites.packages)
			},
		})
	}
	return gates
}

// runGoTests runs the blocking test suites as an external process: they are
// the release-blocking assertions of ADR 0007, and race mode is part of the
// gate.
func runGoTests(ctx context.Context, o Options, packages string) Outcome {
	goCmd := o.GoCommand
	if goCmd == "" {
		goCmd = "go"
	}
	args := []string{"test", "-race", "-count=1"}
	args = append(args, strings.Fields(packages)...)
	out, err := runCommand(ctx, o.WorkDir, goCmd, args...)
	if err != nil {
		return Outcome{OK: false, Detail: firstInteresting(out, err)}
	}
	return Outcome{OK: true}
}

// runtimeGates are the in-process, credential-free checks of AC 4.
func runtimeGates(o Options) []Gate {
	return []Gate{
		{
			ID:    "runtime.winfsp",
			Title: "WinFsp 预检（缺失/架构错误即阻断）",
			Kind:  KindRuntime,
			Run: func(ctx context.Context) Outcome {
				wc := preflight.CheckWinFsp()
				if !wc.OK {
					return Outcome{OK: false, Detail: wc.Detail}
				}
				return Outcome{OK: true, Detail: wc.Detail}
			},
		},
		{
			ID:    "runtime.rclone",
			Title: "rclone 运行包预检（版本与 SHA-256 清单）",
			Kind:  KindRuntime,
			Run: func(ctx context.Context) Outcome {
				bundleDir := o.BundleDir
				if bundleDir == "" {
					bundleDir = preflight.DefaultBundleDir()
				}
				rc := preflight.CheckRclone(ctx, bundleDir, nil)
				if !rc.OK {
					return Outcome{OK: false, Detail: rc.Detail}
				}
				return Outcome{OK: true, Detail: rc.Detail}
			},
		},
		{
			ID:    "runtime.dav",
			Title: "适配层健康（loopback 端点 healthz 与能力声明）",
			Kind:  KindRuntime,
			Run:   checkDavEndpoint,
		},
		{
			ID:    "runtime.upgrade-rollback",
			Title: "升级 staging 换位与回滚",
			Kind:  KindRuntime,
			Run: func(ctx context.Context) Outcome {
				return checkUpgradeRoundTrip(ctx, o.StagingRoot)
			},
		},
		{
			ID:    "runtime.singleinstance",
			Title: "单实例守卫（第二个实例被拒绝）",
			Kind:  KindRuntime,
			Run: func(ctx context.Context) Outcome {
				name := `Local\QuarkDriveMount.ReleaseGate.` + fmt.Sprint(time.Now().UnixNano())
				_, running, err := singleinstance.Acquire(name)
				if err != nil {
					return Outcome{OK: false, Detail: err.Error()}
				}
				_, second, err := singleinstance.Acquire(name)
				if err != nil {
					return Outcome{OK: false, Detail: err.Error()}
				}
				if !second {
					return Outcome{OK: false, Detail: "第二个实例未被拒绝"}
				}
				if running {
					return Outcome{OK: false, Detail: "第一个实例意外报告了已有属主"}
				}
				return Outcome{OK: true}
			},
		},
		{
			ID:    "runtime.autostart",
			Title: "登录自启动注册（当前用户 Run 键读写）",
			Kind:  KindRuntime,
			Run: func(ctx context.Context) Outcome {
				cmd, err := autostart.TrayCommand()
				if err != nil {
					return Outcome{OK: false, Detail: err.Error()}
				}
				// The round-trip runs on a scratch key: the gate verifies the
				// mechanism, it does not flip the user's real autostart.
				store := autostart.Store{KeyPath: `Software\QuarkDriveMount\ReleaseGate`}
				if err := store.Set(cmd); err != nil {
					return Outcome{OK: false, Detail: err.Error()}
				}
				got, err := store.Get()
				if err != nil || got != cmd {
					return Outcome{OK: false, Detail: fmt.Sprintf("读回不一致：%q, %v", got, err)}
				}
				if err := store.Remove(); err != nil {
					return Outcome{OK: false, Detail: err.Error()}
				}
				return Outcome{OK: true}
			},
		},
	}
}

// checkDavEndpoint boots the loopback endpoint in-process and pins its health
// and capability surface — the 适配器健康 of AC 4.
func checkDavEndpoint(ctx context.Context) Outcome {
	server := davServerForGate()
	if err := server.Listen(); err != nil {
		return Outcome{OK: false, Detail: err.Error()}
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	defer func() { _ = server.Close() }()

	health, err := httpGetBody(ctx, "http://"+server.Addr()+"/healthz")
	if err != nil {
		return Outcome{OK: false, Detail: fmt.Sprintf("healthz: %v", err)}
	}
	if !strings.Contains(health, `"ok"`) {
		return Outcome{OK: false, Detail: "healthz 响应异常：" + health}
	}
	caps, err := httpGetBody(ctx, "http://"+server.Addr()+"/capabilities")
	if err != nil {
		return Outcome{OK: false, Detail: fmt.Sprintf("capabilities: %v", err)}
	}
	if !strings.Contains(caps, `"copy":false`) {
		return Outcome{OK: false, Detail: "能力声明缺少 copy 不支持的声明：" + caps}
	}
	return Outcome{OK: true}
}

// checkUpgradeRoundTrip runs the ADR 0006 staging contract end to end on a
// scratch root: stage → activate → verify → and a second stage whose
// activation self-check fails must roll back byte for byte. This is the
// staging-rollback blocking assertion of AC 3, executed live.
func checkUpgradeRoundTrip(ctx context.Context, root string) Outcome {
	scratch := root
	if scratch == "" {
		temp, err := os.MkdirTemp("", "quark-release-gate-")
		if err != nil {
			return Outcome{OK: false, Detail: err.Error()}
		}
		scratch = temp
		defer os.RemoveAll(scratch)
	}

	build := func(body string) (string, error) {
		dir := filepath.Join(scratch, "payload-"+body)
		for rel, content := range map[string]string{
			upgrade.TrayExeName:                                body,
			upgrade.AdapterExeName:                             body,
			filepath.Join(upgrade.BundleDirName, "rclone.exe"): body,
		} {
			p := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return "", err
			}
		}
		return dir, nil
	}

	appRoot := filepath.Join(scratch, "app")
	// A self-check that turns hostile exactly when the activated bundle
	// carries the v3 body: the second activation's post-swap check fails,
	// which is the rollback path under test.
	var activatedBody string
	checker := func(_ context.Context, dir string) error {
		data, err := os.ReadFile(filepath.Join(dir, "rclone.exe"))
		if err != nil {
			return err
		}
		if string(data) == activatedBody && activatedBody == "v3" {
			return errors.New("升级后自检未通过（验收注入）")
		}
		return nil
	}
	m := upgrade.New(appRoot, checker)

	staged, err := build("v2")
	if err != nil {
		return Outcome{OK: false, Detail: err.Error()}
	}
	if err := m.Stage(ctx, "v2", staged); err != nil {
		return Outcome{OK: false, Detail: "stage: " + err.Error()}
	}
	if err := m.Activate(ctx); err != nil {
		return Outcome{OK: false, Detail: "activate: " + err.Error()}
	}
	activatedBody = "v3"
	staged, err = build("v3")
	if err != nil {
		return Outcome{OK: false, Detail: err.Error()}
	}
	if err := m.Stage(ctx, "v3", staged); err != nil {
		return Outcome{OK: false, Detail: "second stage: " + err.Error()}
	}
	if err := m.Activate(ctx); err == nil {
		return Outcome{OK: false, Detail: "自检失败的激活未被回滚"}
	}
	// The rollback restored the previous payload byte for byte.
	data, err := os.ReadFile(filepath.Join(appRoot, upgrade.TrayExeName))
	if err != nil || string(data) != "v2" {
		return Outcome{OK: false, Detail: "回滚后活跃载荷不是上一版本"}
	}
	return Outcome{OK: true}
}
