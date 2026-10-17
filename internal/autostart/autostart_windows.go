// Package autostart manages the tray's current-user logon autostart entry
// (issue #25, ADR 0004). The entry lives under HKCU's Run key: no SYSTEM
// service, no elevated rights, exactly the session the user logged into.
//
// The key path is a parameter, not a constant, so tests can exercise the same
// code against a scratch key instead of the machine's real Run entry.
package autostart

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// runKeyPath is the well-known per-user autostart key.
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// valueName is this application's value inside the Run key.
const valueName = "QuarkDriveMount"

// TrayCommand builds the autostart command line for the running tray
// executable: quoted path, profile mode pinned.
func TrayCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("autostart: resolve executable: %w", err)
	}
	return fmt.Sprintf(`"%s" -profile`, filepath.Clean(exe)), nil
}

// Store is one Run-key binding. The zero value targets the real Run key.
type Store struct {
	// KeyPath overrides the key path (tests point it at a scratch key).
	KeyPath string
}

// Set writes the autostart value, creating the key if absent and overwriting
// whatever was there.
func (s Store) Set(command string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, s.path(), registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open key: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(valueName, command); err != nil {
		return fmt.Errorf("autostart: write value: %w", err)
	}
	return nil
}

// Remove deletes the autostart value. Removing an absent entry is a no-op.
func (s Store) Remove() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, s.path(), registry.SET_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return fmt.Errorf("autostart: open key: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(valueName); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("autostart: delete value: %w", err)
	}
	return nil
}

// Get returns the stored command, or "" when the entry is absent.
func (s Store) Get() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, s.path(), registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return "", nil
		}
		return "", fmt.Errorf("autostart: open key: %w", err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue(valueName)
	if err == registry.ErrNotExist {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("autostart: read value: %w", err)
	}
	return v, nil
}

// path resolves the key path the store targets.
func (s Store) path() string {
	if s.KeyPath != "" {
		return s.KeyPath
	}
	return runKeyPath
}
