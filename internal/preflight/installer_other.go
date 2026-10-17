//go:build !windows

package preflight

import "errors"

// LaunchWinFspInstaller is a stub off Windows: the platform gate has already
// reported the combination as unverified.
func LaunchWinFspInstaller(bundleDir string, confirmed bool) error {
	return errors.New("WinFsp 安装器仅在 Windows 上可用")
}
