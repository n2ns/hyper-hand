package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/win"
	"hyperhand/internal/proto"
)

func TestControlsBounds(t *testing.T) {
	a, err := controlsArgs(proto.ControlsArgs{Handle: 1, PID: 2})
	if err != nil || a.MaxDepth != 4 || a.MaxNodes != 200 {
		t.Fatalf("defaults: %+v %v", a, err)
	}
	for _, a := range []proto.ControlsArgs{{PID: 1}, {Handle: 1}, {Handle: 1, PID: 1, MaxDepth: -1}, {Handle: 1, PID: 1, MaxDepth: 11}, {Handle: 1, PID: 1, MaxNodes: -1}, {Handle: 1, PID: 1, MaxNodes: 1001}} {
		if _, err := controlsArgs(a); err == nil {
			t.Errorf("accepted %+v", a)
		}
	}
	if handled, err := RunControlsHelper([]string{"install"}); handled || err != nil {
		t.Fatalf("claimed unrelated role: %v %v", handled, err)
	}
}

type fakeControl struct {
	name                        string
	child, sibling              *fakeControl
	password, cut, focused      bool
	err                         error
	reads, releases, childCalls int
}

func (e *fakeControl) info() (proto.ControlInfo, bool, bool, error) {
	e.reads++
	return proto.ControlInfo{Name: e.name, Focused: e.focused}, e.password, e.cut, e.err
}
func (e *fakeControl) first() (controlElement, error) {
	e.childCalls++
	if e.child == nil {
		return nil, nil
	}
	return e.child, nil
}
func (e *fakeControl) next() (controlElement, error) {
	if e.sibling == nil {
		return nil, nil
	}
	return e.sibling, nil
}
func (e *fakeControl) release() { e.releases++ }

func TestControlsTraversal(t *testing.T) {
	for _, mode := range []string{"full", "max_depth", "max_nodes", "password_subtree", "text_length", "error"} {
		t.Run(mode, func(t *testing.T) {
			leaf := &fakeControl{name: "leaf"}
			second := &fakeControl{name: "second"}
			first := &fakeControl{name: "first", child: leaf, sibling: second}
			root := &fakeControl{name: "root", child: first}
			a := proto.ControlsArgs{MaxDepth: 4, MaxNodes: 200}
			switch mode {
			case "max_depth":
				a.MaxDepth = 1
			case "max_nodes":
				a.MaxNodes = 2
			case "password_subtree":
				first.password = true
			case "text_length":
				first.cut = true
			case "error":
				first.err = errors.New("provider disappeared")
			}
			r, err := walkControls(root, a)
			if mode == "error" {
				if !errors.Is(err, first.err) || first.releases != 1 {
					t.Fatalf("%+v %v", r, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "full" {
				if len(r.Nodes) != 4 || r.Truncated || r.Nodes[2].Parent != 1 || r.Nodes[2].Depth != 2 || r.Nodes[3].Parent != 0 {
					t.Fatalf("tree: %+v", r)
				}
			} else if !r.Truncated || !slices.Contains(r.Truncation, mode) {
				t.Fatalf("missing truncation %s: %+v", mode, r)
			}
			if r.Nodes[0].Parent != -1 || r.Nodes[0].Index != 0 {
				t.Errorf("root: %+v", r.Nodes[0])
			}
			if mode == "max_nodes" && (len(r.Nodes) != 2 || leaf.reads != 0 || second.reads != 0) {
				t.Errorf("node limit ignored: %+v", r)
			}
			if mode == "password_subtree" && first.childCalls != 0 {
				t.Error("password subtree queried")
			}
			if first.releases != 1 || second.releases != 1 {
				t.Errorf("release counts: %d %d", first.releases, second.releases)
			}
		})
	}
}

func TestControlsHelperProcess(t *testing.T) {
	switch os.Getenv("HYPERHAND_CONTROLS_TEST_HELPER") {
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(1)
	case "serve":
		_, err := RunControlsHelper([]string{controlsHelperRole})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func controlsTestCommand(t *testing.T, ctx context.Context, mode string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestControlsHelperProcess$")
	cmd.Env = append(os.Environ(), "HYPERHAND_CONTROLS_TEST_HELPER="+mode)
	return cmd
}

func TestControlsSubprocessCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runControlsCommand(ctx, controlsTestCommand(t, ctx, "hang"), proto.ControlsArgs{Handle: 1, PID: 1})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation: %v (%s)", err, time.Since(start))
	}
	// A subsequent request still starts and returns an ordinary target error.
	next, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_, err = runControlsCommand(next, controlsTestCommand(t, next, "serve"), proto.ControlsArgs{Handle: 1, PID: 1})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("subsequent request: %v", err)
	}
}

// pumpHelper runs one helper request in a test subprocess while this thread services the messages of its test
// windows, which the helper's UIA provider needs.
func pumpHelper(t *testing.T, req helperRequest) (helperReply, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := controlsTestCommand(t, ctx, "serve")
	type outcome struct {
		r   helperReply
		err error
	}
	done := make(chan outcome, 1)
	go func() { r, err := runHelper(ctx, cmd, req); done <- outcome{r, err} }()
	for {
		select {
		case out := <-done:
			return out.r, out.err
		default:
			var msg win.MSG
			for win.PeekMessage(&msg, 0, 0, 0, co.PM_REMOVE) {
				win.TranslateMessage(&msg)
				win.DispatchMessage(&msg)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func editChild(t *testing.T, parent windows.HWND, text string, style uintptr, x int32) windows.HWND {
	t.Helper()
	cls, _ := windows.UTF16PtrFromString("EDIT")
	name, _ := windows.UTF16PtrFromString(text)
	child, _, err := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(name)), 0x40000000|0x10000000|style, uintptr(x), 10, 100, 20, uintptr(parent), 0, 0, 0)
	if child == 0 {
		t.Fatal(err)
	}
	return windows.HWND(child)
}

func TestControlsNativeSnapshot(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	h := offscreenWindow(t, "hyperhand-controls-test", 0, -30000)
	editChild(t, h, "controls-test-password-secret", 0x20, 10) // ES_PASSWORD
	editChild(t, h, "plain-text", 0, 150)
	a := proto.ControlsArgs{Handle: uint64(h), PID: uint32(os.Getpid()), MaxDepth: 2, MaxNodes: 20}
	wrong := a
	wrong.PID++
	if err := validateControlsWindow(wrong); err == nil {
		t.Fatal("wrong PID accepted")
	}
	reply, err := pumpHelper(t, helperRequest{Controls: &a})
	if err != nil {
		t.Fatal(err)
	}
	r := *reply.Controls
	if len(r.Nodes) == 0 {
		t.Fatal("no root")
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "controls-test-password-secret") {
		t.Fatal("password text leaked")
	}
	var password, plain *proto.ControlInfo
	for i, node := range r.Nodes {
		if node.ControlType != 50004 {
			continue
		}
		if node.Rect.Left == -30000+150 {
			plain = &r.Nodes[i]
		} else {
			password = &r.Nodes[i]
		}
	}
	if password == nil || plain == nil {
		t.Fatalf("edit controls missing: %s", data)
	}
	if password.Name != "" || password.HasValue || password.Value != "" {
		t.Errorf("password not redacted: %+v", password)
	}
	if !plain.HasValue || plain.Value != "plain-text" || !slices.Contains(plain.Patterns, "Value") || plain.RuntimeID == "" {
		t.Errorf("plain edit: %+v", plain)
	}
	if !slices.Contains(r.Truncation, "password_subtree") {
		t.Errorf("truncation: %v", r.Truncation)
	}
	n := r.Nodes[0]
	if n.PID != a.PID || n.Name != "hyperhand-controls-test" || n.Rect.Left != -30000 || n.RuntimeID == "" || n.Index != 0 {
		t.Fatalf("root: %+v", n)
	}
	if strings.Count(n.RuntimeID, ".") < 1 {
		t.Errorf("runtime ID %q is not dot-joined integers", n.RuntimeID)
	}
	if r.Focused != -1 { // the off-screen test window never takes focus
		t.Errorf("focused %d", r.Focused)
	}

	// control_action: SetValue on the plain edit, with read-back, and the two error strings the host maps.
	act := proto.ControlActionArgs{Handle: a.Handle, PID: a.PID, RuntimeID: plain.RuntimeID, Action: "setvalue", Value: "new 文本"}
	reply, err = pumpHelper(t, helperRequest{Action: &act})
	if err != nil {
		t.Fatal(err)
	}
	if res := reply.Action; res == nil || !res.HasValue || res.Value != "new 文本" || res.Verified == nil || !*res.Verified {
		t.Fatalf("SetValue result: %+v", reply.Action)
	}
	act.Action = "Toggle"
	if _, err = pumpHelper(t, helperRequest{Action: &act}); err == nil || !strings.HasPrefix(err.Error(), "unsupported pattern: Toggle; supported: ") || !strings.Contains(err.Error(), "SetValue") {
		t.Fatalf("unsupported pattern: %v", err)
	}
	act.RuntimeID = "1.2.3.4"
	if _, err = pumpHelper(t, helperRequest{Action: &act}); err == nil || err.Error() != "element not found" {
		t.Fatalf("missing element: %v", err)
	}
}

func TestControlsNativeListItemSelect(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	h := offscreenWindow(t, "hyperhand-selection-test", 0, -30000)
	class, _ := windows.UTF16PtrFromString("LISTBOX")
	const wsChild, wsVisible, lbsNotify = 0x40000000, 0x10000000, 1
	list, _, err := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(class)), 0,
		wsChild|wsVisible|lbsNotify, 10, 10, 200, 100, uintptr(h), 0, 0, 0)
	if list == 0 {
		t.Fatal(err)
	}
	send := user32.NewProc("SendMessageW")
	const lbAddString, lbSetCurSel, lbGetCurSel = 0x0180, 0x0186, 0x0188
	for i, name := range []string{"first selection item", "second selection item"} {
		text, _ := windows.UTF16PtrFromString(name)
		got, _, _ := send.Call(list, lbAddString, 0, uintptr(unsafe.Pointer(text)))
		if got != uintptr(i) {
			t.Fatalf("LB_ADDSTRING returned %d, want %d", got, i)
		}
	}
	send.Call(list, lbSetCurSel, 0, 0)
	a := proto.ControlsArgs{Handle: uint64(h), PID: uint32(os.Getpid()), MaxDepth: 3, MaxNodes: 20}
	reply, err := pumpHelper(t, helperRequest{Controls: &a})
	if err != nil {
		t.Fatal(err)
	}
	var item *proto.ControlInfo
	for i, node := range reply.Controls.Nodes {
		if node.ControlType == 50007 && node.Name == "second selection item" {
			item = &reply.Controls.Nodes[i]
			break
		}
	}
	if item == nil || item.RuntimeID == "" || !slices.Contains(item.Patterns, "Select") {
		t.Fatalf("native ListItem must expose Select and a runtime ID: item=%+v tree=%+v", item, reply.Controls.Nodes)
	}
	act := proto.ControlActionArgs{Handle: a.Handle, PID: a.PID, RuntimeID: item.RuntimeID, Action: "Select"}
	if _, err := pumpHelper(t, helperRequest{Action: &act}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := send.Call(list, lbGetCurSel, 0, 0); got != 1 {
		t.Fatalf("Select did not change the native list selection: got %d, want 1", got)
	}
}

// The focused-element helper answers within its budget; what has focus on the test machine is not under our control.
func TestFocusedHelper(t *testing.T) {
	requireUnlockedDesktop(t)
	ctx, cancel := context.WithTimeout(context.Background(), focusedTimeout)
	defer cancel()
	reply, err := runHelper(ctx, controlsTestCommand(t, ctx, "serve"), helperRequest{Focused: true})
	if err != nil {
		t.Fatal(err)
	}
	if f := reply.Focused; f != nil && (f.Window == 0 || f.ControlType == "") {
		t.Errorf("focused: %+v", f)
	}
}

func TestControlActionArgs(t *testing.T) {
	for _, a := range []proto.ControlActionArgs{
		{PID: 1, RuntimeID: "1", Action: "Invoke"},
		{Handle: 1, PID: 1, Action: "Invoke"},
		{Handle: 1, PID: 1, RuntimeID: "1", Action: "Click"},
	} {
		if _, _, err := controlAction(context.Background(), mustJSON(a), nil); err == nil {
			t.Errorf("accepted %+v", a)
		}
	}
}

func TestRuntimeIDFormat(t *testing.T) {
	if s := formatRuntimeID([]int32{42, 1234, 5}); s != "42.1234.5" {
		t.Errorf("%q", s)
	}
	if s := formatRuntimeID(nil); s != "" {
		t.Errorf("%q", s)
	}
	if s := formatRuntimeID([]int32{-7}); s != "-7" {
		t.Errorf("%q", s)
	}
}

func TestParseAction(t *testing.T) {
	for in, want := range map[string]string{"setvalue": "SetValue", "INVOKE": "Invoke", "scrollintoview": "ScrollIntoView", "Collapse": "Collapse"} {
		if got, err := parseAction(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := parseAction("Click"); err == nil || !strings.Contains(err.Error(), "SetValue, Invoke") {
		t.Errorf("unknown action: %v", err)
	}
	for _, a := range proto.ControlActions {
		if patternFor(a) < 0 && a != "Locate" { // Locate needs no pattern: it only re-reads the element
			t.Errorf("no pattern for %s", a)
		}
	}
	if got, err := parseAction("locate"); err != nil || got != "Locate" {
		t.Errorf("locate: %q %v", got, err)
	}
}

func TestPatternNames(t *testing.T) {
	available := make([]bool, len(uiaPatterns))
	for _, action := range []string{"Invoke", "Expand", "SetValue"} {
		available[patternFor(action)] = true
	}
	if got := fmt.Sprint(patternNames(available)); got != "[Invoke Expand Collapse Value]" {
		t.Errorf("patterns: %s", got)
	}
	if got := fmt.Sprint(actionNames(available)); got != "[Invoke Expand Collapse SetValue]" {
		t.Errorf("actions: %s", got)
	}
	if patternNames(make([]bool, len(uiaPatterns))) != nil {
		t.Error("names for no patterns")
	}
	err := unsupportedPattern("Toggle", actionNames(available))
	if err.Error() != "unsupported pattern: Toggle; supported: Invoke, Expand, Collapse, SetValue" {
		t.Errorf("%v", err)
	}
	if errElementNotFound.Error() != "element not found" {
		t.Errorf("%v", errElementNotFound)
	}
}

func TestWalkControlsFocused(t *testing.T) {
	leaf := &fakeControl{name: "leaf", focused: true}
	first := &fakeControl{name: "first", child: leaf, sibling: &fakeControl{name: "second", focused: true}}
	root := &fakeControl{name: "root", child: first}
	r, err := walkControls(root, proto.ControlsArgs{MaxDepth: 4, MaxNodes: 200})
	if err != nil || r.Focused != 2 || !r.Nodes[2].Focused {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = walkControls(&fakeControl{name: "alone"}, proto.ControlsArgs{MaxDepth: 4, MaxNodes: 200})
	if err != nil || r.Focused != -1 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestControlsOutputBound(t *testing.T) {
	var out controlsBuffer
	// Exercise io.Copy too: embedding bytes.Buffer would expose ReadFrom and bypass Write's limit.
	_, err := io.Copy(&out, io.LimitReader(bytes.NewReader(make([]byte, controlsOutputLimit+1)), controlsOutputLimit+1))
	if err == nil || len(out.Bytes()) > controlsOutputLimit {
		t.Fatalf("output limit: bytes=%d err=%v", len(out.Bytes()), err)
	}
}
