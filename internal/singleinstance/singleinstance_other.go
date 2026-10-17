//go:build !windows

// Package singleinstance guards the tray against a second instance of any
// version. Non-Windows platforms have no named mutex here; the guard fails
// loudly instead of pretending exclusivity.
package singleinstance

import "errors"

// ErrUnsupported reports the platform has no single-instance mechanism here.
var ErrUnsupported = errors.New("singleinstance: unsupported on this platform")

// DefaultName is the per-session guard name. It matches the Windows mutex
// name so the guard's meaning is identical everywhere.
const DefaultName = `Local\QuarkDriveMount.Tray`

// Acquire creates (and owns) the named guard.
func Acquire(name string) (release func(), alreadyRunning bool, err error) {
	return nil, false, ErrUnsupported
}
