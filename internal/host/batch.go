package host

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxBatchSteps bounds one vm_batch call.
const maxBatchSteps = 64

// batchExcluded are the tools a batch step may not call: vm_end_turn waits for the task's in-flight calls, which
// include the batch itself, and batches do not nest.
var batchExcluded = map[string]bool{"vm_batch": true, "vm_end_turn": true}

// batchRef is a step argument string that is replaced by a value of an earlier step's result: "${<step>.<path>}".
var batchRef = regexp.MustCompile(`^\$\{(\d+)\.([^{}]+)\}$`)

type batchIn struct {
	VM    string      `json:"vm,omitempty"`
	Steps []batchStep `json:"steps" jsonschema:"Ordered steps (1 to 64). Each runs as if called directly with this vm and task_id; a step stops the batch when it fails or one of its assertions fails."`
}

type batchStep struct {
	Tool   string         `json:"tool" jsonschema:"Tool name, e.g. vm_launch, vm_observe, vm_type, vm_exec, vm_wait. vm_batch and vm_end_turn are not allowed."`
	Args   map[string]any `json:"args,omitempty" jsonschema:"The tool's arguments, without vm and task_id (the batch's are used). A string value that is exactly ${<step>.<path>} is replaced by that value of an earlier step's result, keeping its JSON type, e.g. \"handle\": \"${0.handle}\"; path is dot-separated keys and array indexes (windows.0.handle)."`
	Assert []batchAssert  `json:"assert,omitempty" jsonschema:"Checks on this step's result, in order; the first that fails stops the batch with assertion_failed (the step itself has run)."`
}

type batchAssert struct {
	Path     string  `json:"path" jsonschema:"Dot-separated keys and array indexes into the step's JSON result, e.g. exit_code, stdout, windows.0.title."`
	Equals   any     `json:"equals,omitempty" jsonschema:"The value must equal this JSON value (numbers compare by value)."`
	Contains *string `json:"contains,omitempty" jsonschema:"The value must be a string containing this text."`
	Exists   *bool   `json:"exists,omitempty" jsonschema:"true: the path must exist with a non-null value; false: it must be missing or null."`
}

// batchStepOut is one step's entry in the result: its JSON result (success or failed assertion) or its error object,
// the indexes of its images among the result's image items, and how many assertions passed.
type batchStepOut struct {
	Step       int            `json:"step"`
	Tool       string         `json:"tool"`
	OK         bool           `json:"ok"`
	Result     map[string]any `json:"result,omitempty"`
	Error      map[string]any `json:"error,omitempty"`
	Images     []int          `json:"images,omitempty"`
	Assertions int            `json:"assertions_passed"`
}

type batchOut struct {
	VM        string         `json:"vm"`
	Completed int            `json:"completed"`
	Steps     []batchStepOut `json:"steps"`
}

// registerBatch registers vm_batch. It runs every step through the step tool's own registered handler (see
// addToolIn), so arguments, VM selection, task ownership and errors behave exactly as in a direct call.
func registerBatch(d *deps) {
	addToolIn(d, toolSpec{name: "vm_batch", desc: "Run ordered tool steps in one call, e.g. vm_launch, vm_type into ${0.handle}, vm_wait, vm_observe. Each step runs exactly like a direct call with this vm and task_id; its result is checked against its optional assert list. The batch stops at the first step that fails or whose assertion fails, and never repeats a step. Success returns every step's result (images as image items, indexed by steps[i].images). A stop returns step_failed (steps[i].error is the step's own error object with its next) or assertion_failed (failed_assertion with the actual value), plus failed_step, last_completed (-1 if none) and the results so far; continue with a new vm_batch of the remaining steps after observing the state. Steps are not pre-validated beyond tool names, references and assertions: a step with invalid arguments fails when reached.", destructive: true}, func(ctx context.Context, in batchIn) (*mcp.CallToolResult, error) {
		if err := checkBatch(d, in); err != nil {
			return nil, err
		}
		var task *taskState
		if t, ok := ctx.Value(taskContextKey{}).(*taskState); ok {
			task = t
		}
		out := batchOut{VM: in.VM, Steps: []batchStepOut{}}
		var images []mcp.Content
		results := make([]map[string]any, len(in.Steps))
		for i, step := range in.Steps {
			entry := batchStepOut{Step: i, Tool: step.Tool}
			if err := context.Cause(ctx); ctx.Err() != nil {
				return batchStop(d, ctx, out, images, refuse(codeFailed, "", nil, "the batch was cancelled before step %d (%s): %v", i, step.Tool, err), i, nil)
			}
			args, err := resolveBatchRefs(step.Args, results)
			if err != nil {
				te := refuse(codeInvalidArgument, "", nil, "step %d (%s) was not run: %v", i, step.Tool, err)
				entry.Error = errorObject(te)
				out.Steps = append(out.Steps, entry)
				return batchStop(d, ctx, out, images, te, i, nil)
			}
			argMap, _ := args.(map[string]any)
			if argMap == nil {
				argMap = map[string]any{}
			}
			argMap["vm"] = in.VM
			if task != nil {
				argMap["task_id"] = task.id
			}
			raw, err := json.Marshal(argMap)
			if err != nil {
				return nil, err
			}
			r, err := d.handlers[step.Tool](ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: step.Tool, Arguments: raw}})
			if err != nil {
				r = errorResult(d.taskRunID(ctx), err)
			}
			obj := map[string]any{}
			for _, c := range r.Content {
				switch c := c.(type) {
				case *mcp.ImageContent:
					entry.Images = append(entry.Images, len(images))
					images = append(images, c)
				case *mcp.TextContent:
					var o map[string]any
					if json.Unmarshal([]byte(c.Text), &o) == nil && o != nil {
						obj = o
					}
				}
			}
			delete(obj, "task_id")
			delete(obj, "run_id")
			if r.IsError {
				entry.Error = obj
				out.Steps = append(out.Steps, entry)
				code, _ := obj["error"].(string)
				reason, _ := obj["reason"].(string)
				return batchStop(d, ctx, out, images, refuse(codeStepFailed, "", nil, "step %d (%s) failed with %s: %s", i, step.Tool, code, reason), i, nil)
			}
			entry.Result = obj
			results[i] = obj
			for _, a := range step.Assert {
				if failed := checkBatchAssert(a, obj); failed != nil {
					out.Steps = append(out.Steps, entry)
					return batchStop(d, ctx, out, images, refuse(codeAssertionFailed, "", nil, "step %d (%s) ran, but its result failed the assertion on %s: %s", i, step.Tool, a.Path, failed["message"]), i, failed)
				}
				entry.Assertions++
			}
			entry.OK = true
			out.Steps = append(out.Steps, entry)
			out.Completed++
		}
		r, err := jsonResult(out)
		if err != nil {
			return nil, err
		}
		r.Content = append(images, r.Content...)
		return r, nil
	})
}

// checkBatch refuses a batch whose structure is wrong before any step runs: step count, tool names, a vm or task_id
// in step arguments, malformed or forward references and assertions without exactly one condition.
func checkBatch(d *deps, in batchIn) error {
	bad := func(step int, next, format string, args ...any) error {
		return refuse(codeInvalidArgument, next, map[string]any{"step": step}, format, args...)
	}
	if len(in.Steps) == 0 || len(in.Steps) > maxBatchSteps {
		return refuse(codeInvalidArgument, fmt.Sprintf("pass 1 to %d steps; split longer work into several vm_batch calls", maxBatchSteps), nil, "vm_batch got %d steps", len(in.Steps))
	}
	for i, s := range in.Steps {
		if batchExcluded[s.Tool] {
			return bad(i, "call "+s.Tool+" on its own, outside the batch", "step %d: %s cannot run inside vm_batch", i, s.Tool)
		}
		if d.handlers[s.Tool] == nil {
			return bad(i, "use a tool name from the tool list", "step %d: unknown tool %q", i, s.Tool)
		}
		if v, ok := s.Args["vm"]; ok {
			if name, _ := v.(string); !strings.EqualFold(name, in.VM) {
				return bad(i, "omit vm in step args: every step runs on the batch's vm; use one vm_batch per VM", "step %d: args.vm %v differs from the batch vm %q", i, v, in.VM)
			}
		}
		if _, ok := s.Args["task_id"]; ok {
			return bad(i, "omit task_id in step args: every step runs in the batch's task", "step %d: args.task_id is not allowed", i)
		}
		if err := checkBatchRefs(s.Args, i); err != nil {
			return bad(i, "reference only earlier steps as ${<step>.<path>}, e.g. ${0.handle}", "step %d: %v", i, err)
		}
		for _, a := range s.Assert {
			n := 0
			if a.Equals != nil {
				n++
			}
			if a.Contains != nil {
				n++
			}
			if a.Exists != nil {
				n++
			}
			if strings.TrimSpace(a.Path) == "" || n != 1 {
				return bad(i, "give each assertion a path and exactly one of equals, contains or exists (to check for null use exists: false)", "step %d: invalid assertion %+v", i, a)
			}
		}
	}
	return nil
}

// checkBatchRefs checks that every reference in v names a step before step.
func checkBatchRefs(v any, step int) error {
	switch v := v.(type) {
	case string:
		if m := batchRef.FindStringSubmatch(v); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil || n >= step {
				return fmt.Errorf("reference %s does not name an earlier step", v)
			}
		}
	case map[string]any:
		for _, x := range v {
			if err := checkBatchRefs(x, step); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range v {
			if err := checkBatchRefs(x, step); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveBatchRefs returns a copy of v with every reference replaced by the referenced value of results.
func resolveBatchRefs(v any, results []map[string]any) (any, error) {
	switch v := v.(type) {
	case string:
		m := batchRef.FindStringSubmatch(v)
		if m == nil {
			return v, nil
		}
		n, _ := strconv.Atoi(m[1])
		x, ok := batchPath(results[n], m[2])
		if !ok || x == nil {
			return nil, fmt.Errorf("reference %s: step %d's result has no value at %s", v, n, m[2])
		}
		return x, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			r, err := resolveBatchRefs(x, results)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			r, err := resolveBatchRefs(x, results)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// batchPath looks up a dot-separated path of keys and array indexes in a decoded JSON value.
func batchPath(v any, path string) (any, bool) {
	for _, part := range strings.Split(path, ".") {
		switch c := v.(type) {
		case map[string]any:
			x, ok := c[part]
			if !ok {
				return nil, false
			}
			v = x
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			v = c[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// checkBatchAssert returns nil when result satisfies a, otherwise the failed_assertion object: the assertion, the
// actual value (null when missing) and a message.
func checkBatchAssert(a batchAssert, result map[string]any) map[string]any {
	actual, found := batchPath(result, a.Path)
	failed := func(format string, args ...any) map[string]any {
		f := map[string]any{"path": a.Path, "actual": actual, "message": fmt.Sprintf(format, args...)}
		switch {
		case a.Equals != nil:
			f["equals"] = a.Equals
		case a.Contains != nil:
			f["contains"] = *a.Contains
		default:
			f["exists"] = *a.Exists
		}
		return f
	}
	switch {
	case a.Exists != nil:
		if present := found && actual != nil; present != *a.Exists {
			if *a.Exists {
				return failed("expected a value, found none")
			}
			return failed("expected no value, found %s", jsonText(actual))
		}
	case a.Contains != nil:
		s, ok := actual.(string)
		if !ok {
			return failed("expected a string containing %q, found %s", *a.Contains, jsonText(actual))
		}
		if !strings.Contains(s, *a.Contains) {
			return failed("expected a string containing %q, found %q", *a.Contains, s)
		}
	default:
		if !found || !jsonEqual(actual, a.Equals) {
			return failed("expected %s, found %s", jsonText(a.Equals), jsonText(actual))
		}
	}
	return nil
}

// jsonEqual compares two values by their JSON meaning (numbers by value, whatever their Go type).
func jsonEqual(a, b any) bool {
	var x, y any
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if json.Unmarshal(ab, &x) != nil || json.Unmarshal(bb, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func jsonText(v any) string {
	if v == nil {
		return "null"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// errorObject is the JSON error object of a toolError, as errorResult renders it, without run_id.
func errorObject(te *toolError) map[string]any {
	obj := map[string]any{"error": te.Code, "reason": te.Reason}
	if te.Next != "" {
		obj["next"] = te.Next
	}
	for k, v := range te.Fields {
		obj[k] = v
	}
	return obj
}

// batchStop ends a batch at step failed: te (step_failed, assertion_failed, a reference or cancellation error) gets
// its next step, the steps so far and the stop position as fields, and is rendered as an error result preceded by the
// images collected so far.
func batchStop(d *deps, ctx context.Context, out batchOut, images []mcp.Content, te *toolError, failed int, assertion map[string]any) (*mcp.CallToolResult, error) {
	last := failed - 1
	done := "no step completed"
	if last >= 0 {
		done = fmt.Sprintf("steps 0-%d completed and are not repeated", last)
	}
	switch te.Code {
	case codeAssertionFailed:
		te.Next = fmt.Sprintf("%s; step %d ran, so its effects happened: observe the current state, then call vm_batch again with only the remaining steps that still apply", done, failed)
	case codeStepFailed:
		te.Next = fmt.Sprintf("%s; follow steps[%d].error.next, then call vm_batch again with only the remaining steps (from step %d) that still apply", done, failed, failed)
	default:
		te.Next = fmt.Sprintf("%s; fix the cause, then call vm_batch again with only the remaining steps (from step %d)", done, failed)
	}
	te.Fields = map[string]any{"vm": out.VM, "failed_step": failed, "last_completed": last, "completed": out.Completed, "steps": out.Steps}
	if assertion != nil {
		te.Fields["failed_assertion"] = assertion
	}
	r := errorResult(d.taskRunID(ctx), te)
	r.Content = append(images, r.Content...)
	return r, nil
}
