package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"hyperhand/internal/proto"
)

var (
	dwmapi                     = windows.NewLazySystemDLL("dwmapi.dll")
	pDwmGetWindowAttribute     = dwmapi.NewProc("DwmGetWindowAttribute")
	pGetClassNameW             = user32.NewProc("GetClassNameW")
	pGetWindowRect             = user32.NewProc("GetWindowRect")
	pIsWindowEnabled           = user32.NewProc("IsWindowEnabled")
	pGetWindow                 = user32.NewProc("GetWindow")
	pQueryFullProcessImageName = kernel32.NewProc("QueryFullProcessImageNameW")
	pWindowFromPoint           = user32.NewProc("WindowFromPoint")
	pGetAncestor               = user32.NewProc("GetAncestor")
	pGetSystemMetrics          = user32.NewProc("GetSystemMetrics")
	pFindWindowExW             = user32.NewProc("FindWindowExW")
)

// hostedPID returns, for an ApplicationFrameHost frame of a UWP app (class ApplicationFrameWindow), the PID of the
// app process that owns the frame's child CoreWindow; 0 for other windows, or while the frame hosts no CoreWindow of
// another process (a minimized or suspended app's CoreWindow is detached from its frame).
func hostedPID(h windows.HWND, class string, pid uint32) uint32 {
	if class != "ApplicationFrameWindow" {
		return 0
	}
	core := windows.StringToUTF16Ptr("Windows.UI.Core.CoreWindow")
	var child uintptr
	for {
		child, _, _ = pFindWindowExW.Call(uintptr(h), child, uintptr(unsafe.Pointer(core)), 0)
		if child == 0 {
			return 0
		}
		var p uint32
		windows.GetWindowThreadProcessId(windows.HWND(child), &p)
		if p != 0 && p != pid {
			return p
		}
	}
}

const (
	gwOwner                  = 4
	gaRoot                   = 2
	smXVirtualScreen         = 76 // SM_XVIRTUALSCREEN, then SM_YVIRTUALSCREEN, SM_CXVIRTUALSCREEN, SM_CYVIRTUALSCREEN
	dwmwaExtendedFrameBounds = 9
	dwmwaCloaked             = 14
)

// One callback for all list_windows calls (callbacks are never freed), with its state guarded by listMu.
var (
	listMu  sync.Mutex
	listOut []windows.HWND
	listCB  = syscall.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		listOut = append(listOut, h)
		return 1
	})
)

// topWindows returns the visible, uncloaked top-level windows from the top of the Z order down.
func topWindows() []windows.HWND {
	listMu.Lock()
	defer listMu.Unlock()
	listOut = nil
	windows.EnumWindows(listCB, nil)
	var out []windows.HWND
	for _, h := range listOut {
		if windows.IsWindowVisible(h) && !cloaked(h) {
			out = append(out, h)
		}
	}
	return out
}

// cloaked: DWM hides the window (suspended store apps, windows on other virtual desktops).
func cloaked(h windows.HWND) bool {
	var v uint32
	r, _, _ := pDwmGetWindowAttribute.Call(uintptr(h), dwmwaCloaked, uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
	return r == 0 && v != 0
}

// windowRect is the visible frame (without the invisible resize borders), falling back to GetWindowRect.
func windowRect(h windows.HWND) proto.Rect {
	var r proto.Rect
	if hr, _, _ := pDwmGetWindowAttribute.Call(uintptr(h), dwmwaExtendedFrameBounds, uintptr(unsafe.Pointer(&r)), unsafe.Sizeof(r)); hr != 0 {
		pGetWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&r)))
	}
	return r
}

func className(h windows.HWND) string {
	buf := make([]uint16, 256)
	n, _, _ := pGetClassNameW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

func processName(pid uint32) string {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(p)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if r, _, _ := pQueryFullProcessImageName.Call(uintptr(p), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}

func enabled(h windows.HWND) bool {
	r, _, _ := pIsWindowEnabled.Call(uintptr(h))
	return r != 0
}

func windowInfo(h, fg windows.HWND, names map[uint32]string) proto.WindowInfo {
	var pid uint32
	windows.GetWindowThreadProcessId(h, &pid)
	name, ok := names[pid]
	if !ok {
		name = processName(pid)
		names[pid] = name
	}
	owner, _, _ := pGetWindow.Call(uintptr(h), gwOwner)
	iconic, _, _ := pIsIconic.Call(uintptr(h))
	return proto.WindowInfo{
		Handle:     uint64(h),
		Title:      windowText(h),
		Class:      className(h),
		PID:        pid,
		Process:    name,
		Rect:       windowRect(h),
		Enabled:    enabled(h),
		Foreground: h == fg,
		Minimized:  iconic != 0,
		Owner:      uint64(owner),
		Modal:      owner != 0 && !enabled(windows.HWND(owner)),
	}
}

func listWindows(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.ListWindowsArgs
	if len(args) > 0 {
		if err := decode(args, &a); err != nil {
			return nil, nil, err
		}
	}
	// The focused element comes from a helper process; start it first so it overlaps the window enumeration.
	focused := make(chan *proto.FocusedControl, 1)
	if a.Focused {
		go func() { focused <- focusedControl(ctx) }()
	} else {
		focused <- nil
	}
	fg := windows.GetForegroundWindow()
	names := map[uint32]string{}
	levels := map[uint32]string{}
	r := proto.WindowsResult{Windows: []proto.WindowInfo{}, Foreground: uint64(fg), AgentIntegrity: tokenIntegrity(windows.GetCurrentProcessToken())}
	for _, h := range topWindows() {
		w := windowInfo(h, fg, names)
		level, ok := levels[w.PID]
		if !ok {
			level = processIntegrity(w.PID)
			levels[w.PID] = level
		}
		w.Integrity = level
		w.HostedPID = hostedPID(h, w.Class, w.PID)
		r.Windows = append(r.Windows, w)
	}
	groupRoots(r.Windows)
	if s, _, err := sessionState(ctx, nil, nil); err == nil {
		state := s.(proto.SessionStateResult)
		r.Session = &state
	}
	r.Focused = <-focused
	return r, nil, nil
}

// groupRoots sets each window's GroupRoot: the handle reached by following Owner links through listed windows of the
// same process, at most 8 links; the window itself when it has no such owner.
func groupRoots(ws []proto.WindowInfo) {
	byHandle := make(map[uint64]int, len(ws))
	for i, w := range ws {
		byHandle[w.Handle] = i
	}
	for i := range ws {
		root := i
		for n := 0; n < 8; n++ {
			o, ok := byHandle[ws[root].Owner]
			if !ok || ws[o].PID != ws[i].PID {
				break
			}
			root = o
		}
		ws[i].GroupRoot = ws[root].Handle
	}
}

// processIntegrity is the integrity level of a process's token, "" when it cannot be read (e.g. a protected process).
func processIntegrity(pid uint32) string {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(p)
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok); err != nil {
		return ""
	}
	defer tok.Close()
	return tokenIntegrity(tok)
}

// tokenIntegrity reads TokenIntegrityLevel and names the mandatory label SID's last RID.
func tokenIntegrity(tok windows.Token) string {
	// GetTokenInformation stores a pointer-bearing label followed by its SID.
	// A byte array has no pointer alignment guarantee (notably on the stack).
	var buf struct {
		label windows.Tokenmandatorylabel
		sid   [256]byte
	}
	var n uint32
	if err := windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&buf)), uint32(unsafe.Sizeof(buf)), &n); err != nil {
		return ""
	}
	sid := buf.label.Label.Sid
	if sid == nil {
		return ""
	}
	// Read the SID layout directly. GetSidSubAuthority's uintptr return loses
	// the Go allocation provenance and fails checkptr for a SID in this buffer.
	count := *(*byte)(unsafe.Add(unsafe.Pointer(sid), 1))
	if count == 0 || count > 15 {
		return ""
	}
	rid := *(*uint32)(unsafe.Add(unsafe.Pointer(sid), 8+4*(uintptr(count)-1)))
	return integrityName(rid)
}

// integrityName maps a mandatory label RID (SECURITY_MANDATORY_*_RID) to a level name.
func integrityName(rid uint32) string {
	switch {
	case rid < 0x2000:
		return "low"
	case rid < 0x3000:
		return "medium"
	case rid < 0x4000:
		return "high"
	default:
		return "system"
	}
}

// windowAt returns the top-level window under a screen point, which is the window a click there reaches.
func windowAt(_ context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.PointArgs
	if err := decode(args, &a); err != nil {
		return nil, nil, err
	}
	var m [4]int
	for i := range m {
		r, _, _ := pGetSystemMetrics.Call(uintptr(smXVirtualScreen + i))
		m[i] = int(int32(r))
	}
	if a.X < m[0] || a.Y < m[1] || a.X >= m[0]+m[2] || a.Y >= m[1]+m[3] {
		return proto.HandleResult{}, nil, nil
	}
	h, _, _ := pWindowFromPoint.Call(uintptr(uint32(int32(a.X))) | uintptr(uint32(int32(a.Y)))<<32)
	if h == 0 {
		return proto.HandleResult{}, nil, nil
	}
	root, _, _ := pGetAncestor.Call(h, gaRoot)
	// Describe it too: the covering window may be one list_windows does not show (a shell overlay above the desktop band).
	var pid uint32
	windows.GetWindowThreadProcessId(windows.HWND(root), &pid)
	return proto.HandleResult{Handle: uint64(root), Class: className(windows.HWND(root)), PID: pid, Process: processName(pid)}, nil, nil
}
