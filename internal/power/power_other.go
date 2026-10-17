//go:build !windows

// Package power turns Windows lifecycle broadcasts into the tray's recovery
// actions. Non-Windows platforms have no such broadcasts here; Start fails
// loudly instead of pretending to observe anything.
package power

import "errors"

// Event is a system lifecycle event the tray reacts to.
type Event int

const (
	EventNone Event = iota
	EventSuspend
	EventResume
	EventSessionEnd
)

// ErrUnsupported reports the platform has no power event mechanism here.
var ErrUnsupported = errors.New("power: unsupported on this platform")

// Start never returns on non-Windows platforms.
func Start(fn func(Event)) (stop func(), err error) {
	return nil, ErrUnsupported
}
