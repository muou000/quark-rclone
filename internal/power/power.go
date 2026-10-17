// Package power turns Windows lifecycle broadcasts into the tray's recovery
// actions (issue #25, ADR 0004): sleep/resume re-runs the mount's checks, and
// logoff/shutdown gets the orderly teardown before the session dies.
//
// The events arrive as window messages, so the package owns one hidden
// message-only window and its pump; the classification of messages into
// events is a pure function with its own tests, and the callback the caller
// supplies decides what a resume or a session end means.
package power

// Event is a system lifecycle event the tray reacts to.
type Event int

const (
	// EventNone is a message this package does not act on.
	EventNone Event = iota
	// EventSuspend means the machine is going to sleep.
	EventSuspend
	// EventResume means the machine woke up: the mount's whole check chain
	// must run again (WinFsp, readiness, rclone, drive visibility).
	EventResume
	// EventSessionEnd means logoff or shutdown: the mount must tear down in
	// the supervisor's order and the process must not block the session.
	EventSessionEnd
)

// Window message and wParam values (winuser.h).
const (
	wmPowerBroadcast    = 0x0218
	wmQueryEndSession   = 0x0011
	wmEndSession        = 0x0016
	pbtApmSuspend       = 0x0004
	pbtApmResumeAuto    = 0x0012
	pbtApmResumeSuspend = 0x0006
)

// classify maps one window message onto a lifecycle Event. It is the pure
// half of the package: the WndProc calls it, the tests pin it.
func classify(msg uint32, wParam uintptr) Event {
	switch msg {
	case wmPowerBroadcast:
		switch wParam {
		case pbtApmSuspend:
			return EventSuspend
		case pbtApmResumeAuto, pbtApmResumeSuspend:
			return EventResume
		}
		return EventNone
	case wmEndSession:
		if wParam != 0 {
			return EventSessionEnd
		}
		return EventNone
	}
	return EventNone
}
