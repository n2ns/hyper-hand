package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/agent"
	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

// jobBackend answers every agent op with the real agent handlers, so background jobs run real local processes.
type jobBackend struct{ *observeBackend }

func (b *jobBackend) ListVMs() ([]hyperv.VM, error) {
	return []hyperv.VM{{ID: "A", Name: "CAD", State: "Running"}}, nil
}

func (b *jobBackend) Dial(_ context.Context, _ string) (net.Conn, error) {
	client, guest := net.Pipe()
	go func() {
		defer guest.Close()
		for {
			var req proto.Request
			payload, err := proto.ReadFrame(guest, &req)
			if err != nil {
				return
			}
			result, out, err := agent.Dispatch(context.Background(), req.Op, req.Args, payload)
			response := proto.Response{}
			if err != nil {
				response.Error = err.Error()
			} else {
				response.Result, _ = json.Marshal(result)
			}
			if err := proto.WriteFrame(guest, response, out); err != nil {
				return
			}
		}
	}()
	return client, nil
}

func jobMCP(t *testing.T, ctx context.Context) (*mcp.ClientSession, *mcp.ClientSession) {
	t.Helper()
	m := &Manager{Backend: &jobBackend{newObserveBackend(t)}}
	t.Cleanup(func() { m.Drop("A") })
	s := NewServer(m)
	connect := func() *mcp.ClientSession {
		st, ct := mcp.NewInMemoryTransports()
		if _, err := s.Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "job-test", Version: "test"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	return connect(), connect()
}

func TestJobToolsRunReadAndCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner, other := jobMCP(t, ctx)

	var started map[string]any
	callJSON(t, ctx, owner, "vm_exec", map[string]any{"vm": "CAD", "background": true, "command": "Write-Output first; Start-Sleep -Milliseconds 800; Write-Output 第二; exit 2"}, &started)
	id, _ := started["id"].(string)
	if next, _ := started["next"].(string); !strings.HasPrefix(id, "job-") || started["state"] != "running" || started["vm"] != "CAD" || !strings.Contains(next, "vm_job") || !strings.Contains(next, id) {
		t.Fatalf("start: %v", started)
	}
	// Another task reads the job without the VM's write ownership; output arrives incrementally.
	var out strings.Builder
	args := map[string]any{"vm": "CAD", "job_id": id, "wait_ms": 10000, "max_bytes": 4}
	for i := 0; ; i++ {
		var r map[string]any
		callJSON(t, ctx, other, "vm_job", args, &r)
		out.WriteString(r["stdout"].(string))
		args["stdout_offset"], args["stderr_offset"] = r["stdout_next"], r["stderr_next"]
		if r["complete"] == true {
			if r["state"] != "exited" || r["exit_code"] != float64(2) {
				t.Fatalf("final: %v", r)
			}
			break
		}
		if i > 100 {
			t.Fatalf("job did not complete: %v", r)
		}
	}
	if got := out.String(); !strings.Contains(got, "first") || !strings.Contains(got, "第二") {
		t.Fatalf("stdout %q", got)
	}
	var list map[string]any
	callJSON(t, ctx, other, "vm_job", map[string]any{"vm": "CAD"}, &list)
	if jobs, _ := list["jobs"].([]any); len(jobs) == 0 || !strings.Contains(fmt.Sprint(jobs), id) {
		t.Fatalf("list: %v", list)
	}

	// Cancel needs the write ownership the owner task holds since its vm_exec.
	callJSON(t, ctx, owner, "vm_exec", map[string]any{"vm": "CAD", "background": true, "command": "Start-Sleep -Seconds 60"}, &started)
	long := started["id"].(string)
	if refused := callRefused(t, ctx, other, "vm_job", map[string]any{"vm": "CAD", "job_id": long, "cancel": true}); refused["error"] != "vm_busy" {
		t.Fatalf("cancel without ownership: %v", refused)
	}
	var cancelled map[string]any
	callJSON(t, ctx, owner, "vm_job", map[string]any{"vm": "CAD", "job_id": long, "cancel": true}, &cancelled)
	if cancelled["state"] != "cancelled" {
		t.Fatalf("cancel: %v", cancelled)
	}

	for name, a := range map[string]map[string]any{
		"unknown job": {"vm": "CAD", "job_id": "job-000000000000"},
		"admin":       {"vm": "CAD", "background": true, "admin": true, "command": "hostname"},
		"wait":        {"vm": "CAD", "job_id": id, "wait_ms": 60001},
		"offset":      {"vm": "CAD", "job_id": id, "stdout_offset": 1 << 30},
		"cancel all":  {"vm": "CAD", "cancel": true},
	} {
		tool := "vm_job"
		if _, ok := a["command"]; ok {
			tool = "vm_exec"
		}
		refused := callRefused(t, ctx, owner, tool, a)
		want := "invalid_argument"
		if name == "unknown job" {
			want = "no_job"
		}
		if refused["error"] != want {
			t.Errorf("%s: %v, want %s", name, refused, want)
		}
	}
}

// vm_end_turn cancels its task's waiting vm_job like a vm_wait; the read then answers with the last state.
func TestJobWaitCancelledByEndTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, b := jobMCP(t, ctx)
	var started map[string]any
	callJSON(t, ctx, a, "vm_exec", map[string]any{"vm": "CAD", "task_id": "t", "background": true, "command": "Start-Sleep -Seconds 30"}, &started)
	id := started["id"].(string)
	type res struct {
		out map[string]any
		at  time.Duration
	}
	got := make(chan res, 1)
	begin := time.Now()
	go func() {
		var r map[string]any
		callJSON(t, ctx, a, "vm_job", map[string]any{"vm": "CAD", "task_id": "t", "job_id": id, "wait_ms": 30000}, &r)
		got <- res{r, time.Since(begin)}
	}()
	time.Sleep(1500 * time.Millisecond)
	var ended map[string]any
	callJSON(t, ctx, b, "vm_end_turn", map[string]any{"task_id": "t"}, &ended)
	r := <-got
	if r.out["state"] != "running" || r.at > 15*time.Second {
		t.Fatalf("waiting read after vm_end_turn: %v after %v", r.out, r.at)
	}
	if n, _ := ended["cancelled_waits"].(float64); n < 1 {
		t.Fatalf("vm_end_turn did not count the cancelled read: %v", ended)
	}
	var cancelled map[string]any
	callJSON(t, ctx, b, "vm_job", map[string]any{"vm": "CAD", "task_id": "u", "job_id": id, "cancel": true}, &cancelled)
}
