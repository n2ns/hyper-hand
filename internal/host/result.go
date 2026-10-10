package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Error codes of toolError. Every refusal a tool makes has one of these; "failed" is for errors without a specific
// code (transport, Hyper-V, unexpected agent errors).
const (
	codeFailed              = "failed"
	codeInvalidArgument     = "invalid_argument"
	codeStaleObservation    = "stale_observation"  // the observation's window moved, resized, minimized or closed
	codeStaleElement        = "stale_element"      // the control index/runtime ID no longer resolves
	codeSessionUnusable     = "session_unusable"   // locked, secure desktop or non-console session
	codeActivateFailed      = "activate_failed"    // the target could not be brought to the foreground
	codeTargetDisabled      = "target_disabled"    // the target window is disabled (modal dialog or other blocker)
	codeCovered             = "covered"            // another window covers the point
	codeIntegrityMismatch   = "integrity_mismatch" // the target runs at a higher integrity level than the agent
	codeTargetNotResponding = "target_not_responding"
	codeUnsupportedPattern  = "unsupported_pattern" // the control does not support the requested action
	codePartialInput        = "partial_input"       // some input was injected before the failure
	codeAgentRequired       = "agent_required"      // the operation needs the guest agent, which is not reachable
	codeAgentOutdated       = "agent_outdated"      // the agent's protocol is older than proto.Protocol
	codeAmbiguousTarget     = "ambiguous_target"    // a selector matched several windows
	codeNoWindow            = "no_window"           // no window matched, or none appeared
	codeNoCheckpoint        = "no_checkpoint"       // no checkpoint has the given id or name
)

// toolError is a structured refusal: Code is one of the code constants, Reason says what happened, Next names the
// tool call that makes progress, and Fields carries the facts the caller needs for it (handles, classes, processes).
// It is returned to the MCP client as an isError result whose single text item is the JSON object
// {"error": Code, "reason": Reason, "next": Next, "run_id": ..., <Fields>...}.
type toolError struct {
	Code   string
	Reason string
	Next   string
	Fields map[string]any
}

func (e *toolError) Error() string {
	if e.Next == "" {
		return e.Code + ": " + e.Reason
	}
	return e.Code + ": " + e.Reason + "; " + e.Next
}

// refuse builds a toolError. fields may be nil.
func refuse(code, next string, fields map[string]any, format string, args ...any) *toolError {
	return &toolError{Code: code, Reason: fmt.Sprintf(format, args...), Next: next, Fields: fields}
}

// asToolError returns err as a toolError, wrapping other errors as "failed" (agent "unknown op" errors become
// agent_outdated, since no supported agent lacks an op the host sends).
func asToolError(err error) *toolError {
	var te *toolError
	if errors.As(err, &te) {
		return te
	}
	if strings.HasPrefix(err.Error(), "unknown op") {
		return refuse(codeAgentOutdated, "call vm_update_agent", nil, "the guest agent is too old: %v", err)
	}
	return &toolError{Code: codeFailed, Reason: err.Error(), Next: "call vm_status, then vm_doctor if the VM is running; the action may or may not have happened, so observe before repeating it"}
}

// errorResult renders a toolError as the isError MCP result described on toolError.
func errorResult(runID string, err error) *mcp.CallToolResult {
	te := asToolError(err)
	obj := map[string]any{"error": te.Code, "reason": te.Reason}
	if te.Next != "" {
		obj["next"] = te.Next
	}
	if runID != "" {
		obj["run_id"] = runID
	}
	for k, v := range te.Fields {
		obj[k] = v
	}
	b, _ := json.Marshal(obj)
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

// jsonResult renders v as a tool result with one JSON text item.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

// jsonImageResult renders v as a JSON text item preceded by a PNG image item (omitted when png is nil).
func jsonImageResult(v any, png []byte) (*mcp.CallToolResult, error) {
	r, err := jsonResult(v)
	if err != nil {
		return nil, err
	}
	if png != nil {
		r.Content = append([]mcp.Content{&mcp.ImageContent{Data: png, MIMEType: "image/png"}}, r.Content...)
	}
	return r, nil
}

// toolSpec describes a tool for registration: its MCP annotations are hints for clients, not a security boundary.
type toolSpec struct {
	name        string
	desc        string
	readOnly    bool // ReadOnlyHint
	destructive bool // DestructiveHint (meaningful when !readOnly)
	idempotent  bool // IdempotentHint
}

// addToolIn registers a tool with typed input In on d's server. Handler errors become structured isError results
// (see toolError), never MCP protocol errors, so the client always gets the JSON object.
func addToolIn[In any](d *deps, spec toolSpec, f func(context.Context, In) (*mcp.CallToolResult, error)) {
	destructive, openWorld := spec.destructive, false
	tool := &mcp.Tool{Name: spec.name, Description: spec.desc, Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint: spec.readOnly, DestructiveHint: &destructive, IdempotentHint: spec.idempotent, OpenWorldHint: &openWorld,
	}}
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	if schema.Properties == nil {
		schema.Properties = map[string]*jsonschema.Schema{}
	}
	schema.Properties["task_id"] = &jsonschema.Schema{Type: "string", Description: "Unique AI task identifier. Reuse on every call when sharing a session or reconnecting; omit only for a dedicated persistent MCP session. vm_end_turn without vm ends this task; use a new ID afterwards."}
	// vm is required everywhere but vm_list and vm_end_turn (see vmRequired): a default VM let a call land on another
	// VM whenever the intended one was off.
	if p := schema.Properties["vm"]; p != nil {
		if spec.name == "vm_end_turn" {
			p.Description = "VM name from vm_list. Omit only to end the whole task (this task's own waits, temporary checkpoints and ownership on every VM); required with all_temp."
		} else {
			p.Description = "VM name from vm_list (required). There is no default VM."
			schema.Required = append(schema.Required, "vm")
		}
	}
	tool.InputSchema = schema
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		panic(err)
	}
	// The generic SDK wrapper returns plain text for schema errors before our
	// handler runs. Validate here so invalid arguments obey the same JSON contract.
	d.s.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in In
		inputErr := decodeToolArguments(req.Params.Arguments, resolved, &in)
		var args struct {
			VM string `json:"vm"`
		}
		var identity struct {
			TaskID string `json:"task_id"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &identity); err != nil && inputErr != nil {
				return errorResult("", invalidToolArguments(spec.name, inputErr)), nil
			}
			_ = json.Unmarshal(req.Params.Arguments, &args)
		}
		// Before anything else: a missing vm is refused with the VM names, not a generic schema error, and nothing runs.
		if err := vmRequired(d, spec.name, req.Params.Arguments, args.VM); err != nil {
			return errorResult("", err), nil
		}
		task, err := d.resolveTask(req, identity.TaskID, spec.name == "vm_end_turn")
		if err != nil {
			return errorResult("", err), nil
		}
		if task != nil {
			ctx = context.WithValue(ctx, taskContextKey{}, task)
		}
		if inputErr != nil {
			return taskResult(errorResult(d.taskRunID(ctx), invalidToolArguments(spec.name, inputErr)), task), nil
		}
		readOnly := spec.readOnly
		if spec.name == "vm_push" {
			var p pushIn
			_ = json.Unmarshal(req.Params.Arguments, &p)
			readOnly = p.Mode == "mirror" && (p.Phase == "" || p.Phase == "plan")
		}
		if task != nil {
			if spec.name != "vm_end_turn" {
				vm := args.VM
				if !readOnly || spec.name == "vm_wait" || spec.name == "vm_observe" || spec.name == "vm_push" {
					v, err := d.raw.Find(vm)
					if err != nil {
						switch spec.name {
						case "vm_observe":
							err = refuse(codeFailed, "call vm_list and pass vm", nil, "%v", err)
						case "vm_click", "vm_drag", "vm_scroll", "vm_type", "vm_key", "vm_set_value", "vm_invoke":
							// Input actions have historically returned the raw lookup error.
						default:
							err = vmErr(err)
						}
						return taskResult(errorResult(task.runID, err), task), nil
					}
					vm = v.Name
					// Pin the selector passed to the handler too: resolving the default
					// again could send a write to a different VM than the lease protects.
					b, _ := json.Marshal(in)
					var pinned map[string]json.RawMessage
					_ = json.Unmarshal(b, &pinned)
					pinned["vm"], _ = json.Marshal(vm)
					b, _ = json.Marshal(pinned)
					_ = json.Unmarshal(b, &in)
				}
				var done func()
				ctx, done, err = task.enter(ctx, vm, spec.name == "vm_wait")
				if err != nil {
					return taskResult(errorResult(task.runID, err), task), nil
				}
				defer done()
				if !readOnly {
					if err := d.tasks.claim(task, vm); err != nil {
						return taskResult(errorResult(task.runID, err), task), nil
					}
					switch spec.name {
					case "vm_start", "vm_shutdown", "vm_turn_off", "vm_restore", "vm_unlock", "vm_install_agent", "vm_update_agent":
						defer d.beginExternalMutation(ctx, vm, true)()
					case "vm_exec", "vm_launch":
						defer d.beginExternalMutation(ctx, vm, false)()
					}
				}
			}
		}
		r, err := f(ctx, in)
		if err != nil {
			return taskResult(errorResult(d.taskRunID(ctx), err), task), nil
		}
		return taskResult(r, task), nil
	})
}

func invalidToolArguments(tool string, err error) error {
	return refuse(codeInvalidArgument, "check the "+tool+" input schema and pass the required fields with their declared types", nil, "invalid arguments for %s: %v", tool, err)
}

func decodeToolArguments(raw json.RawMessage, schema *jsonschema.Resolved, in any) error {
	var value any = map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
	}
	// SDK clients may encode omitted arguments as null. Preserve the generic
	// SDK's empty-object behavior for tools with no required fields.
	if value == nil {
		value = map[string]any{}
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("arguments must be a JSON object")
	}
	if err := schema.ApplyDefaults(&value); err != nil {
		return err
	}
	if err := schema.Validate(&value); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, in)
}

// vmRequired refuses a call without vm. Every tool except vm_list names its VM explicitly: with a default ("the only
// running VM") a call meant for a VM that happened to be off reached another one, including vm_restore, vm_shutdown,
// vm_turn_off and vm_exec. vm_end_turn may omit vm to end the whole task, which only touches the task's own resources;
// with all_temp, which deletes every run's temporary checkpoints, it must name the VM.
func vmRequired(d *deps, tool string, raw json.RawMessage, vm string) error {
	if tool == "vm_list" || strings.TrimSpace(vm) != "" {
		return nil
	}
	if tool == "vm_end_turn" {
		var in struct {
			AllTemp bool `json:"all_temp"`
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &in)
		}
		if !in.AllTemp {
			return nil
		}
	}
	names := vmNames(d.raw)
	next := "pass vm with the name of the VM to act on (call vm_list)"
	if len(names) > 0 {
		next = fmt.Sprintf("pass vm with one of: %s", strings.Join(names, ", "))
	}
	what := "vm is required: there is no default VM"
	if tool == "vm_end_turn" {
		what = "vm is required with all_temp: it would otherwise delete temporary checkpoints on every VM"
	}
	return refuse(codeInvalidArgument, next, map[string]any{"vms": names}, "%s", what)
}

// vmNames lists the VM names for an error's next step; empty when they cannot be listed (the refusal still stands).
func vmNames(b Backend) (names []string) {
	names = []string{}
	defer func() {
		if recover() != nil { // a test fake without a VM list
			names = []string{}
		}
	}()
	if vms, err := b.ListVMs(); err == nil {
		for _, v := range vms {
			names = append(names, v.Name)
		}
	}
	return names
}
