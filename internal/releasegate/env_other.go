//go:build !windows

package releasegate

import "runtime"

// Environment is the acceptance run's platform statement. The acceptance
// matrix targets Windows; other platforms report themselves out of scope.
type Environment struct {
	OSVersion     string `json:"os_version"`
	Arch          string `json:"arch"`
	IsWin10Or11   bool   `json:"is_win10_or11"`
	InScope       bool   `json:"in_scope"`
	VerifiedScope string `json:"verified_scope"`
}

// Detect reports the non-Windows platform as out of scope.
func Detect() Environment {
	return Environment{
		OSVersion:     runtime.GOOS,
		Arch:          runtime.GOARCH,
		VerifiedScope: VerifiedScope,
	}
}
