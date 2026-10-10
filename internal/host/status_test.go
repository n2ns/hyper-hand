package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

func TestStatusDoesNotInterruptBusyAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	stop := sync.OnceFunc(func() { close(release) })
	defer stop()
	b := &windowMCPBackend{
		find: func(string) (hyperv.VM, error) { return hyperv.VM{ID: "A", Name: "A", State: "Running"}, nil },
		respond: func(_ string, req proto.Request) (any, error) {
			switch req.Op {
			case proto.OpExec:
				close(started)
				<-release
				return proto.ExecResult{Stdout: "completed"}, nil
			case proto.OpPing:
				return proto.PingResult{Version: "test", Protocol: proto.Protocol}, nil
			case proto.OpSessionState:
				return proto.SessionStateResult{Console: true}, nil
			default:
				return nil, fmt.Errorf("unexpected op %s", req.Op)
			}
		},
	}
	cs := connectWindowMCP(t, ctx, b)
	done := make(chan error, 1)
	go func() {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_exec", Arguments: map[string]any{"vm": "A", "command": "test"}})
		if err == nil && (r.IsError || !strings.Contains(resultText(r), "completed")) {
			err = fmt.Errorf("exec result: %s", resultText(r))
		}
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
	defer probeCancel()
	r, err := cs.CallTool(probeCtx, &mcp.CallToolParams{Name: "vm_status", Arguments: map[string]any{"vm": "A"}})
	if err != nil {
		t.Fatal(err)
	}
	var st statusOut
	if r.IsError || json.Unmarshal([]byte(resultText(r)), &st) != nil || st.Agent == nil || st.Agent.State != "busy" || st.Session != nil {
		t.Fatalf("status: %s", resultText(r))
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_status", Arguments: json.RawMessage(`{"vm":"A"}`)})
	if err != nil {
		t.Fatal(err)
	}
	st = statusOut{}
	if r.IsError || json.Unmarshal([]byte(resultText(r)), &st) != nil || st.Power != "running" || st.Agent == nil || st.Agent.State != "ok" || st.Agent.Protocol != proto.Protocol || st.Session == nil || st.Session.Locked || !st.Session.Console {
		t.Fatalf("idle status: %s", resultText(r))
	}
}

func TestExecElevationTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := &windowMCPBackend{
		find: func(string) (hyperv.VM, error) { return hyperv.VM{ID: "A", Name: "A", State: "Running"}, nil },
		respond: func(_ string, req proto.Request) (any, error) {
			switch req.Op {
			case proto.OpExec:
				// as an agent sends it when the UAC prompt was not answered before timeout_ms
				return map[string]any{"exit_code": -1, "stdout": "", "stderr": "", "timed_out": true, "elevation_pending": true}, nil
			case proto.OpPing:
				return proto.PingResult{Version: "test", Protocol: proto.Protocol}, nil
			default:
				return nil, fmt.Errorf("unexpected op %s", req.Op)
			}
		},
	}
	cs := connectWindowMCP(t, ctx, b)
	for _, tc := range []struct{ in, want int }{{5000, 5000}, {-1, 60000}} {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_exec", Arguments: map[string]any{"vm": "A", "command": "test", "admin": true, "timeout_ms": tc.in}})
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Error, Reason, Next string
			TimeoutMs           int `json:"timeout_ms"`
		}
		if !r.IsError || json.Unmarshal([]byte(resultText(r)), &e) != nil || e.Error != "elevation_timeout" || e.TimeoutMs != tc.want ||
			!strings.Contains(e.Reason, "did not run") || !strings.Contains(e.Next, "ConsentPromptBehaviorAdmin") || !strings.Contains(e.Next, "background: true") || !strings.Contains(e.Next, "vm_key alt+y") {
			t.Fatalf("timeout_ms %d: %s", tc.in, resultText(r))
		}
	}
}
