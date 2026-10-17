package preflight

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RequiredRcloneVersion is the rclone release the bundle ships (ADR 0006:
// a fixed version, never the user's global rclone). Release builds may pin
// the manifest with -ldflags; an empty manifest is a packaging defect the
// pre-check reports instead of passing silently.
const RequiredRcloneVersion = "v1.71.2"

// rcloneManifest is the built-in SHA-256 list, keyed by bundle-relative path.
// Release builds inject the real values at build time.
var rcloneManifest = map[string]string{}

// SetRcloneManifest replaces the built-in manifest. Tests use it; release
// builds inject at link time instead.
func SetRcloneManifest(m map[string]string) {
	rcloneManifest = m
}

// RcloneCheck is the bundled-rclone gate's result.
type RcloneCheck struct {
	// Path is where the bundled rclone.exe was expected.
	Path string
	// OK reports whether startup may proceed.
	OK bool
	// Detail is the Chinese result, including the remedy on failure.
	Detail string
}

// rcloneExeName is the single executable the bundle ships.
const rcloneExeName = "rclone.exe"

// CheckRclone verifies the bundled rclone in order: present → manifest hash
// → declared version. The hash runs before the binary is ever executed, so a
// tampered file is never launched, and a failure never triggers a download —
// the remedy is always a reinstall (ADR 0006).
func CheckRclone(ctx context.Context, bundleDir string, runVersion func(ctx context.Context, exe string) (string, error)) RcloneCheck {
	if runVersion == nil {
		runVersion = runRcloneVersion
	}
	exe := filepath.Join(bundleDir, rcloneExeName)
	if _, err := os.Stat(exe); err != nil {
		return RcloneCheck{Path: exe, OK: false, Detail: fmt.Sprintf(
			"缺少随包组件 %s：请重新安装应用以恢复完整运行时；本程序不会自动下载替换。", exe)}
	}

	// The built-in manifest must know this file, and the bytes must match it.
	sum, err := fileSHA256(exe)
	if err != nil {
		return RcloneCheck{Path: exe, OK: false, Detail: fmt.Sprintf(
			"无法计算 %s 的 SHA-256：%v。请重新安装应用。", exe, err)}
	}
	want, listed := rcloneManifest[rcloneExeName]
	if !listed || want == "" {
		return RcloneCheck{Path: exe, OK: false, Detail: fmt.Sprintf(
			"内置 SHA-256 清单缺少 %s 的条目：安装包打包不完整，请重新安装应用。", rcloneExeName)}
	}
	if !strings.EqualFold(sum, want) {
		return RcloneCheck{Path: exe, OK: false, Detail: fmt.Sprintf(
			"%s 的 SHA-256 校验失败（实际 %s，清单 %s）：文件可能损坏或被篡改，请重新安装应用。", exe, sum, want)}
	}

	// Hash verified — only now is it safe to execute and ask its version.
	version, err := runVersion(ctx, exe)
	if err != nil {
		return RcloneCheck{Path: exe, OK: false, Detail: fmt.Sprintf(
			"无法获取 %s 的版本：%v。请重新安装应用。", exe, err)}
	}
	if !strings.Contains(version, RequiredRcloneVersion) {
		return RcloneCheck{Path: exe, OK: false, Detail: fmt.Sprintf(
			"随包 rclone 版本为 %s，与要求的 %s 不匹配：请重新安装应用，不要替换 rclone.exe。",
			strings.TrimSpace(version), RequiredRcloneVersion)}
	}
	return RcloneCheck{Path: exe, OK: true, Detail: fmt.Sprintf(
		"随包 rclone %s 校验通过（版本与 SHA-256 清单一致）。", RequiredRcloneVersion)}
}

// runRcloneVersion asks the real binary for its version banner.
func runRcloneVersion(ctx context.Context, exe string) (string, error) {
	out, err := exec.CommandContext(ctx, exe, "version").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// fileSHA256 is the manifest's hash function.
func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
