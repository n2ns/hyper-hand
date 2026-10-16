package agent

import (
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"hyperhand/internal/proto"
)

func TestSearchCacheProperties(t *testing.T) {
	for _, tc := range []struct {
		name string
		args proto.FindControlsArgs
		want []int32
	}{
		{"id", proto.FindControlsArgs{AutomationID: "target"}, []int32{30019, 30011}},
		{"type", proto.FindControlsArgs{ControlType: 50004}, []int32{30019, 30003}},
		{"name", proto.FindControlsArgs{Name: "target"}, []int32{30019}},
		{"all", proto.FindControlsArgs{AutomationID: "target", ControlType: 50004, Name: "target"}, []int32{30019, 30003, 30011}},
	} {
		if got := searchCacheProperties(tc.args); !slices.Equal(got, tc.want) {
			t.Errorf("%s properties = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The fixture stays off screen and never activates a window or injects input.
// Compare the same native provider through both implementations, including the
// stopping boundaries and a password node with a descendant that must be skipped.
func TestControlsCacheNativeBoundaries(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	h := offscreenWindow(t, "hyperhand-cache-test", 0, -30000)
	password := editChild(t, h, "cache-password-secret", 0x20, 10)
	editChild(t, password, "cache-descendant-secret", 0, 10)
	group := editChild(t, h, "ordinary", 0, 150)
	user32.NewProc("SetWindowLongPtrW").Call(uintptr(group), ^uintptr(11), 101) // GWLP_ID
	editChild(t, group, "nested", 0, 10)
	base := proto.ControlsArgs{Handle: uint64(h), PID: uint32(os.Getpid()), MaxDepth: 4, MaxNodes: 100}
	find := proto.FindControlsArgs{Handle: base.Handle, PID: base.PID, ControlType: 50004, MaxDepth: 4, MaxVisited: 100, MaxMatches: 100}
	for _, tc := range []struct {
		name string
		req  helperRequest
	}{
		{"snapshot", helperRequest{Controls: &base}},
		{"nodes", helperRequest{Controls: &proto.ControlsArgs{Handle: base.Handle, PID: base.PID, MaxDepth: 4, MaxNodes: 2}}},
		{"depth", helperRequest{Controls: &proto.ControlsArgs{Handle: base.Handle, PID: base.PID, MaxDepth: 1, MaxNodes: 100}}},
		{"search", helperRequest{Find: &find}},
		{"search_name", helperRequest{Find: &proto.FindControlsArgs{Handle: base.Handle, PID: base.PID, Name: "ordinary", MaxDepth: 4, MaxVisited: 100, MaxMatches: 100}}},
		{"search_id", helperRequest{Find: &proto.FindControlsArgs{Handle: base.Handle, PID: base.PID, AutomationID: "101", MaxDepth: 4, MaxVisited: 100, MaxMatches: 100}}},
		{"search_id_type", helperRequest{Find: &proto.FindControlsArgs{Handle: base.Handle, PID: base.PID, AutomationID: "101", ControlType: 50004, MaxDepth: 4, MaxVisited: 100, MaxMatches: 100}}},
		{"search_all", helperRequest{Find: &proto.FindControlsArgs{Handle: base.Handle, PID: base.PID, Name: "ordinary", AutomationID: "missing", ControlType: 50004, MaxDepth: 4, MaxVisited: 100, MaxMatches: 100}}},
		{"visited", helperRequest{Find: &proto.FindControlsArgs{Handle: base.Handle, PID: base.PID, ControlType: 50004, MaxDepth: 4, MaxVisited: 2, MaxMatches: 100}}},
		{"matches", helperRequest{Find: &proto.FindControlsArgs{Handle: base.Handle, PID: base.PID, ControlType: 50004, MaxDepth: 4, MaxVisited: 100, MaxMatches: 1}}},
		{"search_depth", helperRequest{Find: &proto.FindControlsArgs{Handle: base.Handle, PID: base.PID, ControlType: 50004, MaxDepth: 1, MaxVisited: 100, MaxMatches: 100}}},
	} {
		cached, err := pumpHelper(t, tc.req)
		if err != nil {
			t.Fatalf("%s cached: %v", tc.name, err)
		}
		walk, err := pumpControlsHelper(t, tc.req, "serve_uncached")
		if err != nil {
			t.Fatalf("%s walk: %v", tc.name, err)
		}
		if !reflect.DeepEqual(cached, walk) {
			a, _ := json.Marshal(cached)
			b, _ := json.Marshal(walk)
			t.Fatalf("%s cached differs from bounded walk:\n%s\n%s", tc.name, a, b)
		}
		if strings.HasPrefix(tc.name, "search_id") && (cached.Find == nil || len(cached.Find.Matches) != 1 || !cached.Find.Matches[0].HasValue || cached.Find.Matches[0].Value != "ordinary") {
			t.Fatalf("%s did not return the live matching edit: %+v", tc.name, cached.Find)
		}
		data, _ := json.Marshal(cached)
		if strings.Contains(string(data), "cache-password-secret") || strings.Contains(string(data), "cache-descendant-secret") {
			t.Fatalf("%s password subtree leaked: %s", tc.name, data)
		}
	}
}
