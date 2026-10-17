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
// user's explicit confirmation, and waits for the installer to finish so the
// outcome lands where the pre-check can see it.
//
// The confirmation gate is the whole security story (ADR 0006): the app
// itself never elevates — msiexec triggers the UAC prompt on its own — and
// the app never restarts anything afterwards. When the installer reports
// that a reboot is required, a pending marker is left in the bundle
// directory and the next pre-check shows the pending-reboot state.
func LaunchWinFspInstaller(bundleDir string, confirmed bool) error {
	if !confirmed {
		return errors.New("启动 WinFsp 官方安装器需要用户明确确认")
	}
	msi := filepath.Join(bundleDir, winfspInstallerName)
	if _, err := os.Stat(msi); err != nil {
		return fmt.Errorf("安装包缺少 %s：请重新安装应用；或从 https://winfsp.dev/ 手动安装 WinFsp x64 %s 或更高版本。",
			msi, MinWinFspVersion)
	}
	// The installer process owns its own elevation; the app starts it, waits
	// for its verdict and returns without restarting anything.
	cmd := exec.Command("msiexec", "/i", msi)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("WinFsp 官方安装器未成功完成（%v）：请重新运行安装器或手动安装 WinFsp x64 %s 或更高版本。",
			err, MinWinFspVersion)
	}
	if code := cmd.ProcessState.ExitCode(); code == 3010 || code == 1641 {
		// ERROR_SUCCESS_REBOOT_REQUIRED / ERROR_SUCCESS_REBOOT_INITIATED.
		if err := os.WriteFile(filepath.Join(bundleDir, winfspRebootPendingName),
			[]byte("WinFsp 安装要求重启计算机\n"), 0o600); err != nil {
			return fmt.Errorf("安装完成，但要求重启计算机（无法写入待重启标记：%v）。请手动重启后再挂载。", err)
		}
		fmt.Fprintln(os.Stderr, "WinFsp 安装完成，但要求重启计算机：请重启后再运行预检与挂载（本程序不会自动重启）。")
		return nil
	}
	fmt.Fprintln(os.Stderr, "WinFsp 安装完成：请重新运行预检。")
	return nil
}
