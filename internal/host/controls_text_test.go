package host

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"hyperhand/internal/proto"
)

func TestControlSemanticActionsAndState(t *testing.T) {
	off, selected, readOnly := "off", false, true
	n := proto.ControlInfo{ControlType: 50002, Enabled: true, Actions: []string{"Toggle"},
		State: &proto.ControlState{Toggle: &off, Selected: &selected, ReadOnly: &readOnly}}
	want := `[0] CheckBox "" (0,0 0x0) actions=[Toggle] state={"toggle":"off","selected":false,"read_only":true}`
	if got := renderControl(n); got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
	if strings.Contains(renderControl(n), `"offscreen"`) {
		t.Fatal("unavailable state rendered as known")
	}
	legacy := proto.ControlInfo{Enabled: true, Patterns: []string{"Invoke", "Value", "ScrollItem", "Unknown"}}
	if got := controlActions(legacy); !reflect.DeepEqual(got, []string{"Invoke", "SetValue", "ScrollIntoView"}) {
		t.Fatalf("legacy actions = %v", got)
	}
	legacy.Actions = []string{}
	if got := controlActions(legacy); len(got) != 0 {
		t.Fatalf("explicit empty actions should override old patterns: %v", got)
	}
}

func TestControlSemanticDiff(t *testing.T) {
	base := proto.ControlInfo{Enabled: true, RuntimeID: "42.1", Actions: []string{"Toggle"}}
	for _, tt := range []struct {
		name string
		old  *proto.ControlState
		cur  *proto.ControlState
		want bool
	}{
		{"unknown to false", nil, &proto.ControlState{Selected: new(false)}, true},
		{"false to unknown", &proto.ControlState{Selected: new(false)}, nil, true},
		{"selection", &proto.ControlState{Selected: new(false)}, &proto.ControlState{Selected: new(true)}, true},
		{"toggle", &proto.ControlState{Toggle: new("off")}, &proto.ControlState{Toggle: new("on")}, true},
		{"expand", &proto.ControlState{ExpandCollapse: new("collapsed")}, &proto.ControlState{ExpandCollapse: new("expanded")}, true},
		{"read only", &proto.ControlState{ReadOnly: new(false)}, &proto.ControlState{ReadOnly: new(true)}, true},
		{"offscreen", &proto.ControlState{Offscreen: new(false)}, &proto.ControlState{Offscreen: new(true)}, true},
		{"scroll axis", &proto.ControlState{HorizontallyScrollable: new(false)}, &proto.ControlState{HorizontallyScrollable: new(true)}, true},
		{"horizontal scroll", &proto.ControlState{HorizontalScrollPercent: new(0.0)}, &proto.ControlState{HorizontalScrollPercent: new(20.0)}, true},
		{"vertical scroll", &proto.ControlState{VerticalScrollPercent: new(0.0)}, &proto.ControlState{VerticalScrollPercent: new(20.0)}, true},
		{"equal state values", &proto.ControlState{Toggle: new("off"), Selected: new(false)}, &proto.ControlState{Toggle: new("off"), Selected: new(false)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old, cur := base, base
			old.State, cur.State = tt.old, tt.cur
			if got := len(diffControls([]proto.ControlInfo{old}, []proto.ControlInfo{cur}).Changed) != 0; got != tt.want {
				t.Fatalf("changed = %v, want %v", got, tt.want)
			}
		})
	}
	cur := base
	cur.Actions = []string{"Toggle", "Invoke"}
	if !controlChanged(base, cur) {
		t.Fatal("new semantic action was omitted from diff")
	}
	cur.Actions = nil
	cur.Patterns = []string{"Toggle"}
	if controlChanged(base, cur) {
		t.Fatal("equivalent legacy capabilities should not change the rendered diff")
	}
}

func TestSupportedControlActions(t *testing.T) {
	n := proto.ControlInfo{Patterns: []string{"Value", "ScrollItem"}}
	for _, tt := range []struct {
		message string
		want    []string
	}{
		{"unsupported pattern", []string{"SetValue", "ScrollIntoView"}},
		{"unsupported pattern: Toggle; supported: Invoke, SetValue", []string{"Invoke", "SetValue"}},
		{"unsupported pattern: Toggle; supported: ", []string{}},
		{"unsupported pattern: Toggle; supported: none", []string{}},
		{"unsupported pattern: Toggle; supported: ScrollUp, ScrollDown, ScrollLeft, ScrollRight", []string{"ScrollUp", "ScrollDown", "ScrollLeft", "ScrollRight"}},
	} {
		if got := supportedControlActions(tt.message, n); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: supported = %v, want %v", tt.message, got, tt.want)
		}
	}
}

func TestRenderScrollControl(t *testing.T) {
	n := proto.ControlInfo{Enabled: true, Actions: []string{"ScrollUp", "ScrollDown"},
		State: &proto.ControlState{HorizontallyScrollable: new(false), VerticallyScrollable: new(true), VerticalScrollPercent: new(0.0)}}
	line := renderControl(n)
	for _, part := range []string{`actions=[ScrollUp,ScrollDown]`, `"horizontally_scrollable":false`, `"vertically_scrollable":true`, `"vertical_scroll_percent":0`} {
		if !strings.Contains(line, part) {
			t.Fatalf("scroll control missing %q: %s", part, line)
		}
	}
	if strings.Contains(line, "horizontal_scroll_percent") {
		t.Fatalf("unknown percentage rendered as known: %s", line)
	}
	legacy := proto.ControlInfo{Patterns: []string{"Scroll"}}
	if got := controlActions(legacy); !reflect.DeepEqual(got, []string{"ScrollUp", "ScrollDown", "ScrollLeft", "ScrollRight"}) {
		t.Fatalf("legacy Scroll actions: %v", got)
	}
	legacy.Actions = []string{"ScrollLeft", "ScrollRight"}
	if got := controlActions(legacy); !reflect.DeepEqual(got, legacy.Actions) {
		t.Fatalf("explicit axis actions were replaced: %v", got)
	}
}

func TestEmptyScrollActionsSurviveWire(t *testing.T) {
	n := proto.ControlInfo{Enabled: true, Patterns: []string{"Scroll"}, Actions: []string{},
		State: &proto.ControlState{HorizontallyScrollable: new(false), VerticallyScrollable: new(false)}}
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var decoded proto.ControlInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Actions == nil {
		t.Fatalf("explicit empty actions disappeared on the wire: %s", data)
	}
	if actions := controlActions(decoded); len(actions) != 0 {
		t.Fatalf("non-scrollable axes regained actions after decoding: %v", actions)
	}
	if line := renderControl(decoded); strings.Contains(line, "actions=") {
		t.Fatalf("non-scrollable axes advertise actions: %s", line)
	}
}

func TestRenderControl(t *testing.T) {
	for _, tt := range []struct {
		name string
		node proto.ControlInfo
		want string
	}{
		{"root", proto.ControlInfo{Index: 0, Parent: -1, ControlType: 50032, Name: "AutoCAD 2015 - Drawing1.dwg", Enabled: true, Rect: proto.Rect{Right: 1920, Bottom: 1040}},
			`[0] Window "AutoCAD 2015 - Drawing1.dwg" (0,0 1920x1040)`},
		{"focused edit with id and value", proto.ControlInfo{Index: 12, Depth: 1, ControlType: 50004, AutomationID: "cmdline", Enabled: true, Focused: true, HasValue: true, Value: "LINE", Rect: proto.Rect{Top: 980, Right: 1920, Bottom: 1040}},
			`  [12] Edit "" id=cmdline (0,980 1920x60) focused value="LINE"`},
		{"disabled offscreen button at depth 2", proto.ControlInfo{Index: 3, Depth: 2, ControlType: 50000, Name: `Say "hi"`, Offscreen: true, Rect: proto.Rect{Left: 12, Top: 80, Right: 60, Bottom: 128}},
			`    [3] Button "Say \"hi\"" (12,80 48x48) disabled offscreen`},
		{"empty value is still shown", proto.ControlInfo{Index: 1, ControlType: 50004, Enabled: true, HasValue: true},
			`[1] Edit "" (0,0 0x0) value=""`},
		{"unknown type and id with spaces", proto.ControlInfo{Index: 2, ControlType: 1, AutomationID: "a b", Enabled: true},
			`[2] Custom "" id="a b" (0,0 0x0)`},
	} {
		if got := renderControl(tt.node); got != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, got, tt.want)
		}
	}
	nodes := []proto.ControlInfo{{Index: 0, Parent: -1, ControlType: 50032, Enabled: true}, {Index: 1, Depth: 1, ControlType: 50033, Enabled: true}}
	if got, want := renderControls(nodes), "[0] Window \"\" (0,0 0x0)\n  [1] Pane \"\" (0,0 0x0)\n"; got != want {
		t.Errorf("renderControls:\n got %q\nwant %q", got, want)
	}
	if renderControls(nil) != "" {
		t.Error("empty tree renders text")
	}
}

func diffTree() []proto.ControlInfo {
	return []proto.ControlInfo{
		{Index: 0, Parent: -1, Depth: 0, ControlType: 50032, Name: "Options", Enabled: true, RuntimeID: "42.1", Rect: proto.Rect{Right: 500, Bottom: 400}},
		{Index: 1, Parent: 0, Depth: 1, ControlType: 50004, Name: "", AutomationID: "cmdline", Enabled: true, RuntimeID: "42.2", HasValue: true, Value: "", Rect: proto.Rect{Top: 380, Right: 500, Bottom: 400}},
		{Index: 2, Parent: 0, Depth: 1, ControlType: 50000, Name: "OK", Enabled: true, RuntimeID: "42.3", Rect: proto.Rect{Left: 10, Top: 10, Right: 50, Bottom: 30}},
		{Index: 3, Parent: 0, Depth: 1, ControlType: 50000, Name: "Cancel", Enabled: true, RuntimeID: "42.4", Rect: proto.Rect{Left: 60, Top: 10, Right: 100, Bottom: 30}},
	}
}

func TestDiffControlsRuntimeIDs(t *testing.T) {
	old := diffTree()
	cur := diffTree()
	cur[1].Value, cur[1].Focused = "LINE", true // changed
	cur = append(cur[:3], proto.ControlInfo{Index: 3, Parent: 0, Depth: 1, ControlType: 50020, Name: "Specify first point:", Enabled: true, RuntimeID: "42.9", Rect: proto.Rect{Top: 350, Right: 500, Bottom: 380}})
	// Cancel (old index 3) vanished; OK keeps its runtime ID even though its index is unchanged.
	d := diffControls(old, cur)
	want := &controlsDiff{
		Added:   []string{`  [3] Text "Specify first point:" (0,350 500x30)`},
		Removed: []int{3},
		Changed: []string{`  [1] Edit "" id=cmdline (0,380 500x20) focused value="LINE"`},
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("diff = %+v, want %+v", d, want)
	}
	if d := diffControls(old, diffTree()); len(d.Added)+len(d.Removed)+len(d.Changed) != 0 || d.Added == nil || d.Removed == nil || d.Changed == nil {
		t.Errorf("identical trees: %+v", d)
	}
}

func TestDiffControlsFallbackPath(t *testing.T) {
	old := diffTree()
	cur := diffTree()
	for i := range old {
		old[i].RuntimeID, cur[i].RuntimeID = "", ""
	}
	// Re-indexed after an insertion at the top: nodes match by parent path, type and name, not by index.
	cur = append([]proto.ControlInfo{cur[0], {Index: 1, Parent: 0, Depth: 1, ControlType: 50021, Name: "Ribbon", Enabled: true}}, cur[1:]...)
	for i := 2; i < len(cur); i++ {
		cur[i].Index = i
	}
	cur[3].Enabled = false // OK, now at index 3
	d := diffControls(old, cur)
	// Every node after the insertion changed index, which the caller must learn before acting on it.
	want := &controlsDiff{
		Added:   []string{`  [1] ToolBar "Ribbon" (0,0 0x0)`},
		Removed: []int{},
		Changed: []string{`  [2] Edit "" id=cmdline (0,380 500x20) value=""`, `  [3] Button "OK" (10,10 40x20) disabled`, `  [4] Button "Cancel" (60,10 40x20)`},
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("diff = %+v, want %+v", d, want)
	}
	// A renamed node without a runtime ID cannot be matched: it is removed and added.
	cur = diffTree()
	for i := range cur {
		cur[i].RuntimeID = ""
	}
	cur[3].Name = "Close"
	d = diffControls(old, cur)
	if !reflect.DeepEqual(d.Removed, []int{3}) || len(d.Added) != 1 || len(d.Changed) != 0 {
		t.Errorf("renamed: %+v", d)
	}
}

func TestControlOmittedChildren(t *testing.T) {
	n := proto.ControlInfo{Index: 3, Depth: 1, ControlType: 50008, Name: "Files", Enabled: true, RuntimeID: "42.7",
		Actions: []string{"ScrollDown"}, OmittedChildren: 612}
	want := `  [3] List "Files" (0,0 0x0) actions=[ScrollDown] omitted_children=612`
	if got := renderControl(n); got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
	cur := n
	cur.OmittedChildren = 0
	if strings.Contains(renderControl(cur), "omitted_children") {
		t.Fatalf("zero omitted children rendered: %s", renderControl(cur))
	}
	d := diffControls([]proto.ControlInfo{n}, []proto.ControlInfo{cur})
	if !reflect.DeepEqual(d.Changed, []string{renderControl(cur)}) || len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Fatalf("omitted_children change not reported: %+v", d)
	}
	cur.OmittedChildren = 612
	if controlChanged(n, cur) {
		t.Fatal("equal omitted_children reported as changed")
	}
}
