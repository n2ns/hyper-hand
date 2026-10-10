package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

// observeBackend serves a generated 64x40 screenshot and canned agent answers; dialErr makes the agent unreachable.
type observeBackend struct {
	Backend
	shot     []byte
	dialErr  error
	windows  func() proto.WindowsResult
	controls func(proto.ControlsArgs) (proto.ControlsResult, error)
	ops      []proto.Request
}

func (b *observeBackend) Find(name string) (hyperv.VM, error) {
	if name == "" {
		name = "CAD"
	}
	return hyperv.VM{ID: "A", Name: name}, nil
}

func (b *observeBackend) Screenshot(string) ([]byte, int, int, error) { return b.shot, 64, 40, nil }

func (b *observeBackend) Dial(_ context.Context, _ string) (net.Conn, error) {
	if b.dialErr != nil {
		return nil, b.dialErr
	}
	client, guest := net.Pipe()
	go func() {
		defer guest.Close()
		for {
			var req proto.Request
			if _, err := proto.ReadFrame(guest, &req); err != nil {
				return
			}
			var result any
			var err error
			switch req.Op {
			case proto.OpPing: // the client's protocol check on a new connection
				result = proto.PingResult{Version: "test", Protocol: proto.Protocol}
			case proto.OpListWindows:
				b.ops = append(b.ops, req)
				result = b.windows()
			case proto.OpListControls:
				b.ops = append(b.ops, req)
				var args proto.ControlsArgs
				if err = json.Unmarshal(req.Args, &args); err == nil {
					result, err = b.controls(args)
				}
			default:
				b.ops = append(b.ops, req)
				err = fmt.Errorf("unexpected guest op %q", req.Op)
			}
			response := proto.Response{}
			if err != nil {
				response.Error = err.Error()
			} else {
				response.Result, _ = json.Marshal(result)
			}
			if err := proto.WriteFrame(guest, response, nil); err != nil {
				return
			}
		}
	}()
	return client, nil
}

func observeWindows() proto.WindowsResult {
	return proto.WindowsResult{
		Windows: []proto.WindowInfo{
			{Handle: 10, PID: 100, Title: "Options", Class: "#32770", Process: "acad.exe", Rect: proto.Rect{Left: 8, Top: 4, Right: 40, Bottom: 30}, Enabled: true, Foreground: true, GroupRoot: 10, Integrity: "medium"},
			{Handle: 20, PID: 200, Title: "Notepad", Process: "notepad.exe", Rect: proto.Rect{Left: 50, Top: 20, Right: 100, Bottom: 60}, Enabled: true, GroupRoot: 20},
			{Handle: 30, PID: 300, Title: "Calc", Process: "calc.exe", Rect: proto.Rect{Left: -32000, Top: -32000, Right: -31840, Bottom: -31960}, Enabled: true, Minimized: true, GroupRoot: 30},
			{Handle: 40, PID: 400, Title: "A", Process: "x.exe", Rect: proto.Rect{Right: 10, Bottom: 10}, Enabled: true, GroupRoot: 40},
			{Handle: 41, PID: 400, Title: "B", Process: "x.exe", Rect: proto.Rect{Right: 10, Bottom: 10}, Enabled: true, GroupRoot: 40, Owner: 40},
		},
		Foreground: 10,
		Focused:    &proto.FocusedControl{Window: 10, Name: "", ControlType: "Edit", ClassName: "Edit", RuntimeID: "42.2", Rect: proto.Rect{Left: 8, Top: 24, Right: 40, Bottom: 30}},
		Session:    &proto.SessionStateResult{Console: true},
	}
}

func observeControls() proto.ControlsResult {
	return proto.ControlsResult{
		Nodes: []proto.ControlInfo{
			{Index: 0, Parent: -1, Depth: 0, ControlType: 50032, Name: "Options", ClassName: "#32770", PID: 100, Enabled: true, RuntimeID: "42.1", Rect: proto.Rect{Left: 8, Top: 4, Right: 40, Bottom: 30}},
			{Index: 1, Parent: 0, Depth: 1, ControlType: 50004, AutomationID: "cmdline", ClassName: "Edit", PID: 100, Enabled: true, RuntimeID: "42.2", Focused: true, HasValue: true, Value: "LINE", Rect: proto.Rect{Left: 8, Top: 24, Right: 40, Bottom: 30}},
			{Index: 2, Parent: 0, Depth: 1, ControlType: 50000, Name: "OK", PID: 100, RuntimeID: "42.3", Rect: proto.Rect{Left: 10, Top: 6, Right: 20, Bottom: 12}},
		},
		Focused:      1,
		SelectedText: "LI",
	}
}

func newObserveBackend(t *testing.T) *observeBackend {
	t.Helper()
	shot, _ := screenshotFixture(t, 64, 40)
	return &observeBackend{
		shot:     shot,
		windows:  observeWindows,
		controls: func(proto.ControlsArgs) (proto.ControlsResult, error) { return observeControls(), nil },
	}
}

func connectObserveMCP(t *testing.T, ctx context.Context, b *observeBackend) *mcp.ClientSession {
	t.Helper()
	manager := &Manager{Backend: b}
	t.Cleanup(func() { manager.Drop("A") })
	st, ct := mcp.NewInMemoryTransports()
	server, err := NewServer(manager).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "observe-test", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// observe calls vm_observe and returns the decoded JSON item and the image item's dimensions (0x0 without an image).
func observe(t *testing.T, ctx context.Context, cs *mcp.ClientSession, args map[string]any) (map[string]any, int, int) {
	t.Helper()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_observe", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		t.Fatalf("vm_observe %v: %s", args, resultText(r))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(r.Content[len(r.Content)-1].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	if len(r.Content) == 1 {
		return out, 0, 0
	}
	img, ok := r.Content[0].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/png" {
		t.Fatalf("first item is %T, want a PNG image", r.Content[0])
	}
	im, err := png.Decode(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatal(err)
	}
	return out, im.Bounds().Dx(), im.Bounds().Dy()
}

// observeError calls vm_observe expecting a refusal and returns its JSON object.
func observeError(t *testing.T, ctx context.Context, cs *mcp.ClientSession, args map[string]any, code string) map[string]any {
	t.Helper()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_observe", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsError {
		t.Fatalf("vm_observe %v succeeded: %s", args, resultText(r))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resultText(r)), &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] != code || out["next"] == "" || out["reason"] == "" {
		t.Fatalf("vm_observe %v: want error %s with reason and next, got %s", args, code, resultText(r))
	}
	return out
}

func shot(out map[string]any) map[string]any { s, _ := out["screenshot"].(map[string]any); return s }

func TestObserveWholeScreen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectObserveMCP(t, ctx, newObserveBackend(t))
	out, w, h := observe(t, ctx, cs, map[string]any{"vm": "CAD"})
	if w != 64 || h != 40 {
		t.Errorf("image %dx%d", w, h)
	}
	if id, _ := out["observation_id"].(string); !strings.HasPrefix(id, "o-") {
		t.Errorf("observation_id %v", out["observation_id"])
	}
	if out["vm"] != "CAD" || out["captured_at"] == "" {
		t.Errorf("vm/captured_at: %v %v", out["vm"], out["captured_at"])
	}
	if s := shot(out); s["width"] != 64.0 || s["height"] != 40.0 || s["origin_x"] != 0.0 || s["origin_y"] != 0.0 || s["scale"] != 1.0 {
		t.Errorf("screenshot %v", s)
	}
	for _, k := range []string{"window", "controls", "controls_diff", "stale_risk", "agent", "selected_text"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s present in a plain whole-screen observation: %v", k, out[k])
		}
	}
	// The focused control comes from list_windows; -1 says it is not an index into a tree.
	f, _ := out["focused"].(map[string]any)
	if f["index"] != -1.0 || f["control_type"] != "Edit" || f["class_name"] != "Edit" {
		t.Errorf("focused %v", f)
	}
	// screenshot: false returns only the JSON item.
	out, w, h = observe(t, ctx, cs, map[string]any{"vm": "CAD", "screenshot": false})
	if w != 0 || h != 0 || out["screenshot"] != nil {
		t.Errorf("screenshot: false returned an image %dx%d %v", w, h, out["screenshot"])
	}
}

func TestObserveWindowCrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := newObserveBackend(t)
	cs := connectObserveMCP(t, ctx, b)
	out, w, h := observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10})
	if w != 32 || h != 26 {
		t.Errorf("image %dx%d, want 32x26", w, h)
	}
	if s := shot(out); s["width"] != 32.0 || s["height"] != 26.0 || s["origin_x"] != 8.0 || s["origin_y"] != 4.0 || s["scale"] != 1.0 {
		t.Errorf("screenshot %v", s)
	}
	win, _ := out["window"].(map[string]any)
	if win["handle"] != 10.0 || win["pid"] != 100.0 || win["process"] != "acad.exe" || win["title"] != "Options" || win["foreground"] != true || win["enabled"] != true || win["group_root"] != 10.0 || win["integrity"] != "medium" {
		t.Errorf("window %v", win)
	}
	if r, _ := win["rect"].(map[string]any); r["left"] != 8.0 || r["bottom"] != 30.0 {
		t.Errorf("window rect %v", r)
	}
	// By pid alone, when the process has one window.
	if out, _, _ := observe(t, ctx, cs, map[string]any{"vm": "CAD", "pid": 200}); out["window"].(map[string]any)["handle"] != 20.0 {
		t.Errorf("pid selection: %v", out["window"])
	}
	// Partly off screen: cropped to the visible part.
	out, w, h = observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 20})
	if w != 14 || h != 20 || shot(out)["origin_x"] != 50.0 || shot(out)["origin_y"] != 20.0 {
		t.Errorf("partly off screen: %dx%d %v", w, h, shot(out))
	}
	// Entirely off screen (minimized).
	e := observeError(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 30}, codeInvalidArgument)
	if !strings.Contains(e["reason"].(string), "minimized") {
		t.Errorf("minimized reason: %v", e["reason"])
	}
	// Unknown handle: no_window with candidates; pid with two windows: ambiguous_target with those two.
	e = observeError(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 99}, codeNoWindow)
	if c, _ := e["candidates"].([]any); len(c) != 5 || !strings.Contains(e["next"].(string), "vm_windows") {
		t.Errorf("no_window: %v", e)
	}
	e = observeError(t, ctx, cs, map[string]any{"vm": "CAD", "pid": 400}, codeAmbiguousTarget)
	if c, _ := e["candidates"].([]any); len(c) != 2 || c[0].(map[string]any)["handle"] != 40.0 || c[1].(map[string]any)["handle"] != 41.0 {
		t.Errorf("ambiguous: %v", e)
	}
	// Handle outside the pid.
	observeError(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "pid": 200}, codeNoWindow)
}

func TestObserveMaxSizeAndValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectObserveMCP(t, ctx, newObserveBackend(t))
	out, w, h := observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "max_size": 16})
	if w != 16 || h != 13 {
		t.Errorf("image %dx%d, want 16x13", w, h)
	}
	if s := shot(out); s["width"] != 16.0 || s["height"] != 13.0 || s["origin_x"] != 8.0 || s["origin_y"] != 4.0 || s["scale"] != 0.5 {
		t.Errorf("screenshot %v", s)
	}
	// Never upscales.
	if _, w, h := observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "max_size": 500}); w != 32 || h != 26 {
		t.Errorf("upscaled to %dx%d", w, h)
	}
	for _, args := range []map[string]any{{"vm": "CAD", "max_size": -1}, {"vm": "CAD", "max_depth": 11}, {"vm": "CAD", "max_nodes": 1001}, {"vm": "CAD", "max_depth": -1}} {
		e := observeError(t, ctx, cs, args, codeInvalidArgument)
		if n := e["next"].(string); !strings.Contains(n, "max_") {
			t.Errorf("%v: next %q does not name the allowed range", args, n)
		}
	}
}

func TestObserveControls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := newObserveBackend(t)
	var got []proto.ControlsArgs
	b.controls = func(a proto.ControlsArgs) (proto.ControlsResult, error) {
		got = append(got, a)
		r := observeControls()
		r.Truncated = true
		return r, nil
	}
	cs := connectObserveMCP(t, ctx, b)
	out, w, h := observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "controls": true})
	if w != 32 || h != 26 {
		t.Errorf("image %dx%d", w, h)
	}
	if len(got) != 1 || got[0] != (proto.ControlsArgs{Handle: 10, PID: 100, MaxDepth: 4, MaxNodes: 200}) {
		t.Errorf("list_controls args %+v", got)
	}
	want := "[0] Window \"Options\" (8,4 32x26)\n" +
		"  [1] Edit \"\" id=cmdline (8,24 32x6) focused value=\"LINE\"\n" +
		"  [2] Button \"OK\" (10,6 10x6) disabled\n"
	if out["controls"] != want {
		t.Errorf("controls:\n got %q\nwant %q", out["controls"], want)
	}
	if out["controls_truncated"] != true || out["selected_text"] != "LI" {
		t.Errorf("truncated/selected_text: %v %v", out["controls_truncated"], out["selected_text"])
	}
	f, _ := out["focused"].(map[string]any)
	if f["index"] != 1.0 || f["control_type"] != "Edit" || f["class_name"] != "Edit" || f["rect"].(map[string]any)["top"] != 24.0 {
		t.Errorf("focused %v", f)
	}
	if _, ok := out["controls_diff"]; ok {
		t.Errorf("controls_diff present without diff_from")
	}
	// Custom limits are passed through.
	observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "controls": true, "max_depth": 2, "max_nodes": 50})
	if got[1] != (proto.ControlsArgs{Handle: 10, PID: 100, MaxDepth: 2, MaxNodes: 50}) {
		t.Errorf("custom limits: %+v", got[1])
	}
	// Whole screen with controls: the foreground window's tree, named in window; the screenshot stays the whole screen.
	out, w, h = observe(t, ctx, cs, map[string]any{"vm": "CAD", "controls": true})
	if w != 64 || h != 40 || got[2].Handle != 10 || got[2].PID != 100 {
		t.Errorf("whole screen controls: %dx%d args %+v", w, h, got[2])
	}
	if win, _ := out["window"].(map[string]any); win["handle"] != 10.0 || win["foreground"] != true {
		t.Errorf("window %v", win)
	}
	if out["controls"] != want || shot(out)["origin_x"] != 0.0 {
		t.Errorf("whole screen controls: %v %v", out["controls"], shot(out))
	}
}

func TestObserveDiffFrom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := newObserveBackend(t)
	cs := connectObserveMCP(t, ctx, b)
	first, _, _ := observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "controls": true})
	id := first["observation_id"].(string)
	b.controls = func(proto.ControlsArgs) (proto.ControlsResult, error) {
		r := observeControls()
		r.Nodes[1].Value = "CIRCLE"                                                                                                                                                    // changed
		r.Nodes = append(r.Nodes[:2], proto.ControlInfo{Index: 2, Parent: 0, Depth: 1, ControlType: 50020, Name: "Specify center point:", PID: 100, Enabled: true, RuntimeID: "42.7"}) // OK removed, Text added
		return r, nil
	}
	// diff_from implies controls.
	out, _, _ := observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "diff_from": id})
	if _, ok := out["controls"]; ok {
		t.Errorf("controls present with a diff: %v", out["controls"])
	}
	diff, _ := out["controls_diff"].(map[string]any)
	wantDiff := map[string]any{
		"added":   []any{`  [2] Text "Specify center point:" (0,0 0x0)`},
		"removed": []any{2.0},
		"changed": []any{`  [1] Edit "" id=cmdline (8,24 32x6) focused value="CIRCLE"`},
	}
	if fmt.Sprint(diff) != fmt.Sprint(wantDiff) {
		t.Errorf("controls_diff\n got %v\nwant %v", diff, wantDiff)
	}
	if _, ok := out["stale_risk"]; ok {
		t.Errorf("stale_risk on a valid diff: %v", out["stale_risk"])
	}
	if out["observation_id"] == id {
		t.Error("diff observation reuses the id")
	}
	// Unknown id, a different window and a whole-screen base: the full tree comes back and stale_risk explains.
	for _, tt := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"vm": "CAD", "handle": 10, "controls": true, "diff_from": "o-nope"}, "diff_from ignored: observation \"o-nope\" is unknown"},
		{map[string]any{"vm": "CAD", "handle": 20, "controls": true, "diff_from": id}, "diff_from ignored: observation " + id + " is of a different window"},
	} {
		out, _, _ := observe(t, ctx, cs, tt.args)
		if out["controls"] == nil || out["controls_diff"] != nil || !strings.HasPrefix(fmt.Sprint(out["stale_risk"]), tt.want) {
			t.Errorf("%v: controls %v diff %v stale_risk %v", tt.args, out["controls"] != nil, out["controls_diff"], out["stale_risk"])
		}
	}
	// A base without a control tree.
	plain, _, _ := observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10})
	out, _, _ = observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "diff_from": plain["observation_id"]})
	if !strings.Contains(fmt.Sprint(out["stale_risk"]), "has no control tree") || out["controls"] == nil {
		t.Errorf("base without tree: %v", out["stale_risk"])
	}
}

func TestObserveAgentOffline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := newObserveBackend(t)
	b.dialErr = errors.New("hvsock: the VM is not running or the agent is not listening")
	cs := connectObserveMCP(t, ctx, b)
	out, w, h := observe(t, ctx, cs, map[string]any{"vm": "CAD", "controls": true})
	if w != 64 || h != 40 || out["agent"] != "offline" {
		t.Errorf("offline whole screen: %dx%d %v", w, h, out)
	}
	for _, k := range []string{"window", "focused", "controls"} {
		if _, ok := out[k]; ok {
			t.Errorf("offline: %s present: %v", k, out[k])
		}
	}
	e := observeError(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10}, codeAgentRequired)
	if !strings.Contains(e["next"].(string), "vm_status") {
		t.Errorf("agent_required next: %v", e["next"])
	}
	// vm_windows needs the agent.
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_windows", Arguments: map[string]any{"vm": "CAD"}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsError || !strings.Contains(resultText(r), `"error":"agent_required"`) {
		t.Errorf("vm_windows offline: %v %s", r.IsError, resultText(r))
	}
}

func TestObserveUIATimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := newObserveBackend(t)
	b.controls = func(proto.ControlsArgs) (proto.ControlsResult, error) {
		return proto.ControlsResult{}, errors.New("list_controls timed out after 10s")
	}
	cs := connectObserveMCP(t, ctx, b)
	out, w, h := observe(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "controls": true, "diff_from": "o-x"})
	if w != 32 || h != 26 {
		t.Errorf("image %dx%d", w, h)
	}
	if out["stale_risk"] != "target not responding: control tree not read; diff_from ignored: no control tree in this observation" {
		t.Errorf("stale_risk %v", out["stale_risk"])
	}
	if _, ok := out["controls"]; ok {
		t.Errorf("controls present after a timeout: %v", out["controls"])
	}
	if _, ok := out["agent"]; ok {
		t.Error("agent reported offline on a UIA timeout")
	}
	// The focused control falls back to list_windows' answer.
	if f, _ := out["focused"].(map[string]any); f["index"] != -1.0 {
		t.Errorf("focused %v", f)
	}
	// Any other agent error is a failure.
	b.controls = func(proto.ControlsArgs) (proto.ControlsResult, error) {
		return proto.ControlsResult{}, errors.New("window not found")
	}
	observeError(t, ctx, cs, map[string]any{"vm": "CAD", "handle": 10, "controls": true}, codeFailed)
}

func TestVMWindowsResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectObserveMCP(t, ctx, newObserveBackend(t))
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_windows", Arguments: map[string]any{"vm": "CAD"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		t.Fatal(resultText(r))
	}
	var out struct {
		Windows    []proto.WindowInfo        `json:"windows"`
		Foreground uint64                    `json:"foreground"`
		Focused    *proto.FocusedControl     `json:"focused_control"`
		Session    *proto.SessionStateResult `json:"session"`
	}
	if err := json.Unmarshal([]byte(resultText(r)), &out); err != nil {
		t.Fatal(err)
	}
	want := observeWindows()
	if len(out.Windows) != 5 || out.Windows[0] != want.Windows[0] || out.Foreground != 10 || out.Focused == nil || *out.Focused != *want.Focused || out.Session == nil || !out.Session.Console {
		t.Errorf("vm_windows: %s", resultText(r))
	}
	var keys map[string]json.RawMessage
	json.Unmarshal([]byte(resultText(r)), &keys)
	for _, k := range []string{"windows", "foreground", "focused_control", "session", "task_id", "run_id"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing key %s", k)
		}
	}
	if len(keys) != 6 {
		t.Errorf("unexpected keys: %v", keys)
	}
}
