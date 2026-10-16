package agent

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"hyperhand/internal/proto"
)

// Compare bounded cached navigation against the ordinary walk on a guest's live window:
// HH_BENCH_HANDLE, HH_BENCH_PID, optional HH_BENCH_NODES, HH_BENCH_DEPTH, HH_BENCH_ROUNDS,
// HH_BENCH_FIND (automation ID), HH_BENCH_TYPE (numeric UIA type), HH_BENCH_NAME.
func TestUIACacheBench(t *testing.T) {
	h, _ := strconv.ParseUint(os.Getenv("HH_BENCH_HANDLE"), 10, 64)
	pid, _ := strconv.ParseUint(os.Getenv("HH_BENCH_PID"), 10, 32)
	if h == 0 {
		t.Skip("HH_BENCH_HANDLE not set")
	}
	num := func(k string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
			return v
		}
		return def
	}
	a := proto.ControlsArgs{Handle: h, PID: uint32(pid), MaxNodes: num("HH_BENCH_NODES", 5000), MaxDepth: num("HH_BENCH_DEPTH", 64)}
	find := os.Getenv("HH_BENCH_FIND")
	fa := proto.FindControlsArgs{Handle: h, PID: uint32(pid), AutomationID: find, ControlType: int32(num("HH_BENCH_TYPE", 0)), Name: os.Getenv("HH_BENCH_NAME"), MaxVisited: 20000, MaxMatches: 10, MaxDepth: 64}
	type row struct {
		Mode                    string
		ListMS, FindMS          float64
		Nodes, Visited, Matches int
		Truncation              []string
	}
	var last [2]proto.ControlsResult
	var lastFind [2]proto.FindControlsResult
	for round := 0; round < num("HH_BENCH_ROUNDS", 3); round++ {
		for i, cached := range []bool{false, true} {
			r := row{Mode: map[bool]string{false: "walk", true: "cache"}[cached]}
			t0 := time.Now()
			res, err := collectControlsCached(a, cached)
			if err != nil {
				t.Fatalf("%s list: %v", r.Mode, err)
			}
			r.ListMS = float64(time.Since(t0).Microseconds()) / 1000
			r.Nodes, r.Truncation, last[i] = len(res.Nodes), res.Truncation, res
			if fa.AutomationID != "" || fa.ControlType != 0 || fa.Name != "" {
				t0 = time.Now()
				fr, err := collectFindControlsCached(fa, cached)
				if err != nil {
					t.Fatalf("%s find: %v", r.Mode, err)
				}
				r.FindMS = float64(time.Since(t0).Microseconds()) / 1000
				r.Visited, r.Matches, lastFind[i] = fr.Visited, len(fr.Matches), fr
			}
			b, _ := json.Marshal(r)
			t.Log(string(b))
		}
	}
	diffs := 0
	for i := range min(len(last[0].Nodes), len(last[1].Nodes)) {
		if !reflect.DeepEqual(last[0].Nodes[i], last[1].Nodes[i]) {
			if diffs < 5 {
				w, _ := json.Marshal(last[0].Nodes[i])
				c, _ := json.Marshal(last[1].Nodes[i])
				t.Logf("node %d differs\n walk:  %s\n cache: %s", i, w, c)
			}
			diffs++
		}
	}
	t.Logf("SUMMARY nodes walk=%d cache=%d differing=%d focused walk=%d cache=%d selected_text_equal=%v find_equal=%v",
		len(last[0].Nodes), len(last[1].Nodes), diffs, last[0].Focused, last[1].Focused,
		last[0].SelectedText == last[1].SelectedText, reflect.DeepEqual(lastFind[0], lastFind[1]))
	if !reflect.DeepEqual(last[0], last[1]) || !reflect.DeepEqual(lastFind[0], lastFind[1]) {
		t.Fatal("cached and uncached results differ; use a stable fixture for the benchmark")
	}
}
