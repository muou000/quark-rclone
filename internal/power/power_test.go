//go:build windows

package power

import (
	"testing"
	"time"
)

func TestClassifyPinsTheLifecycleContract(t *testing.T) {
	cases := []struct {
		name   string
		msg    uint32
		wParam uintptr
		want   Event
	}{
		{"suspend", wmPowerBroadcast, pbtApmSuspend, EventSuspend},
		{"resume automatic", wmPowerBroadcast, pbtApmResumeAuto, EventResume},
		{"resume suspend", wmPowerBroadcast, pbtApmResumeSuspend, EventResume},
		{"unknown power message", wmPowerBroadcast, 0x0042, EventNone},
		{"session end", wmEndSession, 1, EventSessionEnd},
		{"session end false", wmEndSession, 0, EventNone},
		{"query end session", wmQueryEndSession, 1, EventNone},
		{"unrelated message", 0x0400 + 1, 1, EventNone},
	}
	for _, tc := range cases {
		if got := classify(tc.msg, tc.wParam); got != tc.want {
			t.Errorf("%s: classify(%#x, %x) = %v, want %v", tc.name, tc.msg, tc.wParam, got, tc.want)
		}
	}
}

func TestEventValuesAreDistinct(t *testing.T) {
	seen := map[Event]bool{EventNone: true}
	for _, e := range []Event{EventSuspend, EventResume, EventSessionEnd} {
		if seen[e] {
			t.Fatalf("event value %d is duplicated", e)
		}
		seen[e] = true
	}
	// The zero value must stay "nothing happened": a default-constructed
	// event must never trigger a recovery action.
	if EventNone != 0 {
		t.Fatal("EventNone must stay the zero value")
	}
}

// TestStartCreatesAndStopsTheWindow exercises the real window lifecycle: the
// hidden window comes up, Start returns a working stop, and stopping ends the
// pump instead of leaking the thread.
func TestStartCreatesAndStopsTheWindow(t *testing.T) {
	events := make(chan Event, 8)
	stop, err := Start(func(e Event) {
		select {
		case events <- e:
		default:
		}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if stop == nil {
		t.Fatal("Start returned a nil stop")
	}

	// The window is up; stopping must end the pump promptly. The contract is
	// that stop returns quickly and nothing blocks the test's own exit.
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not complete within 5s; the pump leaked")
	}
}
