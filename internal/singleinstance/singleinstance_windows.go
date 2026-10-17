//go:build windows

// Package singleinstance guards the tray against a second instance of any
// version (issue #25, ADR 0006/0013): two tray processes — old and new — must
// never hold the same drive letter or the same writes cache directory. The
// guard is a named per-session mutex, acquired without elevation, held for
// the process's lifetime.
package singleinstance

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DefaultName is the per-session mutex every tray instance contends for. The
// Local\ namespace scopes it to the logon session, so two user sessions on
// one machine do not fight over it.
const DefaultName = `Local\QuarkDriveMount.Tray`

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procCreateMutexW = kernel32.NewProc("CreateMutexW")
	procReleaseMutex = kernel32.NewProc("ReleaseMutex")
)

// Acquire creates (and owns) the named mutex. alreadyRunning reports that
// another instance of any version owns it right now — the caller must exit
// instead of starting a second mount stack. The returned release func is
// safe to call once at shutdown.
func Acquire(name string) (release func(), alreadyRunning bool, err error) {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, false, fmt.Errorf("singleinstance: name: %w", err)
	}
	handle, _, callErr := procCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))
	if handle == 0 {
		return nil, false, fmt.Errorf("singleinstance: create mutex: %w", callErr)
	}
	// CreateMutexW reports ownership by another handle through the last
	// error even on success.
	if callErr == windows.ERROR_ALREADY_EXISTS {
		_ = windows.CloseHandle(windows.Handle(handle))
		return func() {}, true, nil
	}
	return func() {
		_, _, _ = procReleaseMutex.Call(handle)
		_ = windows.CloseHandle(windows.Handle(handle))
	}, false, nil
}
