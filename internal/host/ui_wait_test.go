package host

import (
	"context"
	"errors"
	"testing"
	"time"

	"hyperhand/internal/proto"
)

func uiWaitSampleDeps(tree proto.ControlsResult) (*deps, *proto.WindowsResult) {
	ws := &proto.WindowsResult{Windows: []proto.WindowInfo{{Handle: 10, PID: 100, Process: "test", Class: "window", Enabled: true}}, Session: &proto.SessionStateResult{Console: true}}
	d := &deps{obs: newObservationStore(), raw: &fakeInput{}, turn: newTurnState()}
	d.call = func(_ context.Context, _, op string, _ any, _ []byte, out any) ([]byte, error) {
		switch op {
		case proto.OpListWindows:
			*out.(*proto.WindowsResult) = *ws
		case proto.OpListControls:
			*out.(*proto.ControlsResult) = tree
		}
		return nil, nil
	}
	return d, ws
}

func TestUIWaitStableRuntimeAcrossTruncation(t *testing.T) {
	n := proto.ControlInfo{RuntimeID: "runtime-1", Enabled: true, HasValue: true, Value: "", State: &proto.ControlState{Selected: new(false), VerticalScrollPercent: new(0.0)}}
	for _, tc := range []struct {
		name       string
		truncation []string
		want       bool
	}{
		{"size", []string{"max_nodes"}, true},
		{"text", []string{"text_length"}, false},
		{"old agent", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ws := uiWaitSampleDeps(proto.ControlsResult{Nodes: []proto.ControlInfo{n}, Truncated: true, Truncation: tc.truncation})
			target := uiWaitTarget{selector: windowSelector{Handle: 10}, window: &ws.Windows[0], runtime: n.RuntimeID}
			in := waitIn{VM: "A", Kind: "control_matches", Enabled: new(true), Value: new(""), State: &proto.ControlState{Selected: new(false), VerticalScrollPercent: new(0.0)}}
			got, last, err := d.sampleUIWait(context.Background(), in, &target)
			if err != nil || got != tc.want {
				t.Fatalf("got=%v err=%v last=%v", got, err, last)
			}
		})
	}
}

func TestUIWaitRejectsReusedWindowAndLifecycle(t *testing.T) {
	for _, kind := range []string{"window_gone", "control_gone", "window_exists"} {
		t.Run(kind, func(t *testing.T) {
			d, ws := uiWaitSampleDeps(proto.ControlsResult{})
			old := ws.Windows[0]
			target := uiWaitTarget{selector: windowSelector{Handle: old.Handle, PID: old.PID}, window: &old}
			ws.Windows[0].PID++
			if satisfied, _, err := d.sampleUIWait(context.Background(), waitIn{VM: "A", Kind: kind}, &target); satisfied || err == nil || asToolError(err).Code != codeStaleObservation {
				t.Fatalf("reused handle: satisfied=%v err=%v", satisfied, err)
			}
			ws.Windows = nil
			d.obs.invalidate("A")
			if satisfied, _, err := d.sampleUIWait(context.Background(), waitIn{VM: "A", Kind: kind}, &target); satisfied || err == nil || asToolError(err).Code != codeStaleObservation {
				t.Fatalf("replaced VM: satisfied=%v err=%v", satisfied, err)
			}
		})
	}
}

func TestUIWaitDisappearanceRequiresUsableSession(t *testing.T) {
	d, ws := uiWaitSampleDeps(proto.ControlsResult{})
	ws.Windows = nil
	for _, locked := range []bool{false, true} {
		ws.Session.Locked = locked
		satisfied, _, err := d.sampleUIWait(context.Background(), waitIn{VM: "A", Kind: "window_gone"}, &uiWaitTarget{selector: windowSelector{Handle: 10}})
		if locked {
			if satisfied || err == nil || asToolError(err).Code != codeSessionUnusable {
				t.Fatalf("locked desktop counted as disappearance: satisfied=%v err=%v", satisfied, err)
			}
		} else if !satisfied || err != nil {
			t.Fatalf("absent window: satisfied=%v err=%v", satisfied, err)
		}
	}
}

func TestUIWaitCallUsesRemainingDeadline(t *testing.T) {
	d, _ := uiWaitSampleDeps(proto.ControlsResult{})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	outerDeadline, _ := ctx.Deadline()
	d.call = func(ctx context.Context, _, _ string, _ any, _ []byte, _ any) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(outerDeadline) {
			t.Errorf("call deadline %v exceeds wait deadline %v", deadline, outerDeadline)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	err := d.uiWaitCall(ctx, "A", proto.OpListControls, nil, &proto.ControlsResult{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("deadline not respected: %v, elapsed=%v", err, time.Since(start))
	}
}

func TestUIWaitConditionTimeoutAndAssertion(t *testing.T) {
	for _, assert := range []bool{false, true} {
		d, _ := uiWaitSampleDeps(proto.ControlsResult{})
		r, err := d.waitUI(context.Background(), waitIn{Kind: "window_gone", Handle: 10, TimeoutMs: 20, Assert: assert})
		if assert {
			if err == nil || asToolError(err).Code != "assertion_failed" || asToolError(err).Fields["satisfied"] != false {
				t.Fatalf("assert timeout: %v", err)
			}
		} else if err != nil || resultJSON(t, r)["satisfied"] != false {
			t.Fatalf("wait timeout: %v %v", r, err)
		}
	}
}

func TestUIWaitValidatesExpectedState(t *testing.T) {
	for _, state := range []*proto.ControlState{
		{Toggle: new("yes")}, {ExpandCollapse: new("open")}, {VerticalScrollPercent: new(-1.0)}, {HorizontalScrollPercent: new(101.0)}, {},
	} {
		if err := validateUIWait(waitIn{Kind: "control_matches", Handle: 10, AutomationID: "edit", State: state}); err == nil {
			t.Fatalf("invalid expectation accepted: %+v", state)
		}
	}
	if err := validateUIWait(waitIn{Kind: "control_matches", Handle: 10, AutomationID: "edit", Value: new("")}); err != nil {
		t.Fatalf("empty string value rejected: %v", err)
	}
}
