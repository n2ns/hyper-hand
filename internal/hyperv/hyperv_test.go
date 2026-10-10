package hyperv

import (
	"context"
	"errors"
	"image/color"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWaitStateJobCompletes(t *testing.T) {
	jobs := []stateJob{{State: 2}, {State: 3}, {State: 4}, {State: 6}, {State: 7}}
	reads := 0
	err := waitStateJob(context.Background(), 0, func() (stateJob, error) {
		if reads >= len(jobs) {
			t.Fatal("polled again after completion")
		}
		j := jobs[reads]
		reads++
		return j, nil
	})
	if err != nil || reads != len(jobs) {
		t.Fatalf("reads=%d error=%v", reads, err)
	}
}

func TestWaitStateJobSurfacesAsyncFailure(t *testing.T) {
	for _, state := range []int{7, 8, 9, 10} {
		reads := 0
		err := waitStateJob(context.Background(), 0, func() (stateJob, error) {
			reads++
			if reads == 1 {
				return stateJob{State: 4}, nil
			}
			return stateJob{State: state, ErrorCode: 32768, Description: "insufficient resources"}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "32768") || !strings.Contains(err.Error(), "insufficient resources") || reads != 2 {
			t.Fatalf("state=%d reads=%d error=%v", state, reads, err)
		}
	}
}

func TestWaitStateJobDoesNotAcceptAbnormalTerminationWithoutErrorCode(t *testing.T) {
	for _, state := range []int{8, 9, 10, 0, 32768} {
		if err := waitStateJob(context.Background(), 0, func() (stateJob, error) { return stateJob{State: state}, nil }); err == nil {
			t.Fatalf("accepted state %d", state)
		}
	}
}

func TestWaitStateJobStopsOnTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reads := 0
	// Cancellation after the first observation deterministically exercises the
	// bounded wait without allowing another query or any new state-change request.
	err := waitStateJob(ctx, time.Hour, func() (stateJob, error) { reads++; cancel(); return stateJob{State: 4}, nil })
	if !errors.Is(err, context.Canceled) || reads != 1 || !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("reads=%d error=%v", reads, err)
	}
	deadline, done := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer done()
	err = waitStateJob(deadline, 0, func() (stateJob, error) { t.Fatal("queried after deadline"); return stateJob{}, nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
}

func TestWaitStateJobQueryFailure(t *testing.T) {
	want := errors.New("job disappeared")
	err := waitStateJob(context.Background(), 0, func() (stateJob, error) { return stateJob{}, want })
	if !errors.Is(err, want) {
		t.Fatalf("query error hidden: %v", err)
	}
}

func TestWaitStateJobPendingDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	reads := 0
	err := waitStateJob(ctx, time.Hour, func() (stateJob, error) { reads++; return stateJob{State: 4}, nil })
	if !errors.Is(err, context.DeadlineExceeded) || reads > 1 {
		t.Fatalf("deadline did not interrupt polling wait: reads=%d error=%v", reads, err)
	}
}

func TestStateName(t *testing.T) {
	// Msvm_ComputerSystem.EnabledState: 6 (Enabled but Offline) and 9 (Quiesce) are what a saved and a paused VM
	// report (observed on Windows 11 Hyper-V); 32769 and 32768 are the older Hyper-V-specific values.
	for state, want := range map[int]string{
		2: "Running", 3: "Off", 6: "Saved", 9: "Paused", 32769: "Saved", 32768: "Paused",
		0: "0", 32770: "32770",
	} {
		if got := stateName(state); got != want {
			t.Errorf("stateName(%d) = %q, want %q", state, got, want)
		}
	}
}

func TestParseKeys(t *testing.T) {
	cases := map[string][]int{
		"enter":          {0x0D},
		"Ctrl+Shift+Esc": {0x11, 0x10, 0x1B},
		"win+r":          {0x5B, 'R'},
		"alt+F4":         {0x12, 0x73},
		"f12":            {0x7B},
		"f13":            {0x7C},
		"F20":            {0x83},
		"ctrl+5":         {0x11, '5'},
		"ctrl + /":       {0x11, 0xBF},
		"ctrl+plus":      {0x11, 0xBB},
		"Control_L+KP_1": {0x11, 0x61},
	}
	for in, want := range cases {
		got, err := parseKeys(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseKeys(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ctrl+", "f21", "f0", "foo", "ctrl+xx", "KP_10", "numpad1"} {
		if _, err := parseKeys(bad); err == nil {
			t.Errorf("parseKeys(%q) should fail", bad)
		}
	}
}

// TestValidateKeys covers the added key names and aliases by their virtual-key codes.
func TestValidateKeys(t *testing.T) {
	codes := map[string]int{
		// Numeric keypad.
		"num0": 0x60, "num1": 0x61, "num2": 0x62, "num3": 0x63, "num4": 0x64, "num5": 0x65, "num6": 0x66, "num7": 0x67, "num8": 0x68, "num9": 0x69,
		"KP_0": 0x60, "KP_5": 0x65, "KP_9": 0x69, "kp_3": 0x63,
		"numenter": 0x0D, "KP_Enter": 0x0D, // no extended flag on the Hyper-V keyboard: the same key as enter
		"numdot": 0x6E, "KP_Decimal": 0x6E, "numplus": 0x6B, "KP_Add": 0x6B, "numminus": 0x6D, "KP_Subtract": 0x6D,
		"nummul": 0x6A, "KP_Multiply": 0x6A, "numdiv": 0x6F, "KP_Divide": 0x6F, "numlock": 0x90,
		// Function and system keys.
		"f13": 0x7C, "f20": 0x83, "printscreen": 0x2C, "scrolllock": 0x91, "pause": 0x13, "apps": 0x5D, "capslock": 0x14,
		// X11/Codex aliases.
		"Return": 0x0D, "Escape": 0x1B, "Control_L": 0x11, "Control_R": 0x11, "Shift_L": 0x10, "Shift_R": 0x10,
		"Alt_L": 0x12, "Alt_R": 0x12, "Super_L": 0x5B, "period": 0xBE, "comma": 0xBC, "slash": 0xBF, "minus": 0xBD,
		"equal": 0xBB, "BackSpace": 0x08, "Delete": 0x2E, "Prior": 0x21, "Next": 0x22, "Up": 0x26, "Down": 0x28,
		"Left": 0x25, "Right": 0x27,
		// Existing names keep their codes.
		"ctrl": 0x11, "win": 0x5B, "pageup": 0x21, "pagedown": 0x22, "esc": 0x1B, "del": 0x2E, "space": 0x20,
	}
	for name, want := range codes {
		if err := ValidateKeys(name); err != nil {
			t.Errorf("ValidateKeys(%q): %v", name, err)
		}
		if got, err := parseKeys(name); err != nil || len(got) != 1 || got[0] != want {
			t.Errorf("parseKeys(%q) = %v, %v; want [%#x]", name, got, err, want)
		}
		if err := ValidateKeys("ctrl+" + name); err != nil {
			t.Errorf("ValidateKeys(ctrl+%q): %v", name, err)
		}
	}
	for _, bad := range []string{"KP_Return", "Super_R", "f21", "numpad0", "num10", ""} {
		if err := ValidateKeys(bad); err == nil {
			t.Errorf("ValidateKeys(%q) should fail", bad)
		}
	}
}

func TestRGB565ToRGBA(t *testing.T) {
	// 2x1: pure red (0xF800), pure blue (0x001F), little-endian.
	img, err := rgb565ToRGBA([]byte{0x00, 0xF8, 0x1F, 0x00}, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := img.RGBAAt(0, 0); got != (color.RGBA{255, 0, 0, 255}) {
		t.Errorf("pixel 0 = %v", got)
	}
	if got := img.RGBAAt(1, 0); got != (color.RGBA{0, 0, 255, 255}) {
		t.Errorf("pixel 1 = %v", got)
	}
	// 0x07E0 = pure green, row 2 of a 1x2 image.
	img, _ = rgb565ToRGBA([]byte{0, 0, 0xE0, 0x07}, 1, 2)
	if got := img.RGBAAt(0, 1); got != (color.RGBA{0, 255, 0, 255}) {
		t.Errorf("green = %v", got)
	}
	if _, err := rgb565ToRGBA([]byte{1, 2}, 2, 1); err == nil {
		t.Error("short data should fail")
	}
}

func TestVariantBytes(t *testing.T) {
	for _, size := range []int{16, 24} {
		want := []byte{0x00, 0x7F, 0xFF}
		raw := make([]byte, len(want)*size)
		for i, b := range want {
			raw[i*size] = 0x11 // VT_UI1 tag, must not be read
			raw[i*size+8] = b
		}
		if got := variantBytes(raw, size); !reflect.DeepEqual(got, want) {
			t.Errorf("variantBytes(size %d) = %v, want %v", size, got, want)
		}
	}
}

func TestParseCheckpoints(t *testing.T) {
	one := Checkpoint{ID: "11111111-2222-3333-4444-555555555555", Name: "干净", CreatedAt: "2026-10-04T10:00:00+08:00", Kind: "standard", State: "off"}
	child := Checkpoint{ID: "66666666-7777-8888-9999-000000000000", Name: "b", ParentID: one.ID, CreatedAt: "2026-10-10T09:05:06+08:00", Kind: "production", State: "running"}
	cases := map[string]CheckpointList{
		`{"checkpoint_type":"Standard","current_parent_id":"","checkpoints":null}`: {CheckpointType: "Standard", Checkpoints: []Checkpoint{}},
		"\xef\xbb\xbf" + `{"checkpoint_type":"Production","current_parent_id":"11111111-2222-3333-4444-555555555555","checkpoints":{"id":"11111111-2222-3333-4444-555555555555","name":"干净","parent_id":"","created_at":"2026-10-04T10:00:00+08:00","kind":"standard","state":"off"}}` + "\r\n":                                                                                                                                                                  {CheckpointType: "Production", CurrentParentID: one.ID, Checkpoints: []Checkpoint{one}},
		`{"checkpoint_type":"Standard","current_parent_id":"66666666-7777-8888-9999-000000000000","checkpoints":[{"id":"11111111-2222-3333-4444-555555555555","name":"干净","parent_id":"","created_at":"2026-10-04T10:00:00+08:00","kind":"standard","state":"off"},{"id":"66666666-7777-8888-9999-000000000000","name":"b","parent_id":"11111111-2222-3333-4444-555555555555","created_at":"2026-10-10T09:05:06+08:00","kind":"production","state":"running"}]}`: {CheckpointType: "Standard", CurrentParentID: child.ID, Checkpoints: []Checkpoint{one, child}},
	}
	for in, want := range cases {
		got, err := parseCheckpoints([]byte(in))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseCheckpoints(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := parseCheckpoints([]byte("not json")); err == nil {
		t.Error("garbage accepted")
	}
}

// TestParseCheckpointsEdges covers ConvertTo-Json's shapes for the checkpoints field (an empty array as [] or null, a
// missing field, one object) and ids as PowerShell prints them (upper-case GUIDs, null parent as "").
func TestParseCheckpointsEdges(t *testing.T) {
	root := Checkpoint{ID: "C85CA8FB-DFC1-4AA3-8F36-532949376CD2", Name: "run-20261010-1017-9699-keep-baseline", CreatedAt: "2026-10-10T10:17:00+07:00", Kind: "standard", State: "running"}
	cases := map[string]CheckpointList{
		`{"checkpoint_type":"Disabled","current_parent_id":"","checkpoints":[]}`: {CheckpointType: "Disabled", Checkpoints: []Checkpoint{}},
		`{"checkpoint_type":"ProductionOnly","current_parent_id":""}`:            {CheckpointType: "ProductionOnly", Checkpoints: []Checkpoint{}},
		`{"checkpoint_type":"Standard","current_parent_id":"C85CA8FB-DFC1-4AA3-8F36-532949376CD2","checkpoints":{"id":"C85CA8FB-DFC1-4AA3-8F36-532949376CD2","name":"run-20261010-1017-9699-keep-baseline","parent_id":"","created_at":"2026-10-10T10:17:00+07:00","kind":"standard","state":"running"}}`:   {CheckpointType: "Standard", CurrentParentID: root.ID, Checkpoints: []Checkpoint{root}},
		`{"checkpoint_type":"Standard","current_parent_id":"C85CA8FB-DFC1-4AA3-8F36-532949376CD2","checkpoints":[{"id":"C85CA8FB-DFC1-4AA3-8F36-532949376CD2","name":"run-20261010-1017-9699-keep-baseline","parent_id":"","created_at":"2026-10-10T10:17:00+07:00","kind":"standard","state":"running"}]}`: {CheckpointType: "Standard", CurrentParentID: root.ID, Checkpoints: []Checkpoint{root}},
	}
	for in, want := range cases {
		got, err := parseCheckpoints([]byte(in))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseCheckpoints(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if got.Checkpoints == nil {
			t.Errorf("parseCheckpoints(%q) returned a nil slice (would encode as JSON null)", in)
		}
	}
	for _, bad := range []string{``, `[]`, `{"checkpoint_type":"Standard","checkpoints":[1]}`, `{"checkpoint_type":"Standard","checkpoints":"x"}`} {
		if _, err := parseCheckpoints([]byte(bad)); err == nil {
			t.Errorf("parseCheckpoints(%q) accepted", bad)
		}
	}
}

func TestIsSnapshotInstance(t *testing.T) {
	const inst = `Microsoft:223B3A66-684A-40B9-A273-F88806C0A2FB\C85CA8FB-DFC1-4AA3-8F36-532949376CD2`
	for _, id := range []string{"C85CA8FB-DFC1-4AA3-8F36-532949376CD2", "c85ca8fb-dfc1-4aa3-8f36-532949376cd2"} {
		if !isSnapshotInstance(inst, id) {
			t.Errorf("isSnapshotInstance(%q, %q) = false", inst, id)
		}
	}
	for _, id := range []string{
		"223B3A66-684A-40B9-A273-F88806C0A2FB", // the VM id, not the snapshot
		"532949376CD2",                         // a bare tail: the match needs the whole GUID after the backslash
		"8F36-532949376CD2",
		"",
	} {
		if isSnapshotInstance(inst, id) {
			t.Errorf("isSnapshotInstance(%q, %q) = true", inst, id)
		}
	}
	// The VM's own (realized) settings have no snapshot part.
	if isSnapshotInstance("Microsoft:223B3A66-684A-40B9-A273-F88806C0A2FB", "223B3A66-684A-40B9-A273-F88806C0A2FB") {
		t.Error("matched the VM's realized settings")
	}
}
