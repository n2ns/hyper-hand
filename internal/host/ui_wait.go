package host

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

const uiWaitCallTimeout = 12 * time.Second

func isUIWait(kind string) bool {
	switch kind {
	case "window_exists", "window_gone", "window_foreground", "control_exists", "control_gone", "control_matches":
		return true
	}
	return false
}

func hasUIWaitArguments(in waitIn) bool {
	return in.Handle != 0 || in.PID != 0 || in.ObservationID != "" || in.Index != nil || in.AutomationID != "" || in.ControlName != "" || in.Enabled != nil || in.Value != nil || in.State != nil || in.Assert || in.CheckOnly || in.MaxDepth != 0 || in.MaxNodes != 0
}

func validateUIWait(in waitIn) error {
	invalid := func(reason string) error {
		return refuse(codeInvalidArgument, "use a window handle/pid, or a control selector and the documented UI condition fields", nil, "%s", reason)
	}
	if in.TimeoutMs < 0 || in.TimeoutMs > 600000 {
		return invalid("UI timeout_ms must be 0 (default 60000) or 1..600000")
	}
	if in.Name != "" || in.Path != "" {
		return invalid("name and path are only for process and file conditions; control names use control_name")
	}
	if in.MaxDepth < 0 || in.MaxDepth > limitMaxDepth || in.MaxNodes < 0 || in.MaxNodes > limitMaxNodes {
		return invalid("max_depth must be 0..10 and max_nodes 0..1000; zero uses the vm_observe defaults")
	}
	control := strings.HasPrefix(in.Kind, "control_")
	if !control {
		if in.Handle == 0 && in.PID == 0 {
			return invalid("a window condition requires handle or pid")
		}
		if in.ObservationID != "" || in.Index != nil || in.AutomationID != "" || in.ControlName != "" || in.MaxDepth != 0 || in.MaxNodes != 0 {
			return invalid("window conditions do not accept control selectors or tree limits")
		}
	} else if in.ObservationID != "" || in.Index != nil {
		if in.ObservationID == "" || in.Index == nil || *in.Index < 0 {
			return invalid("observation_id and a nonnegative index must be supplied together")
		}
		if in.Handle != 0 || in.PID != 0 || in.AutomationID != "" || in.ControlName != "" {
			return invalid("observation_id/index and handle/pid/automation_id/control_name are alternative selectors")
		}
	} else if (in.Handle == 0 && in.PID == 0) || (in.AutomationID == "" && in.ControlName == "") {
		return invalid("a control condition requires observation_id/index, or handle/pid with automation_id or control_name")
	}
	if in.Kind != "control_matches" {
		if in.Enabled != nil || in.Value != nil || in.State != nil {
			return invalid("enabled, value and state apply only to control_matches")
		}
		return nil
	}
	state := uiStateFields(in.State)
	if in.Enabled == nil && in.Value == nil && len(state) == 0 {
		return invalid("control_matches requires at least one expected enabled, value or state field")
	}
	if in.State != nil {
		s := in.State
		if s.Toggle != nil && !slices.Contains([]string{"off", "on", "indeterminate"}, *s.Toggle) {
			return invalid("state.toggle must be off, on or indeterminate")
		}
		if s.ExpandCollapse != nil && !slices.Contains([]string{"collapsed", "expanded", "partially_expanded", "leaf"}, *s.ExpandCollapse) {
			return invalid("state.expand_collapse must be collapsed, expanded, partially_expanded or leaf")
		}
		for _, pct := range []*float64{s.HorizontalScrollPercent, s.VerticalScrollPercent} {
			if pct != nil && (math.IsNaN(*pct) || math.IsInf(*pct, 0) || *pct < 0 || *pct > 100) {
				return invalid("state scroll percentages must be in 0..100")
			}
		}
	}
	return nil
}

// uiStateFields preserves the distinction between missing fields and false/zero.
func uiStateFields(s *proto.ControlState) map[string]any {
	fields := map[string]any{}
	if s != nil {
		b, _ := json.Marshal(s)
		_ = json.Unmarshal(b, &fields)
	}
	return fields
}

type uiWaitTarget struct {
	selector windowSelector
	window   *proto.WindowInfo
	runtime  string
	epoch    uint64
}

func (d *deps) waitUI(ctx context.Context, in waitIn) (*mcp.CallToolResult, error) {
	if err := validateUIWait(in); err != nil {
		return nil, err
	}
	v, err := d.raw.Find(in.VM)
	if err != nil {
		return nil, vmErr(err)
	}
	in.VM = v.Name
	if in.TimeoutMs == 0 {
		in.TimeoutMs = 60000
	}
	if in.MaxDepth == 0 {
		in.MaxDepth = defaultMaxDepth
	}
	if in.MaxNodes == 0 {
		in.MaxNodes = defaultMaxNodes
	}
	store := d.taskObs(ctx)
	target := uiWaitTarget{selector: windowSelector{Handle: in.Handle, PID: in.PID}, epoch: store.version(in.VM).Epoch}
	if in.ObservationID != "" {
		o, err := store.get(in.ObservationID)
		if err != nil {
			return nil, err
		}
		if o.VM != in.VM || o.Epoch != target.epoch {
			return nil, refuse(codeStaleObservation, "call vm_observe again on this VM and use its observation_id", nil, "observation belongs to a different VM or predates a VM lifecycle change")
		}
		node, err := o.node(*in.Index)
		if err != nil {
			return nil, err
		}
		if o.treeWindow() == nil || node.RuntimeID == "" {
			return nil, refuse(codeInvalidArgument, "observe a window with controls: true and select a control with a runtime ID, or use handle/pid with automation_id/control_name", nil, "the observation has no stable control/window identity")
		}
		w := *o.treeWindow()
		target.window, target.runtime = &w, node.RuntimeID
		target.selector = windowSelector{Handle: w.Handle, PID: w.PID}
	}
	start := time.Now()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer d.taskTurn(ctx).addVMWait(in.VM, cancel)()
	timeout := time.Duration(in.TimeoutMs) * time.Millisecond
	if in.CheckOnly {
		timeout = min(timeout, uiWaitCallTimeout)
	}
	deadline, stop := context.WithTimeout(wctx, timeout)
	defer stop()
	last := map[string]any{"unknown": "not sampled"}
	finish := func(satisfied bool) (*mcp.CallToolResult, error) {
		out := map[string]any{"satisfied": satisfied, "elapsed_ms": time.Since(start).Milliseconds(), "last": last}
		if in.Assert && !satisfied {
			return nil, refuse("assertion_failed", "inspect last, update the target or expected state, and retry", out, "UI condition %s was not satisfied", in.Kind)
		}
		return jsonResult(out)
	}
	for {
		if deadline.Err() != nil && wctx.Err() == nil {
			return finish(false)
		}
		satisfied, sample, err := d.sampleUIWait(deadline, in, &target)
		last = sample
		if err != nil {
			if errors.Is(context.Cause(ctx), errTaskEnd) || wctx.Err() != nil && ctx.Err() == nil {
				return nil, refuse(codeFailed, "", map[string]any{"elapsed_ms": time.Since(start).Milliseconds(), "last": last}, "the wait was cancelled by vm_end_turn")
			}
			te := asToolError(err)
			fields := map[string]any{"satisfied": false, "elapsed_ms": time.Since(start).Milliseconds(), "last": last}
			for key, value := range te.Fields {
				fields[key] = value
			}
			return nil, &toolError{Code: te.Code, Reason: te.Reason, Next: te.Next, Fields: fields}
		}
		if satisfied || in.CheckOnly {
			return finish(satisfied)
		}
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-deadline.Done():
			timer.Stop()
			if wctx.Err() != nil {
				if errors.Is(context.Cause(ctx), errTaskEnd) || ctx.Err() == nil {
					return nil, refuse(codeFailed, "", map[string]any{"elapsed_ms": time.Since(start).Milliseconds(), "last": last}, "the wait was cancelled by vm_end_turn")
				}
				return nil, ctx.Err()
			}
			return finish(false)
		case <-timer.C:
		}
	}
}

// Calls release the agent connection between samples; waiting never holds d.input.
func (d *deps) uiWaitCall(ctx context.Context, vm, op string, args, out any) error {
	cctx, cancel := context.WithTimeout(ctx, uiWaitCallTimeout)
	defer cancel()
	_, err := d.call(cctx, vm, op, args, nil, out)
	if err == nil {
		err = cctx.Err()
	}
	if err != nil {
		return agentErr(err)
	}
	return nil
}

func (d *deps) sampleUIWait(ctx context.Context, in waitIn, target *uiWaitTarget) (bool, map[string]any, error) {
	last := map[string]any{}
	stale := func() error {
		return refuse(codeStaleObservation, "call vm_observe or vm_windows again and restart the wait", map[string]any{"last": last}, "the VM lifecycle or selected window identity changed during the wait")
	}
	checkEpoch := func() error {
		if d.taskObs(ctx).version(in.VM).Epoch != target.epoch {
			return stale()
		}
		return nil
	}
	readWindows := func() (*proto.WindowsResult, error) {
		if err := checkEpoch(); err != nil {
			return nil, err
		}
		var ws proto.WindowsResult
		if err := d.uiWaitCall(ctx, in.VM, proto.OpListWindows, nil, &ws); err != nil {
			return nil, err
		}
		if err := checkEpoch(); err != nil {
			return nil, err
		}
		if err := (&action{ws: &ws}).checkSession(true); err != nil {
			return nil, err
		}
		return &ws, nil
	}
	ws, err := readWindows()
	if err != nil {
		last["unknown"] = "window/session read failed"
		return false, last, err
	}
	if target.window != nil {
		if current, found := findWindow(ws.Windows, target.window.Handle); found && !sameWindowIdentity(current, *target.window) {
			return false, last, stale()
		}
	}
	w, err := resolveWindow(ws.Windows, target.selector)
	if err != nil {
		if asToolError(err).Code != codeNoWindow {
			return false, last, err
		}
		last["window_exists"] = false
		if target.window != nil {
			last["window"] = refOf(*target.window)
		}
		if strings.HasPrefix(in.Kind, "control_") {
			last["control_exists"] = false
		}
		return in.Kind == "window_gone" || in.Kind == "control_gone", last, nil
	}
	if target.window != nil && !sameWindowIdentity(w, *target.window) {
		return false, last, stale()
	}
	if target.window == nil {
		copy := w
		target.window = &copy
	}
	last["window_exists"], last["window"] = true, observedWindow(w)
	switch in.Kind {
	case "window_exists":
		return true, last, nil
	case "window_gone":
		return false, last, nil
	case "window_foreground":
		return ws.Foreground == w.Handle || w.Foreground, last, nil
	}
	var tree proto.ControlsResult
	if err := d.uiWaitCall(ctx, in.VM, proto.OpListControls, proto.ControlsArgs{Handle: w.Handle, PID: w.PID, MaxDepth: in.MaxDepth, MaxNodes: in.MaxNodes}, &tree); err != nil {
		last["unknown"] = "control tree read failed"
		return false, last, err
	}
	// A window can disappear or be reused while UIA reads its tree. Recheck its
	// identity and the session before using that tree as current evidence.
	after, err := readWindows()
	if err != nil {
		return false, last, err
	}
	current, found := findWindow(after.Windows, w.Handle)
	if !found {
		last["window_exists"], last["control_exists"] = false, false
		return in.Kind == "control_gone", last, nil
	}
	if !sameWindowIdentity(current, w) {
		return false, last, stale()
	}
	last["window"] = observedWindow(current)
	var matches []proto.ControlInfo
	for _, n := range tree.Nodes {
		if target.runtime != "" {
			if n.RuntimeID == target.runtime {
				matches = append(matches, n)
			}
		} else if (in.AutomationID == "" || n.AutomationID == in.AutomationID) && (in.ControlName == "" || n.Name == in.ControlName) {
			matches = append(matches, n)
		}
	}
	if len(matches) > 1 {
		return false, last, refuse(codeAmbiguousTarget, "use a more specific control selector or observation_id/index", map[string]any{"matches": matches}, "control selector matched %d controls", len(matches))
	}
	last["controls_truncated"] = tree.Truncated
	if tree.Truncated && (target.runtime == "" || len(matches) == 0) {
		last["unknown"] = "truncated control tree cannot prove absence or selector uniqueness; increase max_depth/max_nodes or use a known runtime ID"
		return false, last, nil
	}
	last["control_exists"] = len(matches) == 1
	if len(matches) == 0 {
		return in.Kind == "control_gone", last, nil
	}
	n := matches[0]
	last["control"] = n
	switch in.Kind {
	case "control_exists":
		return true, last, nil
	case "control_gone":
		return false, last, nil
	}
	if in.Enabled != nil && n.Enabled != *in.Enabled {
		return false, last, nil
	}
	if in.Value != nil {
		if !n.HasValue || slices.Contains(tree.Truncation, "text_length") || tree.Truncated && len(tree.Truncation) == 0 {
			last["unknown"] = "control value is unavailable (including password controls) or may be truncated"
			return false, last, nil
		}
		if n.Value != *in.Value {
			return false, last, nil
		}
	}
	actual := uiStateFields(n.State)
	for key, expected := range uiStateFields(in.State) {
		value, ok := actual[key]
		if !ok {
			last["unknown"] = "control state field is unavailable: " + key
			return false, last, nil
		}
		if value != expected {
			return false, last, nil
		}
	}
	return true, last, nil
}
