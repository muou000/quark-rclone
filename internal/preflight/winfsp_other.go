//go:build !windows

package preflight

import "fmt"

// WinFspCheck is the non-Windows stub: the platform gate already reports the
// combination as unverified, so this check never runs there.
type WinFspCheck struct {
	OK     bool
	Detail string
}

// CheckWinFsp is a stub off Windows.
func CheckWinFsp() WinFspCheck {
	return WinFspCheck{OK: false, Detail: "WinFsp 检查仅在 Windows 上运行。"}
}

// winfspInstallDir exists so the installer stub compiles uniformly.
func winfspInstallDir() (string, error) { return "", fmt.Errorf("windows only") }
