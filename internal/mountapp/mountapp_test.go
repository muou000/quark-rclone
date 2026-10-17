package mountapp

import (
	"errors"
	"testing"
)

// freeWhen reports every probed root except the named letters as absent.
func freeWhen(taken map[string]bool) func(string) error {
	return func(root string) error {
		if taken[root[:1]] {
			return nil
		}
		return errors.New("no such volume")
	}
}

func TestFirstFreeLetterPicksTheFirstAbsentVolume(t *testing.T) {
	if got := FirstFreeLetter(freeWhen(nil)); got != "D:" {
		t.Fatalf("FirstFreeLetter(empty machine) = %q, want D:", got)
	}
	if got := FirstFreeLetter(freeWhen(map[string]bool{"D": true})); got != "E:" {
		t.Fatalf("FirstFreeLetter(D taken) = %q, want E:", got)
	}
}

func TestFirstFreeLetterSkipsSystemDriveAndReportsExhaustion(t *testing.T) {
	taken := map[string]bool{"D": true, "E": true, "F": true}
	if got := FirstFreeLetter(freeWhen(taken)); got != "G:" {
		t.Fatalf("FirstFreeLetter(D-F taken) = %q, want G:", got)
	}
	// Every candidate occupied: the empty result defers to the supervisor's
	// drive probe, which names the collision instead of guessing.
	all := map[string]bool{}
	for l := byte('D'); l <= 'Z'; l++ {
		all[string(l)] = true
	}
	if got := FirstFreeLetter(freeWhen(all)); got != "" {
		t.Fatalf("FirstFreeLetter(all taken) = %q, want empty", got)
	}
}
