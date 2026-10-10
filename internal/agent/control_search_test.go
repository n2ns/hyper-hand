package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"hyperhand/internal/proto"
)

type fakeSearchControl struct {
	n                                          proto.ControlInfo
	child, sibling                             *fakeSearchControl
	password, cut                              bool
	err                                        error
	reads, selectorReads, childReads, releases int
	onInfo                                     func(*fakeSearchControl)
	infoCut                                    bool
}

func (e *fakeSearchControl) info() (proto.ControlInfo, bool, bool, error) {
	e.reads++
	if e.onInfo != nil {
		e.onInfo(e)
	}
	return e.n, e.password, e.cut || e.infoCut, e.err
}
func (e *fakeSearchControl) searchMatch(a proto.FindControlsArgs) (bool, bool, bool, error) {
	e.selectorReads++
	return !e.cut && (a.Name == "" || !e.password && e.n.Name == a.Name) && (a.AutomationID == "" || e.n.AutomationID == a.AutomationID) && (a.ControlType == 0 || e.n.ControlType == a.ControlType), e.password, e.cut, e.err
}
func (e *fakeSearchControl) first() (controlElement, error) {
	e.childReads++
	if e.child == nil {
		return nil, nil
	}
	return e.child, nil
}
func (e *fakeSearchControl) next() (controlElement, error) {
	if e.sibling == nil {
		return nil, nil
	}
	return e.sibling, nil
}
func (e *fakeSearchControl) release() { e.releases++ }

func TestFindControlsBounds(t *testing.T) {
	base := proto.FindControlsArgs{Handle: 1, PID: 2, Name: "target"}
	a, err := findControlsArgs(base)
	if err != nil || a.MaxDepth != 32 || a.MaxVisited != 5000 || a.MaxMatches != 20 {
		t.Fatalf("defaults: %+v %v", a, err)
	}
	for _, mutate := range []func(*proto.FindControlsArgs){
		func(a *proto.FindControlsArgs) { a.Handle = 0 }, func(a *proto.FindControlsArgs) { a.PID = 0 },
		func(a *proto.FindControlsArgs) { a.Name = "" }, func(a *proto.FindControlsArgs) { a.Name = "x\x00" },
		func(a *proto.FindControlsArgs) { a.ControlType = 50041 }, func(a *proto.FindControlsArgs) { a.ControlType = -1 },
		func(a *proto.FindControlsArgs) { a.MaxDepth = 65 }, func(a *proto.FindControlsArgs) { a.MaxDepth = -1 },
		func(a *proto.FindControlsArgs) { a.MaxVisited = 20001 }, func(a *proto.FindControlsArgs) { a.MaxVisited = -1 },
		func(a *proto.FindControlsArgs) { a.MaxMatches = 101 }, func(a *proto.FindControlsArgs) { a.MaxMatches = -1 },
	} {
		a := base
		mutate(&a)
		if _, err := findControlsArgs(a); err == nil {
			t.Fatalf("accepted %+v", a)
		}
	}
	for _, op := range []string{proto.OpListControls, proto.OpListControlSubtree} {
		a := proto.ControlsArgs{Handle: 1, PID: 2}
		if op == proto.OpListControls {
			a.RootRuntimeID = "42.1"
		}
		data, _ := json.Marshal(a)
		if _, _, err := Dispatch(context.Background(), op, data, nil); err == nil {
			t.Fatalf("accepted wrong scoped op %s", op)
		}
	}
}

func TestFindControlsBeyondSnapshotCap(t *testing.T) {
	root := &fakeSearchControl{n: proto.ControlInfo{Name: "root"}}
	last := root
	var children []*fakeSearchControl
	for i := 0; i < 1200; i++ {
		child := &fakeSearchControl{n: proto.ControlInfo{Name: "duplicate", AutomationID: fmt.Sprintf("item-%d", i), ControlType: 50004, RuntimeID: fmt.Sprintf("42.%d", i)}}
		if i == 0 {
			root.child = child
		} else {
			last.sibling = child
		}
		last = child
		children = append(children, child)
	}
	a, _ := findControlsArgs(proto.FindControlsArgs{Handle: 1, PID: 2, Name: "duplicate", AutomationID: "item-1199", ControlType: 50004})
	r, err := searchControls(root, a)
	if err != nil || r.Truncated || r.Visited != 1201 || len(r.Matches) != 1 || r.Matches[0].RuntimeID != "42.1199" || r.Matches[0].Index != 0 || r.Matches[0].Parent != -1 || r.Matches[0].Depth != 1 {
		t.Fatalf("search: %+v %v", r, err)
	}
	if root.reads != 0 {
		t.Fatal("nonmatch full info read")
	}
	for i, child := range children {
		want := 0
		if i == 1199 {
			want = 1
		}
		if child.reads != want || child.releases != 1 {
			t.Fatalf("child %d reads=%d releases=%d", i, child.reads, child.releases)
		}
	}
}

func TestFindControlsChangedDuringRead(t *testing.T) {
	for _, mode := range []string{"name", "automation_id", "control_type", "password", "selector_truncated", "value_truncated"} {
		t.Run(mode, func(t *testing.T) {
			child := &fakeSearchControl{n: proto.ControlInfo{Name: "private child"}}
			root := &fakeSearchControl{n: proto.ControlInfo{Name: "target", AutomationID: "id", ControlType: 50004}, child: child}
			root.onInfo = func(e *fakeSearchControl) {
				switch mode {
				case "name":
					e.n.Name = "changed"
				case "automation_id":
					e.n.AutomationID = "changed"
				case "control_type":
					e.n.ControlType = 50000
				case "password":
					e.password = true
					e.n.Name = ""
				case "selector_truncated":
					e.cut = true
				case "value_truncated":
					e.infoCut = true
				}
			}
			a, _ := findControlsArgs(proto.FindControlsArgs{Handle: 1, PID: 2, Name: "target", AutomationID: "id", ControlType: 50004})
			r, err := searchControls(root, a)
			if err != nil || !r.Truncated {
				t.Fatalf("changed: %+v %v", r, err)
			}
			want := 0
			if mode == "value_truncated" {
				want = 1
			}
			if len(r.Matches) != want {
				t.Fatalf("mismatched candidate: %+v", r)
			}
			if mode == "password" && (root.childReads != 0 || child.selectorReads != 0) {
				t.Fatal("crossed newly private subtree")
			}
		})
	}
}

func TestFindControlsPartialAndExactLimits(t *testing.T) {
	for _, mode := range []string{"complete", "exact_matches", "max_matches", "exact_visited", "max_visited", "max_depth", "password_subtree", "text_length", "provider_error"} {
		t.Run(mode, func(t *testing.T) {
			leaf := &fakeSearchControl{n: proto.ControlInfo{Name: "target"}}
			second := &fakeSearchControl{n: proto.ControlInfo{Name: "target"}}
			first := &fakeSearchControl{n: proto.ControlInfo{Name: "target"}, child: leaf, sibling: second}
			root := &fakeSearchControl{n: proto.ControlInfo{Name: "root"}, child: first}
			a, _ := findControlsArgs(proto.FindControlsArgs{Handle: 1, PID: 2, Name: "target"})
			switch mode {
			case "exact_matches":
				a.MaxMatches = 3
			case "max_matches":
				a.MaxMatches = 1
			case "exact_visited":
				a.MaxVisited = 4
			case "max_visited":
				a.MaxVisited = 2
			case "max_depth":
				a.MaxDepth = 1
			case "password_subtree":
				first.password = true
			case "text_length":
				first.cut = true
			case "provider_error":
				first.err = errors.New("provider failure")
			}
			r, err := searchControls(root, a)
			if mode == "provider_error" {
				if !errors.Is(err, first.err) || first.releases != 1 {
					t.Fatalf("error: %+v %v", r, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "complete" || strings.HasPrefix(mode, "exact_") {
				if r.Truncated || len(r.Matches) != 3 {
					t.Fatalf("complete: %+v", r)
				}
			} else if !r.Truncated || !slices.Contains(r.Truncation, mode) {
				t.Fatalf("expected %s: %+v", mode, r)
			}
			if mode == "password_subtree" && (first.reads != 0 || first.childReads != 0 || leaf.selectorReads != 0) {
				t.Fatal("password name or descendants read")
			}
			if mode == "text_length" && first.reads != 0 {
				t.Fatal("truncated name counted as exact match")
			}
			if mode == "max_visited" && (r.Visited != 2 || leaf.selectorReads != 0 || second.selectorReads != 0) {
				t.Fatal("visited cap exceeded")
			}
		})
	}
}

func TestFindControlsNativeSearchAndSubtree(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	h := offscreenWindow(t, "hyperhand-control-search-test", 0, -30000)
	editChild(t, h, "first-value", 0, 10)
	editChild(t, h, "second-value", 0, 150)
	a := proto.FindControlsArgs{Handle: uint64(h), PID: uint32(os.Getpid()), ControlType: 50004}
	reply, err := pumpHelper(t, helperRequest{Find: &a})
	if err != nil {
		t.Fatal(err)
	}
	r := reply.Find
	if r == nil || r.Truncated || len(r.Matches) != 2 {
		t.Fatalf("search: %+v", r)
	}
	n := r.Matches[1]
	if n.Index != 1 || n.Parent != -1 || n.RuntimeID == "" || !n.HasValue {
		t.Fatalf("match: %+v", n)
	}
	sub := proto.ControlsArgs{Handle: a.Handle, PID: a.PID, RootRuntimeID: n.RuntimeID, MaxDepth: 1, MaxNodes: 10}
	reply, err = pumpHelper(t, helperRequest{Controls: &sub})
	if err != nil {
		t.Fatal(err)
	}
	if got := reply.Controls; got == nil || len(got.Nodes) != 1 || got.Nodes[0].RuntimeID != n.RuntimeID || got.Nodes[0].Parent != -1 || got.Nodes[0].Depth != 0 {
		t.Fatalf("subtree: %+v", got)
	}
	a.RootRuntimeID = n.RuntimeID
	reply, err = pumpHelper(t, helperRequest{Find: &a})
	if err != nil || reply.Find == nil || len(reply.Find.Matches) != 1 || reply.Find.Matches[0].RuntimeID != n.RuntimeID || reply.Find.Matches[0].Depth != 0 {
		t.Fatalf("scoped search: %+v %v", reply.Find, err)
	}
	act := proto.ControlActionArgs{Handle: a.Handle, PID: a.PID, RuntimeID: n.RuntimeID, Action: "SetValue", Value: "search-action-result"}
	reply, err = pumpHelper(t, helperRequest{Action: &act})
	if err != nil || reply.Action == nil || reply.Action.Value != act.Value {
		t.Fatalf("action: %+v %v", reply.Action, err)
	}
	sub.RootRuntimeID = "42.99999999.1"
	if _, err := pumpHelper(t, helperRequest{Controls: &sub}); err == nil || err.Error() != "element not found" {
		t.Fatalf("missing root: %v", err)
	}
	editChild(t, h, "search-password-secret", 0x20, 260)
	a.RootRuntimeID = ""
	reply, err = pumpHelper(t, helperRequest{Find: &a})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(reply.Find)
	if strings.Contains(string(data), "search-password-secret") || !slices.Contains(reply.Find.Truncation, "password_subtree") {
		t.Fatalf("password handling: %s", data)
	}
	if _, err := pumpHelper(t, helperRequest{Controls: &sub}); err == nil || !strings.HasPrefix(err.Error(), "control search incomplete:") {
		t.Fatalf("private missing root: %v", err)
	}
}
