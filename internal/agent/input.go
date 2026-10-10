package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
	"hyperhand/internal/proto"
)

const keyUp = 2
const keyUnicode = 4

// INPUT's union is sized for MOUSEINPUT, with native pointer alignment.
type keyboardInput struct {
	VK, Scan    uint16
	Flags, Time uint32
	Extra       uintptr
}
type inputEvent struct {
	Type    uint32
	Key     keyboardInput
	Padding [8]byte
}

var pGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")

type inputBackend struct {
	validate func(proto.TypeKeysArgs) error
	pressed  func(uint16) bool
	send     func([]inputEvent) (int, error)
}

func typeKeys(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.TypeKeysArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, nil, err
	}
	var anchor windows.HWND
	validate := func(a proto.TypeKeysArgs) error {
		if anchor == 0 {
			anchor = inputAnchor(windows.HWND(a.Handle), liveInputWin)
		}
		return validateInputTarget(a, anchor)
	}
	r, err := typeKeysWith(ctx, a, inputBackend{validate: validate, pressed: inputKeyPressed, send: sendInputEvents})
	return r, nil, err
}

const (
	wsPopup = 0x80000000
	// A caption, sizing border or system menu marks a window the user works in (a dialog, AutoCAD's floating
	// palettes: WS_THICKFRAME|WS_SYSMENU without WS_CAPTION), not a transient input popup.
	wsFrame  = 0x00C00000 | 0x00040000 | 0x00080000 // WS_CAPTION | WS_THICKFRAME | WS_SYSMENU
	gwlStyle = ^uintptr(15)                         // GWL_STYLE (-16)
)

var pGetWindowLongPtr = user32.NewProc("GetWindowLongPtrW")

// inputWin is what the input target check reads about a window.
type inputWin struct {
	pid, tid                          uint32
	owner                             windows.HWND
	style                             uint32
	class                             string
	visible, enabled, iconic, cloaked bool
}

func liveInputWin(h windows.HWND) (inputWin, bool) {
	var w inputWin
	tid, err := windows.GetWindowThreadProcessId(h, &w.pid)
	if err != nil || tid == 0 {
		return w, false
	}
	w.tid = tid
	owner, _, _ := pGetWindow.Call(uintptr(h), gwOwner)
	w.owner = windows.HWND(owner)
	style, _, _ := pGetWindowLongPtr.Call(uintptr(h), gwlStyle)
	w.style = uint32(style)
	w.class = className(h)
	iconic, _, _ := pIsIconic.Call(uintptr(h))
	w.visible, w.enabled, w.iconic, w.cloaked = windows.IsWindowVisible(h), enabled(h), iconic != 0, cloaked(h)
	return w, true
}

// inputHelperOwner returns the owner of h when h is an input helper of it: a bare popup (no caption, sizing border or
// system menu; not a dialog) of the owner's own UI thread, such as AutoCAD's dynamic-input tooltip (CAcDynInputWndControl), which takes the foreground
// and keyboard focus as soon as a command name is typed and passes the keys on to the command line.
func inputHelperOwner(h windows.HWND, look func(windows.HWND) (inputWin, bool)) (windows.HWND, bool) {
	w, ok := look(h)
	if !ok || w.owner == 0 || w.style&wsPopup == 0 || w.style&wsFrame != 0 || w.class == "#32770" {
		return 0, false
	}
	o, ok := look(w.owner)
	if !ok || o.pid != w.pid || o.tid != w.tid {
		return 0, false
	}
	return w.owner, true
}

// inputAnchor is the window whose input helpers may take the foreground while keys are typed into h: h itself, or
// for a helper the first owner that is not one.
func inputAnchor(h windows.HWND, look func(windows.HWND) (inputWin, bool)) windows.HWND {
	for n := 0; n < 8; n++ {
		o, ok := inputHelperOwner(h, look)
		if !ok {
			break
		}
		h = o
	}
	return h
}

func validateInputTarget(a proto.TypeKeysArgs, anchor windows.HWND) error {
	var id uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &id); err != nil {
		return err
	}
	locked, err := sessionLocked(id)
	if err != nil {
		return err
	}
	secure, err := secureInputDesktop()
	if err != nil {
		return err
	}
	if locked || secure {
		return errors.New("keyboard input requires an unlocked, non-secure desktop")
	}
	return checkInputForeground(a, anchor, windows.GetForegroundWindow(), liveInputWin)
}

// checkInputForeground accepts the foreground window fg for input meant for a.Handle when fg is that window or, with
// anchor = inputAnchor(a.Handle), anchor or one of its input helpers while anchor is still visible, enabled and not
// minimized. A modal dialog disables its owner and has a caption, so it never qualifies.
func checkInputForeground(a proto.TypeKeysArgs, anchor, fg windows.HWND, look func(windows.HWND) (inputWin, bool)) error {
	usable := func(h windows.HWND) bool {
		w, ok := look(h)
		return ok && w.pid == a.PID && w.visible && w.enabled && !w.iconic && !w.cloaked
	}
	if fg == windows.HWND(a.Handle) && usable(fg) {
		return nil
	}
	if usable(anchor) && usable(fg) && inputAnchor(fg, look) == anchor {
		return nil
	}
	w, _ := look(fg)
	return fmt.Errorf("keyboard input target is no longer the visible, enabled foreground window with the requested PID (or a bare input popup of it): the foreground is window %d (class %q, pid %d)", uint64(fg), w.class, w.pid)
}

func inputKeyPressed(vk uint16) bool {
	r, _, _ := pGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

func sendInputEvents(events []inputEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	n, _, err := pSendInput.Call(uintptr(len(events)), uintptr(unsafe.Pointer(&events[0])), unsafe.Sizeof(inputEvent{}))
	runtime.KeepAlive(events)
	if int(n) == len(events) {
		return int(n), nil
	}
	return int(n), err
}

func runeInputs(r rune) []inputEvent {
	var units []uint16
	var vk uint16
	if r == '\n' || r == '\r' {
		vk = 0x0d
	} else if r == '\t' {
		vk = 0x09
	}
	if vk != 0 {
		units = []uint16{0}
	} else {
		units = utf16.Encode([]rune{r})
	}
	var events []inputEvent
	for _, unit := range units {
		k := keyboardInput{VK: vk, Scan: unit}
		if vk == 0 {
			k.Flags = keyUnicode
		}
		events = append(events, inputEvent{Type: 1, Key: k})
		k.Flags |= keyUp
		events = append(events, inputEvent{Type: 1, Key: k})
	}
	return events
}

func typeKeysWith(ctx context.Context, a proto.TypeKeysArgs, b inputBackend) (proto.TypeKeysResult, error) {
	r := proto.TypeKeysResult{}
	if a.Handle == 0 || a.PID == 0 {
		return r, errors.New("keyboard input requires handle and PID")
	}
	if !utf8.ValidString(a.Text) {
		return r, errors.New("keyboard input requires valid UTF-8")
	}
	if len(a.Text) > 16*1024 {
		return r, errors.New("keyboard input text exceeds 16384 UTF-8 bytes")
	}
	fail := func(err error) (proto.TypeKeysResult, error) {
		return r, fmt.Errorf("keyboard input stopped after %d injected events; text may be partial, do not retry automatically: %w", r.Events, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := b.validate(a); err != nil {
		return fail(err)
	}
	for _, ch := range strings.ReplaceAll(a.Text, "\r\n", "\n") {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := b.validate(a); err != nil {
			return fail(err)
		}
		events := runeInputs(ch)
		for _, vk := range []uint16{0x10, 0x11, 0x12, 0x5b, 0x5c, events[0].Key.VK} {
			if vk != 0 && b.pressed(vk) {
				return fail(fmt.Errorf("key 0x%02x is already held; release it before typing", vk))
			}
		}
		n, err := b.send(events)
		if n < 0 || n > len(events) {
			return fail(errors.New("SendInput returned an invalid event count"))
		}
		r.Events += n
		if n != len(events) {
			// Every batch consists of adjacent down/up pairs. Only an odd prefix
			// leaves our own last key down; never release unrelated user keys.
			cleanup := ""
			if n%2 != 0 {
				if released, _ := b.send(events[n : n+1]); released != 1 {
					cleanup = "; injected key release also failed"
				}
			}
			return fail(fmt.Errorf("SendInput inserted %d/%d events (UIPI may block input): %v%s", n, len(events), err, cleanup))
		}
	}
	return r, nil
}
