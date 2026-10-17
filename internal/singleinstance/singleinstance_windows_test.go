//go:build windows

package singleinstance

import (
	"testing"
)

// TestAcquireDetectsTheSecondInstance is the cross-version guarantee (AC 5):
// a second Acquire of any name reports the running owner instead of taking
// over.
func TestAcquireDetectsTheSecondInstance(t *testing.T) {
	name := "Local\\QuarkDriveMount.Test." + t.Name()

	release, running, err := Acquire(name)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if running {
		t.Fatal("first Acquire reported another owner")
	}
	t.Cleanup(release)

	_, running2, err := Acquire(name)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if !running2 {
		t.Fatal("second Acquire must report the running owner")
	}

	// Releasing frees the name for the next instance.
	release()
	_, running3, err := Acquire(name)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if running3 {
		t.Fatal("Acquire after release must succeed")
	}
}
