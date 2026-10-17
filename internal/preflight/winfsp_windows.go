//go:build windows

package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// winfspRegistryKey is where the WinFsp installer records itself (ADR 0006:
// the registry install path is one of the pre-check gates). The 64-bit view
// is forced because this build is x64-only.
const winfspRegistryKey = `SOFTWARE\WinFsp`

// winfspDLLName names the architecture-matching driver support library. The
// x64 suffix is itself the architecture check: an x86 WinFsp would install
// winfsp-x86.dll and fail this gate.
const winfspDLLName = "winfsp-x64.dll"

// MinWinFspVersion is the minimum WinFsp release this application declares
// (ADR 0006: the app states its minimum; it never silently upgrades the
// driver).
const MinWinFspVersion = "2.0"

// WinFspCheck is the WinFsp gate's result.
type WinFspCheck struct {
	Installed  bool
	InstallDir string
	DLLPath    string
	DLLVersion string
	OK         bool
	Detail     string
}

// CheckWinFsp verifies, in order: the registry install path, an
// architecture-matching DLL, and the declared minimum version. Every failure
// answers with the Chinese repair path; none of them upgrades anything.
func CheckWinFsp() WinFspCheck {
	installDir, err := winfspInstallDir()
	if err != nil {
		return WinFspCheck{OK: false, Detail: fmt.Sprintf(
			"未检测到 WinFsp（注册表 %s 无安装信息）：请安装 WinFsp x64 %s 或更高版本——可运行应用自带的官方安装器，或从 https://winfsp.dev/ 下载。安装后重新运行预检。",
			winfspRegistryKey, MinWinFspVersion)}
	}

	dllPath, dllVersion, err := winfspDLL(installDir)
	if err != nil {
		return WinFspCheck{Installed: true, InstallDir: installDir, OK: false, Detail: fmt.Sprintf(
			"WinFsp 安装目录 %s 缺少匹配的 %s：%v。请重新安装 WinFsp x64（应用只支持 x64 驱动），完成后重新运行预检。",
			installDir, winfspDLLName, err)}
	}
	if !versionAtLeast(dllVersion, MinWinFspVersion) {
		return WinFspCheck{Installed: true, InstallDir: installDir, DLLPath: dllPath,
			DLLVersion: dllVersion, OK: false, Detail: fmt.Sprintf(
				"WinFsp 版本 %s 低于应用要求的最低版本 %s：请升级 WinFsp x64（不要降级或替换系统驱动），完成后重新运行预检。",
				dllVersion, MinWinFspVersion)}
	}
	return WinFspCheck{
		Installed: true, InstallDir: installDir, DLLPath: dllPath,
		DLLVersion: dllVersion, OK: true,
		Detail: fmt.Sprintf("WinFsp 预检通过：%s（版本 %s，注册表路径 %s）。", dllPath, dllVersion, installDir),
	}
}

// winfspInstallDir reads the installer's record from the 64-bit registry view.
func winfspInstallDir() (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, winfspRegistryKey,
		registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", err
	}
	defer key.Close()
	dir, _, err := key.GetStringValue("InstallDir")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("InstallDir is empty")
	}
	return dir, nil
}

// winfspDLL locates the architecture-matching DLL, preferring the system
// copy the installer maintains, and reads its file version.
func winfspDLL(installDir string) (path, version string, err error) {
	candidates := []string{
		filepath.Join(os.Getenv("SystemRoot"), "System32", winfspDLLName),
		filepath.Join(installDir, "bin", winfspDLLName),
		filepath.Join(installDir, winfspDLLName),
	}
	for _, candidate := range candidates {
		if _, statErr := os.Stat(candidate); statErr != nil {
			continue
		}
		v, verr := fileVersion(candidate)
		if verr != nil {
			return candidate, "", verr
		}
		return candidate, v, nil
	}
	return "", "", fmt.Errorf("在候选路径中未找到 %s", winfspDLLName)
}

// fileVersion reads the DLL's VS_FIXEDFILEINFO version string.
func fileVersion(path string) (string, error) {
	var handle windows.Handle
	size, err := windows.GetFileVersionInfoSize(path, &handle)
	if err != nil || size == 0 {
		return "", fmt.Errorf("no version info: %w", err)
	}
	data := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&data[0])); err != nil {
		return "", err
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var verLen uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&data[0]), `\`, unsafe.Pointer(&fixed), &verLen); err != nil || fixed == nil {
		return "", fmt.Errorf("VerQueryValue: %w", err)
	}
	return fmt.Sprintf("%d.%d.%d.%d",
		fixed.FileVersionMS>>16&0xffff,
		fixed.FileVersionMS&0xffff,
		fixed.FileVersionLS>>16&0xffff,
		fixed.FileVersionLS&0xffff), nil
}

// versionAtLeast compares dotted numeric versions: actual >= required.
// Non-numeric tails are ignored; a required field the actual version lacks
// counts as zero.
func versionAtLeast(actual, required string) bool {
	a, aok := parseVersion(actual)
	r, rok := parseVersion(required)
	if !aok || !rok {
		return false
	}
	for i := 0; i < 2; i++ {
		if a[i] != r[i] {
			return a[i] > r[i]
		}
	}
	return true
}

// parseVersion reads the first dotted numeric fields of a version string,
// e.g. "2.1.25312" or "v2.0".
func parseVersion(s string) ([2]int, bool) {
	var out [2]int
	s = strings.TrimPrefix(s, "v")
	fields := strings.SplitN(s, ".", 3)
	if len(fields) < 2 {
		return out, false
	}
	for i := 0; i < 2; i++ {
		n := 0
		for _, ch := range fields[i] {
			if ch < '0' || ch > '9' {
				return out, false
			}
			n = n*10 + int(ch-'0')
		}
		out[i] = n
	}
	return out, true
}
