package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

// execRecorder is a fake agent that answers exec with stdout "out:<command>" and records the commands; a command
// starting with "fail" is refused by the agent.
type execRecorder struct {
	mu       sync.Mutex
	commands []string
}

func (e *execRecorder) respond(req proto.Request) (any, error) {
	if req.Op != proto.OpExec {
		return nil, errors.New("unexpected op " + req.Op)
	}
	var a proto.ExecArgs
	_ = json.Unmarshal(req.Args, &a)
	e.mu.Lock()
	e.commands = append(e.commands, a.Command)
	e.mu.Unlock()
	if strings.HasPrefix(a.Command, "fail") {
		return nil, errors.New("command refused")
	}
	return proto.ExecResult{ExitCode: 0, Stdout: "out:" + a.Command}, nil
}

func (e *execRecorder) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.commands...)
}

func TestBatchRunsStepsWithReferencesAndAssertions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec := &execRecorder{}
	cs, _ := connectTools(t, ctx, newFakeAgentBackend(rec.respond))
	var out batchOut
	callJSON(t, ctx, cs, "vm_batch", map[string]any{"steps": []any{
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "a"}, "assert": []any{
			map[string]any{"path": "exit_code", "equals": 0},
			map[string]any{"path": "stdout", "contains": "out:a"},
			map[string]any{"path": "stderr", "exists": true},
			map[string]any{"path": "missing", "exists": false},
		}},
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "${0.stdout}"}},
	}}, &out)
	if out.VM != "Win10" || out.Completed != 2 || len(out.Steps) != 2 || !out.Steps[0].OK || out.Steps[0].Assertions != 4 || !out.Steps[1].OK {
		t.Fatalf("batch %+v", out)
	}
	if got := rec.seen(); len(got) != 2 || got[0] != "a" || got[1] != "out:a" {
		t.Errorf("commands %q", got)
	}
	if out.Steps[1].Result["stdout"] != "out:out:a" || out.Steps[1].Result["task_id"] != nil {
		t.Errorf("step 1 result %v", out.Steps[1].Result)
	}
}

func TestBatchStopsAtFailedAssertion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec := &execRecorder{}
	cs, _ := connectTools(t, ctx, newFakeAgentBackend(rec.respond))
	e := callRefused(t, ctx, cs, "vm_batch", map[string]any{"steps": []any{
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "a"}},
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "b"}, "assert": []any{map[string]any{"path": "exit_code", "equals": 1}}},
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "c"}},
	}})
	if e["error"] != codeAssertionFailed || e["failed_step"] != float64(1) || e["last_completed"] != float64(0) || e["completed"] != float64(1) {
		t.Fatalf("refusal %v", e)
	}
	fa, _ := e["failed_assertion"].(map[string]any)
	if fa["path"] != "exit_code" || fa["equals"] != float64(1) || fa["actual"] != float64(0) {
		t.Errorf("failed_assertion %v", fa)
	}
	steps, _ := e["steps"].([]any)
	if len(steps) != 2 || steps[1].(map[string]any)["ok"] != false || steps[1].(map[string]any)["result"] == nil {
		t.Errorf("steps %v", steps)
	}
	if !strings.Contains(e["next"].(string), "steps 0-0 completed") {
		t.Errorf("next %q", e["next"])
	}
	if got := rec.seen(); len(got) != 2 {
		t.Errorf("commands %q: the step after the failed assertion ran", got)
	}
}

func TestBatchStopsAtFailedStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec := &execRecorder{}
	cs, _ := connectTools(t, ctx, newFakeAgentBackend(rec.respond))
	e := callRefused(t, ctx, cs, "vm_batch", map[string]any{"steps": []any{
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "fail now"}},
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "b"}},
	}})
	if e["error"] != codeStepFailed || e["failed_step"] != float64(0) || e["last_completed"] != float64(-1) || e["completed"] != float64(0) {
		t.Fatalf("refusal %v", e)
	}
	steps, _ := e["steps"].([]any)
	stepErr, _ := steps[0].(map[string]any)["error"].(map[string]any)
	if stepErr["error"] != codeFailed || !strings.Contains(stepErr["reason"].(string), "command refused") || stepErr["next"] == nil {
		t.Errorf("step error %v", stepErr)
	}
	if got := rec.seen(); len(got) != 1 {
		t.Errorf("commands %q: the step after the failure ran", got)
	}

	// A step whose arguments are invalid fails with the tool's own invalid_argument when reached.
	e = callRefused(t, ctx, cs, "vm_batch", map[string]any{"steps": []any{
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "c"}},
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": 5}},
	}})
	steps, _ = e["steps"].([]any)
	if e["error"] != codeStepFailed || e["failed_step"] != float64(1) || steps[1].(map[string]any)["error"].(map[string]any)["error"] != codeInvalidArgument {
		t.Errorf("invalid step refusal %v", e)
	}

	// A reference to a missing value fails its step before it runs.
	e = callRefused(t, ctx, cs, "vm_batch", map[string]any{"steps": []any{
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "d"}},
		map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "${0.nothing}"}},
	}})
	if e["error"] != codeInvalidArgument || e["failed_step"] != float64(1) || e["last_completed"] != float64(0) {
		t.Errorf("missing reference refusal %v", e)
	}
	if got := rec.seen(); len(got) != 3 || got[2] != "d" {
		t.Errorf("commands %q", got)
	}
}

func TestBatchRefusesBadStructureBeforeAnyStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec := &execRecorder{}
	cs, _ := connectTools(t, ctx, newFakeAgentBackend(rec.respond))
	ok := map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "a"}}
	for name, steps := range map[string][]any{
		"none":          {},
		"unknown tool":  {ok, map[string]any{"tool": "vm_nothing"}},
		"end turn":      {ok, map[string]any{"tool": "vm_end_turn"}},
		"nested":        {ok, map[string]any{"tool": "vm_batch", "args": map[string]any{"steps": []any{}}}},
		"other vm":      {ok, map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "a", "vm": "Win11"}}},
		"task id":       {ok, map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "a", "task_id": "x"}}},
		"forward ref":   {ok, map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "${1.stdout}"}}},
		"self ref":      {map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "${0.stdout}"}}},
		"two checks":    {map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "a"}, "assert": []any{map[string]any{"path": "stdout", "contains": "a", "exists": true}}}},
		"no check":      {map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "a"}, "assert": []any{map[string]any{"path": "stdout"}}}},
		"no path":       {map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "a"}, "assert": []any{map[string]any{"path": " ", "exists": true}}}},
		"ref in nested": {ok, map[string]any{"tool": "vm_launch", "args": map[string]any{"path": "x", "args": []any{"${5.pid}"}}}},
	} {
		e := callRefused(t, ctx, cs, "vm_batch", map[string]any{"steps": steps})
		if e["error"] != codeInvalidArgument || e["next"] == nil {
			t.Errorf("%s: %v", name, e)
		}
	}
	if got := rec.seen(); len(got) != 0 {
		t.Errorf("commands %q ran for refused batches", got)
	}
	// Without vm the batch is refused like any other tool.
	if e := callRefused(t, ctx, cs, "vm_batch", map[string]any{"vm": "", "steps": []any{ok}}); e["error"] != codeInvalidArgument {
		t.Errorf("no vm: %v", e)
	}
}

func TestBatchStepsUseTheBatchTaskForOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec := &execRecorder{}
	cs, d := connectTools(t, ctx, newFakeAgentBackend(rec.respond))
	d.tasks = newTaskRegistry()
	step := []any{map[string]any{"tool": "vm_exec", "args": map[string]any{"command": "a"}}}
	var out batchOut
	callJSON(t, ctx, cs, "vm_batch", map[string]any{"task_id": "task-one", "steps": step}, &out)
	if o := d.tasks.owner("Win10", nil); o == nil || o.TaskID != "task-one" {
		t.Fatalf("owner after the batch %+v", o)
	}
	e := callRefused(t, ctx, cs, "vm_batch", map[string]any{"task_id": "task-two", "steps": step})
	steps, _ := e["steps"].([]any)
	if e["error"] != codeStepFailed || steps[0].(map[string]any)["error"].(map[string]any)["error"] != "vm_busy" || e["task_id"] != "task-two" {
		t.Errorf("other task's batch %v", e)
	}
	if got := rec.seen(); len(got) != 1 {
		t.Errorf("commands %q", got)
	}
}

func TestBatchKeepsStepImages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, d := connectTools(t, ctx, newFakeAgentBackend((&execRecorder{}).respond))
	png := []byte("\x89PNG fake")
	addToolIn(d, toolSpec{name: "vm_fake_image", readOnly: true}, func(ctx context.Context, in vmIn) (*mcp.CallToolResult, error) {
		return jsonImageResult(map[string]any{"n": 1}, png)
	})
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_batch", Arguments: map[string]any{"vm": "Win10", "steps": []any{
		map[string]any{"tool": "vm_fake_image"},
		map[string]any{"tool": "vm_fake_image", "assert": []any{map[string]any{"path": "n", "equals": 2}}},
	}}})
	if err != nil || !r.IsError || len(r.Content) != 3 {
		t.Fatalf("batch %v %+v", err, r)
	}
	for i := 0; i < 2; i++ {
		if img, ok := r.Content[i].(*mcp.ImageContent); !ok || string(img.Data) != string(png) {
			t.Errorf("content %d %+v", i, r.Content[i])
		}
	}
	var e struct {
		Steps []batchStepOut `json:"steps"`
	}
	if err := json.Unmarshal([]byte(resultText(r)), &e); err != nil || len(e.Steps) != 2 || len(e.Steps[1].Images) != 1 || e.Steps[1].Images[0] != 1 {
		t.Errorf("steps %v %+v", err, e)
	}
}
