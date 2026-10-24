package host

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"hyperhand/internal/proto"
)

// renderControl renders one node of the control tree as a line (without the newline), indented two spaces per depth:
//
//	[12] Edit "" id=cmdline (0,980 1920x60) focused value="LINE"
//
// Index, control type name, quoted name, id=<automation_id> (only when set), (x,y wxh) of the rect, then the state
// words disabled, offscreen and focused, value="...", executable actions, readable semantic state and
// omitted_children=N when the per-parent child limit left N children out.
func renderControl(n proto.ControlInfo) string {
	var b strings.Builder
	b.WriteString(strings.Repeat("  ", max(0, n.Depth)))
	fmt.Fprintf(&b, "[%d] %s %q", n.Index, proto.ControlTypeName(n.ControlType), n.Name)
	if n.AutomationID != "" {
		if strings.ContainsAny(n.AutomationID, " \t\n\"") {
			fmt.Fprintf(&b, " id=%q", n.AutomationID)
		} else {
			b.WriteString(" id=" + n.AutomationID)
		}
	}
	r := n.Rect
	fmt.Fprintf(&b, " (%d,%d %dx%d)", r.Left, r.Top, r.Right-r.Left, r.Bottom-r.Top)
	if !n.Enabled {
		b.WriteString(" disabled")
	}
	if n.Offscreen {
		b.WriteString(" offscreen")
	}
	if n.Focused {
		b.WriteString(" focused")
	}
	if n.HasValue {
		fmt.Fprintf(&b, " value=%q", n.Value)
	}
	if actions := controlActions(n); len(actions) > 0 {
		fmt.Fprintf(&b, " actions=[%s]", strings.Join(actions, ","))
	}
	if n.State != nil {
		state, _ := json.Marshal(n.State)
		fmt.Fprintf(&b, " state=%s", state)
	}
	if n.OmittedChildren > 0 {
		fmt.Fprintf(&b, " omitted_children=%d", n.OmittedChildren)
	}
	return b.String()
}

// controlActions also supports agents that only reported pattern names.
func controlActions(n proto.ControlInfo) []string {
	if n.Actions != nil {
		return n.Actions
	}
	actions := make([]string, 0, len(n.Patterns))
	for _, pattern := range n.Patterns {
		switch pattern {
		case "Value":
			actions = append(actions, "SetValue")
		case "ScrollItem":
			actions = append(actions, "ScrollIntoView")
		case "Scroll":
			actions = append(actions, "ScrollUp", "ScrollDown", "ScrollLeft", "ScrollRight")
		case "Invoke", "Toggle", "Expand", "Collapse", "Select":
			actions = append(actions, pattern)
		}
	}
	return actions
}

// supportedControlActions prefers the agent's fresh capabilities over the observation's cached ones.
func supportedControlActions(message string, n proto.ControlInfo) []string {
	_, supported, ok := strings.Cut(message, "; supported:")
	if !ok {
		return controlActions(n)
	}
	actions := []string{}
	for _, name := range strings.Split(supported, ",") {
		if name = strings.TrimSpace(name); slices.Contains(proto.ControlActions, name) && name != "Locate" {
			actions = append(actions, name)
		}
	}
	return actions
}

// renderControls renders the tree as indexed text, one node per line, each line newline-terminated.
func renderControls(nodes []proto.ControlInfo) string {
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(renderControl(n))
		b.WriteByte('\n')
	}
	return b.String()
}

// controlKeys returns a matching key per node: the runtime ID when the agent reports one, else the node's path of
// (control type, name) pairs from the root, so that nodes without runtime IDs still match across snapshots when their
// ancestry, type and name are unchanged. Nodes with equal keys are matched in tree order.
func controlKeys(nodes []proto.ControlInfo) []string {
	paths := make([]string, len(nodes))
	keys := make([]string, len(nodes))
	for i, n := range nodes {
		parent := ""
		if n.Parent >= 0 && n.Parent < i {
			parent = paths[n.Parent]
		}
		paths[i] = fmt.Sprintf("%s/%s\x00%s", parent, proto.ControlTypeName(n.ControlType), n.Name)
		if n.RuntimeID != "" {
			keys[i] = "rid:" + n.RuntimeID
		} else {
			keys[i] = "path:" + paths[i]
		}
	}
	return keys
}

// controlChanged reports whether the facts an agent acts on differ between two snapshots of one element, including
// its index: the caller must learn the new index before acting on the element in the new observation.
func controlChanged(a, b proto.ControlInfo) bool {
	return a.Index != b.Index || a.Name != b.Name || a.Rect != b.Rect || a.Enabled != b.Enabled || a.Offscreen != b.Offscreen ||
		a.HasValue != b.HasValue || a.Value != b.Value || a.Focused != b.Focused || a.OmittedChildren != b.OmittedChildren ||
		!slices.Equal(controlActions(a), controlActions(b)) || !reflect.DeepEqual(a.State, b.State)
}

// diffControls compares an earlier tree with the current one: Added and Changed are rendered lines of the current
// tree, Removed the indexes in the earlier tree of nodes that vanished. The slices are never nil.
func diffControls(old, cur []proto.ControlInfo) *controlsDiff {
	d := &controlsDiff{Added: []string{}, Removed: []int{}, Changed: []string{}}
	oldKeys, curKeys := controlKeys(old), controlKeys(cur)
	unmatched := map[string][]int{} // key -> old indexes not yet matched, in tree order
	for i, k := range oldKeys {
		unmatched[k] = append(unmatched[k], i)
	}
	for i, n := range cur {
		olds := unmatched[curKeys[i]]
		if len(olds) == 0 {
			d.Added = append(d.Added, renderControl(n))
			continue
		}
		unmatched[curKeys[i]] = olds[1:]
		if controlChanged(old[olds[0]], n) {
			d.Changed = append(d.Changed, renderControl(n))
		}
	}
	for _, olds := range unmatched {
		for _, i := range olds {
			d.Removed = append(d.Removed, old[i].Index)
		}
	}
	sort.Ints(d.Removed)
	return d
}
