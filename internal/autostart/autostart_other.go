//go:build !windows

// Package autostart manages the tray's current-user logon autostart entry.
// Non-Windows platforms have no Run key; every operation fails loudly so a
// misconfigured cross-build cannot silently claim to have enabled autostart.
package autostart

import "errors"

// ErrUnsupported reports the platform has no autostart mechanism here.
var ErrUnsupported = errors.New("autostart: unsupported on this platform")

// TrayCommand builds the autostart command line for the running executable.
func TrayCommand() (string, error) { return "", ErrUnsupported }

// Store is the autostart binding; every operation fails off Windows.
type Store struct{}

// Set writes the autostart value.
func (s Store) Set(string) error { return ErrUnsupported }

// Remove deletes the autostart value.
func (s Store) Remove() error { return ErrUnsupported }

// Get returns the stored command.
func (s Store) Get() (string, error) { return "", ErrUnsupported }
