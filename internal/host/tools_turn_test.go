package host

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

func TestEndTurnCancelsWaits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	b := newFakeAgentBackend(func(req proto.Request) (any, error) {
		if req.Op != proto.OpWait {
			return nil, errors.New("unexpected op " + req.Op)
		}
		entered <- struct{}{}
		<-release // the guest condition never comes true
		return proto.WaitResult{Satisfied: false}, nil
	})
	defer close(release)
	cs, _ := connectTools(t, ctx, b)
	done := make(chan map[string]any, 1)
	go func() {
		done <- callRefusedQuiet(ctx, cs, "vm_wait", map[string]any{"kind": "process_exit", "name": "acad", "timeout_ms": 60000})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("vm_wait never reached the agent")
	}
	var out endTurnOut
	callJSON(t, ctx, cs, "vm_end_turn", nil, &out)
	if out.CancelledWaits != 1 || len(out.DeletedCheckpoints) != 0 || len(out.Errors) != 0 {
		t.Fatalf("end_turn %+v", out)
	}
	select {
	case e := <-done:
		if e == nil || e["error"] != codeFailed || e["reason"] != "the wait was cancelled by vm_end_turn" {
			t.Fatalf("vm_wait after cancel: %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("vm_wait did not return after vm_end_turn")
	}
	// The cancelled wait is unregistered: a second end_turn cancels nothing.
	callJSON(t, ctx, cs, "vm_end_turn", nil, &out)
	if out.CancelledWaits != 0 {
		t.Errorf("second end_turn %+v", out)
	}
}

// callRefusedQuiet is callRefused without a *testing.T, for calls made from another goroutine; nil means the call did
// not produce a refusal. Like callRefused it passes the fixture VM when args omit vm.
func callRefusedQuiet(ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: withFixtureVM(cs, name, args)})
	if err != nil || !r.IsError {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(resultText(r)), &obj); err != nil {
		return nil
	}
	return obj
}
