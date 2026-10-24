package host

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

func testWindows() []proto.WindowInfo {
	return []proto.WindowInfo{
		{Handle: 10, PID: 100, Title: "Options", Process: "acad.exe", Rect: proto.Rect{Left: 100, Top: 50, Right: 600, Bottom: 450}, Enabled: true, Foreground: true, Owner: 20, Modal: true},
		{Handle: 20, PID: 100, Title: "Autodesk AutoCAD 2015 - [a.dwg]", Process: "acad.exe", Rect: proto.Rect{Right: 1920, Bottom: 1040}},
		{Handle: 30, PID: 200, Title: "Autodesk AutoCAD 2015 - [b.dwg]", Process: "acad.exe", Rect: proto.Rect{Right: 1920, Bottom: 1040}, Enabled: true},
	}
}

// code returns err's toolError code ("" for nil).
func code(err error) string {
	if err == nil {
		return ""
	}
	return asToolError(err).Code
}

// field returns a field of err's toolError.
func field(err error, name string) any {
	if err == nil {
		return nil
	}
	return asToolError(err).Fields[name]
}

func TestResolveWindow(t *testing.T) {
	ws := testWindows()
	for _, tt := range []struct {
		name string
		sel  windowSelector
		want uint64
		code string
	}{
		{"handle", windowSelector{Handle: 30}, 30, ""},
		{"handle and PID", windowSelector{Handle: 10, PID: 100}, 10, ""},
		{"PID only", windowSelector{PID: 200}, 30, ""},
		{"missing handle", windowSelector{Handle: 99}, 0, codeNoWindow},
		{"PID excludes handle", windowSelector{Handle: 30, PID: 100}, 0, codeNoWindow},
		{"missing PID", windowSelector{PID: 999}, 0, codeNoWindow},
		{"ambiguous PID", windowSelector{PID: 100}, 0, codeAmbiguousTarget},
		{"empty", windowSelector{}, 0, codeInvalidArgument},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, err := resolveWindow(ws, tt.sel)
			if code(err) != tt.code || w.Handle != tt.want {
				t.Fatalf("want handle %d code %q, got %+v %v", tt.want, tt.code, w, err)
			}
		})
	}
	// An ambiguous PID lists the candidate handles for the next call.
	_, err := resolveWindow(ws, windowSelector{PID: 100})
	if hs, _ := field(err, "handles").([]uint64); !slices.Equal(hs, []uint64{10, 20}) {
		t.Errorf("ambiguous handles: %v (%v)", hs, err)
	}
}

func TestCheckUsable(t *testing.T) {
	ws := testWindows()
	if err := checkUsable(ws, ws[0]); err != nil {
		t.Errorf("foreground: %v", err)
	}
	// Not foreground: activate_failed names the actual foreground window.
	err := checkUsable(ws, ws[2])
	if code(err) != codeActivateFailed || field(err, "foreground").(*windowRef).Handle != 10 {
		t.Errorf("background: %v %v", err, field(err, "foreground"))
	}
	// Foreground but disabled by its modal dialog: the dialog is the window to act on.
	w := ws[1]
	w.Foreground = true
	err = checkUsable(ws, w)
	if code(err) != codeTargetDisabled || field(err, "act_on") != uint64(10) || asToolError(err).Next != "act on handle 10 first" {
		t.Errorf("disabled: %v %v", err, asToolError(err).Fields)
	}
}

func TestCheckHit(t *testing.T) {
	ws := testWindows()
	if hit, err := checkHit(ws, ws[0], 110, 70, proto.HandleResult{Handle: 10}); err != nil || hit.Handle != 10 {
		t.Errorf("hit: %+v %v", hit, err)
	}
	_, err := checkHit(ws, ws[0], 110, 70, proto.HandleResult{Handle: 30})
	if code(err) != codeCovered || field(err, "window").(map[string]any)["handle"] != uint64(30) {
		t.Errorf("covered: %v %v", err, field(err, "window"))
	}
	if _, err := checkHit(ws, ws[0], 110, 70, proto.HandleResult{}); code(err) != codeInvalidArgument {
		t.Errorf("off screen: %v", err)
	}
}

// fakeCall answers ops from canned results (JSON-encoded), errors or per-op functions (fn wins) and records the ops it
// was asked with their typed args.
type fakeCall struct {
	results map[string]any
	errs    map[string]error
	fn      map[string]func(args any) (any, error)
	ops     []string
	args    []any
}

func (f *fakeCall) call(ctx context.Context, _, op string, args any, _ []byte, result any) ([]byte, error) {
	f.ops, f.args = append(f.ops, op), append(f.args, args)
	var r any
	if h := f.fn[op]; h != nil {
		var err error
		if r, err = h(args); err != nil {
			return nil, err
		}
	} else if err := f.errs[op]; err != nil {
		return nil, err
	} else {
		r = f.results[op]
	}
	if r != nil && result != nil {
		b, _ := json.Marshal(r)
		return nil, json.Unmarshal(b, result)
	}
	return nil, nil
}

// count returns how often op was asked.
func (f *fakeCall) count(op string) int {
	n := 0
	for _, o := range f.ops {
		if o == op {
			n++
		}
	}
	return n
}

// newAgent answers list_windows with testWindows (window 10 in the foreground) and window_at with at.
func newAgent(at uint64) *fakeCall {
	return &fakeCall{results: map[string]any{
		proto.OpListWindows: proto.WindowsResult{Windows: testWindows(), Foreground: 10, Session: &proto.SessionStateResult{Console: true}},
		proto.OpWindowAt:    proto.HandleResult{Handle: at},
		proto.OpFocusWindow: proto.FocusResult{Text: "Options", Handle: 10},
	}}
}

// offlineAgent fails every op the way an unreachable agent does.
func offlineAgent() *fakeCall {
	return &fakeCall{fn: map[string]func(any) (any, error){}, errs: map[string]error{
		proto.OpListWindows: errors.New("agent: connect to agent: no such VM socket"),
		proto.OpWindowAt:    errors.New("agent: connect to agent: no such VM socket"),
		proto.OpTypeKeys:    errors.New("agent: connect to agent: no such VM socket"),
		proto.OpHScroll:     errors.New("agent: connect to agent: no such VM socket"),
	}}
}

// resultText returns the JSON text item of a tool result (the last content item: an image may precede it).
func resultText(r *mcp.CallToolResult) string {
	return r.Content[len(r.Content)-1].(*mcp.TextContent).Text
}

// resultJSON decodes the JSON text item of a tool result.
func resultJSON(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(resultText(r)), &m); err != nil {
		t.Fatalf("result is not JSON: %q", resultText(r))
	}
	return m
}

func TestListWindowsOldAgent(t *testing.T) {
	f := &fakeCall{errs: map[string]error{proto.OpListWindows: errors.New(`unknown op "list_windows"`)}}
	if _, err := listWindows(context.Background(), f.call, ""); code(err) != codeAgentOutdated {
		t.Errorf("list: %v", err)
	}
}

func TestResolveWindowHostedPID(t *testing.T) {
	ws := []proto.WindowInfo{{Handle: 2, PID: 900, Process: "ApplicationFrameHost.exe", Class: "ApplicationFrameWindow", HostedPID: 6100}, {Handle: 3, PID: 7000}}
	for _, s := range []windowSelector{{Handle: 2, PID: 6100}, {PID: 6100}, {Handle: 2, PID: 900}, {Handle: 2}} {
		if w, err := resolveWindow(ws, s); err != nil || w.Handle != 2 {
			t.Errorf("%+v: %+v %v", s, w, err)
		}
	}
	if _, err := resolveWindow(ws, windowSelector{Handle: 3, PID: 6100}); err == nil {
		t.Error("hosted pid selected another window")
	}
	// A minimized UWP app: its frame hosts nothing, so handle and the app's pid still select the frame by handle.
	ws[0].HostedPID = 0
	if w, err := resolveWindow(ws, windowSelector{Handle: 2, PID: 6100}); err != nil || w.Handle != 2 {
		t.Errorf("idle frame by handle: %+v %v", w, err)
	}
	if _, err := resolveWindow(ws, windowSelector{PID: 6100}); err == nil {
		t.Error("pid alone selected an idle frame")
	}
	if c := windowCandidates([]proto.WindowInfo{{Handle: 2, PID: 900, HostedPID: 6100}, {Handle: 3, PID: 7000}}, 6100); len(c) != 1 || c[0].Handle != 2 {
		t.Errorf("candidates of a hosted pid: %+v", c)
	}
}
