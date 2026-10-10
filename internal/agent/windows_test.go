package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

var (
	pCreateWindowExW = user32.NewProc("CreateWindowExW")
	pDestroyWindow   = user32.NewProc("DestroyWindow")
	pEnableWindow    = user32.NewProc("EnableWindow")
)

// testWindow creates a visible, unactivated popup window ("STATIC" with SS_NOTIFY, so hit tests do not pass through it).
func testWindow(t *testing.T, title string, owner windows.HWND, exStyle uintptr, x, y, w, h int32) windows.HWND {
	t.Helper()
	const wsPopup, wsVisible, ssNotify = 0x80000000, 0x10000000, 0x100
	cls, _ := windows.UTF16PtrFromString("STATIC")
	name, _ := windows.UTF16PtrFromString(title)
	hw, _, err := pCreateWindowExW.Call(exStyle, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(name)),
		wsPopup|wsVisible|ssNotify, uintptr(x), uintptr(y), uintptr(w), uintptr(h), uintptr(owner), 0, 0, 0)
	if hw == 0 {
		t.Fatal(err)
	}
	t.Cleanup(func() { pDestroyWindow.Call(hw) })
	return windows.HWND(hw)
}

// offscreenWindow creates a visible, unactivated popup window far off screen so the test does not disturb the desktop.
func offscreenWindow(t *testing.T, title string, owner windows.HWND, x int32) windows.HWND {
	y := int32(-30000)
	t.Helper()
	const wsPopup, wsVisible, wsExNoActivate, wsExToolWindow = 0x80000000, 0x10000000, 0x08000000, 0x80
	cls, _ := windows.UTF16PtrFromString("STATIC")
	name, _ := windows.UTF16PtrFromString(title)
	h, _, err := pCreateWindowExW.Call(wsExNoActivate|wsExToolWindow, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(name)),
		wsPopup|wsVisible, uintptr(x), uintptr(y), 300, 200, uintptr(owner), 0, 0, 0)
	if h == 0 {
		t.Fatal(err)
	}
	t.Cleanup(func() { pDestroyWindow.Call(h) })
	return windows.HWND(h)
}

func TestListWindows(t *testing.T) {
	runtime.LockOSThread() // the windows belong to this thread
	defer runtime.UnlockOSThread()
	// Match the agent executable's DPI awareness: DWM returns physical bounds,
	// but an unaware test thread otherwise creates these windows in scaled units.
	setDPI := user32.NewProc("SetThreadDpiAwarenessContext")
	oldDPI, _, err := setDPI.Call(^uintptr(1)) // DPI_AWARENESS_CONTEXT_SYSTEM_AWARE (-2)
	if oldDPI == 0 {
		t.Fatal(err)
	}
	defer setDPI.Call(oldDPI)
	tag := fmt.Sprintf("hyperhand-test-%d", os.Getpid())
	owner := offscreenWindow(t, tag+"-owner", 0, -30000)
	dialog := offscreenWindow(t, tag+"-dialog", owner, -29000)
	pEnableWindow.Call(uintptr(owner), 0) // what a modal dialog does to its owner

	r, _, err := listWindows(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]proto.WindowInfo{}
	for _, w := range r.(proto.WindowsResult).Windows {
		if strings.HasPrefix(w.Title, tag) {
			got[w.Title] = w
		}
	}
	exe, _ := os.Executable()
	o, d := got[tag+"-owner"], got[tag+"-dialog"]
	if o.Handle != uint64(owner) || o.Enabled || o.Modal || o.Owner != 0 || o.Class != "Static" || o.PID != uint32(os.Getpid()) || !strings.EqualFold(o.Process, filepath.Base(exe)) {
		t.Errorf("owner: %+v", o)
	}
	if o.Rect != (proto.Rect{Left: -30000, Top: -30000, Right: -29700, Bottom: -29800}) {
		t.Errorf("owner rect: %+v", o.Rect)
	}
	if d.Handle != uint64(dialog) || !d.Enabled || !d.Modal || d.Owner != uint64(owner) {
		t.Errorf("dialog: %+v", d)
	}
	if o.GroupRoot != uint64(owner) || d.GroupRoot != uint64(owner) {
		t.Errorf("group roots: owner %d dialog %d, want %d", o.GroupRoot, d.GroupRoot, owner)
	}
	own := tokenIntegrity(windows.GetCurrentProcessToken())
	if own == "" || o.Integrity != own || d.Integrity != own {
		t.Errorf("integrity: agent %q owner %q dialog %q", own, o.Integrity, d.Integrity)
	}
}

// The summary fields of list_windows: foreground, session and the agent's level; the focused control is nil or
// consistent (what has focus on the test machine is not under our control).
func TestListWindowsSummary(t *testing.T) {
	res, _, err := listWindows(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := res.(proto.WindowsResult)
	if r.Foreground != uint64(windows.GetForegroundWindow()) && r.Foreground == 0 {
		t.Errorf("foreground %d", r.Foreground)
	}
	if r.Session == nil || !r.Session.Console {
		t.Errorf("session: %+v", r.Session)
	}
	if r.AgentIntegrity != "medium" && r.AgentIntegrity != "high" {
		t.Errorf("agent integrity %q", r.AgentIntegrity)
	}
	if f := r.Focused; f != nil && (f.Window == 0 || f.ControlType == "") {
		t.Errorf("focused: %+v", f)
	}
	for _, w := range r.Windows {
		if w.GroupRoot == 0 {
			t.Errorf("no group root: %+v", w)
		}
	}
}

func TestGroupRoots(t *testing.T) {
	ws := []proto.WindowInfo{
		{Handle: 1, PID: 10},            // root
		{Handle: 2, PID: 10, Owner: 1},  // owned by 1
		{Handle: 3, PID: 10, Owner: 2},  // owned through 2
		{Handle: 4, PID: 11, Owner: 1},  // other process: its own root
		{Handle: 5, PID: 10, Owner: 99}, // owner not listed
		{Handle: 6, PID: 12, Owner: 7},  // cycle
		{Handle: 7, PID: 12, Owner: 6},
	}
	groupRoots(ws)
	want := []uint64{1, 1, 1, 4, 5, 6, 7} // a cycle stops after 8 links, back on the window itself
	for i, w := range ws {
		if w.GroupRoot != want[i] {
			t.Errorf("handle %d: group root %d, want %d", w.Handle, w.GroupRoot, want[i])
		}
	}
	// A chain longer than 8 links stops after 8.
	var chain []proto.WindowInfo
	for i := uint64(1); i <= 12; i++ {
		chain = append(chain, proto.WindowInfo{Handle: i, PID: 1, Owner: i - 1})
	}
	groupRoots(chain)
	if chain[11].GroupRoot != 4 || chain[8].GroupRoot != 1 {
		t.Errorf("long chain: %d %d", chain[11].GroupRoot, chain[8].GroupRoot)
	}
}

func TestIntegrityName(t *testing.T) {
	for rid, want := range map[uint32]string{0: "low", 0x1000: "low", 0x2000: "medium", 0x2100: "medium", 0x3000: "high", 0x4000: "system", 0x5000: "system"} {
		if got := integrityName(rid); got != want {
			t.Errorf("0x%x: %q, want %q", rid, got, want)
		}
	}
	if processIntegrity(0xFFFFFFFF) != "" {
		t.Error("integrity of a missing process")
	}
	if own := tokenIntegrity(windows.GetCurrentProcessToken()); own != "medium" && own != "high" {
		t.Errorf("own integrity %q", own)
	}
}

var pGetCursorPos = user32.NewProc("GetCursorPos")

// hscroll moves the cursor to the point and injects a horizontal wheel event; a small window of ours receives it.
func TestHScroll(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var saved struct{ X, Y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&saved)))
	defer pSetCursorPos.Call(uintptr(uint32(saved.X)), uintptr(uint32(saved.Y)))
	const wsExTopmost, wsExNoActivate, wsExToolWindow = 0x8, 0x08000000, 0x80
	testWindow(t, "hyperhand-test-hscroll", 0, wsExTopmost|wsExNoActivate|wsExToolWindow, 40, 300, 30, 30)
	if _, _, err := hscroll(context.Background(), mustJSON(proto.HScrollArgs{X: 55, Y: 315, Delta: -2}), nil); err != nil {
		t.Fatal(err)
	}
	var now struct{ X, Y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&now)))
	if now.X != 55 || now.Y != 315 {
		t.Errorf("cursor at (%d, %d)", now.X, now.Y)
	}
}

func TestFocusWindowBadHandle(t *testing.T) {
	_, _, err := focusWindow(context.Background(), mustJSON(proto.TitleArgs{Handle: 1}), nil)
	if err == nil || !strings.Contains(err.Error(), "handle 1") {
		t.Fatalf("want a missing-handle error, got %v", err)
	}
}

// A window whose thread stops pumping messages is refused at once instead of blocking focus_window until it recovers.
func TestFocusWindowRefusesHungWindow(t *testing.T) {
	created, release, done := make(chan windows.HWND), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread() // the window belongs to this thread; it is destroyed with it
		defer runtime.UnlockOSThread()
		const wsPopup, wsVisible, wsExNoActivate, wsExToolWindow = 0x80000000, 0x10000000, 0x08000000, 0x80
		cls, _ := windows.UTF16PtrFromString("STATIC")
		name, _ := windows.UTF16PtrFromString("HyperHand hung window test")
		h, _, _ := pCreateWindowExW.Call(wsExNoActivate|wsExToolWindow, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(name)),
			wsPopup|wsVisible, 0, uintptr(uint32(0xFFFF8AD0)), 300, 200, 0, 0, 0, 0) // y = -30000
		var msg [48]byte
		user32.NewProc("PeekMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 0) // pump once, then hang
		created <- windows.HWND(h)
		<-release
		pDestroyWindow.Call(h)
	}()
	h := <-created
	defer func() { close(release); <-done }()
	if h == 0 {
		t.Fatal("CreateWindowExW failed")
	}
	deadline := time.Now().Add(15 * time.Second)
	for r, _, _ := pIsHungAppWindow.Call(uintptr(h)); r == 0; r, _, _ = pIsHungAppWindow.Call(uintptr(h)) {
		if time.Now().After(deadline) {
			t.Fatal("the window never became hung")
		}
		time.Sleep(250 * time.Millisecond)
	}
	start := time.Now()
	_, _, err := focusWindow(context.Background(), mustJSON(proto.TitleArgs{Handle: uint64(h)}), nil)
	if err == nil || !strings.HasPrefix(err.Error(), errNotResponding) {
		t.Fatalf("focusWindow on a hung window: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("focusWindow took %v", d)
	}
}

func TestWindowAtOffScreen(t *testing.T) {
	r, _, err := windowAt(context.Background(), mustJSON(proto.PointArgs{X: -30000, Y: -30000}), nil)
	if err != nil || r.(proto.HandleResult).Handle != 0 {
		t.Fatalf("off screen: %+v %v", r, err)
	}
}

// A small always-on-top window on screen: window_at finds it at a point inside it, and not with x and y swapped.
func TestWindowAt(t *testing.T) {
	runtime.LockOSThread() // window_at sends WM_NCHITTEST to this thread's window
	defer runtime.UnlockOSThread()
	const wsExTopmost, wsExNoActivate, wsExToolWindow = 0x8, 0x08000000, 0x80
	h := testWindow(t, "hyperhand-test-hit", 0, wsExTopmost|wsExNoActivate|wsExToolWindow, 40, 300, 30, 30)
	for _, c := range []struct {
		x, y int
		want bool
	}{{55, 315, true}, {315, 55, false}} {
		r, _, err := windowAt(context.Background(), mustJSON(proto.PointArgs{X: c.x, Y: c.y}), nil)
		if err != nil {
			t.Fatal(err)
		}
		hit := r.(proto.HandleResult)
		if got := hit.Handle == uint64(h); got != c.want {
			t.Errorf("(%d, %d): handle %d, test window %d", c.x, c.y, hit.Handle, h)
		}
		// The hit window is described as well, since list_windows may not show it.
		if c.want && (!strings.EqualFold(hit.Class, "Static") || hit.PID != windows.GetCurrentProcessId() || hit.Process == "") {
			t.Errorf("hit description: %+v", hit)
		}
	}
}

func TestMouseInputSize(t *testing.T) {
	if n := unsafe.Sizeof(mouseInput{}); n != 40 { // sizeof(INPUT) on 64-bit Windows; SendInput rejects other sizes
		t.Fatalf("mouseInput is %d bytes", n)
	}
}
