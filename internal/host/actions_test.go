package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

// fakeInput is the Hyper-V side of an action test: it records every input event as one line.
type fakeInput struct {
	Backend
	mu     sync.Mutex
	log    []string
	fail   error  // returned by every input op when set
	failOn string // when set, the input op whose line equals it fails with fail (others succeed)
}

func (b *fakeInput) record(s string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil && (b.failOn == "" || b.failOn == s) {
		return b.fail
	}
	b.log = append(b.log, s)
	return nil
}

func (b *fakeInput) events() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.log, ",")
}

func (b *fakeInput) Find(string) (hyperv.VM, error) { return hyperv.VM{ID: "A", Name: "A"}, nil }

var actionScreenshot = sync.OnceValue(func() []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 1920, 1080))); err != nil {
		panic(err)
	}
	return b.Bytes()
})

func (b *fakeInput) Screenshot(string) ([]byte, int, int, error) {
	return actionScreenshot(), 1920, 1080, nil
}
func (b *fakeInput) Click(_ string, x, y, button, count int, mods []string) error {
	return b.record(fmt.Sprintf("click %d,%d b%d c%d %v", x, y, button, count, mods))
}
func (b *fakeInput) Drag(_ string, x1, y1, x2, y2 int, mods []string) error {
	return b.record(fmt.Sprintf("drag %d,%d-%d,%d %v", x1, y1, x2, y2, mods))
}
func (b *fakeInput) Scroll(_ string, x, y, delta int) error {
	return b.record(fmt.Sprintf("scroll %d,%d %d", x, y, delta))
}
func (b *fakeInput) PressKeys(_, keys string) error { return b.record("press " + keys) }
func (b *fakeInput) TypeText(_, text string) error  { return b.record("type " + text) }

// testDeps is a deps built by hand around a fakeCall and a fakeInput, with the action tools registered and a fake
// observe that records its inputs.
type testDeps struct {
	*deps
	f        *fakeCall
	b        *fakeInput
	cs       *mcp.ClientSession
	ctx      context.Context
	observed []observeIn
}

func newTestDeps(t *testing.T, f *fakeCall) *testDeps {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	input := &sync.Mutex{}
	b := &fakeInput{}
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	td := &testDeps{f: f, b: b, ctx: ctx}
	td.deps = &deps{s: s, raw: b, backend: lockedInput{b, input}, input: input, call: f.call, obs: newObservationStore(), runID: "run-test", turn: newTurnState()}
	td.deps.observe = func(_ context.Context, in observeIn) (*observeOut, []byte, error) {
		td.observed = append(td.observed, in)
		var png []byte
		if in.Screenshot == nil || *in.Screenshot {
			png = []byte("png")
		}
		return &observeOut{ObservationID: "o-after", VM: in.VM}, png, nil
	}
	registerActions(td.deps)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	td.cs, err = mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { td.cs.Close() })
	return td
}

// call runs a tool and decodes its JSON item; for an error result the map is the error object. vm defaults to the
// fixture VM "A" when args omit it (every tool requires vm).
func (td *testDeps) call(t *testing.T, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	if _, ok := args["vm"]; !ok {
		if args == nil {
			args = map[string]any{}
		}
		args["vm"] = "A"
	}
	r, err := td.cs.CallTool(td.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r, resultJSON(t, r)
}

// put stores an observation of w (nil: the whole screen) scaled by scale, with nodes as its control tree, and
// returns its ID. The crop is w's rect (the screen for nil).
func (td *testDeps) put(w *proto.WindowInfo, scale float64, nodes []proto.ControlInfo) string {
	o := &observation{VM: "A", Window: w, TreeWindow: w, Scale: scale, HasImage: true, SourcePNG: actionScreenshot(), Nodes: nodes, Crop: screenshotRegion{Width: 1920, Height: 1080}}
	if w != nil {
		o.Crop = screenshotRegion{X: int(w.Rect.Left), Y: int(w.Rect.Top), Width: int(w.Rect.Right - w.Rect.Left), Height: int(w.Rect.Bottom - w.Rect.Top)}
	}
	o.OutputWidth, o.OutputHgt = int(float64(o.Crop.Width)*scale), int(float64(o.Crop.Height)*scale)
	td.obs.put(o)
	return o.ID
}

// options is window 10 of testWindows: the Options dialog at (100, 50), 500x400.
func options() *proto.WindowInfo {
	w := testWindows()[0]
	return &w
}

func TestClickObservationMapping(t *testing.T) {
	td := newTestDeps(t, newAgent(10))
	id := td.put(options(), 0.5, nil)
	// Image pixel (10, 20) of a half-scale crop at (100, 50): x = 100 + floor(10.5/0.5), y = 50 + floor(20.5/0.5).
	r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 10, "y": 20, "button": "right", "count": 2, "modifiers": []string{"shift"}})
	if r.IsError {
		t.Fatalf("refused: %v", m)
	}
	if got := td.b.events(); got != "click 121,91 b2 c2 [shift]" {
		t.Errorf("events %q", got)
	}
	if at := td.f.args[slices.Index(td.f.ops, proto.OpWindowAt)].(proto.PointArgs); at != (proto.PointArgs{X: 121, Y: 91}) {
		t.Errorf("window_at asked for %+v", at)
	}
	if m["ok"] != true || m["window"].(map[string]any)["handle"] != float64(10) || m["window"].(map[string]any)["process"] != "acad.exe" {
		t.Errorf("result %v", m)
	}
	// observe_after defaults to a screenshot of the observed window: image item first, then the JSON with after.
	if _, ok := r.Content[0].(*mcp.ImageContent); !ok || len(r.Content) != 2 || m["after"].(map[string]any)["observation_id"] != "o-after" {
		t.Errorf("after: %T %v", r.Content[0], m["after"])
	}
	if len(td.observed) != 1 || td.observed[0].Handle != 10 || !*td.observed[0].Screenshot || td.observed[0].Controls {
		t.Errorf("observed %+v", td.observed)
	}
	// Outside the image: refused before any agent call beyond the list.
	td = newTestDeps(t, newAgent(10))
	id = td.put(options(), 0.5, nil)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 250, "y": 0}); !r.IsError || m["error"] != codeInvalidArgument || td.b.events() != "" {
		t.Errorf("outside: %v %q", m, td.b.events())
	}
}

func TestClickStaleObservation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(ws []proto.WindowInfo) []proto.WindowInfo
	}{
		{"moved", func(ws []proto.WindowInfo) []proto.WindowInfo {
			ws[0].Rect.Left += 10
			ws[0].Rect.Right += 10
			return ws
		}},
		{"resized", func(ws []proto.WindowInfo) []proto.WindowInfo { ws[0].Rect.Bottom += 1; return ws }},
		{"minimized", func(ws []proto.WindowInfo) []proto.WindowInfo { ws[0].Minimized = true; return ws }},
		{"closed", func(ws []proto.WindowInfo) []proto.WindowInfo { return ws[1:] }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgent(10)
			f.results[proto.OpListWindows] = proto.WindowsResult{Windows: tt.change(testWindows()), Session: &proto.SessionStateResult{Console: true}}
			td := newTestDeps(t, f)
			id := td.put(options(), 1, nil)
			r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1})
			if !r.IsError || m["error"] != codeStaleObservation || m["next"] != "call vm_observe again and use its observation_id" {
				t.Errorf("result %v", m)
			}
			if td.b.events() != "" || f.count(proto.OpWindowAt) != 0 || len(td.observed) != 0 {
				t.Errorf("acted on a stale observation: %q %v", td.b.events(), f.ops)
			}
		})
	}
	// Unknown or evicted IDs are stale too; a whole-screen observation is always fresh.
	td := newTestDeps(t, newAgent(10))
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": "o-gone", "x": 1, "y": 1}); !r.IsError || m["error"] != codeStaleObservation {
		t.Errorf("unknown id: %v", m)
	}
	id := td.put(nil, 1, nil)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 7, "y": 8, "observe_after": "none"}); r.IsError || td.b.events() != "click 7,8 b1 c1 []" || m["window"] != nil {
		t.Errorf("whole screen: %v %q", m, td.b.events())
	}
}

func TestClickIndexCentre(t *testing.T) {
	td := newTestDeps(t, newAgent(10))
	nodes := []proto.ControlInfo{
		{Index: 0, Parent: -1, Rect: options().Rect, RuntimeID: "42.1"},
		{Index: 1, Parent: 0, Name: "OK", ControlType: 50000, Rect: proto.Rect{Left: 200, Top: 100, Right: 300, Bottom: 141}, RuntimeID: "42.2", Patterns: []string{"Invoke"}},
	}
	id := td.put(options(), 0.5, nodes)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "index": 1, "observe_after": "none"}); r.IsError || td.b.events() != "click 250,120 b1 c1 []" {
		t.Errorf("index: %v %q", m, td.b.events())
	}
	td = newTestDeps(t, newAgent(10))
	id = td.put(options(), 0.5, nodes)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "index": 5}); !r.IsError || m["error"] != codeStaleElement || td.b.events() != "" {
		t.Errorf("bad index: %v", m)
	}
	// index 0 is the root node, not "unset"; an observation without a tree refuses index.
	id = td.put(options(), 0.5, nil)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "index": 0}); !r.IsError || m["error"] != codeInvalidArgument {
		t.Errorf("no tree: %v", m)
	}
	if r, m := td.call(t, "vm_click", map[string]any{"index": 0, "x": 1, "y": 1}); !r.IsError || m["error"] != codeInvalidArgument {
		t.Errorf("index without observation: %v", m)
	}
}

// backgroundAgent lists window 30 (another process) in the foreground until focus_window has been called, when
// focused is true.
func backgroundAgent(focused bool) *fakeCall {
	f := newAgent(10)
	fg := uint64(30)
	f.fn = map[string]func(any) (any, error){
		proto.OpListWindows: func(any) (any, error) {
			ws := testWindows()
			for i := range ws {
				ws[i].Foreground = ws[i].Handle == fg
			}
			return proto.WindowsResult{Windows: ws, Foreground: fg, Session: &proto.SessionStateResult{Console: true}}, nil
		},
		proto.OpFocusWindow: func(args any) (any, error) {
			if focused {
				fg = args.(proto.TitleArgs).Handle
			}
			return proto.FocusResult{Handle: fg}, nil
		},
	}
	return f
}

func TestClickAutoActivation(t *testing.T) {
	// Success: focus_window with the handle, a fresh list, then the click.
	f := backgroundAgent(true)
	td := newTestDeps(t, f)
	id := td.put(options(), 1, nil)
	r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1, "observe_after": "none"})
	if r.IsError || td.b.events() != "click 101,51 b1 c1 []" {
		t.Fatalf("activated: %v %q", m, td.b.events())
	}
	if want := []string{proto.OpListWindows, proto.OpFocusWindow, proto.OpListWindows, proto.OpListWindows, proto.OpWindowAt}; !slices.Equal(f.ops, want) || f.args[1].(proto.TitleArgs) != (proto.TitleArgs{Handle: 10}) {
		t.Errorf("ops %v args %+v", f.ops, f.args)
	}
	// Failure: the other process's window keeps the foreground and is named as the one to act on.
	f = backgroundAgent(false)
	td = newTestDeps(t, f)
	id = td.put(options(), 1, nil)
	r, m = td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1})
	if !r.IsError || m["error"] != codeActivateFailed || td.b.events() != "" || f.count(proto.OpWindowAt) != 0 {
		t.Fatalf("not activated: %v %q", m, td.b.events())
	}
	if fg := m["foreground"].(map[string]any); fg["handle"] != float64(30) || fg["process"] != "acad.exe" || !strings.Contains(m["next"].(string), "act on handle 30 first") {
		t.Errorf("foreground facts: %v", m)
	}
	// activate: false keeps the strict rule and never asks for focus.
	f = backgroundAgent(true)
	td = newTestDeps(t, f)
	id = td.put(options(), 1, nil)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1, "activate": false}); !r.IsError || m["error"] != codeActivateFailed || f.count(proto.OpFocusWindow) != 0 {
		t.Errorf("activate false: %v %v", m, f.ops)
	}
}

func TestIntegrityRefusal(t *testing.T) {
	for _, tt := range []struct {
		target, agent string
		refused       bool
	}{
		{"high", "medium", true}, {"system", "high", true}, {"medium", "medium", false}, {"low", "medium", false}, {"", "medium", false}, {"high", "", false},
	} {
		f := newAgent(10)
		ws := testWindows()
		ws[0].Integrity = tt.target
		f.results[proto.OpListWindows] = proto.WindowsResult{Windows: ws, AgentIntegrity: tt.agent, Session: &proto.SessionStateResult{Console: true}}
		td := newTestDeps(t, f)
		id := td.put(options(), 1, nil)
		r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1, "observe_after": "none"})
		if r.IsError != tt.refused || (tt.refused && (m["error"] != codeIntegrityMismatch || m["next"] != "launch the application without admin, or use vm_launch with admin:true")) {
			t.Errorf("%s vs %s: %v", tt.target, tt.agent, m)
		}
		if tt.refused && td.b.events() != "" {
			t.Errorf("%s vs %s: clicked", tt.target, tt.agent)
		}
	}
}

func TestSessionUnusable(t *testing.T) {
	session := func(s proto.SessionStateResult) *fakeCall {
		f := newAgent(10)
		f.results[proto.OpListWindows] = proto.WindowsResult{Windows: testWindows(), Session: &s}
		return f
	}
	// A targeted click on a locked session: vm_unlock is the next step.
	td := newTestDeps(t, session(proto.SessionStateResult{Locked: true, Console: true, SecureDesktop: true, LogonUI: true}))
	id := td.put(options(), 1, nil)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1}); !r.IsError || m["error"] != codeSessionUnusable || m["next"] != "call vm_unlock" || td.b.events() != "" {
		t.Errorf("locked: %v", m)
	}
	// Raw screen input is never checked: it is how the sign-in screen is operated while the agent answers from the
	// locked session.
	td = newTestDeps(t, session(proto.SessionStateResult{Locked: true, Console: true}))
	if r, m := td.call(t, "vm_key", map[string]any{"keys": "enter"}); r.IsError || td.b.events() != "press enter" {
		t.Errorf("raw key, locked: %v %q", m, td.b.events())
	}
	if r, m := td.call(t, "vm_click", map[string]any{"x": 5, "y": 6, "observe_after": "none"}); r.IsError || td.b.events() != "press enter,click 5,6 b1 c1 []" {
		t.Errorf("raw click, locked: %v %q", m, td.b.events())
	}
	// Untargeted ASCII text goes through the Hyper-V keyboard while the session is locked; other text needs vm_unlock.
	if r, m := td.call(t, "vm_type", map[string]any{"text": "pass1"}); r.IsError || td.b.events() != "press enter,click 5,6 b1 c1 [],type pass1" {
		t.Errorf("raw type, locked: %v %q", m, td.b.events())
	}
	if r, m := td.call(t, "vm_type", map[string]any{"text": "密码"}); !r.IsError || m["error"] != codeSessionUnusable {
		t.Errorf("raw non-ASCII type, locked: %v", m)
	}
	// A UAC prompt holds the secure desktop: raw input may answer it, targeted input may not.
	td = newTestDeps(t, session(proto.SessionStateResult{Console: true, SecureDesktop: true, Consent: true}))
	if r, m := td.call(t, "vm_click", map[string]any{"x": 5, "y": 6, "observe_after": "none"}); r.IsError || td.b.events() != "click 5,6 b1 c1 []" {
		t.Errorf("raw click on UAC: %v %q", m, td.b.events())
	}
	id = td.put(options(), 1, nil)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1}); !r.IsError || m["error"] != codeSessionUnusable {
		t.Errorf("targeted click on UAC: %v", m)
	}
	// Not the console session.
	td = newTestDeps(t, session(proto.SessionStateResult{Console: false}))
	if r, m := td.call(t, "vm_type", map[string]any{"text": "x", "handle": 10}); !r.IsError || m["error"] != codeSessionUnusable {
		t.Errorf("not console: %v", m)
	}
	// No agent at all (sign-in screen before any logon): raw input goes through.
	td = newTestDeps(t, offlineAgent())
	if r, m := td.call(t, "vm_click", map[string]any{"x": 5, "y": 6, "observe_after": "none"}); r.IsError || td.b.events() != "click 5,6 b1 c1 []" {
		t.Errorf("offline raw click: %v %q", m, td.b.events())
	}
	if r, m := td.call(t, "vm_key", map[string]any{"sequence": []string{"ctrl", "enter"}}); r.IsError || m["combinations"] != float64(2) || td.b.events() != "click 5,6 b1 c1 [],press ctrl,press enter" {
		t.Errorf("offline raw keys: %v %q", m, td.b.events())
	}
}

func TestClickCovered(t *testing.T) {
	// A shell overlay list_windows does not show is described from window_at.
	f := newAgent(0)
	f.results[proto.OpWindowAt] = proto.HandleResult{Handle: 131160, Class: "Shell_LightDismissOverlay", PID: 5488, Process: "explorer.exe"}
	td := newTestDeps(t, f)
	id := td.put(options(), 1, nil)
	r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1})
	if !r.IsError || m["error"] != codeCovered || td.b.events() != "" {
		t.Fatalf("covered: %v", m)
	}
	if w := m["window"].(map[string]any); w["handle"] != float64(131160) || w["class"] != "Shell_LightDismissOverlay" || w["process"] != "explorer.exe" || !strings.Contains(m["next"].(string), "vm_key esc") {
		t.Errorf("covering window: %v", m)
	}
	// A listed window of another process: act on it.
	td = newTestDeps(t, newAgent(30))
	id = td.put(options(), 1, nil)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1}); !r.IsError || m["error"] != codeCovered || !strings.HasPrefix(m["next"].(string), "act on handle 30 first") {
		t.Errorf("listed cover: %v", m)
	}
}

func TestClickDisabledTarget(t *testing.T) {
	// The Options dialog (10) is foreground and disables its owner (20): a click into 20 names 10.
	td := newTestDeps(t, newAgent(20))
	main := testWindows()[1]
	id := td.put(&main, 1, nil)
	r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 900, "y": 900})
	if !r.IsError || m["error"] != codeTargetDisabled || m["act_on"] != float64(10) || m["next"] != "act on handle 10 first" || td.b.events() != "" {
		t.Errorf("disabled: %v", m)
	}
}

// controlAgent answers control_action with result or err and lists the Options dialog in the foreground.
func controlAgent(result proto.ControlActionResult, err error) *fakeCall {
	f := newAgent(10)
	f.results[proto.OpControlAction] = result
	if err != nil {
		f.errs = map[string]error{proto.OpControlAction: err}
	}
	return f
}

func controlNodes() []proto.ControlInfo {
	return []proto.ControlInfo{
		{Index: 0, Parent: -1, PID: 100, Rect: options().Rect, RuntimeID: "42.1"},
		{Index: 1, Parent: 0, PID: 100, Name: "Name", ControlType: 50004, Rect: proto.Rect{Left: 200, Top: 100, Right: 300, Bottom: 140}, RuntimeID: "42.7", Patterns: []string{"Invoke", "Expand"}},
	}
}

func TestControlActionStateResult(t *testing.T) {
	for _, tt := range []struct {
		name   string
		action string
		result proto.ControlActionResult
		want   map[string]any
	}{
		{"toggle verified", "Toggle", proto.ControlActionResult{Verified: new(true), State: &proto.ControlState{Toggle: new("on"), Offscreen: new(false)}}, map[string]any{"toggle": "on", "offscreen": false}},
		{"toggle unchanged", "Toggle", proto.ControlActionResult{Verified: new(false), State: &proto.ControlState{Toggle: new("off")}}, map[string]any{"toggle": "off"}},
		{"invoke state without business verification", "Invoke", proto.ControlActionResult{State: &proto.ControlState{Selected: new(false)}}, map[string]any{"selected": false}},
		{"unsupported state remains null", "Invoke", proto.ControlActionResult{}, nil},
		{"set value carries read only false", "SetValue", proto.ControlActionResult{Verified: new(true), Value: "abc", HasValue: true, State: &proto.ControlState{ReadOnly: new(false)}}, map[string]any{"read_only": false}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := controlAgent(tt.result, nil)
			td := newTestDeps(t, f)
			id := td.put(options(), 1, controlNodes())
			tool := "vm_invoke"
			args := map[string]any{"observation_id": id, "index": 1, "observe_after": "none"}
			if tt.action == "SetValue" {
				tool, args["value"] = "vm_set_value", "abc"
			} else {
				args["action"] = tt.action
			}
			r, m := td.call(t, tool, args)
			if r.IsError || m["ok"] != true {
				t.Fatalf("action failed: %v", m)
			}
			if verified, exists := m["verified"]; !exists || (tt.result.Verified == nil && verified != nil) || (tt.result.Verified != nil && verified != *tt.result.Verified) {
				t.Fatalf("verified changed or omitted: %v", m)
			}
			state, exists := m["state"]
			if !exists || (tt.want == nil && state != nil) {
				t.Fatalf("unknown state must remain explicit null: %v", m)
			}
			if tt.want != nil && !reflect.DeepEqual(state, tt.want) {
				t.Fatalf("state = %#v, want %#v", state, tt.want)
			}
			actions := 0
			for _, op := range f.ops {
				if op == proto.OpControlAction {
					actions++
				}
			}
			if actions != 1 {
				t.Fatalf("semantic action executed %d times", actions)
			}
		})
	}
}

func TestSetValue(t *testing.T) {
	yes := true
	f := controlAgent(proto.ControlActionResult{Value: "abc", HasValue: true, Verified: &yes}, nil)
	td := newTestDeps(t, f)
	id := td.put(options(), 1, controlNodes())
	r, m := td.call(t, "vm_set_value", map[string]any{"observation_id": id, "index": 1, "value": "abc", "observe_after": "controls"})
	if r.IsError || m["ok"] != true || m["verified"] != true || m["value"] != "abc" || m["window"].(map[string]any)["handle"] != float64(10) {
		t.Fatalf("result %v", m)
	}
	if a := f.args[slices.Index(f.ops, proto.OpControlAction)].(proto.ControlActionArgs); !reflect.DeepEqual(a, proto.ControlActionArgs{Handle: 10, PID: 100, RuntimeID: "42.7", Action: "SetValue", Value: "abc", HintRect: &controlNodes()[1].Rect}) {
		t.Errorf("control_action args %+v", a)
	}
	// observe_after controls: the tree of the observed window, no image.
	if len(r.Content) != 1 || len(td.observed) != 1 || td.observed[0].Handle != 10 || !td.observed[0].Controls || *td.observed[0].Screenshot {
		t.Errorf("after: %d items, observed %+v", len(r.Content), td.observed)
	}
	// Error mapping.
	for _, tt := range []struct {
		err  string
		code string
	}{
		{"element not found: runtime id 42.7", codeStaleElement},
		{"unsupported pattern: SetValue; supported: Invoke, Expand", codeUnsupportedPattern},
		{"COM error 0x80070005", codeFailed},
		{"SetValue failed: UI Automation method 3: HRESULT 0x80131509", codeFailed},
		{"connect to agent: connection refused", codeAgentRequired},
		{"agent: unexpected EOF", codeAgentRequired},
	} {
		td := newTestDeps(t, controlAgent(proto.ControlActionResult{}, errors.New(tt.err)))
		id := td.put(options(), 1, controlNodes())
		r, m := td.call(t, "vm_set_value", map[string]any{"observation_id": id, "index": 1, "value": "abc"})
		if !r.IsError || m["error"] != tt.code || len(td.observed) != 0 {
			t.Errorf("%q: %v", tt.err, m)
		}
		if tt.code == codeUnsupportedPattern {
			if s, _ := m["supported"].([]any); len(s) != 2 || s[0] != "Invoke" || s[1] != "Expand" {
				t.Errorf("supported: %v", m["supported"])
			}
		}
	}
	// A hung target: the call is bounded and reported as not responding.
	old := controlActionTimeout
	controlActionTimeout = 50 * time.Millisecond
	t.Cleanup(func() { controlActionTimeout = old })
	f = controlAgent(proto.ControlActionResult{}, nil)
	f.fn = map[string]func(any) (any, error){proto.OpControlAction: func(any) (any, error) {
		time.Sleep(200 * time.Millisecond)
		return nil, context.DeadlineExceeded
	}}
	td = newTestDeps(t, f)
	id = td.put(options(), 1, controlNodes())
	if r, m := td.call(t, "vm_set_value", map[string]any{"observation_id": id, "index": 1, "value": "abc"}); !r.IsError || m["error"] != codeTargetNotResponding {
		t.Errorf("hung: %v", m)
	}
	// A whole-screen observation has no window for the agent to search.
	td = newTestDeps(t, controlAgent(proto.ControlActionResult{}, nil))
	id = td.put(nil, 1, controlNodes())
	if r, m := td.call(t, "vm_set_value", map[string]any{"observation_id": id, "index": 1, "value": "abc"}); !r.IsError || m["error"] != codeInvalidArgument {
		t.Errorf("whole screen: %v", m)
	}
}

func TestProviderErrorsDoNotImplyAgentOffline(t *testing.T) {
	const providerError = "Invoke failed: UI Automation method 3: HRESULT 0x80040200"
	td := newTestDeps(t, controlAgent(proto.ControlActionResult{}, errors.New(providerError)))
	id := td.put(options(), 1, controlNodes())
	r, result := td.call(t, "vm_invoke", map[string]any{"observation_id": id, "index": 1, "action": "Invoke"})
	if !r.IsError || result["error"] != codeFailed || result["reason"] != providerError || strings.Contains(result["next"].(string), "vm_install_agent") {
		t.Fatalf("provider response misclassified as offline: %v", result)
	}
	r, result = td.call(t, "vm_key", map[string]any{"handle": 10, "keys": "ctrl", "observe_after": "none"})
	if r.IsError || result["ok"] != true {
		t.Fatalf("responding agent unusable after provider refusal: %v", result)
	}

	// A responding agent whose UIA query failed must not silently fall back to
	// untargeted Hyper-V keyboard input as though it were unreachable.
	f := newAgent(10)
	f.errs = map[string]error{proto.OpListWindows: errors.New("focused control: UI Automation method 8 failed")}
	td = newTestDeps(t, f)
	r, result = td.call(t, "vm_type", map[string]any{"text": "do not inject", "observe_after": "none"})
	if !r.IsError || result["error"] != codeFailed || td.b.events() != "" {
		t.Fatalf("provider failure used raw input fallback: %v; events %q", result, td.b.events())
	}
}

func TestInvoke(t *testing.T) {
	f := controlAgent(proto.ControlActionResult{}, nil)
	td := newTestDeps(t, f)
	id := td.put(options(), 1, controlNodes())
	r, m := td.call(t, "vm_invoke", map[string]any{"observation_id": id, "index": 1, "action": "toggle", "observe_after": "none"})
	if r.IsError || m["ok"] != true || m["verified"] != nil || m["value"] != nil {
		t.Fatalf("result %v", m)
	}
	if a := f.args[slices.Index(f.ops, proto.OpControlAction)].(proto.ControlActionArgs); a.Action != "Toggle" || a.Value != "" {
		t.Errorf("args %+v", a)
	}
	for _, action := range []string{"setvalue", "Click"} {
		if r, m := td.call(t, "vm_invoke", map[string]any{"observation_id": id, "index": 1, "action": action}); !r.IsError || m["error"] != codeInvalidArgument {
			t.Errorf("%q: %v", action, m)
		}
	}
	if f.count(proto.OpControlAction) != 1 {
		t.Errorf("invalid actions reached the agent: %v", f.ops)
	}
}

func TestInvokeDefaultAction(t *testing.T) {
	// One invocable action (SetValue belongs to vm_set_value): an omitted action uses it.
	f := controlAgent(proto.ControlActionResult{}, nil)
	td := newTestDeps(t, f)
	nodes := controlNodes()
	nodes[1].Actions = []string{"Invoke", "SetValue"}
	id := td.put(options(), 1, nodes)
	if r, m := td.call(t, "vm_invoke", map[string]any{"observation_id": id, "index": 1, "observe_after": "none"}); r.IsError || m["ok"] != true {
		t.Fatalf("single action: %v", m)
	}
	if a := f.args[slices.Index(f.ops, proto.OpControlAction)].(proto.ControlActionArgs); a.Action != "Invoke" || a.RuntimeID != "42.7" {
		t.Errorf("args %+v", a)
	}

	// Several actions (Invoke and Expand from the patterns) or none: refused before the agent, listing them.
	for _, tt := range []struct {
		name    string
		actions []string
		want    []any
		next    string
	}{
		{"several", nil, []any{"Invoke", "Expand"}, "Invoke, Expand"},
		{"only SetValue", []string{"SetValue"}, []any{"SetValue"}, "vm_set_value"},
		{"none", []string{}, []any{}, "vm_click"},
	} {
		f := controlAgent(proto.ControlActionResult{}, nil)
		td := newTestDeps(t, f)
		nodes := controlNodes()
		nodes[1].Actions = tt.actions
		id := td.put(options(), 1, nodes)
		r, m := td.call(t, "vm_invoke", map[string]any{"observation_id": id, "index": 1, "observe_after": "none"})
		if !r.IsError || m["error"] != codeInvalidArgument || !reflect.DeepEqual(m["supported"], tt.want) || !strings.Contains(m["next"].(string), tt.next) {
			t.Errorf("%s: %v", tt.name, m)
		}
		if f.count(proto.OpControlAction) != 0 {
			t.Errorf("%s: refused default reached the agent: %v", tt.name, f.ops)
		}
	}
}

func TestInvokeSemanticScroll(t *testing.T) {
	state := &proto.ControlState{HorizontallyScrollable: new(false), VerticallyScrollable: new(true), VerticalScrollPercent: new(0.0)}
	for _, action := range []string{"ScrollUp", "ScrollDown", "ScrollLeft", "ScrollRight"} {
		t.Run(action, func(t *testing.T) {
			f := controlAgent(proto.ControlActionResult{State: state, Verified: new(false)}, nil)
			td := newTestDeps(t, f)
			id := td.put(options(), 1, controlNodes())
			r, m := td.call(t, "vm_invoke", map[string]any{"observation_id": id, "index": 1, "action": strings.ToLower(action), "observe_after": "none"})
			if r.IsError || m["ok"] != true || m["verified"] != false {
				t.Fatalf("scroll result: %v", m)
			}
			want := map[string]any{"horizontally_scrollable": false, "vertically_scrollable": true, "vertical_scroll_percent": float64(0)}
			if !reflect.DeepEqual(m["state"], want) {
				t.Fatalf("scroll state lost false or zero: %v", m["state"])
			}
			if f.count(proto.OpControlAction) != 1 {
				t.Fatalf("scroll repeated: %v", f.ops)
			}
			if a := f.args[slices.Index(f.ops, proto.OpControlAction)].(proto.ControlActionArgs); a.Action != action || a.RuntimeID != "42.7" || a.Value != "" {
				t.Fatalf("scroll did not use canonical semantic target: %+v", a)
			}
		})
	}
	// The unsupported action response reports fresh usable directions, not the cached pattern.
	f := controlAgent(proto.ControlActionResult{}, errors.New("unsupported pattern: ScrollLeft; supported: ScrollUp, ScrollDown, ScrollLeft, ScrollRight"))
	td := newTestDeps(t, f)
	id := td.put(options(), 1, controlNodes())
	r, m := td.call(t, "vm_invoke", map[string]any{"observation_id": id, "index": 1, "action": "ScrollLeft", "observe_after": "none"})
	want := []any{"ScrollUp", "ScrollDown", "ScrollLeft", "ScrollRight"}
	if !r.IsError || m["error"] != codeUnsupportedPattern || !reflect.DeepEqual(m["supported"], want) {
		t.Fatalf("scroll supported list: %v", m)
	}
}

func TestObserveAfter(t *testing.T) {
	for _, tt := range []struct {
		mode       string
		shot, tree bool
		items      int
	}{
		{"screenshot", true, false, 2}, {"controls", false, true, 1}, {"both", true, true, 2},
	} {
		td := newTestDeps(t, newAgent(10))
		id := td.put(options(), 1, nil)
		r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1, "observe_after": tt.mode, "settle_ms": 1})
		if r.IsError || len(r.Content) != tt.items || m["after"] == nil {
			t.Errorf("%s: %d items %v", tt.mode, len(r.Content), m)
		}
		if len(td.observed) != 1 || *td.observed[0].Screenshot != tt.shot || td.observed[0].Controls != tt.tree || td.observed[0].VM != "A" {
			t.Errorf("%s: observed %+v", tt.mode, td.observed)
		}
	}
	// none: no observation; invalid values are refused before anything happens.
	td := newTestDeps(t, newAgent(10))
	id := td.put(options(), 1, nil)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1, "observe_after": "none"}); r.IsError || len(r.Content) != 1 || m["after"] != nil || len(td.observed) != 0 {
		t.Errorf("none: %v %+v", m, td.observed)
	}
	for _, args := range []map[string]any{{"observe_after": "tree"}, {"settle_ms": 6000}, {"settle_ms": -1}} {
		args["x"], args["y"] = 1, 1
		if r, m := td.call(t, "vm_click", args); !r.IsError || m["error"] != codeInvalidArgument {
			t.Errorf("%v: %v", args, m)
		}
	}
	// A failed action never observes; a failed observation does not hide a performed action.
	td = newTestDeps(t, newAgent(30))
	id = td.put(options(), 1, nil)
	if r, _ := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1}); !r.IsError || len(td.observed) != 0 {
		t.Errorf("failed action observed: %+v", td.observed)
	}
	td = newTestDeps(t, newAgent(10))
	td.deps.observe = func(context.Context, observeIn) (*observeOut, []byte, error) {
		return nil, nil, errors.New("screenshot failed")
	}
	id = td.put(options(), 1, nil)
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 1, "y": 1, "settle_ms": 1}); r.IsError || m["ok"] != true || !strings.HasPrefix(m["after_error"].(string), "failed: screenshot failed") || td.b.events() == "" {
		t.Errorf("observe failure: %v", m)
	}
}

func TestDragAndScroll(t *testing.T) {
	td := newTestDeps(t, newAgent(10))
	id := td.put(options(), 0.5, nil)
	r, m := td.call(t, "vm_drag", map[string]any{"observation_id": id, "from": map[string]any{"x": 0, "y": 0}, "to": map[string]any{"x": 10, "y": 20}, "modifiers": []string{"ctrl"}, "observe_after": "none"})
	if r.IsError || td.b.events() != "drag 101,51-121,91 [ctrl]" || m["window"].(map[string]any)["handle"] != float64(10) {
		t.Errorf("drag: %v %q", m, td.b.events())
	}
	// Raw screen pixels without an observation; a bad modifier is refused first.
	td = newTestDeps(t, offlineAgent())
	if r, _ := td.call(t, "vm_drag", map[string]any{"from": map[string]any{"x": 1, "y": 2}, "to": map[string]any{"x": 3, "y": 4}, "observe_after": "none"}); r.IsError || td.b.events() != "drag 1,2-3,4 []" {
		t.Errorf("raw drag: %q", td.b.events())
	}
	if r, m := td.call(t, "vm_drag", map[string]any{"from": map[string]any{"x": 1, "y": 2}, "to": map[string]any{"x": 3, "y": 4}, "modifiers": []string{"meta"}}); !r.IsError || m["error"] != codeInvalidArgument {
		t.Errorf("bad modifier: %v", m)
	}
	// Scroll: vertical through Hyper-V, horizontal through the agent.
	f := newAgent(10)
	td = newTestDeps(t, f)
	id = td.put(options(), 1, nil)
	if r, m := td.call(t, "vm_scroll", map[string]any{"observation_id": id, "x": 5, "y": 5, "delta_y": 3, "delta_x": -2, "observe_after": "none"}); r.IsError || td.b.events() != "scroll 105,55 3" {
		t.Errorf("scroll: %v %q", m, td.b.events())
	}
	if h := f.args[slices.Index(f.ops, proto.OpHScroll)].(proto.HScrollArgs); h != (proto.HScrollArgs{X: 105, Y: 55, Delta: -2}) {
		t.Errorf("hscroll args %+v", h)
	}
	td = newTestDeps(t, offlineAgent())
	if r, m := td.call(t, "vm_scroll", map[string]any{"x": 5, "y": 5, "delta_x": 1}); !r.IsError || m["error"] != codeAgentRequired {
		t.Errorf("hscroll offline: %v", m)
	}
	if r, m := td.call(t, "vm_scroll", map[string]any{"x": 5, "y": 5}); !r.IsError || m["error"] != codeInvalidArgument {
		t.Errorf("no delta: %v", m)
	}
}

func TestTypeModes(t *testing.T) {
	// With the agent: type_keys to the selected window.
	f := newAgent(10)
	f.results[proto.OpTypeKeys] = proto.TypeKeysResult{Events: 6}
	td := newTestDeps(t, f)
	r, m := td.call(t, "vm_type", map[string]any{"text": "ab\n", "handle": 10, "observe_after": "screenshot", "settle_ms": 1})
	if r.IsError || m["applied_chars"] != float64(3) || m["total_chars"] != float64(3) || m["window"].(map[string]any)["handle"] != float64(10) || m["after"] == nil {
		t.Fatalf("type: %v", m)
	}
	if a := f.args[slices.Index(f.ops, proto.OpTypeKeys)].(proto.TypeKeysArgs); a != (proto.TypeKeysArgs{Text: "ab\n", Handle: 10, PID: 100}) || td.b.events() != "" {
		t.Errorf("type_keys args %+v, events %q", a, td.b.events())
	}
	if len(td.observed) != 1 || td.observed[0].Handle != 0 {
		t.Errorf("after without an observation observes the screen: %+v", td.observed)
	}
	// Partial: the agent's event count becomes applied_chars.
	f = newAgent(10)
	f.errs = map[string]error{proto.OpTypeKeys: errors.New("keyboard input stopped after 5 injected events; text may be partial, do not retry automatically: SendInput inserted 1/2 events")}
	td = newTestDeps(t, f)
	if r, m := td.call(t, "vm_type", map[string]any{"text": "héllo", "handle": 10}); !r.IsError || m["error"] != codePartialInput || m["applied_chars"] != float64(2) || m["total_chars"] != float64(5) {
		t.Errorf("partial: %v", m)
	}
	// Without the agent: ASCII through the Hyper-V keyboard, other text refused.
	td = newTestDeps(t, offlineAgent())
	if r, m := td.call(t, "vm_type", map[string]any{"text": "dir\n"}); r.IsError || td.b.events() != "type dir\n" || m["applied_chars"] != float64(4) || m["window"] != nil {
		t.Errorf("offline ASCII: %v %q", m, td.b.events())
	}
	if r, m := td.call(t, "vm_type", map[string]any{"text": "汉字"}); !r.IsError || m["error"] != codeAgentRequired || td.b.events() != "type dir\n" {
		t.Errorf("offline non-ASCII: %v", m)
	}
	// A selector needs the agent.
	if r, m := td.call(t, "vm_type", map[string]any{"text": "x", "handle": 10}); !r.IsError || m["error"] != codeAgentRequired {
		t.Errorf("offline selector: %v", m)
	}
	// index + observation_id: the control is clicked, then the text goes to the window the click reached.
	f = newAgent(10)
	f.results[proto.OpTypeKeys] = proto.TypeKeysResult{Events: 2}
	td = newTestDeps(t, f)
	id := td.put(options(), 1, controlNodes())
	if r, m := td.call(t, "vm_type", map[string]any{"text": "x", "observation_id": id, "index": 1}); r.IsError || td.b.events() != "click 250,120 b1 c1 []" || m["window"].(map[string]any)["handle"] != float64(10) {
		t.Errorf("index: %v %q", m, td.b.events())
	}
	for _, args := range []map[string]any{{"text": "x", "index": 1}, {"text": "x", "observation_id": id}, {"text": "x", "observation_id": id, "index": 1, "handle": 10}, {"text": ""}} {
		if r, m := td.call(t, "vm_type", args); !r.IsError || m["error"] != codeInvalidArgument {
			t.Errorf("%v: %v", args, m)
		}
	}
}

func TestAppliedChars(t *testing.T) {
	for _, tt := range []struct {
		text   string
		events int
		want   int
	}{
		{"abc", 6, 3}, {"abc", 5, 2}, {"abc", 0, 0}, {"a😀b", 2, 1}, {"a😀b", 5, 1}, {"a😀b", 6, 2}, {"a😀b", 8, 3},
	} {
		if got := appliedChars(tt.text, tt.events); got != tt.want {
			t.Errorf("%q after %d events: %d, want %d", tt.text, tt.events, got, tt.want)
		}
	}
	if n := injectedEvents(errors.New("keyboard input stopped after 14 injected events; text may be partial")); n != 14 {
		t.Errorf("events %d", n)
	}
}

func TestKeyPartialSequence(t *testing.T) {
	td := newTestDeps(t, newAgent(10))
	r, m := td.call(t, "vm_key", map[string]any{"handle": 10, "sequence": []string{"ctrl+a", "x"}})
	if r.IsError || m["ok"] != true || m["combinations"] != float64(2) || m["window"].(map[string]any)["handle"] != float64(10) || td.b.events() != "press ctrl+a,press x" {
		t.Fatalf("sequence: %v %q", m, td.b.events())
	}
	// The Hyper-V keyboard fails on the first combination: nothing was sent, so the error is the plain failure.
	td = newTestDeps(t, newAgent(10))
	td.b.fail = errors.New("WMI: device busy")
	if r, m := td.call(t, "vm_key", map[string]any{"keys": "enter"}); !r.IsError || m["error"] != codeFailed || m["applied"] != nil {
		t.Errorf("first fails: %v", m)
	}
	// It fails on the second combination: partial_input with what was sent.
	td = newTestDeps(t, newAgent(10))
	td.b.fail, td.b.failOn = errors.New("WMI: device busy"), "press x"
	if r, m := td.call(t, "vm_key", map[string]any{"handle": 10, "sequence": []string{"ctrl+a", "x", "enter"}}); !r.IsError || m["error"] != codePartialInput || m["applied"] != float64(1) || m["total"] != float64(3) || td.b.events() != "press ctrl+a" {
		t.Errorf("second fails: %v %q", m, td.b.events())
	}
}

// screenObservation stores a whole-screen observation whose control tree (nodes) belongs to tree, as vm_observe does
// for controls: true without a handle.
func (td *testDeps) screenObservation(tree *proto.WindowInfo, nodes []proto.ControlInfo) string {
	o := &observation{VM: "A", Scale: 1, HasImage: true, SourcePNG: actionScreenshot(), Nodes: nodes, TreeWindow: tree, Crop: screenshotRegion{Width: 1920, Height: 1080}, OutputWidth: 1920, OutputHgt: 1080}
	td.obs.put(o)
	return o.ID
}

func TestScreenObservationWithControls(t *testing.T) {
	// Clicking by coordinates on a whole-screen observation has no window checks, even when its tree belongs to the
	// foreground window: another window on the screenshot is simply clicked.
	f := controlAgent(proto.ControlActionResult{Rect: &proto.Rect{Left: 300, Top: 200, Right: 400, Bottom: 240}}, nil)
	f.results[proto.OpWindowAt] = proto.HandleResult{Handle: 30}
	td := newTestDeps(t, f)
	id := td.screenObservation(options(), controlNodes())
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "x": 700, "y": 700, "observe_after": "none"}); r.IsError || td.b.events() != "click 700,700 b1 c1 []" || m["window"] != nil || slices.Contains(td.f.ops, proto.OpWindowAt) {
		t.Errorf("screen click: %v %q", m, td.b.events())
	}
	// By index the tree's window is the target: it is checked, the control re-located (Locate), and the click lands on
	// the control's current rectangle, not the one of the observation.
	f = controlAgent(proto.ControlActionResult{Rect: &proto.Rect{Left: 300, Top: 200, Right: 400, Bottom: 240}}, nil)
	td = newTestDeps(t, f)
	id = td.screenObservation(options(), controlNodes())
	r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "index": 1, "observe_after": "none"})
	if r.IsError || td.b.events() != "click 350,220 b1 c1 []" || m["window"].(map[string]any)["handle"] != float64(10) {
		t.Errorf("index click: %v %q", m, td.b.events())
	}
	if i := slices.Index(f.ops, proto.OpControlAction); i < 0 || !reflect.DeepEqual(f.args[i].(proto.ControlActionArgs), proto.ControlActionArgs{Handle: 10, PID: 100, RuntimeID: "42.7", Action: "Locate", HintRect: &controlNodes()[1].Rect}) {
		t.Errorf("locate args: %v", f.args)
	}
	// A control that vanished since the observation is stale_element, nothing is clicked.
	td = newTestDeps(t, controlAgent(proto.ControlActionResult{}, errors.New("element not found: runtime id 42.7")))
	id = td.screenObservation(options(), controlNodes())
	if r, m := td.call(t, "vm_click", map[string]any{"observation_id": id, "index": 1}); !r.IsError || m["error"] != codeStaleElement || td.b.events() != "" {
		t.Errorf("vanished: %v", m)
	}
	// Control actions work on a whole-screen observation through its tree window.
	td = newTestDeps(t, controlAgent(proto.ControlActionResult{Value: "v", HasValue: true}, nil))
	id = td.screenObservation(options(), controlNodes())
	if r, m := td.call(t, "vm_invoke", map[string]any{"observation_id": id, "index": 1, "action": "invoke", "observe_after": "none"}); r.IsError || m["value"] != "v" || m["window"].(map[string]any)["handle"] != float64(10) {
		t.Errorf("invoke on screen observation: %v", m)
	}
	// The agent's own UI Automation timeout is target_not_responding, not agent_required.
	td = newTestDeps(t, controlAgent(proto.ControlActionResult{}, errors.New("UI Automation interrupted: context deadline exceeded")))
	id = td.screenObservation(options(), controlNodes())
	if r, m := td.call(t, "vm_set_value", map[string]any{"observation_id": id, "index": 1, "value": "x"}); !r.IsError || m["error"] != codeTargetNotResponding {
		t.Errorf("agent UIA timeout: %v", m)
	}
}

func TestTypeByIndexReadsBack(t *testing.T) {
	f := controlAgent(proto.ControlActionResult{Value: "one\r\nLINE", HasValue: true}, nil) // edit controls store CRLF
	f.results[proto.OpTypeKeys] = proto.TypeKeysResult{Events: 18}
	td := newTestDeps(t, f)
	id := td.put(options(), 1, controlNodes())
	r, m := td.call(t, "vm_type", map[string]any{"text": "one\nLINE\n", "observation_id": id, "index": 1})
	if r.IsError || m["verified"] != true || m["value"] != "one\r\nLINE" || m["applied_chars"] != float64(9) {
		t.Errorf("verified: %v", m)
	}
	// The application dropped the text: verified is false and the value tells what is there.
	f = controlAgent(proto.ControlActionResult{Value: "", HasValue: true}, nil)
	f.results[proto.OpTypeKeys] = proto.TypeKeysResult{Events: 8}
	td = newTestDeps(t, f)
	id = td.put(options(), 1, controlNodes())
	if r, m := td.call(t, "vm_type", map[string]any{"text": "LINE", "observation_id": id, "index": 1}); r.IsError || m["verified"] != false || m["value"] != "" {
		t.Errorf("dropped: %v", m)
	}
	// Nothing typed at all is a plain failure, not partial_input.
	f = newAgent(10)
	f.errs = map[string]error{proto.OpTypeKeys: errors.New("keyboard input stopped after 0 injected events; text may be partial: key 0x10 is already held")}
	td = newTestDeps(t, f)
	if r, m := td.call(t, "vm_type", map[string]any{"text": "abc", "handle": 10}); !r.IsError || m["error"] != codeFailed || m["applied_chars"] != float64(0) {
		t.Errorf("nothing typed: %v", m)
	}
}
