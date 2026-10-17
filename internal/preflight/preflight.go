// Package preflight is the runtime pre-check the mount runs before touching
// the provider (issue #20, ADR 0006/0007).
//
// The first verified combination is Windows 10/11 x64 only. Everything the
// mount depends on is checked before startup — the bundled rclone's version
// and built-in SHA-256 manifest, the WinFsp architecture, registry install
// path, DLL and declared minimum version — and every failure carries a
// Chinese remedy. Nothing here silently modifies the system: rclone is never
// re-downloaded, and the WinFsp installer only starts after the user
// explicitly confirms.
package preflight

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Status is the pre-check verdict. Each value corresponds to the ADR 0004
// state the tray maps a repair action onto: StatusWinFspRequired is
// WinFspRequired, StatusDriveConflict is DriveConflict; the rclone and
// unverified verdicts both present as WinFspRequired with their own finding.
type Status string

const (
	// StatusOK means every gate passed.
	StatusOK Status = "ok"
	// StatusWinFspRequired means WinFsp is missing, mismatched, or the
	// platform is outside the verified matrix (ADR 0004: WinFspRequired).
	StatusWinFspRequired Status = "winfsp_required"
	// StatusDriveConflict means the mount target letter is taken
	// (ADR 0004: DriveConflict).
	StatusDriveConflict Status = "drive_conflict"
	// StatusRemoteError means the self-test failed on the far side — network
	// or authentication — not on the local runtime.
	StatusRemoteError Status = "remote_error"
	// StatusRcloneInvalid means the bundled rclone is missing, wrong, or
	// fails its SHA-256 manifest; startup is blocked.
	StatusRcloneInvalid Status = "rclone_invalid"
)

// Finding is one check's outcome with the Chinese remedy a user can act on.
type Finding struct {
	// Check names the gate: platform, rclone, winfsp, mount.
	Check string
	// OK reports whether the gate passed.
	OK bool
	// Detail is the human-readable result, in Chinese, including the fix
	// path on failure.
	Detail string
}

// Report is the whole pre-check run.
type Report struct {
	// Verified is true only on Windows x64 (ADR 0007's first-release matrix).
	Verified bool
	// Status summarises the run; it is the state the tray shows.
	Status Status
	// Findings lists every gate in run order.
	Findings []Finding
}

// OK reports whether the run cleared every gate it ran.
func (r *Report) OK() bool {
	for _, f := range r.Findings {
		if !f.OK {
			return false
		}
	}
	return r.Verified
}

// Options tunes one pre-check run. Tests override the probes; production
// leaves them nil.
type Options struct {
	// BundleDir is the directory holding rclone.exe, the manifest and the
	// WinFsp installer. Empty means the directory of the running executable.
	BundleDir string
	// SelfTest, when non-nil, performs one real mount attempt; its error is
	// classified into the mount finding. A nil self-test skips that gate.
	SelfTest func(ctx context.Context) error
	// VersionProbe overrides how the bundled rclone's version banner is
	// obtained. Nil runs the real binary.
	VersionProbe func(ctx context.Context, exe string) (string, error)
}

// platformVerified is the first-release compatibility matrix (ADR 0007):
// Windows x64, nothing else.
func platformVerified(goos, goarch string) bool {
	return goos == "windows" && goarch == "amd64"
}

// Run executes the pre-check chain: platform → rclone → WinFsp → self-test.
// A failed gate stops nothing after it that is still safe to run, but the
// report's Status carries the first blocking verdict and OK() is false.
func Run(ctx context.Context, opts Options) *Report {
	if opts.VersionProbe == nil {
		opts.VersionProbe = runRcloneVersion
	}
	report := &Report{Verified: platformVerified(runtime.GOOS, runtime.GOARCH)}

	// Platform gate. Outside the verified matrix every later check would be
	// guesswork, so the report says unverified and stops (issue #20: other
	// architectures enter WinFspRequired and show unverified).
	if !report.Verified {
		report.Status = StatusWinFspRequired
		report.Findings = append(report.Findings, Finding{
			Check: "platform",
			OK:    false,
			Detail: fmt.Sprintf("当前平台 %s/%s 不在首版验证范围内（Windows x64），挂载能力未验证：请在 Windows 10/11 x64 上使用。",
				runtime.GOOS, runtime.GOARCH),
		})
		return report
	}
	report.Findings = append(report.Findings, Finding{
		Check: "platform", OK: true,
		Detail: fmt.Sprintf("平台 %s/%s 在首版验证范围内。", runtime.GOOS, runtime.GOARCH),
	})

	bundleDir := opts.BundleDir
	if bundleDir == "" {
		bundleDir = DefaultBundleDir()
	}

	// rclone gate: presence, SHA-256 manifest, declared version.
	rcloneCheck := CheckRclone(ctx, bundleDir, opts.VersionProbe)
	report.Findings = append(report.Findings, Finding{
		Check: "rclone", OK: rcloneCheck.OK, Detail: rcloneCheck.Detail,
	})
	if !rcloneCheck.OK {
		report.Status = StatusRcloneInvalid
		return report
	}

	// WinFsp gate: registry install path, matching DLL, minimum version.
	winfspCheck := CheckWinFsp()
	report.Findings = append(report.Findings, Finding{
		Check: "winfsp", OK: winfspCheck.OK, Detail: winfspCheck.Detail,
	})
	if !winfspCheck.OK {
		report.Status = StatusWinFspRequired
		return report
	}

	// Self-test gate: one real mount attempt, classified.
	if opts.SelfTest != nil {
		err := opts.SelfTest(ctx)
		if err != nil {
			status := ClassifyMountFailure(err.Error())
			report.Status = status
			report.Findings = append(report.Findings, Finding{
				Check: "mount", OK: false,
				Detail: mountFailureRemedy(status, err.Error()),
			})
			return report
		}
		report.Findings = append(report.Findings, Finding{
			Check: "mount", OK: true,
			Detail: "实际挂载自检通过。",
		})
	}

	report.Status = StatusOK
	return report
}

// DefaultBundleDir is the bundle directory beside the running executable,
// which is how the release package lays rclone.exe, the manifest and the
// WinFsp installer out (ADR 0006).
func DefaultBundleDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Join(filepath.Dir(exe), "bundle")
}

// ClassifyMountFailure maps one real mount attempt's failure text onto the
// state the tray can repair. The order matters: WinFsp wording wins because
// a missing driver masks everything downstream; explicit remote signals (an
// HTTP status from the far side) come next, so a provider 403 is never
// mistaken for a taken letter; then the drive-letter wording; and anything
// left is the far side too.
func ClassifyMountFailure(stderr string) Status {
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "winfsp"):
		return StatusWinFspRequired
	case strings.Contains(lower, "401"), strings.Contains(lower, "403"),
		strings.Contains(lower, "forbidden"), strings.Contains(lower, "unauthorized"):
		return StatusRemoteError
	case strings.Contains(lower, "drive letter"),
		strings.Contains(lower, "already in use"),
		strings.Contains(lower, "cannot create a file when that file already exists"),
		strings.Contains(lower, "access is denied"):
		return StatusDriveConflict
	default:
		return StatusRemoteError
	}
}

// mountFailureRemedy renders the Chinese remedy for a classified self-test
// failure. The raw error rides along for diagnostics; it never carries
// credentials because mount failures are local runtime text.
func mountFailureRemedy(status Status, raw string) string {
	switch status {
	case StatusWinFspRequired:
		return "实际挂载自检失败：WinFsp 驱动不可用。请安装或修复 WinFsp x64（可使用应用自带的官方安装器），完成后重新运行预检。"
	case StatusDriveConflict:
		return "实际挂载自检失败：目标盘符已被占用。请在配置中更换盘符，或释放该盘符后重试。"
	case StatusRemoteError:
		return "实际挂载自检失败：远端网络或认证错误。请检查网络连接，或重新导入 Cookie。"
	default:
		return fmt.Sprintf("实际挂载自检失败：%s", raw)
	}
}
