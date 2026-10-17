//go:build windows

package preflight

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// winfspInstallerName is the official WinFsp installer the release bundle
// ships (ADR 0006: the package bundles the matching official installer; the
// app never downloads one).
const winfspInstallerName = "winfsp.msi"

// LaunchWinFspInstaller starts the bundled official installer after the
// user's explicit confirmation.
//
// The confirmation gate is the whole security story (ADR 0006): the app
// itself never elevates — msiexec triggers the UAC prompt on its own — and
// the app never restarts anything afterwards. The user finishes or skips the
// install and re-runs the pre-check.
func LaunchWinFspInstaller(bundleDir string, confirmed bool) error {
	if !confirmed {
		return errors.New("启动 WinFsp 官方安装器需要用户明确确认")
	}
	msi := filepath.Join(bundleDir, winfspInstallerName)
	if _, err := os.Stat(msi); err != nil {
		return fmt.Errorf("安装包缺少 %s：请重新安装应用；或从 https://winfsp.dev/ 手动安装 WinFsp x64 %s 或更高版本。",
			msi, MinWinFspVersion)
	}
	// The installer process owns its own elevation; the app starts it and
	// returns immediately, never waiting and never restarting.
	cmd := exec.Command("msiexec", "/i", msi)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 WinFsp 官方安装器失败：%v", err)
	}
	fmt.Fprintln(os.Stderr, "WinFsp 官方安装器已启动：完成安装（如系统要求重启，请在方便时重启）后重新运行预检。")
	return nil
}
