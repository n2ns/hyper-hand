package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pOpenClipboard       = user32.NewProc("OpenClipboard")
	pCloseClipboard      = user32.NewProc("CloseClipboard")
	pEmptyClipboard      = user32.NewProc("EmptyClipboard")
	pGetClipboardData    = user32.NewProc("GetClipboardData")
	pSetClipboardData    = user32.NewProc("SetClipboardData")
	pGetWindowTextW      = user32.NewProc("GetWindowTextW")
	pIsIconic            = user32.NewProc("IsIconic")
	pShowWindow          = user32.NewProc("ShowWindow")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pAttachThreadInput   = user32.NewProc("AttachThreadInput")
	pBringWindowToTop    = user32.NewProc("BringWindowToTop")
	pIsHungAppWindow     = user32.NewProc("IsHungAppWindow")
	pSendInput           = user32.NewProc("SendInput")
	pSetCursorPos        = user32.NewProc("SetCursorPos")
	pGlobalAlloc         = kernel32.NewProc("GlobalAlloc")
	pGlobalFree          = kernel32.NewProc("GlobalFree")
	pGlobalLock          = kernel32.NewProc("GlobalLock")
	pGlobalUnlock        = kernel32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText     = 13
	gmemMoveable      = 0x0002
	swRestore         = 9
	inputMouse        = 0
	mouseeventfMove   = 0x0001
	mouseeventfHWheel = 0x1000
	wheelDelta        = 120
)

func openClipboard() error {
	for i := 0; i < 20; i++ {
		if r, _, _ := pOpenClipboard.Call(0); r != 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errBusy
}

func clipboardGet(context.Context, json.RawMessage, []byte) (any, []byte, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := openClipboard(); err != nil {
		return nil, nil, err
	}
	defer pCloseClipboard.Call()
	h, _, _ := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return proto.TextResult{}, nil, nil
	}
	p, _, err := pGlobalLock.Call(h)
	if p == 0 {
		return nil, nil, err
	}
	defer pGlobalUnlock.Call(h)
	return proto.TextResult{Text: windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&p)))}, nil, nil
}

func clipboardSet(_ context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.TextArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	u, err := windows.UTF16FromString(a.Text)
	if err != nil {
		return nil, nil, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := openClipboard(); err != nil {
		return nil, nil, err
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	h, _, err := pGlobalAlloc.Call(gmemMoveable, uintptr(len(u)*2))
	if h == 0 {
		return nil, nil, err
	}
	p, _, err := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return nil, nil, err
	}
	copy(unsafe.Slice(*(**uint16)(unsafe.Pointer(&p)), len(u)), u)
	pGlobalUnlock.Call(h)
	if r, _, err := pSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		pGlobalFree.Call(h)
		return nil, nil, err
	}
	return nil, nil, nil
}

func windowText(h windows.HWND) string {
	buf := make([]uint16, 512)
	n, _, _ := pGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

// One callback for all EnumWindows calls (callbacks are never freed), with its state guarded by enumMu.
var (
	enumMu    sync.Mutex
	enumWant  string
	enumFound windows.HWND
	enumTitle string
	enumCB    = syscall.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		if !windows.IsWindowVisible(h) {
			return 1
		}
		if t := windowText(h); t != "" && strings.Contains(strings.ToLower(t), enumWant) {
			enumFound, enumTitle = h, t
			return 0
		}
		return 1
	})
)

// findWindow returns the first visible top-level window whose title contains want (case-insensitive).
func findWindow(want string) (windows.HWND, string) {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumWant, enumFound, enumTitle = strings.ToLower(want), 0, ""
	windows.EnumWindows(enumCB, nil) // returns an error when the callback stops early
	return enumFound, enumTitle
}

// errNotResponding starts focus_window's error for a hung window; the host maps it to target_not_responding.
const errNotResponding = "window not responding"

func focusWindow(_ context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.TitleArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	var found windows.HWND
	var title string
	if a.Handle != 0 {
		found = windows.HWND(a.Handle)
		if !slices.Contains(topWindows(), found) {
			return nil, nil, fmt.Errorf("no visible top-level window has handle %d", a.Handle)
		}
	} else if found, title = findWindow(a.Title); found == 0 {
		return nil, nil, fmt.Errorf("no visible window title contains %q", a.Title)
	}
	// ShowWindow and BringWindowToTop send messages to the window's thread and wait: a hung window (Windows shows a
	// "Not Responding" ghost in its place) would block this op, and every later one on the connection, until it recovers.
	// Checked before reading the title, which also sends a message when the window belongs to this process.
	if r, _, _ := pIsHungAppWindow.Call(uintptr(found)); r != 0 {
		return nil, nil, fmt.Errorf("%s: window %d has not processed window messages for 5 s", errNotResponding, found)
	}
	if title == "" {
		title = windowText(found)
	}
	runtime.LockOSThread() // AttachThreadInput is per thread
	defer runtime.UnlockOSThread()
	if r, _, _ := pIsIconic.Call(uintptr(found)); r != 0 {
		pShowWindow.Call(uintptr(found), swRestore)
	}
	if r, _, _ := pSetForegroundWindow.Call(uintptr(found)); r == 0 || windows.GetForegroundWindow() != found {
		// Usual workaround: attach to the foreground thread's input and make this process the source of the last
		// input event. That input is a zero mouse move: an injected Alt press is delivered after the switch, to the
		// window being focused, and puts ribbon programs such as AutoCAD into key-tip mode.
		fgTid, _ := windows.GetWindowThreadProcessId(windows.GetForegroundWindow(), nil)
		me := windows.GetCurrentThreadId()
		if fgTid != 0 && fgTid != me {
			pAttachThreadInput.Call(uintptr(me), uintptr(fgTid), 1)
			defer pAttachThreadInput.Call(uintptr(me), uintptr(fgTid), 0)
		}
		in := mouseInput{typ: inputMouse, flags: mouseeventfMove}
		pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
		pBringWindowToTop.Call(uintptr(found))
		pSetForegroundWindow.Call(uintptr(found))
	}
	if windows.GetForegroundWindow() != found {
		return nil, nil, fmt.Errorf("could not bring %q to the foreground", title)
	}
	return proto.FocusResult{Text: title, Handle: uint64(found)}, nil, nil
}

// hscroll turns the horizontal wheel at a screen point through SendInput; the Hyper-V synthetic mouse has no
// horizontal wheel, so the host cannot do this itself.
func hscroll(_ context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.HScrollArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	if r, _, err := pSetCursorPos.Call(uintptr(uint32(int32(a.X))), uintptr(uint32(int32(a.Y)))); r == 0 {
		return nil, nil, fmt.Errorf("SetCursorPos: %w", err)
	}
	in := mouseInput{typ: inputMouse, mouseData: uint32(int32(a.Delta * wheelDelta)), flags: mouseeventfHWheel}
	if n, _, err := pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in)); n != 1 {
		return nil, nil, fmt.Errorf("SendInput inserted no event (UIPI may block input): %v", err)
	}
	return nil, nil, nil
}

// mouseInput is a Win32 INPUT holding a MOUSEINPUT (40 bytes on 64-bit Windows).
type mouseInput struct {
	typ       uint32
	_         uint32
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	extra     uintptr
}

var errBusy = errors.New("clipboard busy")
