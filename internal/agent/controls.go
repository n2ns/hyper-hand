package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hyperhand/internal/proto"
)

const controlsHelperRole = "--hyperhand-controls-helper"
const controlsOutputLimit = 16 << 20
const controlsInputLimit = 1 << 20

// uiaTimeout bounds one UI Automation helper run; focusedTimeout bounds the focused-element lookup of list_windows.
const uiaTimeout = 10 * time.Second
const focusedTimeout = 2 * time.Second

func controlsArgs(a proto.ControlsArgs) (proto.ControlsArgs, error) {
	if a.Handle == 0 || a.PID == 0 {
		return a, errors.New("controls require a nonzero window handle and PID")
	}
	if a.MaxDepth == 0 {
		a.MaxDepth = 4
	}
	if a.MaxNodes == 0 {
		a.MaxNodes = 200
	}
	if a.MaxDepth < 1 || a.MaxDepth > 10 {
		return a, errors.New("max_depth must be between 1 and 10")
	}
	if a.MaxNodes < 1 || a.MaxNodes > 1000 {
		return a, errors.New("max_nodes must be between 1 and 1000")
	}
	return a, nil
}

func listControls(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.ControlsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, nil, err
	}
	if a.RootRuntimeID != "" {
		return nil, nil, errors.New("scoped controls require list_control_subtree")
	}
	return listControlsArgs(ctx, a)
}

func listControlsArgs(ctx context.Context, a proto.ControlsArgs) (any, []byte, error) {
	a, err := controlsArgs(a)
	if err != nil {
		return nil, nil, err
	}
	opctx, cancel := context.WithTimeout(ctx, uiaTimeout)
	defer cancel()
	cmd, err := helperCommand(opctx)
	if err != nil {
		return nil, nil, err
	}
	r, err := runControlsCommand(opctx, cmd, a)
	return r, nil, err
}

func controlAction(ctx context.Context, args json.RawMessage, _ []byte) (any, []byte, error) {
	var a proto.ControlActionArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, nil, err
	}
	if _, err := controlsArgs(proto.ControlsArgs{Handle: a.Handle, PID: a.PID}); err != nil {
		return nil, nil, err
	}
	if a.RuntimeID == "" {
		return nil, nil, errors.New("control_action requires a runtime_id")
	}
	if _, err := parseAction(a.Action); err != nil {
		return nil, nil, err
	}
	if strings.ContainsRune(a.Value, 0) {
		return nil, nil, errors.New("control_action value must not contain NUL")
	}
	opctx, cancel := context.WithTimeout(ctx, uiaTimeout)
	defer cancel()
	cmd, err := helperCommand(opctx)
	if err != nil {
		return nil, nil, err
	}
	reply, err := runHelper(opctx, cmd, helperRequest{Action: &a})
	if err != nil {
		return nil, nil, err
	}
	if reply.Action == nil {
		return nil, nil, errors.New("UI Automation helper returned no action result")
	}
	return *reply.Action, nil, nil
}

// focusedControl asks a disposable helper for the UIA focused element; nil when there is none or the helper fails or
// times out, so list_windows never blocks on a broken provider.
func focusedControl(ctx context.Context) *proto.FocusedControl {
	opctx, cancel := context.WithTimeout(ctx, focusedTimeout)
	defer cancel()
	cmd, err := helperCommand(opctx)
	if err != nil {
		return nil
	}
	reply, err := runHelper(opctx, cmd, helperRequest{Focused: true})
	if err != nil {
		return nil
	}
	return reply.Focused
}

func helperCommand(ctx context.Context) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, exe, controlsHelperRole), nil
}

// helperRequest is what the agent sends to the UIA helper on stdin; exactly one field is set.
type helperRequest struct {
	Controls *proto.ControlsArgs      `json:"controls,omitempty"`
	Find     *proto.FindControlsArgs  `json:"find,omitempty"`
	Action   *proto.ControlActionArgs `json:"action,omitempty"`
	Focused  bool                     `json:"focused,omitempty"`
}

type helperReply struct {
	Controls *proto.ControlsResult      `json:"controls,omitempty"`
	Find     *proto.FindControlsResult  `json:"find,omitempty"`
	Action   *proto.ControlActionResult `json:"action,omitempty"`
	Focused  *proto.FocusedControl      `json:"focused,omitempty"`
	Error    string                     `json:"error,omitempty"`
}

func runControlsCommand(ctx context.Context, cmd *exec.Cmd, a proto.ControlsArgs) (proto.ControlsResult, error) {
	reply, err := runHelper(ctx, cmd, helperRequest{Controls: &a})
	if err != nil {
		return proto.ControlsResult{}, err
	}
	if reply.Controls == nil {
		return proto.ControlsResult{}, errors.New("UI Automation helper returned no controls")
	}
	return *reply.Controls, nil
}

// A broken provider can hang even during COM Release. Only the disposable process calls UIA.
func runHelper(ctx context.Context, cmd *exec.Cmd, req helperRequest) (helperReply, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return helperReply{}, err
	}
	cmd.Stdin = bytes.NewReader(data)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.WaitDelay = time.Second
	var out controlsBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return helperReply{}, fmt.Errorf("UI Automation interrupted: %w", ctx.Err())
		}
		return helperReply{}, fmt.Errorf("UI Automation helper: %w", err)
	}
	var reply helperReply
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		return helperReply{}, fmt.Errorf("UI Automation helper response: %w", err)
	}
	if reply.Error != "" {
		return helperReply{}, errors.New(reply.Error)
	}
	return reply, nil
}

type controlsBuffer struct{ buffer bytes.Buffer }

func (b *controlsBuffer) Bytes() []byte { return b.buffer.Bytes() }
func (b *controlsBuffer) Write(p []byte) (int, error) {
	if len(p) > controlsOutputLimit-b.buffer.Len() {
		return 0, errors.New("UI Automation output exceeds limit")
	}
	return b.buffer.Write(p)
}

// RunControlsHelper is a private process role, handled before the tray and its single-instance mutex.
func RunControlsHelper(args []string) (bool, error) {
	if len(args) == 0 || args[0] != controlsHelperRole {
		return false, nil
	}
	if len(args) != 1 {
		return true, errors.New("invalid controls helper arguments")
	}
	var req helperRequest
	if err := json.NewDecoder(io.LimitReader(os.Stdin, controlsInputLimit)).Decode(&req); err != nil {
		return true, err
	}
	var reply helperReply
	var err error
	switch {
	case req.Find != nil:
		var a proto.FindControlsArgs
		a, err = findControlsArgs(*req.Find)
		if err == nil {
			var r proto.FindControlsResult
			r, err = collectFindControls(a)
			reply.Find = &r
		}
	case req.Controls != nil:
		var a proto.ControlsArgs
		a, err = controlsArgs(*req.Controls)
		if err == nil {
			var r proto.ControlsResult
			r, err = collectControls(a)
			reply.Controls = &r
		}
	case req.Action != nil:
		var r proto.ControlActionResult
		r, err = performControlAction(*req.Action)
		reply.Action = &r
	case req.Focused:
		reply.Focused, err = collectFocused()
	default:
		err = errors.New("empty UI Automation helper request")
	}
	if err != nil {
		reply = helperReply{Error: err.Error()}
	}
	return true, json.NewEncoder(os.Stdout).Encode(reply)
}

type controlElement interface {
	info() (proto.ControlInfo, bool, bool, error) // node, password, text truncated
	first() (controlElement, error)
	next() (controlElement, error)
	release()
}

func walkControls(root controlElement, a proto.ControlsArgs) (proto.ControlsResult, error) {
	r := proto.ControlsResult{Nodes: []proto.ControlInfo{}, Focused: -1}
	truncate := func(reason string) {
		r.Truncated = true
		if !slices.Contains(r.Truncation, reason) {
			r.Truncation = append(r.Truncation, reason)
		}
	}
	var visit func(controlElement, int, int) error
	visit = func(e controlElement, parent, depth int) error {
		n, password, textCut, err := e.info()
		if err != nil {
			return err
		}
		if textCut {
			truncate("text_length")
		}
		n.Index, n.Parent, n.Depth = len(r.Nodes), parent, depth
		if n.Focused && r.Focused < 0 {
			r.Focused = n.Index
		}
		r.Nodes = append(r.Nodes, n)
		if password {
			truncate("password_subtree")
			return nil
		}
		child, err := e.first()
		if err != nil {
			return err
		}
		if child == nil {
			return nil
		}
		if depth == a.MaxDepth {
			child.release()
			truncate("max_depth")
			return nil
		}
		for child != nil {
			if len(r.Nodes) == a.MaxNodes {
				child.release()
				truncate("max_nodes")
				return nil
			}
			if err := visit(child, n.Index, depth+1); err != nil {
				child.release()
				return err
			}
			next, err := child.next()
			child.release()
			if err != nil {
				return err
			}
			child = next
		}
		return nil
	}
	if err := visit(root, -1, 0); err != nil {
		return proto.ControlsResult{}, err
	}
	return r, nil
}

// formatRuntimeID renders IUIAutomationElement::GetRuntimeId's integers as "42.1234.5".
func formatRuntimeID(ids []int32) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(int64(id), 10)
	}
	return strings.Join(parts, ".")
}

// uiaPattern is a UIA pattern the agent exposes: its UIA_Is*PatternAvailablePropertyId, its PATTERNID, the names
// ControlInfo.Patterns lists and the control_action Action names it provides.
type uiaPattern struct {
	available int32
	id        int32
	names     []string
	actions   []string
}

var uiaPatterns = []uiaPattern{
	{30031, 10000, []string{"Invoke"}, []string{"Invoke"}},
	{30041, 10015, []string{"Toggle"}, []string{"Toggle"}},
	{30028, 10005, []string{"Expand", "Collapse"}, []string{"Expand", "Collapse"}},
	{30036, 10010, []string{"Select"}, []string{"Select"}},
	{30043, 10002, []string{"Value"}, []string{"SetValue"}},
	{30035, 10017, []string{"ScrollItem"}, []string{"ScrollIntoView"}},
	{30034, 10004, []string{"Scroll"}, []string{"ScrollUp", "ScrollDown", "ScrollLeft", "ScrollRight"}},
}

// patternNames lists ControlInfo.Patterns for the available patterns (indexes into uiaPatterns).
func patternNames(available []bool) []string {
	var names []string
	for i, p := range uiaPatterns {
		if available[i] {
			names = append(names, p.names...)
		}
	}
	return names
}

// actionNames lists the control_action Action names the available patterns provide.
func actionNames(available []bool) []string {
	var names []string
	for i, p := range uiaPatterns {
		if available[i] {
			names = append(names, p.actions...)
		}
	}
	return names
}

// actionNamesForState omits directions only when UIA explicitly says that axis
// cannot scroll. Unknown state still advertises the provider's ScrollPattern.
func actionNamesForState(available []bool, state *proto.ControlState) []string {
	actions := actionNames(available)
	return slices.DeleteFunc(actions, func(action string) bool { return !scrollAxisSupported(action, state) })
}

func scrollAxisSupported(action string, state *proto.ControlState) bool {
	if state == nil {
		return true
	}
	switch action {
	case "ScrollUp", "ScrollDown":
		return state.VerticallyScrollable == nil || *state.VerticallyScrollable
	case "ScrollLeft", "ScrollRight":
		return state.HorizontallyScrollable == nil || *state.HorizontallyScrollable
	default:
		return true
	}
}

// parseAction returns the canonical spelling of a control_action Action (case-insensitive).
func parseAction(action string) (string, error) {
	for _, a := range proto.ControlActions {
		if strings.EqualFold(a, action) {
			return a, nil
		}
	}
	return "", fmt.Errorf("unknown action %q; expected one of %s", action, strings.Join(proto.ControlActions, ", "))
}

// patternFor returns the index into uiaPatterns of the pattern that provides a canonical action.
func patternFor(action string) int {
	for i, p := range uiaPatterns {
		if slices.Contains(p.actions, action) {
			return i
		}
	}
	return -1
}

var errElementNotFound = errors.New("element not found")

// unsupportedPattern is the error the host maps to unsupported_pattern; supported lists this element's action names.
func unsupportedPattern(action string, supported []string) error {
	return fmt.Errorf("unsupported pattern: %s; supported: %s", action, strings.Join(supported, ", "))
}
