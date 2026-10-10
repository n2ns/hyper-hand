package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"hyperhand/internal/proto"
)

func TestInputLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 && (unsafe.Sizeof(inputEvent{}) != 40 || unsafe.Offsetof(inputEvent{}.Key) != 8 || unsafe.Offsetof(keyboardInput{}.Extra) != 16) {
		t.Fatal("INPUT/KEYBDINPUT must match Windows amd64 ABI")
	}
}

func TestTypeKeysUnicodeAndControls(t *testing.T) {
	var got []inputEvent
	b := inputBackend{validate: func(proto.TypeKeysArgs) error { return nil }, pressed: func(uint16) bool { return false }, send: func(e []inputEvent) (int, error) { got = append(got, e...); return len(e), nil }}
	r, err := typeKeysWith(context.Background(), proto.TypeKeysArgs{Text: "A中😀\r\n\t", Handle: 1, PID: 2}, b)
	if err != nil || r.Events != 12 {
		t.Fatalf("result=%+v err=%v", r, err)
	}
	var scans []uint16
	for i := 0; i < len(got); i += 2 {
		if got[i].Key.Flags&keyUp != 0 || got[i+1].Key.Flags != got[i].Key.Flags|keyUp {
			t.Fatal("unbalanced key events")
		}
		scans = append(scans, got[i].Key.Scan)
	}
	if !reflect.DeepEqual(scans, []uint16{65, 0x4e2d, 0xd83d, 0xde00, 0, 0}) || got[8].Key.VK != 13 || got[10].Key.VK != 9 {
		t.Fatalf("events=%+v", got)
	}
}

func TestTypeKeysPartialSend(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3} {
		t.Run(string(rune('0'+n)), func(t *testing.T) {
			calls := 0
			b := inputBackend{validate: func(proto.TypeKeysArgs) error { return nil }, pressed: func(uint16) bool { return false }, send: func(e []inputEvent) (int, error) {
				calls++
				if calls == 1 {
					return n, errors.New("blocked")
				}
				if len(e) != 1 || e[0].Key.Flags&keyUp == 0 {
					t.Fatalf("cleanup released wrong events: %+v", e)
				}
				return 1, nil
			}}
			r, err := typeKeysWith(context.Background(), proto.TypeKeysArgs{Text: "😀more", Handle: 1, PID: 2}, b)
			if err == nil || !strings.Contains(err.Error(), "do not retry") || r.Events != n || calls != 1+n%2 {
				t.Fatalf("result=%+v err=%v calls=%d", r, err, calls)
			}
		})
	}
}

func TestTypeKeysStopsBeforeInput(t *testing.T) {
	for _, mode := range []string{"cancel", "target", "modifier", "handle", "pid"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := proto.TypeKeysArgs{Text: "abc", Handle: 1, PID: 2}
			if mode == "cancel" {
				cancel()
			}
			if mode == "handle" {
				a.Handle = 0
			}
			if mode == "pid" {
				a.PID = 0
			}
			b := inputBackend{validate: func(proto.TypeKeysArgs) error {
				if mode == "target" {
					return errors.New("changed")
				}
				return nil
			}, pressed: func(uint16) bool { return mode == "modifier" }, send: func([]inputEvent) (int, error) { t.Fatal("must not inject input"); return 0, nil }}
			if _, err := typeKeysWith(ctx, a, b); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestTypeKeysStopsBetweenCharacters(t *testing.T) {
	for _, mode := range []string{"cancel", "focus"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			b := inputBackend{validate: func(proto.TypeKeysArgs) error {
				if calls != 0 && mode == "focus" {
					return errors.New("focus changed")
				}
				return nil
			}, pressed: func(uint16) bool { return false }, send: func(e []inputEvent) (int, error) {
				calls++
				if mode == "cancel" {
					cancel()
				}
				return len(e), nil
			}}
			r, err := typeKeysWith(ctx, proto.TypeKeysArgs{Text: "ab", Handle: 1, PID: 2}, b)
			if err == nil || calls != 1 || r.Events != 2 {
				t.Fatalf("result=%+v err=%v calls=%d", r, err, calls)
			}
		})
	}
}

// The windows of TestInputForeground, modelled on AutoCAD 2015 on Win10 (2026-10-10): the main frame, its
// dynamic-input tooltip (WS_POPUP|WS_VISIBLE|WS_CLIPSIBLINGS, no extended style, owned by the frame, same thread),
// a modal dialog that disabled the frame, a captionless dialog, the floating Properties palette (WS_THICKFRAME|
// WS_SYSMENU without WS_CAPTION), a popup of another thread and another process.
func inputWins(frameEnabled bool) func(windows.HWND) (inputWin, bool) {
	ws := map[windows.HWND]inputWin{
		10: {pid: 2, tid: 7, style: 0x17CF0000, class: "AfxMDIFrame110u", visible: true, enabled: frameEnabled},
		11: {pid: 2, tid: 7, owner: 10, style: 0x94000000, class: "Afx:00007FF6D7500000:803", visible: true, enabled: true},
		12: {pid: 2, tid: 7, owner: 10, style: 0x94C80000, class: "#32770", visible: true, enabled: true},
		13: {pid: 2, tid: 7, owner: 10, style: 0x94000000, class: "#32770", visible: true, enabled: true},
		14: {pid: 2, tid: 8, owner: 10, style: 0x94000000, class: "Popup", visible: true, enabled: true},
		15: {pid: 3, tid: 9, owner: 10, style: 0x94000000, class: "Popup", visible: true, enabled: true},
		16: {pid: 2, tid: 7, owner: 11, style: 0x94000000, class: "Nested", visible: true, enabled: true},
		17: {pid: 2, tid: 7, owner: 10, style: 0x94000000, class: "Hidden", enabled: true},
		18: {pid: 2, tid: 7, owner: 10, style: 0x960C3500, class: "Afx:00007FF6D7500000:8", visible: true, enabled: true},
	}
	return func(h windows.HWND) (inputWin, bool) { w, ok := ws[h]; return w, ok }
}

func TestInputForeground(t *testing.T) {
	for _, c := range []struct {
		name         string
		handle, fg   windows.HWND
		frameEnabled bool
		ok           bool
	}{
		{"target itself", 10, 10, true, true},
		{"tooltip takes the foreground", 10, 11, true, true},
		{"nested helper", 10, 16, true, true},
		{"typing started in the tooltip, back to the frame", 11, 10, true, true},
		{"modal dialog disables the frame", 10, 12, false, false},
		{"captionless dialog", 10, 13, true, false},
		{"popup of another thread", 10, 14, true, false},
		{"popup of another process", 10, 15, true, false},
		{"hidden helper", 10, 17, true, false},
		{"floating palette (sizing border, system menu, no caption)", 10, 18, true, false},
		{"typing into the palette, then the frame takes over", 18, 10, true, false},
		{"frame disabled under its tooltip", 10, 11, false, false},
		{"typing into the modal dialog itself", 12, 12, false, true},
		{"unknown window", 10, 99, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			look := inputWins(c.frameEnabled)
			err := checkInputForeground(proto.TypeKeysArgs{Handle: uint64(c.handle), PID: 2}, inputAnchor(c.handle, look), c.fg, look)
			if (err == nil) != c.ok {
				t.Fatalf("err=%v, want ok=%v", err, c.ok)
			}
		})
	}
}
