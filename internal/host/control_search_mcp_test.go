package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

func searchMCPBackend(respond func(proto.Request) (any, error)) *windowMCPBackend {
	return &windowMCPBackend{
		find: func(name string) (hyperv.VM, error) {
			if name == "" {
				name = "A"
			}
			return hyperv.VM{ID: name, Name: name, State: "Running"}, nil
		},
		respond: func(_ string, req proto.Request) (any, error) {
			if req.Op == proto.OpListWindows {
				return observeWindows(), nil
			}
			return respond(req)
		},
	}
}

func searchMCPNode(runtime string) proto.ControlInfo {
	n := observeControls().Nodes[1]
	n.Index, n.Parent, n.Depth, n.RuntimeID = 0, -1, 0, runtime
	return n
}

func TestControlSearchMCPStatusAndSelectors(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		count        int
		truncated    bool
	}{
		{"unique", "unique", 1, false},
		{"multiple", "multiple", 2, false},
		{"absent", "not_found", 0, false},
		{"partial single", "incomplete", 1, true},
		{"partial empty", "incomplete", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			b := searchMCPBackend(func(req proto.Request) (any, error) {
				if req.Op != proto.OpFindControls {
					return nil, fmt.Errorf("unexpected operation %s", req.Op)
				}
				var a proto.FindControlsArgs
				if err := json.Unmarshal(req.Args, &a); err != nil {
					return nil, err
				}
				if a.Handle != 10 || a.PID != 100 || a.AutomationID != "cmdline" || a.Name != "Command" || a.ControlType != 50004 || a.MaxDepth != 32 || a.MaxVisited != 5000 || a.MaxMatches != 20 {
					return nil, fmt.Errorf("selectors/defaults lost: %+v", a)
				}
				result := proto.FindControlsResult{Matches: []proto.ControlInfo{}, Visited: 1001, Truncated: tc.truncated}
				for i := 0; i < tc.count; i++ {
					n := searchMCPNode(fmt.Sprint(i))
					n.Index = i
					result.Matches = append(result.Matches, n)
				}
				if tc.truncated {
					result.Truncation = []string{"max_visited"}
				}
				return result, nil
			})
			cs := connectWindowMCP(t, ctx, b)
			var out map[string]any
			callJSON(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "handle": 10, "pid": 100, "automation_id": "cmdline", "control_name": "Command", "control_type": "Edit"}, &out)
			matches, ok := out["matches"].([]any)
			if !ok || len(matches) != tc.count || out["status"] != tc.status || out["observation_id"] == "" || out["visited"] != float64(1001) || out["truncated"] != tc.truncated {
				t.Fatalf("search status: %v", out)
			}
			for i, match := range matches {
				if match.(map[string]any)["index"] != float64(i) {
					t.Fatalf("index is not local: %v", matches)
				}
			}
		})
	}
}

func TestControlSearchMCPInvalidArguments(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var searches atomic.Int32
	cs := connectWindowMCP(t, ctx, searchMCPBackend(func(req proto.Request) (any, error) {
		searches.Add(1)
		return nil, fmt.Errorf("invalid arguments reached agent: %s", req.Op)
	}))
	for _, args := range []map[string]any{
		{"handle": 10},
		{"automation_id": "x"},
		{"handle": 10, "control_type": "NotAControlType"},
		{"handle": 10, "control_name": "x", "max_depth": 65},
		{"handle": 10, "control_name": "x", "max_visited": 20001},
		{"handle": 10, "control_name": "x", "max_matches": 101},
		{"handle": 10, "control_name": "x", "max_depth": -1},
		{"handle": 10, "control_name": "x", "observation_id": "old", "index": 0},
		{"control_name": "x", "observation_id": "old"},
		{"control_name": "x", "index": 0},
	} {
		out := callRefused(t, ctx, cs, "vm_find_controls", args)
		if out["error"] != codeInvalidArgument {
			t.Errorf("%v: %v", args, out)
		}
	}
	if searches.Load() != 0 {
		t.Fatalf("%d invalid calls reached search", searches.Load())
	}
}

func TestControlSearchMCPReferencesWorkForWaitSubtreeAndAction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var scoped, actions atomic.Int32
	b := searchMCPBackend(func(req proto.Request) (any, error) {
		switch req.Op {
		case proto.OpFindControls:
			var a proto.FindControlsArgs
			_ = json.Unmarshal(req.Args, &a)
			if a.RootRuntimeID != "" && a.RootRuntimeID != "deep.runtime" {
				return nil, fmt.Errorf("wrong scoped search root: %+v", a)
			}
			if a.RootRuntimeID != "" && (a.HintRect == nil || *a.HintRect != searchMCPNode("deep.runtime").Rect) {
				return nil, fmt.Errorf("scoped search lost observed bounds: %+v", a)
			}
			return proto.FindControlsResult{Matches: []proto.ControlInfo{searchMCPNode("deep.runtime")}, Visited: 1201}, nil
		case proto.OpListControlSubtree:
			var a proto.ControlsArgs
			_ = json.Unmarshal(req.Args, &a)
			if a.Handle != 10 || a.PID != 100 || a.RootRuntimeID != "deep.runtime" {
				return nil, fmt.Errorf("wrong subtree target: %+v", a)
			}
			if a.HintRect == nil || *a.HintRect != searchMCPNode("deep.runtime").Rect {
				return nil, fmt.Errorf("wait/subtree lost observed bounds: %+v", a)
			}
			scoped.Add(1)
			return proto.ControlsResult{Nodes: []proto.ControlInfo{searchMCPNode("deep.runtime")}, Focused: -1}, nil
		case proto.OpControlAction:
			var a proto.ControlActionArgs
			_ = json.Unmarshal(req.Args, &a)
			if a.Handle != 10 || a.PID != 100 || a.RuntimeID != "deep.runtime" || a.Action != "SetValue" || a.Value != "CIRCLE" {
				return nil, fmt.Errorf("wrong semantic target: %+v", a)
			}
			if a.HintRect == nil || *a.HintRect != searchMCPNode("deep.runtime").Rect {
				return nil, fmt.Errorf("action lost observed bounds: %+v", a)
			}
			actions.Add(1)
			return proto.ControlActionResult{HasValue: true, Value: a.Value}, nil
		default:
			return nil, fmt.Errorf("deep target must not use full window snapshot: %s", req.Op)
		}
	})
	cs := connectWindowMCP(t, ctx, b)
	var found map[string]any
	callJSON(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "task_id": "owner", "handle": 10, "automation_id": "cmdline"}, &found)
	id := found["observation_id"]
	var waited map[string]any
	callJSON(t, ctx, cs, "vm_wait", map[string]any{"vm": "A", "task_id": "owner", "kind": "control_matches", "observation_id": id, "index": 0, "value": "LINE", "check_only": true}, &waited)
	if waited["satisfied"] != true {
		t.Fatalf("found control cannot be waited on: %v", waited)
	}
	var nested map[string]any
	callJSON(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "task_id": "owner", "observation_id": id, "index": 0, "control_type": "Edit"}, &nested)
	subtree, _, _ := observe(t, ctx, cs, map[string]any{"vm": "A", "task_id": "owner", "observation_id": id, "index": 0, "screenshot": false})
	if !strings.Contains(fmt.Sprint(subtree["controls"]), "[0] Edit") {
		t.Fatalf("subtree not rooted at selected control: %v", subtree)
	}
	callJSON(t, ctx, cs, "vm_wait", map[string]any{"vm": "A", "task_id": "owner", "kind": "control_exists", "observation_id": subtree["observation_id"], "index": 0, "check_only": true}, &waited)
	if waited["satisfied"] != true {
		t.Fatalf("subtree reference cannot be waited on: %v", waited)
	}
	var changed map[string]any
	callJSON(t, ctx, cs, "vm_set_value", map[string]any{"vm": "A", "task_id": "owner", "observation_id": id, "index": 0, "value": "CIRCLE", "observe_after": "none"}, &changed)
	if changed["ok"] != true || scoped.Load() < 3 || actions.Load() != 1 {
		t.Fatalf("reference integration: %v scoped=%d actions=%d", changed, scoped.Load(), actions.Load())
	}
}

func TestControlSearchMCPTaskAndVMIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectWindowMCP(t, ctx, searchMCPBackend(func(req proto.Request) (any, error) {
		if req.Op == proto.OpFindControls {
			return proto.FindControlsResult{Matches: []proto.ControlInfo{searchMCPNode("deep.runtime")}}, nil
		}
		return nil, fmt.Errorf("unexpected operation %s", req.Op)
	}))
	var found map[string]any
	callJSON(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "task_id": "owner", "handle": 10, "control_type": "Edit"}, &found)
	for _, tc := range []struct{ task, vm string }{{"other", "A"}, {"owner", "B"}} {
		for _, tool := range []string{"vm_find_controls", "vm_observe", "vm_wait"} {
			args := map[string]any{"vm": tc.vm, "task_id": tc.task, "observation_id": found["observation_id"], "index": 0}
			switch tool {
			case "vm_find_controls":
				args["control_type"] = "Edit"
			case "vm_observe":
				args["screenshot"] = false
			case "vm_wait":
				args["kind"], args["check_only"] = "control_exists", true
			}
			out := callRefused(t, ctx, cs, tool, args)
			if out["error"] != codeStaleObservation && out["error"] != codeInvalidArgument {
				t.Fatalf("foreign reference %s: %v", tool, out)
			}
		}
	}
}

func TestControlSearchMCPWindowReuseDuringRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var searched atomic.Bool
	b := searchMCPBackend(func(req proto.Request) (any, error) {
		if req.Op == proto.OpFindControls {
			searched.Store(true)
			return proto.FindControlsResult{Matches: []proto.ControlInfo{searchMCPNode("deep.runtime")}}, nil
		}
		return nil, fmt.Errorf("unexpected operation %s", req.Op)
	})
	respond := b.respond
	b.respond = func(vm string, req proto.Request) (any, error) {
		if req.Op == proto.OpListWindows && searched.Load() {
			ws := observeWindows()
			ws.Windows[0].PID = 999
			return ws, nil
		}
		return respond(vm, req)
	}
	cs := connectWindowMCP(t, ctx, b)
	out := callRefused(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "handle": 10, "control_type": "Edit"})
	if out["error"] != codeStaleObservation || out["observation_id"] != nil {
		t.Fatalf("window reused during search accepted: %v", out)
	}
}

// A search result is a flat set, not a whole-window tree. Subtrees from distinct
// roots must not report each other's controls as newly added or removed.
func TestControlSearchMCPDiffScopeIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectWindowMCP(t, ctx, searchMCPBackend(func(req proto.Request) (any, error) {
		switch req.Op {
		case proto.OpFindControls:
			first, second := searchMCPNode("first"), searchMCPNode("second")
			second.Index = 1
			return proto.FindControlsResult{Matches: []proto.ControlInfo{first, second}}, nil
		case proto.OpListControls:
			return observeControls(), nil
		case proto.OpListControlSubtree:
			var a proto.ControlsArgs
			_ = json.Unmarshal(req.Args, &a)
			return proto.ControlsResult{Nodes: []proto.ControlInfo{searchMCPNode(a.RootRuntimeID)}, Focused: -1}, nil
		}
		return nil, fmt.Errorf("unexpected operation %s", req.Op)
	}))
	var found map[string]any
	callJSON(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "handle": 10, "control_type": "Edit"}, &found)
	id := found["observation_id"]
	first, _, _ := observe(t, ctx, cs, map[string]any{"vm": "A", "observation_id": id, "index": 0, "screenshot": false})
	for _, args := range []map[string]any{
		{"vm": "A", "handle": 10, "diff_from": id, "screenshot": false},
		{"vm": "A", "handle": 10, "diff_from": first["observation_id"], "screenshot": false},
		{"vm": "A", "observation_id": id, "index": 1, "diff_from": first["observation_id"], "screenshot": false},
	} {
		out, _, _ := observe(t, ctx, cs, args)
		if out["controls_diff"] != nil || out["controls"] == nil || !strings.Contains(fmt.Sprint(out["stale_risk"]), "diff_from ignored") {
			t.Fatalf("cross-scope diff accepted: %v", out)
		}
	}
	same, _, _ := observe(t, ctx, cs, map[string]any{"vm": "A", "observation_id": id, "index": 0, "diff_from": first["observation_id"], "screenshot": false})
	if same["controls_diff"] == nil || same["controls"] != nil {
		t.Fatalf("same-scope diff refused: %v", same)
	}
}

func TestControlSearchMCPDirectWaitDistinguishesGoneFromUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		gone          bool
	}{
		{"gone", "element not found: selected control", true},
		{"lookup bounded", "control search incomplete: max_visited", false},
		{"provider failure", "UI Automation provider failed", false},
		{"old agent", "unknown op: list_control_subtree", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cs := connectWindowMCP(t, ctx, searchMCPBackend(func(req proto.Request) (any, error) {
				switch req.Op {
				case proto.OpFindControls:
					return proto.FindControlsResult{Matches: []proto.ControlInfo{searchMCPNode("deep.runtime")}}, nil
				case proto.OpListControlSubtree:
					return nil, fmt.Errorf("%s", tc.message)
				}
				return nil, fmt.Errorf("unexpected operation %s", req.Op)
			}))
			var found map[string]any
			callJSON(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "handle": 10, "control_type": "Edit"}, &found)
			args := map[string]any{"vm": "A", "kind": "control_gone", "observation_id": found["observation_id"], "index": 0, "check_only": true}
			if tc.gone {
				var out map[string]any
				callJSON(t, ctx, cs, "vm_wait", args, &out)
				if out["satisfied"] != true {
					t.Fatalf("definitive absence not accepted: %v", out)
				}
			} else {
				out := callRefused(t, ctx, cs, "vm_wait", args)
				if out["satisfied"] == true {
					t.Fatalf("unknown became absence: %v", out)
				}
				if tc.name == "old agent" && out["error"] != codeAgentOutdated {
					t.Fatalf("old agent update guidance missing: %v", out)
				}
			}
		})
	}
}

func TestControlSearchMCPRejectsSessionChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var searched atomic.Bool
	b := searchMCPBackend(func(req proto.Request) (any, error) {
		if req.Op == proto.OpFindControls {
			searched.Store(true)
			return proto.FindControlsResult{Matches: []proto.ControlInfo{searchMCPNode("deep.runtime")}}, nil
		}
		return nil, fmt.Errorf("unexpected operation %s", req.Op)
	})
	respond := b.respond
	b.respond = func(vm string, req proto.Request) (any, error) {
		if req.Op == proto.OpListWindows && searched.Load() {
			ws := observeWindows()
			ws.Session.Locked = true
			return ws, nil
		}
		return respond(vm, req)
	}
	cs := connectWindowMCP(t, ctx, b)
	out := callRefused(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "handle": 10, "control_type": "Edit"})
	if out["error"] != codeSessionUnusable || out["observation_id"] != nil {
		t.Fatalf("session changed during search accepted: %v", out)
	}
}

func TestControlSearchScopeRejectsLifecycleChange(t *testing.T) {
	store := newObservationStore()
	w := observeWindows().Windows[0]
	o := &observation{VM: "A", Window: &w, TreeWindow: &w, Nodes: []proto.ControlInfo{searchMCPNode("deep.runtime")}, SearchResults: true}
	store.put(o)
	d := &deps{obs: store}
	if _, _, err := d.loadControlScope(context.Background(), "A", o.ID, 0); err != nil {
		t.Fatal(err)
	}
	store.invalidate("A")
	if _, _, err := d.loadControlScope(context.Background(), "A", o.ID, 0); err == nil || asToolError(err).Code != codeStaleObservation {
		t.Fatalf("lifecycle changed scope accepted: %v", err)
	}
}

func TestControlSearchMCPGoneReadRechecksSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var missing atomic.Bool
	b := searchMCPBackend(func(req proto.Request) (any, error) {
		switch req.Op {
		case proto.OpFindControls:
			return proto.FindControlsResult{Matches: []proto.ControlInfo{searchMCPNode("deep.runtime")}}, nil
		case proto.OpListControlSubtree:
			missing.Store(true)
			return nil, fmt.Errorf("element not found")
		}
		return nil, fmt.Errorf("unexpected operation %s", req.Op)
	})
	respond := b.respond
	b.respond = func(vm string, req proto.Request) (any, error) {
		if req.Op == proto.OpListWindows && missing.Load() {
			ws := observeWindows()
			ws.Session.Locked = true
			return ws, nil
		}
		return respond(vm, req)
	}
	cs := connectWindowMCP(t, ctx, b)
	var found map[string]any
	callJSON(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "handle": 10, "control_type": "Edit"}, &found)
	out := callRefused(t, ctx, cs, "vm_wait", map[string]any{"vm": "A", "kind": "control_gone", "observation_id": found["observation_id"], "index": 0, "check_only": true})
	if out["error"] != codeSessionUnusable || out["satisfied"] == true {
		t.Fatalf("locked session mistaken for gone control: %v", out)
	}
}

func TestControlSearchMCPOldAgentFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var outdated atomic.Bool
	b := searchMCPBackend(func(req proto.Request) (any, error) {
		if req.Op == proto.OpFindControls && !outdated.Load() {
			return proto.FindControlsResult{Matches: []proto.ControlInfo{searchMCPNode("deep.runtime")}}, nil
		}
		return nil, fmt.Errorf("unknown op: %s", req.Op)
	})
	cs := connectWindowMCP(t, ctx, b)
	var found map[string]any
	callJSON(t, ctx, cs, "vm_find_controls", map[string]any{"vm": "A", "handle": 10, "control_type": "Edit"}, &found)
	outdated.Store(true)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"vm_find_controls", map[string]any{"vm": "A", "handle": 10, "control_type": "Edit"}},
		{"vm_observe", map[string]any{"vm": "A", "observation_id": found["observation_id"], "index": 0, "screenshot": false}},
	} {
		out := callRefused(t, ctx, cs, tc.tool, tc.args)
		if out["error"] != codeAgentOutdated || out["next"] != "call vm_update_agent" {
			t.Fatalf("old agent error for %s: %v", tc.tool, out)
		}
	}
}
