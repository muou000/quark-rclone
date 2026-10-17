//go:build windows

package power

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassW   = user32.NewProc("RegisterClassW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMsg     = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procShutdownBlock    = user32.NewProc("ShutdownBlockReasonCreate")
	procShutdownUnblock  = user32.NewProc("ShutdownBlockReasonDestroy")
)

const (
	hwndMessage = ^uintptr(2) // HWND_MESSAGE: a message-only window
	wmClose     = 0x0010

	// blockReason is the string Windows shows while the tray processes an
	// ENDSESSION inline; the teardown runs to completion before the window
	// procedure returns and the session is allowed to die. The API carries
	// the reason text only — no extra grace is granted by it.
	blockReason = "夸克网盘挂载正在卸载盘符并停止组件…"
)

// wndClass carries the callback state; the WndProc is created from a method
// value so it can reach it directly.
type wndClass struct {
	fn func(Event)
}

// Start creates the hidden message-only window on a locked OS thread and
// pumps its messages until the returned stop runs. fn is invoked for every
// lifecycle event; the session-end delivery is synchronous, so the teardown
// finishes before the WndProc returns and the session is allowed to die.
func Start(fn func(Event)) (stop func(), err error) {
	errCh := make(chan error, 1)
	hwndCh := make(chan uintptr, 1)
	var stopOnce sync.Once

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := pump(fn, hwndCh); err != nil {
			errCh <- err
		}
	}()

	// Start returns as soon as the window exists; a pump that died on the
	// way surfaces as the startup error.
	select {
	case err := <-errCh:
		return nil, err
	case hwnd := <-hwndCh:
		return func() {
			stopOnce.Do(func() {
				procPostMessageW.Call(hwnd, wmClose, 0, 0)
			})
		}, nil
	}
}

// pump creates the window, hands its handle to the caller and runs the
// message loop. It returns when the window is destroyed (the stop path posts
// WM_CLOSE, which destroys the window and quits the thread's message loop)
// or when GetMessage fails.
func pump(fn func(Event), hwndCh chan<- uintptr) error {
	className, _ := syscall.UTF16PtrFromString("QuarkDriveMountPower")
	// WNDCLASSW, field for field: UINT style; WNDPROC wndProc; INT
	// cbClsExtra; INT cbWndExtra; HINSTANCE hInstance; HICON hIcon; HCURSOR
	// hCursor; HBRUSH hbrBackground; LPCWSTR lpszMenuName; LPCWSTR
	// lpszClassName. Go's layout matches the C one on x64.
	type wndClassW struct {
		style      uint32
		wndProc    uintptr
		clsExtra   int32
		wndExtra   int32
		instance   uintptr
		icon       uintptr
		cursor     uintptr
		background uintptr
		menuName   uintptr
		name       uintptr
	}
	wc := &wndClass{fn: fn}
	class := wndClassW{wndProc: syscall.NewCallback(wc.wndProc), name: uintptr(unsafe.Pointer(className))}

	atom, _, callErr := procRegisterClassW.Call(uintptr(unsafe.Pointer(&class)))
	if atom == 0 && callErr != windows.ERROR_CLASS_ALREADY_EXISTS {
		// A previous Start in this process already registered the class; the
		// window creation below works all the same.
		return fmt.Errorf("power: register class: %w", callErr)
	}

	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0, 0,
		hwndMessage, 0, 0, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("power: create window: %w", callErr)
	}
	hwndCh <- hwnd
	defer procShutdownUnblock.Call(hwnd)

	// MSG on x64 is 48 bytes; the pump owns it.
	var buf [48]byte
	for {
		ret, _, _ := procGetMessageW.Call(
			uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0)
		if ret == 0 || ret == ^uintptr(0) {
			// WM_QUIT (posted by the WndProc after the window was destroyed)
			// or a GetMessage failure: the pump is done either way.
			return nil
		}
		procTranslateMsg.Call(uintptr(unsafe.Pointer(&buf[0])))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&buf[0])))
	}
}

// wndProc is the window procedure: lifecycle events go to fn, everything
// else to DefWindowProc. The returns follow the contracts of the messages —
// WM_POWERBROADCAST wants TRUE, WM_ENDSESSION wants TRUE once processed,
// WM_QUERYENDSESSION wants TRUE to allow the session to end.
func (wc *wndClass) wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch classify(msg, wParam) {
	case EventSuspend:
		// Nothing to do while the machine sleeps; the resume path re-runs the
		// mount's checks.
		return 1
	case EventResume:
		releaseShutdownGrace(hwnd)
		go wc.fn(EventResume)
		return 1
	case EventSessionEnd:
		// The teardown must finish before the session dies: run it inline and
		// only then allow the session to proceed.
		releaseShutdownGrace(hwnd)
		wc.fn(EventSessionEnd)
		return 1
	}
	switch msg {
	case wmClose:
		// The stop path: destroy the window and quit the thread's loop, so
		// GetMessage returns 0 and the pump ends.
		procDestroyWindow.Call(hwnd)
		procPostQuitMessage.Call(0)
		return 0
	case wmQueryEndSession:
		requestShutdownReason(hwnd)
		return 1
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

// requestShutdownReason shows Windows' shutdown-blocking reason while the
// tray processes an ENDSESSION inline. The API carries a reason string only —
// the actual grace the tray gets is Windows' own end-session timeout, which
// the synchronous teardown is designed to fit (ADR 0013).
func requestShutdownReason(hwnd uintptr) {
	reason, _ := syscall.UTF16PtrFromString(blockReason)
	procShutdownBlock.Call(hwnd, uintptr(unsafe.Pointer(reason)))
}

// releaseShutdownGrace drops the shutdown-blocking reason.
func releaseShutdownGrace(hwnd uintptr) {
	procShutdownUnblock.Call(hwnd)
}
