//go:build windows

package autostart

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// scratchStore targets a scratch key under HKCU so the tests never touch the
// machine's real Run entry; the tree is deleted afterwards.
func scratchStore(t *testing.T) Store {
	t.Helper()
	key := `Software\QuarkDriveMount\Tests\autostart-` + strings.ReplaceAll(t.Name(), "/", "-")
	t.Cleanup(func() {
		// The scratch key holds values but no subkeys, so a plain delete is
		// enough; the best-effort parent cleanup keeps the test namespace
		// from growing across runs.
		_ = registry.DeleteKey(registry.CURRENT_USER, key)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\QuarkDriveMount\Tests`)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\QuarkDriveMount`)
	})
	return Store{KeyPath: key}
}

func TestSetGetRemoveRoundTrip(t *testing.T) {
	s := scratchStore(t)

	if got, err := s.Get(); err != nil || got != "" {
		t.Fatalf("Get on an empty store = %q, %v; want empty", got, err)
	}

	const cmd = `"C:\Program Files\QuarkDriveMount\quark-tray.exe" -profile`
	if err := s.Set(cmd); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get()
	if err != nil || got != cmd {
		t.Fatalf("Get = %q, %v; want %q", got, err, cmd)
	}

	// Setting again overwrites, it does not duplicate.
	if err := s.Set(`"C:\other\quark-tray.exe" -profile`); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	if got, _ := s.Get(); got != `"C:\other\quark-tray.exe" -profile` {
		t.Fatalf("Get after overwrite = %q", got)
	}

	if err := s.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, err := s.Get(); err != nil || got != "" {
		t.Fatalf("Get after remove = %q, %v; want empty", got, err)
	}

	// Removing an absent entry is a no-op.
	if err := s.Remove(); err != nil {
		t.Fatalf("Remove twice: %v", err)
	}
}

func TestTrayCommandQuotesAndPinsProfile(t *testing.T) {
	cmd, err := TrayCommand()
	if err != nil {
		t.Fatalf("TrayCommand: %v", err)
	}
	if !strings.HasPrefix(cmd, `"`) || !strings.HasSuffix(cmd, ` -profile`) {
		t.Fatalf("command = %q, want a quoted executable path with -profile", cmd)
	}
}
