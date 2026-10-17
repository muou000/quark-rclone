package releasegate

import (
	"fmt"
	"runtime"
	"strconv"

	"golang.org/x/sys/windows/registry"
)

// Environment is the acceptance run's platform statement (AC 5): the verified
// scope is Windows 10/11 x64, and everything else is marked 未验证.
type Environment struct {
	OSVersion     string `json:"os_version"`
	Arch          string `json:"arch"`
	IsWin10Or11   bool   `json:"is_win10_or11"`
	InScope       bool   `json:"in_scope"`
	VerifiedScope string `json:"verified_scope"`
}

// Detect reads the running platform's version statement.
func Detect() Environment {
	env := Environment{
		Arch:          runtime.GOARCH,
		VerifiedScope: VerifiedScope,
	}
	build, product, err := windowsVersion()
	if err != nil {
		env.OSVersion = fmt.Sprintf("Windows（版本读取失败：%v）", err)
		return env
	}
	env.OSVersion = fmt.Sprintf("%s (build %s)", product, build)
	env.IsWin10Or11 = classifyWindowsBuild(build)
	env.InScope = env.IsWin10Or11 && runtime.GOARCH == "amd64"
	return env
}

// windowsVersion reads the build number and product name from the
// CurrentVersion key, which is where Windows states its own version.
func windowsVersion() (build, product string, err error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "", "", err
	}
	defer k.Close()
	build, _, err = k.GetStringValue("CurrentBuild")
	if err != nil {
		return "", "", err
	}
	product, _, err = k.GetStringValue("ProductName")
	if err != nil {
		return "", "", err
	}
	if n, convErr := strconv.Atoi(build); convErr == nil && n >= 22000 {
		// Windows 11 keeps ProductName "Windows 10 …" in some installs; the
		// build number is the truth.
		product = "Windows 11"
	}
	return build, product, nil
}

// classifyWindowsBuild decides Windows 10 vs 11 from the build number: 22000
// is where Windows 11 starts.
func classifyWindowsBuild(build string) bool {
	n, err := strconv.Atoi(build)
	if err != nil {
		return false
	}
	return n >= 10240 // every supported Win10/11 build
}
