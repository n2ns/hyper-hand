package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/win"
	"hyperhand/internal/proto"
)

func TestControlHintPoint(t *testing.T) {
	for _, r := range []*proto.Rect{nil, {}, {Left: 2, Right: 1, Bottom: 5}, {Right: 5, Top: 2, Bottom: 1}} {
		if _, ok := hintPoint(r); ok {
			t.Fatalf("accepted invalid bounds %+v", r)
		}
	}
	for _, tt := range []struct {
		r    proto.Rect
		x, y int32
	}{
		{proto.Rect{Left: -20, Top: -30, Right: -10, Bottom: -10}, -15, -20},
		{proto.Rect{Left: -2147483648, Top: -2147483648, Right: 2147483647, Bottom: 2147483647}, 0, 0},
		{proto.Rect{Left: 2147483645, Top: 2147483645, Right: 2147483647, Bottom: 2147483647}, 2147483646, 2147483646},
	} {
		p, ok := hintPoint(&tt.r)
		if !ok || int32(uint32(p)) != tt.x || int32(uint32(p>>32)) != tt.y {
			t.Fatalf("point for %+v: %#x %v", tt.r, p, ok)
		}
	}
}

func TestControlHintWorker(t *testing.T) {
	if os.Getenv("HYPERHAND_CONTROL_HINT_WORKER") != "1" {
		return
	}
	var req helperRequest
	err := json.NewDecoder(os.Stdin).Decode(&req)
	var result proto.ControlsResult
	if err == nil {
		result, err = controlHintWorker(*req.Controls)
	}
	reply := helperReply{Controls: &result}
	if err != nil {
		reply = helperReply{Error: err.Error()}
	}
	if err := json.NewEncoder(os.Stdout).Encode(reply); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func controlHintWorker(a proto.ControlsArgs) (proto.ControlsResult, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	r := proto.ControlsResult{Nodes: []proto.ControlInfo{}, Focused: -1}
	s, err := openUIA()
	if err != nil {
		return r, err
	}
	defer s.close()
	root, err := s.openWindow(a)
	if err != nil {
		return r, err
	}
	e, ok := s.findHintedRuntimeID(root, a, a.RootRuntimeID, a.HintRect)
	if !ok {
		return r, nil
	}
	defer e.release()
	n, _, _, err := e.info()
	if err != nil {
		return r, err
	}
	r.Nodes = append(r.Nodes, n)
	return r, nil
}

func pumpHintHelper(t *testing.T, a proto.ControlsArgs) []proto.ControlInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), uiaTimeout)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestControlHintWorker$")
	cmd.Env = append(os.Environ(), "HYPERHAND_CONTROL_HINT_WORKER=1")
	type outcome struct {
		r   helperReply
		err error
	}
	done := make(chan outcome, 1)
	go func() { r, err := runHelper(ctx, cmd, helperRequest{Controls: &a}); done <- outcome{r, err} }()
	for {
		select {
		case out := <-done:
			if out.err != nil {
				t.Fatal(out.err)
			}
			return out.r.Controls.Nodes
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

func TestControlHintNativeIdentityAndFallback(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const noActivateToolTopmost = 0x08000000 | 0x80 | 0x8
	h := testWindow(t, "hyperhand-hint-test", 0, noActivateToolTopmost, 100, 100, 450, 100)
	editChild(t, h, "first-value", 0, 10)
	editChild(t, h, "second-value", 0, 150)
	editChild(t, h, "hint-password-secret", 0x20, 280)
	a := proto.ControlsArgs{Handle: uint64(h), PID: uint32(os.Getpid()), MaxDepth: 2, MaxNodes: 20}
	reply, err := pumpHelper(t, helperRequest{Controls: &a})
	if err != nil {
		t.Fatal(err)
	}
	var first, second, password *proto.ControlInfo
	for i := range reply.Controls.Nodes {
		n := &reply.Controls.Nodes[i]
		switch {
		case n.Value == "first-value":
			first = n
		case n.Value == "second-value":
			second = n
		case n.ControlType == 50004 && !n.HasValue:
			password = n
		}
	}
	if first == nil || second == nil || password == nil {
		t.Fatalf("fixture controls: %+v", reply.Controls.Nodes)
	}
	windowRootID := reply.Controls.Nodes[0].RuntimeID
	for _, n := range []*proto.ControlInfo{first, second, password} {
		a.RootRuntimeID, a.HintRect = n.RuntimeID, &n.Rect
		got := pumpHintHelper(t, a)
		if len(got) != 1 || got[0].RuntimeID != n.RuntimeID {
			t.Fatalf("exact hit: target=%+v got=%+v", n, got)
		}
		if n == password && (got[0].Name != "" || got[0].Value != "" || got[0].HasValue) {
			t.Fatal("hint read exposed password")
		}
	}
	// All three production entry points can reuse the validated visible identity.
	a.RootRuntimeID, a.HintRect = first.RuntimeID, &first.Rect
	reply, err = pumpHelper(t, helperRequest{Controls: &a})
	if err != nil || reply.Controls == nil || len(reply.Controls.Nodes) != 1 || reply.Controls.Nodes[0].RuntimeID != first.RuntimeID {
		t.Fatalf("hinted subtree: %+v %v", reply.Controls, err)
	}
	find := proto.FindControlsArgs{Handle: a.Handle, PID: a.PID, RootRuntimeID: first.RuntimeID, HintRect: &first.Rect, ControlType: 50004}
	reply, err = pumpHelper(t, helperRequest{Find: &find})
	if err != nil || reply.Find == nil || len(reply.Find.Matches) != 1 || reply.Find.Matches[0].RuntimeID != first.RuntimeID {
		t.Fatalf("hinted scoped search: %+v %v", reply.Find, err)
	}
	act := proto.ControlActionArgs{Handle: a.Handle, PID: a.PID, RuntimeID: first.RuntimeID, HintRect: &first.Rect, Action: "SetValue", Value: "hint-current-value"}
	reply, err = pumpHelper(t, helperRequest{Action: &act})
	if err != nil || reply.Action == nil || reply.Action.Value != act.Value {
		t.Fatalf("hinted action: %+v %v", reply.Action, err)
	}
	// The point can hit a descendant of the requested identity.
	a.RootRuntimeID, a.HintRect = windowRootID, &first.Rect
	if got := pumpHintHelper(t, a); len(got) != 1 || got[0].RuntimeID != a.RootRuntimeID {
		t.Fatalf("ancestor hit: %+v", got)
	}
	// Wrong coordinates never substitute the control under the point.
	a.RootRuntimeID, a.HintRect = first.RuntimeID, &second.Rect
	if got := pumpHintHelper(t, a); len(got) != 0 {
		t.Fatalf("wrong hint accepted: %+v", got)
	}
	reply, err = pumpHelper(t, helperRequest{Controls: &a})
	if err != nil || len(reply.Controls.Nodes) == 0 || reply.Controls.Nodes[0].RuntimeID != first.RuntimeID {
		t.Fatalf("subtree did not fall back: %+v %v", reply.Controls, err)
	}
	act.HintRect, act.Value = &second.Rect, "fallback-current-value"
	reply, err = pumpHelper(t, helperRequest{Action: &act})
	if err != nil || reply.Action == nil || reply.Action.Value != act.Value {
		t.Fatalf("action fallback: %+v %v", reply.Action, err)
	}
	a.RootRuntimeID = "42.999999999"
	if _, err := pumpHelper(t, helperRequest{Controls: &a}); err == nil || err.Error() != "control search incomplete: password_subtree" {
		t.Fatalf("hint miss bypassed incomplete fallback: %v", err)
	}
	// An overlapping window of the same process must not pass the root check.
	testWindow(t, "hyperhand-hint-cover", 0, noActivateToolTopmost, 100, 100, 450, 100)
	a.RootRuntimeID, a.HintRect = first.RuntimeID, &first.Rect
	if got := pumpHintHelper(t, a); len(got) != 0 {
		t.Fatalf("covered hint accepted: %+v", got)
	}
}
