package host

import (
	"context"
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

func uiWaitMCPBackend(controls func() (proto.ControlsResult, error)) *windowMCPBackend {
	return &windowMCPBackend{
		find: func(name string) (hyperv.VM, error) {
			if name == "" {
				name = "A"
			}
			return hyperv.VM{ID: name, Name: name, State: "Running"}, nil
		},
		respond: func(_ string, req proto.Request) (any, error) {
			switch req.Op {
			case proto.OpListWindows:
				return observeWindows(), nil
			case proto.OpListControls:
				return controls()
			default:
				return nil, fmt.Errorf("unexpected operation %s", req.Op)
			}
		},
	}
}

func TestUIWaitMCPCheckAndAssert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var samples atomic.Int32
	cs := connectWindowMCP(t, ctx, uiWaitMCPBackend(func() (proto.ControlsResult, error) {
		samples.Add(1)
		return observeControls(), nil
	}))
	for _, assertion := range []bool{false, true} {
		args := map[string]any{"kind": "control_matches", "handle": 10, "pid": 100, "automation_id": "cmdline", "enabled": true, "value": "CIRCLE", "check_only": true, "assert": assertion, "task_id": "check"}
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_wait", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		out := resultJSON(t, r)
		if r.IsError != assertion || out["satisfied"] != false || out["task_id"] != "check" || out["last"] == nil {
			t.Fatalf("assert=%v: %v", assertion, out)
		}
		if assertion && out["error"] != "assertion_failed" {
			t.Fatalf("assertion refusal: %v", out)
		}
	}
	if samples.Load() != 2 {
		t.Fatalf("check_only sampled %d times, want one per call", samples.Load())
	}
}

func TestUIWaitMCPUnknownCannotSatisfy(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		args map[string]any
		tree proto.ControlsResult
	}{
		{"truncated absence", "control_gone", nil, proto.ControlsResult{Truncated: true, Truncation: []string{"max_nodes"}}},
		{"missing ValuePattern is not empty value", "control_matches", map[string]any{"value": ""}, proto.ControlsResult{Nodes: []proto.ControlInfo{{AutomationID: "target", PID: 100}}}},
		{"unknown selected is not false", "control_matches", map[string]any{"state": map[string]any{"selected": false}}, proto.ControlsResult{Nodes: []proto.ControlInfo{{AutomationID: "target", PID: 100}}}},
		{"truncated value is not equality", "control_matches", map[string]any{"value": "prefix"}, proto.ControlsResult{Truncated: true, Truncation: []string{"text_length"}, Nodes: []proto.ControlInfo{{AutomationID: "target", PID: 100, HasValue: true, Value: "prefix"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cs := connectWindowMCP(t, ctx, uiWaitMCPBackend(func() (proto.ControlsResult, error) { return tc.tree, nil }))
			args := map[string]any{"kind": tc.kind, "handle": 10, "pid": 100, "automation_id": "target", "check_only": true}
			for key, value := range tc.args {
				args[key] = value
			}
			var out map[string]any
			callJSON(t, ctx, cs, "vm_wait", args, &out)
			last, _ := out["last"].(map[string]any)
			if out["satisfied"] != false || last["unknown"] == nil || last["unknown"] == "" {
				t.Fatalf("unknown state became known: %v", out)
			}
		})
	}
}

func TestUIWaitMCPAmbiguousSelector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectWindowMCP(t, ctx, uiWaitMCPBackend(func() (proto.ControlsResult, error) {
		return proto.ControlsResult{Nodes: []proto.ControlInfo{{Name: "Save", PID: 100, RuntimeID: "one"}, {Name: "Save", PID: 100, RuntimeID: "two"}}}, nil
	}))
	args := map[string]any{"kind": "control_exists", "handle": 10, "pid": 100, "control_name": "Save", "check_only": true}
	out := callRefused(t, ctx, cs, "vm_wait", args)
	if out["error"] != codeAmbiguousTarget {
		t.Fatalf("ambiguous control: %v", out)
	}
}

func TestUIWaitMCPObservationIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectWindowMCP(t, ctx, uiWaitMCPBackend(func() (proto.ControlsResult, error) { return observeControls(), nil }))
	obs, _, _ := observe(t, ctx, cs, map[string]any{"vm": "A", "task_id": "owner", "handle": 10, "controls": true, "screenshot": false})
	for _, tc := range []struct{ task, vm string }{{"other", "A"}, {"owner", "B"}} {
		args := map[string]any{"vm": tc.vm, "task_id": tc.task, "kind": "control_exists", "observation_id": obs["observation_id"], "index": 1, "check_only": true}
		out := callRefused(t, ctx, cs, "vm_wait", args)
		if out["error"] != codeStaleObservation && out["error"] != codeInvalidArgument {
			t.Fatalf("foreign observation: %v", out)
		}
	}
	var out map[string]any
	callJSON(t, ctx, cs, "vm_wait", map[string]any{"vm": "A", "task_id": "owner", "kind": "control_matches", "observation_id": obs["observation_id"], "index": 1, "value": "LINE", "check_only": true}, &out)
	if out["satisfied"] != true {
		t.Fatalf("own observation failed: %v", out)
	}
}

func TestUIWaitMCPPinsVMAndReleasesInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var defaultFinds, samples atomic.Int32
	var clicked atomic.Bool
	firstSample := make(chan struct{})
	first := sync.OnceFunc(func() { close(firstSample) })
	b := uiWaitMCPBackend(func() (proto.ControlsResult, error) {
		samples.Add(1)
		tree := observeControls()
		tree.Nodes[1].Value = "before"
		if clicked.Load() {
			tree.Nodes[1].Value = "after"
		}
		first()
		return tree, nil
	})
	b.find = func(name string) (hyperv.VM, error) {
		if name == "" {
			name = "A"
			if defaultFinds.Add(1) > 1 {
				name = "B"
			}
		}
		return hyperv.VM{ID: name, Name: name, State: "Running"}, nil
	}
	respond := b.respond
	b.respond = func(id string, req proto.Request) (any, error) {
		if id != "A" {
			return nil, fmt.Errorf("wait moved to VM %s", id)
		}
		return respond(id, req)
	}
	b.click = func(vm string, _, _, _, _ int, _ []string) error {
		if vm != "A" {
			return fmt.Errorf("click moved to VM %s", vm)
		}
		clicked.Store(true)
		return nil
	}
	cs := connectWindowMCP(t, ctx, b)
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_wait", Arguments: map[string]any{"kind": "control_matches", "handle": 10, "pid": 100, "automation_id": "cmdline", "value": "after", "timeout_ms": 3000, "task_id": "worker"}})
		done <- outcome{r, err}
	}()
	select {
	case <-firstSample:
	case <-ctx.Done():
		t.Fatal("wait never sampled")
	}
	var clickedOut map[string]any
	callJSON(t, ctx, cs, "vm_click", map[string]any{"vm": "A", "task_id": "worker", "x": 1, "y": 1, "observe_after": "none"}, &clickedOut)
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatal(out.err)
		}
		if result := resultJSON(t, out.result); out.result.IsError || result["satisfied"] != true {
			t.Fatalf("wait after concurrent click: %v", result)
		}
	case <-ctx.Done():
		t.Fatal("wait blocked concurrent input")
	}
	if defaultFinds.Load() != 1 || samples.Load() < 2 {
		t.Fatalf("default lookups=%d samples=%d", defaultFinds.Load(), samples.Load())
	}
}

func TestUIWaitMCPEndTurnCancelsPolling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	first := sync.OnceFunc(func() { close(started) })
	cs := connectWindowMCP(t, ctx, uiWaitMCPBackend(func() (proto.ControlsResult, error) {
		first()
		return observeControls(), nil
	}))
	done := make(chan map[string]any, 1)
	go func() {
		done <- callRefusedQuiet(ctx, cs, "vm_wait", map[string]any{"task_id": "waiting", "vm": "A", "kind": "control_gone", "handle": 10, "automation_id": "cmdline", "timeout_ms": 60000})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("wait never sampled")
	}
	var out endTurnOut
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"task_id": "waiting"}, &out)
	if out.CancelledWaits != 1 {
		t.Fatalf("cancelled waits=%d", out.CancelledWaits)
	}
	select {
	case result := <-done:
		if result["error"] != codeFailed || result["reason"] != "the wait was cancelled by vm_end_turn" {
			t.Fatalf("cancelled wait: %v", result)
		}
	case <-ctx.Done():
		t.Fatal("wait survived task cleanup")
	}
}

func TestUIWaitMCPProviderFailureIsNotConditionTimeout(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("provider_failure=%v", failed), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cs := connectWindowMCP(t, ctx, uiWaitMCPBackend(func() (proto.ControlsResult, error) {
				if failed {
					return proto.ControlsResult{}, fmt.Errorf("UI Automation helper timed out")
				}
				return observeControls(), nil
			}))
			for _, assertion := range []bool{false, true} {
				r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_wait", Arguments: map[string]any{"kind": "control_gone", "handle": 10, "automation_id": "cmdline", "timeout_ms": 80, "assert": assertion}})
				if err != nil {
					t.Fatal(err)
				}
				out := resultJSON(t, r)
				if failed {
					if !r.IsError || out["error"] == "assertion_failed" || out["satisfied"] == true || !strings.Contains(fmt.Sprint(out["reason"]), "timed out") {
						t.Fatalf("provider failure became condition evidence: %v", out)
					}
				} else if r.IsError != assertion || out["satisfied"] != false || assertion && out["error"] != "assertion_failed" {
					t.Fatalf("ordinary condition timeout: %v", out)
				}
			}
		})
	}
}

func TestUIWaitMCPSamplingDeadlineReleasesConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	cs := connectWindowMCP(t, ctx, uiWaitMCPBackend(func() (proto.ControlsResult, error) {
		<-release
		return proto.ControlsResult{}, nil
	}))
	out := callRefused(t, ctx, cs, "vm_wait", map[string]any{"kind": "control_gone", "handle": 10, "automation_id": "cmdline", "timeout_ms": 80, "assert": true})
	if out["error"] == "assertion_failed" || out["satisfied"] == true {
		t.Fatalf("unfinished sample proved condition: %v", out)
	}
	unblock()
	// The timed-out call must drop the busy connection so the next UI read can run.
	var recovered map[string]any
	callJSON(t, ctx, cs, "vm_wait", map[string]any{"kind": "window_exists", "handle": 10, "check_only": true}, &recovered)
	if recovered["satisfied"] != true {
		t.Fatalf("connection did not recover: %v", recovered)
	}
}

type uiWaitLifecycleBackend struct{ *windowMCPBackend }

func (*uiWaitLifecycleBackend) Stop(string) error { return nil }

func TestUIWaitMCPRejectsPreLifecycleObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := &uiWaitLifecycleBackend{uiWaitMCPBackend(func() (proto.ControlsResult, error) { return observeControls(), nil })}
	cs, d := connectTools(t, ctx, b)
	d.tasks = newTaskRegistry()
	registerObserve(d)
	t.Cleanup(func() { d.m.Drop("A") })
	obs, _, _ := observe(t, ctx, cs, map[string]any{"vm": "A", "task_id": "owner", "handle": 10, "controls": true, "screenshot": false})
	var poweredOff map[string]any
	callJSON(t, ctx, cs, "vm_turn_off", map[string]any{"vm": "A", "task_id": "owner"}, &poweredOff)
	out := callRefused(t, ctx, cs, "vm_wait", map[string]any{"vm": "A", "task_id": "owner", "kind": "control_gone", "observation_id": obs["observation_id"], "index": 1, "check_only": true})
	if out["error"] != codeStaleObservation {
		t.Fatalf("old epoch accepted: %v", out)
	}
}
