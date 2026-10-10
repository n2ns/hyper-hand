package host

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// eventually retries f until it returns true or ctx ends: a deleted session is released asynchronously.
func eventually(t *testing.T, ctx context.Context, what string, f func() bool) {
	t.Helper()
	for !f() {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

// A client that opens and deletes one MCP session per call (like Example's Invoke-HyperHand.ps1) gets a new
// default task each time; deleting the session ends that task and releases its VM, so the next call can write.
func TestTaskHTTPDeletedSessionReleasesDefaultTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	endpoint, b := taskHTTPServer(t, false)
	first := connectTaskHTTP(t, ctx, endpoint)
	var out map[string]any
	callJSON(t, ctx, first, "vm_key", map[string]any{"vm": "CAD", "keys": "enter"}, &out)
	firstTask := out["task_id"]
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := connectTaskHTTP(t, ctx, endpoint)
	eventually(t, ctx, "the deleted session's task released CAD", func() bool {
		r, err := second.CallTool(ctx, &mcp.CallToolParams{Name: "vm_key", Arguments: map[string]any{"vm": "CAD", "keys": "enter"}})
		return err == nil && !r.IsError
	})
	if b.keys.Load() != 2 {
		t.Fatalf("keys %d, want 2", b.keys.Load())
	}
	// The ended task's ID is not reachable any more; using it explicitly starts a fresh task, not the old ownership.
	refused := callRefused(t, ctx, connectTaskHTTP(t, ctx, endpoint), "vm_key", map[string]any{"task_id": firstTask, "vm": "CAD", "keys": "enter"})
	if refused["error"] != "vm_busy" || refused["owner_task_id"] == firstTask {
		t.Fatalf("old default task kept or regained ownership: %v", refused)
	}
}

// An explicit task_id keeps its ownership after the session that used it is deleted.
func TestTaskHTTPDeletedSessionKeepsExplicitTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	endpoint, b := taskHTTPServer(t, false)
	first := connectTaskHTTP(t, ctx, endpoint)
	var out map[string]any
	callJSON(t, ctx, first, "vm_key", map[string]any{"task_id": "script", "vm": "CAD", "keys": "enter"}, &out)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	other := connectTaskHTTP(t, ctx, endpoint)
	refused := callRefused(t, ctx, other, "vm_key", map[string]any{"vm": "CAD", "keys": "enter"})
	if refused["error"] != "vm_busy" || refused["owner_task_id"] != "script" || refused["owner_in_flight"] != float64(0) {
		t.Fatalf("explicit task lost ownership or busy facts are missing: %v", refused)
	}
	if idle, ok := refused["owner_idle_ms"].(float64); !ok || idle < 0 {
		t.Fatalf("owner_idle_ms: %v", refused)
	}
	callJSON(t, ctx, other, "vm_key", map[string]any{"task_id": "script", "vm": "CAD", "keys": "enter"}, &out)
	if b.keys.Load() != 2 {
		t.Fatalf("keys %d, want 2", b.keys.Load())
	}
}

// A default task whose session is deleted while one of its calls runs keeps its ownership until that call returns.
func TestTaskHTTPDeletedSessionWaitsForCallInFlight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	endpoint, b := taskHTTPServer(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	b.press = func() {
		if b.keys.Load() == 1 {
			close(entered)
			<-release
		}
	}
	first := connectTaskHTTP(t, ctx, endpoint)
	written := make(chan error, 1)
	go func() {
		_, err := first.CallTool(ctx, &mcp.CallToolParams{Name: "vm_key", Arguments: map[string]any{"vm": "CAD", "keys": "enter"}})
		written <- err
	}()
	<-entered
	other := connectTaskHTTP(t, ctx, endpoint)
	refused := callRefused(t, ctx, other, "vm_key", map[string]any{"vm": "CAD", "keys": "enter"})
	if refused["error"] != "vm_busy" || refused["owner_in_flight"] != float64(1) || refused["owner_idle_ms"] != float64(0) {
		t.Fatalf("busy facts during the write: %v", refused)
	}
	var status map[string]any
	callJSON(t, ctx, other, "vm_status", map[string]any{"vm": "CAD"}, &status)
	owner, _ := status["owner"].(map[string]any)
	if owner == nil || owner["task_id"] != refused["owner_task_id"] || owner["in_flight"] != float64(1) || owner["this_task"] != false {
		t.Fatalf("vm_status owner: %v", status)
	}
	closed := make(chan error, 1)
	go func() { closed <- first.Close() }()
	time.Sleep(100 * time.Millisecond)
	refused = callRefused(t, ctx, other, "vm_key", map[string]any{"vm": "CAD", "keys": "enter"})
	if refused["error"] != "vm_busy" {
		t.Fatalf("ownership released while a call was in flight: %v", refused)
	}
	unblock()
	<-written
	<-closed
	eventually(t, ctx, "the task released CAD after its call", func() bool {
		r, err := other.CallTool(ctx, &mcp.CallToolParams{Name: "vm_key", Arguments: map[string]any{"vm": "CAD", "keys": "enter"}})
		return err == nil && !r.IsError
	})
	callJSON(t, ctx, other, "vm_status", map[string]any{"vm": "CAD"}, &status)
	if owner, _ := status["owner"].(map[string]any); owner == nil || owner["this_task"] != true || owner["in_flight"] != float64(1) {
		t.Fatalf("vm_status owner after taking over: %v", status)
	}
}

// vm_end_turn that reaches beginEnd after the task was abandoned (ended by its deleted session) does nothing, so it
// cannot reserve VMs for a forgotten task (all_temp).
func TestBeginEndRefusesEndedTask(t *testing.T) {
	task := &taskState{id: "x", active: map[int]taskCall{}, changed: make(chan struct{}), ended: true}
	if _, err := task.beginEnd(context.Background(), ""); err != errTaskEnded {
		t.Fatalf("err = %v, want errTaskEnded", err)
	}
	if task.closing {
		t.Fatal("an ended task entered cleanup")
	}
}
