package host

import (
	"context"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"hyperhand/internal/proto"
)

type findControlsIn struct {
	VM            string `json:"vm,omitempty" jsonschema:"VM name; default the only running VM"`
	Handle        uint64 `json:"handle,omitempty" jsonschema:"window to search"`
	PID           uint32 `json:"pid,omitempty" jsonschema:"restrict handle or select the process's only visible window"`
	ObservationID string `json:"observation_id,omitempty" jsonschema:"with index, search inside this observed control instead of a whole window"`
	Index         *int   `json:"index,omitempty" jsonschema:"root control index; requires observation_id"`
	AutomationID  string `json:"automation_id,omitempty" jsonschema:"exact AutomationId; all supplied filters must match"`
	ControlName   string `json:"control_name,omitempty" jsonschema:"exact UIA Name; case-sensitive"`
	ControlType   string `json:"control_type,omitempty" jsonschema:"UIA type name, such as Button, Edit, Group or Pane"`
	MaxDepth      int    `json:"max_depth,omitempty" jsonschema:"search depth from root; default 32, maximum 64"`
	MaxVisited    int    `json:"max_visited,omitempty" jsonschema:"visited-node budget; default 5000, maximum 20000"`
	MaxMatches    int    `json:"max_matches,omitempty" jsonschema:"returned-match limit; default 20, maximum 100"`
}

const findControlsDesc = "Search a window's UI Automation control view by exact automation_id, control_name and/or control_type (AND). Select handle/pid or a subtree with observation_id/index. " +
	"Returns {observation_id,vm,window,matches,visited,truncated,truncation,status}. status is unique, multiple, not_found, or incomplete; incomplete results cannot prove uniqueness or absence. " +
	"Matches contain index, runtime_id, rect, actions and available value/state. Pass observation_id plus a chosen index directly to vm_observe for its subtree, vm_wait, or control actions. " +
	"Does not activate windows. Search is bounded by node/depth/match limits and provider timeout. Password subtrees are not searched. Needs an updated guest agent. " + descUntrusted

func registerControlSearch(d *deps) {
	addToolIn(d, toolSpec{name: "vm_find_controls", desc: findControlsDesc, readOnly: true}, func(ctx context.Context, in findControlsIn) (*mcp.CallToolResult, error) {
		return d.findControls(ctx, in)
	})
}

func validateControlScope(handle uint64, pid uint32, id string, index *int) error {
	if id != "" || index != nil {
		if id == "" || index == nil || *index < 0 || handle != 0 || pid != 0 {
			return refuse(codeInvalidArgument, "supply observation_id and a nonnegative index together, without handle/pid", nil, "control scope must use one complete selector")
		}
	}
	return nil
}

// loadControlScope preserves the task, VM and lifecycle boundary of a cached identity.
func (d *deps) loadControlScope(ctx context.Context, vm, id string, index int) (*proto.WindowInfo, *proto.ControlInfo, error) {
	o, err := d.taskObs(ctx).get(id)
	if err != nil {
		return nil, nil, err
	}
	if o.VM != vm || o.Epoch != d.taskObs(ctx).version(vm).Epoch {
		return nil, nil, refuse(codeStaleObservation, "find or observe the control again on this VM", nil, "control scope belongs to another VM or predates a lifecycle change")
	}
	n, err := o.node(index)
	if err != nil {
		return nil, nil, err
	}
	if o.treeWindow() == nil || n.RuntimeID == "" {
		return nil, nil, refuse(codeInvalidArgument, "use a control returned by vm_find_controls or vm_observe with a runtime ID", nil, "control scope has no stable window/control identity")
	}
	w := *o.treeWindow()
	return &w, &n, nil
}

func controlReadError(err error) error {
	if strings.HasPrefix(err.Error(), "element not found") {
		return refuse(codeStaleElement, "call vm_find_controls again; the selected control no longer exists", nil, "%v", err)
	}
	if strings.HasPrefix(err.Error(), "control search incomplete") {
		return refuse("search_incomplete", "narrow the search to an observed subtree; absence has not been established", nil, "%v", err)
	}
	return agentErr(err)
}

func (d *deps) findControls(ctx context.Context, in findControlsIn) (*mcp.CallToolResult, error) {
	if err := validateControlScope(in.Handle, in.PID, in.ObservationID, in.Index); err != nil {
		return nil, err
	}
	if in.Handle == 0 && in.PID == 0 && in.ObservationID == "" {
		return nil, refuse(codeInvalidArgument, "pass handle/pid or observation_id/index", nil, "control search requires a window or control scope")
	}
	if in.AutomationID == "" && in.ControlName == "" && in.ControlType == "" {
		return nil, refuse(codeInvalidArgument, "supply automation_id, control_name or control_type", nil, "control search requires at least one property")
	}
	if strings.ContainsRune(in.AutomationID+in.ControlName, 0) {
		return nil, refuse(codeInvalidArgument, "remove NUL characters from control selectors", nil, "control selectors must not contain NUL")
	}
	var controlType int32
	if in.ControlType != "" {
		for id := int32(50000); id <= 50040; id++ {
			if strings.EqualFold(proto.ControlTypeName(id), in.ControlType) {
				controlType = id
				break
			}
		}
		if controlType == 0 {
			return nil, refuse(codeInvalidArgument, "use a UIA type name such as Button, Edit, Group or Pane", nil, "unknown control_type %q", in.ControlType)
		}
	}
	if in.MaxDepth < 0 || in.MaxDepth > 64 || in.MaxVisited < 0 || in.MaxVisited > 20000 || in.MaxMatches < 0 || in.MaxMatches > 100 {
		return nil, refuse(codeInvalidArgument, "use max_depth 0..64, max_visited 0..20000 and max_matches 0..100; zero uses defaults", nil, "search limits are out of range")
	}
	if in.MaxDepth == 0 {
		in.MaxDepth = 32
	}
	if in.MaxVisited == 0 {
		in.MaxVisited = 5000
	}
	if in.MaxMatches == 0 {
		in.MaxMatches = 20
	}
	v, err := d.raw.Find(in.VM)
	if err != nil {
		return nil, vmErr(err)
	}
	vm := v.Name
	d.input.Lock()
	defer d.input.Unlock()
	store := d.taskObs(ctx)
	version := store.version(vm)
	if version.Busy != 0 {
		return nil, refuse(codeStaleObservation, "wait for the mutating operation to finish, then search again", nil, "VM has a mutating operation in progress")
	}
	var old *proto.WindowInfo
	root := ""
	var hint *proto.Rect
	selector := windowSelector{Handle: in.Handle, PID: in.PID}
	if in.ObservationID != "" {
		var node *proto.ControlInfo
		old, node, err = d.loadControlScope(ctx, vm, in.ObservationID, *in.Index)
		if err != nil {
			return nil, err
		}
		root, hint = node.RuntimeID, &node.Rect
		selector = windowSelector{Handle: old.Handle, PID: old.PID}
	}
	ctx, cancel := context.WithTimeout(ctx, uiWaitCallTimeout)
	defer cancel()
	ws, err := listWindowsResult(ctx, d.call, vm)
	if err != nil {
		return nil, agentErr(err)
	}
	if err := (&action{ws: ws}).checkSession(true); err != nil {
		return nil, err
	}
	if old != nil {
		current, found := findWindow(ws.Windows, old.Handle)
		if !found || !sameWindowIdentity(current, *old) {
			return nil, refuse(codeStaleObservation, "find the control again", nil, "control scope window identity changed")
		}
	}
	w, err := resolveWindow(ws.Windows, selector)
	if err != nil {
		return nil, err
	}
	var found proto.FindControlsResult
	args := proto.FindControlsArgs{Handle: w.Handle, PID: w.PID, RootRuntimeID: root, HintRect: hint, AutomationID: in.AutomationID, Name: in.ControlName, ControlType: controlType, MaxDepth: in.MaxDepth, MaxVisited: in.MaxVisited, MaxMatches: in.MaxMatches}
	if _, err = d.call(ctx, vm, proto.OpFindControls, args, nil, &found); err != nil {
		return nil, controlReadError(err)
	}
	after, err := listWindowsResult(ctx, d.call, vm)
	if err != nil {
		return nil, agentErr(err)
	}
	if err := (&action{ws: after}).checkSession(true); err != nil {
		return nil, err
	}
	cur, ok := findWindow(after.Windows, w.Handle)
	if !ok || !sameWindowIdentity(cur, w) || cur.Rect != w.Rect || cur.Minimized != w.Minimized || store.version(vm) != version {
		return nil, refuse(codeStaleObservation, "search again and use the new observation_id", nil, "window or VM changed during control search")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if found.Matches == nil {
		found.Matches = []proto.ControlInfo{}
	}
	for i := range found.Matches {
		found.Matches[i].Index = i
		found.Matches[i].Parent = -1
	}
	status := "not_found"
	switch {
	case found.Truncated:
		status = "incomplete"
	case len(found.Matches) == 1:
		status = "unique"
	case len(found.Matches) > 1:
		status = "multiple"
	}
	o := &observation{ID: newID(), VM: vm, At: time.Now(), Revision: version.Revision, Epoch: version.Epoch, Window: &w, TreeWindow: &w, Nodes: found.Matches, TreeRootRuntimeID: root, SearchResults: true}
	store.put(o)
	return jsonResult(map[string]any{"observation_id": o.ID, "vm": vm, "window": observedWindow(w), "matches": found.Matches, "status": status, "visited": found.Visited, "truncated": found.Truncated, "truncation": found.Truncation})
}
