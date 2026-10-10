package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"hyperhand/internal/hyperv"
)

type lifecycleBackend struct {
	Backend    // Unexpected operations fail rather than reaching the real machine.
	calls      []string
	state      string
	restoreErr error
}

func (b *lifecycleBackend) Find(name string) (hyperv.VM, error) {
	b.calls = append(b.calls, "find:"+name)
	return hyperv.VM{Name: "test", ID: "test-id", State: b.state}, nil
}
func (b *lifecycleBackend) Start(name string) error {
	b.calls = append(b.calls, "start:"+name)
	b.state = "Running"
	return nil
}
func (b *lifecycleBackend) ListCheckpoints(vm string) (hyperv.CheckpointList, error) {
	b.calls = append(b.calls, "list:"+vm)
	return hyperv.CheckpointList{CheckpointType: "Standard", CurrentParentID: "cp-1", Checkpoints: []hyperv.Checkpoint{{ID: "cp-1", Name: "baseline", Kind: "standard", State: "off"}}}, nil
}
func (b *lifecycleBackend) RestoreCheckpoint(vm, id string) error {
	b.calls = append(b.calls, "restore:"+vm+":"+id)
	b.state = "Off"
	return b.restoreErr
}

func TestRestoreThroughBackend(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		fail bool
		want []string
	}{
		{"default starts", map[string]any{"vm": "test", "id": "cp-1"}, false, []string{"find:test", "find:test", "list:test", "restore:test:cp-1", "find:test", "start:test"}},
		{"stay off", map[string]any{"vm": "test", "id": "cp-1", "start": false}, false, []string{"find:test", "find:test", "list:test", "restore:test:cp-1", "find:test"}},
		{"failure does not start", map[string]any{"vm": "test", "id": "cp-1"}, true, []string{"find:test", "find:test", "list:test", "restore:test:cp-1"}},
		// No default VM: a restore without vm is refused before the backend is touched.
		{"missing vm refused", map[string]any{"id": "cp-1"}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &lifecycleBackend{state: "Running"}
			if tc.fail {
				b.restoreErr = errors.New("backend restore failed")
			}
			m := &Manager{Backend: b}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			st, ct := mcp.NewInMemoryTransports()
			ss, err := NewServer(m).Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_restore", Arguments: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			if r.IsError != tc.fail {
				t.Fatalf("tool result error=%v, want %v: %+v", r.IsError, tc.fail, r)
			}
			if !reflect.DeepEqual(b.calls, tc.want) {
				t.Fatalf("backend calls=%v, want %v", b.calls, tc.want)
			}
			if tc.want == nil {
				var e map[string]any
				if err := json.Unmarshal([]byte(resultText(r)), &e); err != nil || e["error"] != codeInvalidArgument || !strings.Contains(fmt.Sprint(e["reason"]), "vm is required") {
					t.Fatalf("missing vm: %s (%v)", resultText(r), err)
				}
			}
			if !tc.fail {
				var out restoreOut
				if err := json.Unmarshal([]byte(resultText(r)), &out); err != nil || out.VM != "test" || out.Restored != (checkpointRef{ID: "cp-1", Name: "baseline", Type: "manual"}) || out.SavedCurrent != nil || out.Next == "" {
					t.Fatalf("result %s: %v", resultText(r), err)
				}
				if want := map[bool]string{true: "running", false: "off"}[tc.name == "default starts"]; out.State != want {
					t.Fatalf("state %q, want %q", out.State, want)
				}
			}
		})
	}
}
