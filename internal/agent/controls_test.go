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

// fakeListed is a parent whose children are also listed for the per-parent child limit, each with a rectangle and an
// offscreen flag; first and next walk the same children.
type fakeListed struct {
	fakeControl
	set *fakeChildren
}

func (e *fakeListed) children() (childSet, error) {
	e.set.listCalls++
	return e.set, nil
}

type fakeChildren struct {
	kids                    []*fakeControl
	rects                   []proto.Rect
	offscreen               []bool
	listCalls, listReleases int
	boundsCalls, loadCalls  int
}

func (c *fakeChildren) len() int { return len(c.kids) }
func (c *fakeChildren) bounds(i int) (proto.Rect, bool, error) {
	c.boundsCalls++
	return c.rects[i], c.offscreen[i], nil
}
func (c *fakeChildren) load(i int) (controlElement, error) {
	c.loadCalls++
	return c.kids[i], nil
}
func (c *fakeChildren) release() { c.listReleases++ }

var (
	fakeWindow    = proto.Rect{Left: 0, Top: 0, Right: 1000, Bottom: 800}
	fakeInside    = proto.Rect{Left: 10, Top: 10, Right: 60, Bottom: 30}
	fakeOutside   = proto.Rect{Left: 10, Top: 900, Right: 60, Bottom: 920}
	fakeTouching  = proto.Rect{Left: 1000, Top: 10, Right: 1050, Bottom: 30} // shares only an edge
	fakeStraddles = proto.Rect{Left: 990, Top: 790, Right: 1050, Bottom: 850}
)

// listedParent builds a parent with n children; rect and offscreen give each child's listed bounds.
func listedParent(n int, rect func(int) proto.Rect, offscreen func(int) bool) *fakeListed {
	c := &fakeChildren{}
	for i := 0; i < n; i++ {
		c.kids = append(c.kids, &fakeControl{name: fmt.Sprint("item ", i)})
		c.rects = append(c.rects, rect(i))
		c.offscreen = append(c.offscreen, offscreen(i))
		if i > 0 {
			c.kids[i-1].sibling = c.kids[i]
		}
	}
	p := &fakeListed{fakeControl: fakeControl{name: "list"}, set: c}
	if n > 0 {
		p.child = c.kids[0]
	}
	return p
}

func TestControlsChildLimit(t *testing.T) {
	// Every tenth child is visible; the others alternate between outside the window and offscreen inside it.
	sparse := func(i int) proto.Rect {
		if i%10 == 0 || i%2 == 1 {
			return fakeInside
		}
		return fakeOutside
	}
	sparseOff := func(i int) bool { return i%10 != 0 && i%2 == 1 }
	visibleCount := func(n int) int { return (n + 9) / 10 }
	none := func(int) bool { return false }
	for _, tc := range []struct {
		name    string
		n       int
		rect    func(int) proto.Rect
		off     func(int) bool
		visited int
		omitted int
	}{
		{"100 all visited", 100, func(int) proto.Rect { return fakeOutside }, func(int) bool { return true }, 100, 0},
		{"101 visible only", 101, sparse, sparseOff, visibleCount(101), 101 - visibleCount(101)},
		{"101 all visible", 101, func(int) proto.Rect { return fakeStraddles }, none, 101, 0},
		{"edge contact is not visible", 101, func(i int) proto.Rect {
			if i == 0 {
				return fakeInside
			}
			return fakeTouching
		}, none, 1, 100},
		{"499 visible only", 499, sparse, sparseOff, visibleCount(499), 499 - visibleCount(499)},
		{"500 none", 500, func(int) proto.Rect { return fakeInside }, none, 0, 500},
		{"5000 none", 5000, func(int) proto.Rect { return fakeInside }, none, 0, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := listedParent(tc.n, tc.rect, tc.off)
			r, err := walkControls(&fakeTopLevel{fakeControl: &fakeControl{name: "root"}, only: list}, proto.ControlsArgs{MaxDepth: 4, MaxNodes: 1000}, fakeWindow)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(r.Nodes) - 2; got != tc.visited {
				t.Fatalf("visited %d children, want %d", got, tc.visited)
			}
			if got := r.Nodes[1].OmittedChildren; got != tc.omitted {
				t.Fatalf("omitted_children %d, want %d", got, tc.omitted)
			}
			if r.Nodes[0].OmittedChildren != 0 {
				t.Errorf("root omitted %d", r.Nodes[0].OmittedChildren)
			}
			limited := slices.Contains(r.Truncation, "child_limit")
			if limited != (tc.omitted > 0) || r.Truncated != (tc.omitted > 0) {
				t.Errorf("truncation %v %v with %d omitted", r.Truncated, r.Truncation, tc.omitted)
			}
			for _, n := range r.Nodes[2:] {
				if n.Parent != 1 || n.Depth != 2 {
					t.Fatalf("child placement: %+v", n)
				}
				var i int
				fmt.Sscanf(n.Name, "item %d", &i)
				if tc.n > childLimitAll && (tc.off(i) || !rectsIntersect(tc.rect(i), fakeWindow)) {
					t.Fatalf("invisible child %d visited", i)
				}
			}
			if tc.n >= childLimitNone && (list.set.boundsCalls != 0 || list.set.loadCalls != 0) {
				t.Errorf("read %d bounds and loaded %d children of a %d-child parent", list.set.boundsCalls, list.set.loadCalls, tc.n)
			}
			if list.set.listCalls != 1 || list.set.listReleases != 1 {
				t.Errorf("listing calls %d releases %d", list.set.listCalls, list.set.listReleases)
			}
			for i, kid := range list.set.kids {
				want := kid.reads
				if i == 0 && tc.n > childLimitAll {
					want++ // the walker's first child, released when the listing takes over
				}
				if kid.reads > 1 || kid.releases != want {
					t.Fatalf("child %d read %d released %d", i, kid.reads, kid.releases)
				}
			}
		})
	}
}

// fakeTopLevel is a root with exactly one child, the listed parent under test.
type fakeTopLevel struct {
	*fakeControl
	only *fakeListed
}

func (e *fakeTopLevel) first() (controlElement, error) { return e.only, nil }

func TestControlsChildLimitWithOtherLimits(t *testing.T) {
	// 200 children, the first 50 visible, each visible child with one grandchild.
	list := listedParent(200, func(i int) proto.Rect {
		if i < 50 {
			return fakeInside
		}
		return fakeOutside
	}, func(int) bool { return false })
	for i := 0; i < 50; i++ {
		list.set.kids[i].child = &fakeControl{name: "grandchild"}
	}
	r, err := walkControls(&fakeTopLevel{fakeControl: &fakeControl{name: "root"}, only: list}, proto.ControlsArgs{MaxDepth: 4, MaxNodes: 10}, fakeWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Nodes) != 10 || !slices.Contains(r.Truncation, "max_nodes") || !slices.Contains(r.Truncation, "child_limit") {
		t.Fatalf("nodes %d truncation %v", len(r.Nodes), r.Truncation)
	}
	if r.Nodes[1].OmittedChildren != 150 {
		t.Errorf("omitted_children %d, want the 150 invisible children", r.Nodes[1].OmittedChildren)
	}
	if r.Nodes[3].Name != "grandchild" || r.Nodes[3].Depth != 3 {
		t.Errorf("visible child's subtree not walked: %+v", r.Nodes[3])
	}
	if list.set.loadCalls != 4 {
		t.Errorf("loaded %d children after the node limit", list.set.loadCalls)
	}

	// A parent at max_depth is not listed.
	list = listedParent(600, func(int) proto.Rect { return fakeInside }, func(int) bool { return false })
	r, err = walkControls(&fakeTopLevel{fakeControl: &fakeControl{name: "root"}, only: list}, proto.ControlsArgs{MaxDepth: 1, MaxNodes: 1000}, fakeWindow)
	if err != nil || list.set.listCalls != 0 || r.Nodes[1].OmittedChildren != 0 || !slices.Equal(r.Truncation, []string{"max_depth"}) {
		t.Fatalf("max_depth: listed %d %+v %v", list.set.listCalls, r, err)
	}
	// Nor is a parent reached with the node budget spent.
	list = listedParent(600, func(int) proto.Rect { return fakeInside }, func(int) bool { return false })
	r, err = walkControls(&fakeTopLevel{fakeControl: &fakeControl{name: "root"}, only: list}, proto.ControlsArgs{MaxDepth: 4, MaxNodes: 2}, fakeWindow)
	if err != nil || list.set.listCalls != 0 || r.Nodes[1].OmittedChildren != 0 || !slices.Equal(r.Truncation, []string{"max_nodes"}) {
		t.Fatalf("max_nodes: listed %d %+v %v", list.set.listCalls, r, err)
	}
}

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
			r, err := walkControls(root, a, proto.Rect{})
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
	case "serve_uncached":
		var req helperRequest
		err := json.NewDecoder(os.Stdin).Decode(&req)
		var reply helperReply
		if err == nil {
			if req.Find != nil {
				var r proto.FindControlsResult
				r, err = collectFindControlsCached(*req.Find, false)
				reply.Find = &r
			} else {
				var r proto.ControlsResult
				r, err = collectControlsCached(*req.Controls, false)
				reply.Controls = &r
			}
		}
		if err != nil {
			reply = helperReply{Error: err.Error()}
		}
		if err := json.NewEncoder(os.Stdout).Encode(reply); err != nil {
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
	return pumpControlsHelper(t, req, "serve")
}

func pumpControlsHelper(t *testing.T, req helperRequest, mode string) (helperReply, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := controlsTestCommand(t, ctx, mode)
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

// A native list box with many items gets the per-parent child limit in the cached and the uncached walk. The test
// window lies off every monitor, so UIA reports its items offscreen and a 101-499-item list has items omitted.
func TestControlsNativeChildLimit(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	send := user32.NewProc("SendMessageW")
	for _, items := range []int{150, 600} {
		h := offscreenWindow(t, fmt.Sprint("hyperhand-child-limit-", items), 0, -30000)
		class, _ := windows.UTF16PtrFromString("LISTBOX")
		list, _, err := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(class)), 0, 0x40000000|0x10000000, 10, 10, 200, 100, uintptr(h), 0, 0, 0)
		if list == 0 {
			t.Fatal(err)
		}
		for i := 0; i < items; i++ {
			text, _ := windows.UTF16PtrFromString(fmt.Sprint("item ", i))
			send.Call(list, 0x0180, 0, uintptr(unsafe.Pointer(text))) // LB_ADDSTRING
		}
		a := proto.ControlsArgs{Handle: uint64(h), PID: uint32(os.Getpid()), MaxDepth: 3, MaxNodes: 1000}
		for _, mode := range []string{"serve", "serve_uncached"} {
			reply, err := pumpControlsHelper(t, helperRequest{Controls: &a}, mode)
			if err != nil {
				t.Fatal(err)
			}
			r := reply.Controls
			box := -1
			for i, n := range r.Nodes {
				if n.ControlType == 50008 { // List
					box = i
				}
			}
			if box < 0 {
				t.Fatalf("%d/%s: no list: %+v", items, mode, r.Nodes)
			}
			visited := 0
			for _, n := range r.Nodes {
				if n.Parent == box {
					visited++
				}
			}
			omitted := r.Nodes[box].OmittedChildren
			if !slices.Contains(r.Truncation, "child_limit") || !r.Truncated {
				t.Errorf("%d/%s: truncation %v", items, mode, r.Truncation)
			}
			if items >= childLimitNone && (visited != 0 || omitted < items) {
				t.Errorf("%d/%s: visited %d omitted %d", items, mode, visited, omitted)
			}
			if items < childLimitNone && (omitted == 0 || visited+omitted < items || visited+omitted >= childLimitNone) {
				t.Errorf("%d/%s: visited %d omitted %d", items, mode, visited, omitted)
			}
		}
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
	r, err := walkControls(root, proto.ControlsArgs{MaxDepth: 4, MaxNodes: 200}, proto.Rect{})
	if err != nil || r.Focused != 2 || !r.Nodes[2].Focused {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = walkControls(&fakeControl{name: "alone"}, proto.ControlsArgs{MaxDepth: 4, MaxNodes: 200}, proto.Rect{})
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
