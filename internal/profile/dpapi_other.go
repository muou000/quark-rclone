//go:build !windows

package profile

import "errors"

// errNoDPAPI keeps the package compiling off-Windows. The credential store
// refuses to operate rather than silently storing plaintext, so a build
// without DPAPI cannot degrade the security contract (ADR 0003).
var errNoDPAPI = errors.New("dpapi credential storage requires Windows")

func protectData([]byte) ([]byte, error) { return nil, errNoDPAPI }

func unprotectData([]byte) ([]byte, error) { return nil, errNoDPAPI }
