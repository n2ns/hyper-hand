package proto

const OpListControls = "list_controls"

// Separate ops make scoped reads fail closed on agents that only support whole-window snapshots.
const OpListControlSubtree = "list_control_subtree"
const OpFindControls = "find_controls"

// OpControlAction performs a UI Automation pattern action on one element: ControlActionArgs -> ControlActionResult.
const OpControlAction = "control_action"

type ControlsArgs struct {
	Handle        uint64 `json:"handle"`
	PID           uint32 `json:"pid"`
	MaxDepth      int    `json:"max_depth,omitempty"`
	MaxNodes      int    `json:"max_nodes,omitempty"`
	RootRuntimeID string `json:"root_runtime_id,omitempty"`
	HintRect      *Rect  `json:"hint_rect,omitempty"` // optional old screen bounds; identity must still match
}

// FindControlsArgs searches the control view with exact, AND-combined selectors.
// RootRuntimeID scopes the search to an existing element; empty means the window.
type FindControlsArgs struct {
	Handle        uint64 `json:"handle"`
	PID           uint32 `json:"pid"`
	RootRuntimeID string `json:"root_runtime_id,omitempty"`
	HintRect      *Rect  `json:"hint_rect,omitempty"` // hint for RootRuntimeID, never a selector
	AutomationID  string `json:"automation_id,omitempty"`
	Name          string `json:"name,omitempty"`
	ControlType   int32  `json:"control_type,omitempty"`
	MaxDepth      int    `json:"max_depth,omitempty"`
	MaxVisited    int    `json:"max_visited,omitempty"`
	MaxMatches    int    `json:"max_matches,omitempty"`
}

// Matches form a flat list: indexes are local, Parent is -1, and Depth is relative
// to the search root. Truncated means uniqueness and absence are not established.
type FindControlsResult struct {
	Matches    []ControlInfo `json:"matches"`
	Visited    int           `json:"visited"`
	Truncated  bool          `json:"truncated"`
	Truncation []string      `json:"truncation,omitempty"`
}

// ControlInfo is one node of the control-view tree. Rect uses physical screen pixels. Index is local to this snapshot;
// RuntimeID (IUIAutomationElement::GetRuntimeId, dot-joined integers) identifies the element across snapshots while
// it exists and is what control_action takes. ControlType is a Microsoft UIA control type ID (ControlTypeName gives
// its name). Patterns lists the supported pattern names among Invoke, Toggle, Expand, Collapse, Select, Value,
// ScrollItem and Scroll (see ControlActions). Actions contains callable action names, filtered by known scroll-axis
// capability. Value and HasValue carry ValuePattern's current value (never for password
// controls). Focused marks the element with keyboard focus. The root has Parent -1 and Depth 0.
type ControlInfo struct {
	Index        int           `json:"index"`
	Parent       int           `json:"parent"`
	Depth        int           `json:"depth"`
	Name         string        `json:"name"`
	ControlType  int32         `json:"control_type"`
	AutomationID string        `json:"automation_id"`
	ClassName    string        `json:"class_name"`
	PID          uint32        `json:"pid"`
	Enabled      bool          `json:"enabled"`
	Offscreen    bool          `json:"offscreen"`
	Rect         Rect          `json:"rect"`
	RuntimeID    string        `json:"runtime_id,omitempty"`
	Patterns     []string      `json:"patterns,omitempty"`
	Actions      []string      `json:"actions"`
	State        *ControlState `json:"state,omitempty"`
	Value        string        `json:"value,omitempty"`
	HasValue     bool          `json:"has_value,omitempty"`
	Focused      bool          `json:"focused,omitempty"`
}

// ControlState contains readable UIA state. Nil fields mean unavailable, not false.
type ControlState struct {
	Toggle                  *string  `json:"toggle,omitempty"`
	ExpandCollapse          *string  `json:"expand_collapse,omitempty"`
	Selected                *bool    `json:"selected,omitempty"`
	ReadOnly                *bool    `json:"read_only,omitempty"`
	Offscreen               *bool    `json:"offscreen,omitempty"`
	HorizontallyScrollable  *bool    `json:"horizontally_scrollable,omitempty"`
	VerticallyScrollable    *bool    `json:"vertically_scrollable,omitempty"`
	HorizontalScrollPercent *float64 `json:"horizontal_scroll_percent,omitempty"`
	VerticalScrollPercent   *float64 `json:"vertical_scroll_percent,omitempty"`
}

// ControlsResult: Focused is the index of the focused node, -1 when none is in the tree. SelectedText is the text
// selected in the window when a TextPattern reports one.
type ControlsResult struct {
	Nodes        []ControlInfo `json:"nodes"`
	Truncated    bool          `json:"truncated"`
	Truncation   []string      `json:"truncation,omitempty"`
	Focused      int           `json:"focused"`
	SelectedText string        `json:"selected_text,omitempty"`
}

// ControlActions are the Action values control_action accepts (case-insensitive). SetValue uses ValuePattern and
// takes Value; the others take no value. Invoke: InvokePattern; Toggle: TogglePattern; Expand/Collapse:
// ExpandCollapsePattern; Select: SelectionItemPattern; ScrollIntoView: ScrollItemPattern. ScrollUp/Down/Left/Right
// use ScrollPattern's provider-defined small step. Locate performs nothing:
// it re-finds the element and returns its current Rect and value, so the host can act on fresh coordinates.
var ControlActions = []string{"SetValue", "Invoke", "Toggle", "Expand", "Collapse", "Select", "ScrollIntoView", "ScrollUp", "ScrollDown", "ScrollLeft", "ScrollRight", "Locate"}

// ControlActionArgs: the element RuntimeID inside the top-level window Handle of process PID. The agent re-finds the
// element by runtime ID; a missing element is the error "element not found" (host code stale_element); an action the
// element does not support is the error "unsupported pattern: <action>; supported: <list>".
type ControlActionArgs struct {
	Handle    uint64 `json:"handle"`
	PID       uint32 `json:"pid"`
	RuntimeID string `json:"runtime_id"`
	Action    string `json:"action"`
	Value     string `json:"value,omitempty"`
	HintRect  *Rect  `json:"hint_rect,omitempty"` // hint only; RuntimeID remains authoritative
}

// ControlActionResult: after the action, Rect is the element's current bounding rectangle (physical screen pixels)
// and Value/HasValue re-read its ValuePattern (when supported). State contains the readable post-action state.
// Verified reports whether read-back observed the action's target state, nil when unavailable or not applicable.
type ControlActionResult struct {
	Rect     *Rect         `json:"rect,omitempty"`
	Value    string        `json:"value,omitempty"`
	HasValue bool          `json:"has_value,omitempty"`
	Verified *bool         `json:"verified,omitempty"`
	State    *ControlState `json:"state,omitempty"`
}

// ControlTypeName maps a UIA control type ID (UIA_*ControlTypeId) to its name without the "ControlType" suffix.
func ControlTypeName(id int32) string {
	if n, ok := controlTypeNames[id]; ok {
		return n
	}
	return "Custom"
}

var controlTypeNames = map[int32]string{
	50000: "Button", 50001: "Calendar", 50002: "CheckBox", 50003: "ComboBox", 50004: "Edit", 50005: "Hyperlink",
	50006: "Image", 50007: "ListItem", 50008: "List", 50009: "Menu", 50010: "MenuBar", 50011: "MenuItem",
	50012: "ProgressBar", 50013: "RadioButton", 50014: "ScrollBar", 50015: "Slider", 50016: "Spinner",
	50017: "StatusBar", 50018: "Tab", 50019: "TabItem", 50020: "Text", 50021: "ToolBar", 50022: "ToolTip",
	50023: "Tree", 50024: "TreeItem", 50025: "Custom", 50026: "Group", 50027: "Thumb", 50028: "DataGrid",
	50029: "DataItem", 50030: "Document", 50031: "SplitButton", 50032: "Window", 50033: "Pane", 50034: "Header",
	50035: "HeaderItem", 50036: "Table", 50037: "TitleBar", 50038: "Separator", 50039: "SemanticZoom",
	50040: "AppBar",
}
