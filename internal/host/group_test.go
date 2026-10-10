package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

// groupWindows mirrors AutoCAD 2015 as observed: an untitled command line owned by the main window, a transient
// command history popup owned by the command line, and windows that must stay outside the main window's group.
func groupWindows(fg uint64) []proto.WindowInfo {
	ws := []proto.WindowInfo{
		{Handle: 22, PID: 100, Class: "HwndWrapper[hist]", Process: "acad.exe", Owner: 21, Enabled: true, Rect: proto.Rect{Left: 438, Top: 923, Right: 510, Bottom: 985}},
		{Handle: 21, PID: 100, Class: "HwndWrapper[cmd]", Process: "acad.exe", Owner: 20, Enabled: true, Rect: proto.Rect{Left: 384, Top: 984, Right: 1536, Bottom: 1011}},
		{Handle: 20, PID: 100, Title: "Autodesk AutoCAD 2015 - [Drawing1.dwg]", Class: "AfxMDIFrame110u", Process: "acad.exe", Enabled: true, Rect: proto.Rect{Right: 1920, Bottom: 1040}},
		{Handle: 24, PID: 300, Title: "Helper", Process: "other.exe", Owner: 20, Enabled: true}, // owned, other process
		{Handle: 25, PID: 100, Title: "Unowned", Process: "acad.exe", Enabled: true},            // same process, not owned
		{Handle: 26, PID: 100, Title: "Orphan", Process: "acad.exe", Owner: 99, Enabled: true},  // owner not listed
		{Handle: 40, PID: 100, Title: "Loop A", Process: "acad.exe", Owner: 41, Enabled: true},  // owner cycle
		{Handle: 41, PID: 100, Title: "Loop B", Process: "acad.exe", Owner: 40, Enabled: true},
		{Handle: 50, PID: 400, Title: "Autodesk Licensing", Class: "QWidget", Process: "LMU.exe", Enabled: true},
	}
	for i := range ws {
		ws[i].Foreground = ws[i].Handle == fg
	}
	return ws
}

func TestInGroup(t *testing.T) {
	ws := groupWindows(20)
	main := ws[2]
	for h, want := range map[uint64]bool{20: true, 21: true, 22: true, 24: false, 25: false, 26: false, 40: false, 50: false, 0: false, 999: false} {
		if got := inGroup(ws, main, h); got != want {
			t.Errorf("handle %d: in group %v, want %v", h, got, want)
		}
	}
	// Groups only go down the owner chain: the command line's group holds its popup but not the main window.
	if !inGroup(ws, ws[1], 22) || inGroup(ws, ws[1], 20) {
		t.Error("command line group")
	}
	// A cycle ends the walk instead of looping.
	if inGroup(ws, proto.WindowInfo{Handle: 7, PID: 100}, 40) {
		t.Error("cycle reached an unrelated root")
	}
}

func TestCheckUsableGroupForeground(t *testing.T) {
	// The command line is foreground: the main window is usable.
	ws := groupWindows(21)
	if err := checkUsable(ws, ws[2]); err != nil {
		t.Errorf("group foreground: %v", err)
	}
	// Another process's window is foreground: refused, naming it.
	ws = groupWindows(50)
	err := checkUsable(ws, ws[2])
	if code(err) != codeActivateFailed || field(err, "foreground").(*windowRef).Process != "LMU.exe" {
		t.Errorf("other foreground: %v", err)
	}
	// A same-process window that the main window does not own does not count either.
	ws = groupWindows(25)
	if err := checkUsable(ws, ws[2]); err == nil {
		t.Error("unowned same-process foreground accepted")
	}
}

func TestDisabledTarget(t *testing.T) {
	main := windowSelector{Handle: 20, PID: 100}
	actOn := func(err error) uint64 {
		h, _ := field(err, "act_on").(uint64)
		return h
	}
	// AutoCAD's licensing dialog (another process, unowned) disables the main window: click and input name it as the
	// window to act on, not the disabled main window.
	ws := groupWindows(50)
	ws[2].Enabled = false
	err := checkUsable(ws, ws[2])
	if code(err) != codeTargetDisabled || actOn(err) != 50 || !strings.Contains(asToolError(err).Reason, "probably blocks it") {
		t.Errorf("click, other process: %v", err)
	}
	if _, _, err := inputWindow(ws, main); code(err) != codeTargetDisabled || actOn(err) != 50 {
		t.Errorf("input, other process: %v", err)
	}
	// As observed live: the disabled main window also disables its command line, which list_windows then reports as
	// modal (its owner is disabled). That is not a dialog; the other process's window is still the one to act on.
	ws[1].Enabled, ws[1].Modal = false, true
	if err := checkUsable(ws, ws[2]); actOn(err) != 50 {
		t.Errorf("click, disabled command line: %v", err)
	}
	// Its own modal dialog in the foreground: named, and input is refused rather than sent to the dialog.
	ws = append(ws, proto.WindowInfo{Handle: 30, PID: 100, Title: "Customer Involvement Program", Process: "acad.exe", Owner: 20, Modal: true, Enabled: true})
	for i := range ws {
		ws[i].Foreground = ws[i].Handle == 30
	}
	err = checkUsable(ws, ws[2])
	if actOn(err) != 30 || !strings.Contains(asToolError(err).Reason, "probably a modal dialog") {
		t.Errorf("click, own dialog: %v", err)
	}
	// As observed live: with the Options dialog open, the history popup is enabled and flagged modal (its owner, the
	// command line, is disabled). Only the foreground dialog is named.
	ws[0].Modal = true
	if err := checkUsable(ws, ws[2]); actOn(err) != 30 {
		t.Errorf("click, own dialog with flagged popup: %v", err)
	}
	// Nested dialogs: the inner one in the foreground is named, not the disabled outer one.
	ws[len(ws)-1].Enabled, ws[len(ws)-1].Foreground = false, false
	ws = append(ws, proto.WindowInfo{Handle: 31, PID: 100, Title: "Inner", Process: "acad.exe", Owner: 30, Modal: true, Enabled: true, Foreground: true})
	if err := checkUsable(ws, ws[2]); actOn(err) != 31 {
		t.Errorf("click, nested dialogs: %v", err)
	}
	ws = ws[:len(ws)-1]
	ws[len(ws)-1].Enabled, ws[len(ws)-1].Foreground = true, true
	if _, _, err := inputWindow(ws, main); actOn(err) != 30 {
		t.Errorf("input, own dialog: %v", err)
	}
	// The dialog itself, selected by its handle, takes input.
	if _, target, err := inputWindow(ws, windowSelector{Handle: 30, PID: 100}); err != nil || target.Handle != 30 {
		t.Errorf("input to dialog: %+v %v", target, err)
	}
}

// An owner chain of exactly maxOwnerDepth links is in the group; one more link is not.
func TestInGroupDepth(t *testing.T) {
	var ws []proto.WindowInfo
	for h := uint64(1); h <= maxOwnerDepth+1; h++ {
		ws = append(ws, proto.WindowInfo{Handle: h, PID: 1, Owner: h + 1})
	}
	root := proto.WindowInfo{Handle: maxOwnerDepth + 2, PID: 1}
	ws = append(ws, root)
	if !inGroup(ws, root, 2) || inGroup(ws, root, 1) {
		t.Errorf("depth: %v %v", inGroup(ws, root, 2), inGroup(ws, root, 1))
	}
}

func TestCheckHitGroup(t *testing.T) {
	ws := groupWindows(20)
	main := ws[2]
	for _, h := range []uint64{21, 22} {
		hit, err := checkHit(ws, main, 900, 997, proto.HandleResult{Handle: h})
		if err != nil || hit.Handle != h || hit.Class == "" {
			t.Errorf("own window %d: %+v %v", h, hit, err)
		}
	}
	for _, h := range []uint64{24, 25, 50} {
		if _, err := checkHit(ws, main, 900, 997, proto.HandleResult{Handle: h}); code(err) != codeCovered {
			t.Errorf("foreign window %d: %v", h, err)
		}
	}
	// A window list_windows does not show is described from window_at.
	_, err := checkHit(ws, main, 900, 997, proto.HandleResult{Handle: 131160, Class: "Shell_LightDismissOverlay", PID: 5488, Process: "explorer.exe"})
	w, _ := field(err, "window").(map[string]any)
	if code(err) != codeCovered || w["class"] != "Shell_LightDismissOverlay" || w["process"] != "explorer.exe" || !strings.Contains(asToolError(err).Next, "dismiss it") {
		t.Errorf("unlisted overlay: %v %v", err, w)
	}
}

func groupAgent(fg, at uint64) *fakeCall {
	return &fakeCall{results: map[string]any{
		proto.OpListWindows: proto.WindowsResult{Windows: groupWindows(fg), Foreground: fg, Session: &proto.SessionStateResult{Console: true}},
		proto.OpWindowAt:    proto.HandleResult{Handle: at},
	}}
}

func TestClickGroup(t *testing.T) {
	// Observation of the main window, point on the command line: allowed, and the command line is returned.
	td := newTestDeps(t, groupAgent(20, 21))
	main := groupWindows(20)[2]
	id := td.put(&main, 0.5, nil)
	_, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 450, "y": 498, "observe_after": "none"})
	w, _ := m["window"].(map[string]any)
	if td.b.events() != "click 901,997 b1 c1 []" || w["handle"] != float64(21) || w["class"] != "HwndWrapper[cmd]" || w["process"] != "acad.exe" {
		t.Fatalf("command line: %v %q", m, td.b.events())
	}
	// The listed description wins for own windows over window_at's.
	f := groupAgent(20, 21)
	f.results[proto.OpWindowAt] = proto.HandleResult{Handle: 21, Class: "ignored", PID: 100, Process: "acad.exe"}
	td = newTestDeps(t, f)
	id = td.put(&main, 1, nil)
	if _, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 900, "y": 997, "observe_after": "none"}); m["window"].(map[string]any)["class"] != "HwndWrapper[cmd]" {
		t.Errorf("listed hit: %v", m)
	}
}

// inputMCP serves list_windows from windows(n), n counting list_windows calls, and records keys and type_keys.
func inputMCP(t *testing.T, windows func(n int32) []proto.WindowInfo) (*mcp.ClientSession, *[]string, context.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	var lists atomic.Int32
	var mu sync.Mutex
	var log []string
	record := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	b := &inputMCPBackend{windowMCPBackend: &windowMCPBackend{
		find: func(string) (hyperv.VM, error) { return hyperv.VM{ID: "A", Name: "A"}, nil },
		respond: func(_ string, req proto.Request) (any, error) {
			switch req.Op {
			case proto.OpListWindows:
				ws := windows(lists.Add(1))
				fg, _ := foreground(ws)
				return proto.WindowsResult{Windows: ws, Foreground: fg.Handle, Session: &proto.SessionStateResult{Console: true}}, nil
			case proto.OpTypeKeys:
				var a proto.TypeKeysArgs
				if err := json.Unmarshal(req.Args, &a); err != nil {
					return nil, err
				}
				record(fmt.Sprintf("type_keys %d", a.Handle))
				return proto.TypeKeysResult{Events: 2 * len([]rune(a.Text))}, nil
			}
			return nil, fmt.Errorf("unexpected operation %s", req.Op)
		},
	}, press: func(_, keys string) error { record("press " + keys); return nil }}
	return connectInputMCP(t, ctx, b), &log, ctx
}

func TestKeyAndTypeGroupTarget(t *testing.T) {
	// vm_key with the main window selector while the command line is foreground: sent, and the command line reported.
	cs, log, ctx := inputMCP(t, func(int32) []proto.WindowInfo { return groupWindows(21) })
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_key", Arguments: map[string]any{"vm": "A", "handle": 20, "pid": 100, "sequence": []string{"a", "enter"}}})
	if err != nil || r.IsError || strings.Join(*log, ",") != "press a,press enter" {
		t.Fatalf("vm_key: %v %v %v", r, err, *log)
	}
	if m := resultJSON(t, r); m["combinations"] != float64(2) || m["window"].(map[string]any)["handle"] != float64(21) {
		t.Errorf("vm_key result: %v", m)
	}
	// The foreground leaves the group mid-sequence: stopped before the second combination.
	cs, log, ctx = inputMCP(t, func(n int32) []proto.WindowInfo {
		if n > 1 {
			return groupWindows(50)
		}
		return groupWindows(21)
	})
	r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_key", Arguments: map[string]any{"vm": "A", "handle": 20, "pid": 100, "sequence": []string{"a", "enter"}}})
	if err != nil || !r.IsError || strings.Join(*log, ",") != "press a" {
		t.Fatalf("vm_key left group: %v %v %v", r, err, *log)
	}
	if m := resultJSON(t, r); m["error"] != codePartialInput || m["applied"] != float64(1) || m["total"] != float64(2) {
		t.Errorf("partial: %v", m)
	}
	// vm_type with the main window selector goes to the command line through the agent.
	cs, log, ctx = inputMCP(t, func(int32) []proto.WindowInfo { return groupWindows(21) })
	r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_type", Arguments: map[string]any{"vm": "A", "handle": 20, "pid": 100, "text": "(+ 1 2)"}})
	if err != nil || r.IsError || strings.Join(*log, ",") != "type_keys 21" {
		t.Fatalf("type: %v %v %v", r, err, *log)
	}
	if m := resultJSON(t, r); m["applied_chars"] != float64(7) || m["window"].(map[string]any)["handle"] != float64(21) || m["window"].(map[string]any)["pid"] != float64(100) {
		t.Errorf("type result: %v", m)
	}
}

func TestInputWindowGroup(t *testing.T) {
	main := windowSelector{Handle: 20, PID: 100}
	// The command line is foreground: input for the main window goes to it.
	root, target, err := inputWindow(groupWindows(21), main)
	if err != nil || root.Handle != 20 || target.Handle != 21 {
		t.Errorf("group: %+v %+v %v", root, target, err)
	}
	// The selected window itself is foreground.
	if _, target, err := inputWindow(groupWindows(20), main); err != nil || target.Handle != 20 {
		t.Errorf("self: %+v %v", target, err)
	}
	// Without a selector, the foreground window.
	if root, target, err := inputWindow(groupWindows(50), windowSelector{}); err != nil || root.Handle != 50 || target.Handle != 50 {
		t.Errorf("no selector: %+v %+v %v", root, target, err)
	}
	// Another process in the foreground: refused with it named.
	if _, _, err := inputWindow(groupWindows(50), main); code(err) != codeActivateFailed {
		t.Errorf("foreign foreground: %v", err)
	}
	// A disabled receiving window is refused.
	ws := groupWindows(21)
	ws[1].Enabled = false
	if _, _, err := inputWindow(ws, main); code(err) != codeTargetDisabled || field(err, "act_on") != uint64(21) {
		t.Errorf("disabled command line: %v", err)
	}
}
