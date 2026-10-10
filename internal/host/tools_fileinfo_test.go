package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

func connectFileInfoMCP(t *testing.T, ctx context.Context, call agentCall) (*mcp.ClientSession, *deps) {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "fileinfo-test", Version: "test"}, nil)
	d := &deps{s: s, call: call, runID: "fileinfo-test", obs: newObservationStore(), tasks: newTaskRegistry()}
	registerFileInfo(d)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "fileinfo-client", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, d
}

func TestFileInfoRegisteredSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectObserveMCP(t, ctx, newObserveBackend(t))
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "vm_file_info" {
			continue
		}
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Fatalf("annotations: %+v", a)
		}
		b, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Type  any
				Items struct{ Type string }
			}
			Required []string
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatal(err)
		}
		sort.Strings(schema.Required)
		if !reflect.DeepEqual(schema.Required, []string{"paths", "vm"}) || len(schema.Properties) != 3 || schema.Properties["task_id"].Type != "string" ||
			schema.Properties["vm"].Type != "string" || !strings.Contains(fmt.Sprint(schema.Properties["paths"].Type), "array") || schema.Properties["paths"].Items.Type != "string" {
			t.Fatalf("schema: %s", b)
		}
		return
	}
	t.Fatal("vm_file_info is not registered")
}

func TestFileInfoForwardsWithoutOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	size := int64(12)
	want := proto.FileInfoResult{Files: []proto.FileInfo{
		{Path: `%APPDATA%\app\a.dll`, Resolved: `C:\Users\t\AppData\Roaming\app\a.dll`, Exists: true, Type: "file", Size: &size, SHA256: strings.Repeat("ab", 32), Version: "1.2.3.4", Modified: "2026-01-02T03:04:05Z"},
		{Path: `C:\missing`, Resolved: `C:\missing`},
		{Path: `C:\locked.bin`, Resolved: `C:\locked.bin`, Exists: true, Type: "file", Size: &size, Modified: "2026-01-02T03:04:05Z", Error: "sharing violation"},
	}}
	calls := 0
	cs, d := connectFileInfoMCP(t, ctx, func(callCtx context.Context, vm, op string, args any, payload []byte, result any) ([]byte, error) {
		calls++
		if callCtx.Err() != nil || vm != "Win10" || op != proto.OpFileInfo || !reflect.DeepEqual(args, proto.PathsArgs{Paths: []string{`%APPDATA%\app\a.dll`, `C:\missing`, `C:\locked.bin`}}) || payload != nil {
			t.Errorf("forwarded vm=%q op=%q args=%+v payload=%v", vm, op, args, payload)
		}
		*result.(*proto.FileInfoResult) = want
		return nil, nil
	})
	d.tasks.owners["WIN10"] = "writer" // another task owns the VM: a read-only tool still runs and claims nothing
	var got map[string]any
	callJSON(t, ctx, cs, "vm_file_info", map[string]any{"vm": "Win10", "task_id": "reader", "paths": []string{`%APPDATA%\app\a.dll`, `C:\missing`, `C:\locked.bin`}}, &got)
	b, _ := json.Marshal(want)
	var wantMap map[string]any
	json.Unmarshal(b, &wantMap)
	if got["task_id"] != "reader" {
		t.Errorf("task_id: %v", got["task_id"])
	}
	delete(got, "task_id")
	delete(got, "run_id")
	if calls != 1 || !reflect.DeepEqual(got, wantMap) || d.tasks.owners["WIN10"] != "writer" {
		t.Fatalf("calls=%d owners=%v result=%v", calls, d.tasks.owners, got)
	}
	missing := got["files"].([]any)[1].(map[string]any)
	if len(missing) != 3 || missing["exists"] != false {
		t.Fatalf("missing entry carries extra fields: %v", missing)
	}
}

func TestFileInfoArguments(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := &fakeCall{results: map[string]any{proto.OpFileInfo: proto.FileInfoResult{Files: []proto.FileInfo{}}}}
	cs, _ := connectFileInfoMCP(t, ctx, f.call)
	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = `C:\x`
	}
	for name, args := range map[string]map[string]any{
		"missing paths": {"vm": "A", "task_id": "t"},
		"empty paths":   {"vm": "A", "task_id": "t", "paths": []string{}},
		"too many":      {"vm": "A", "task_id": "t", "paths": tooMany},
		"empty entry":   {"vm": "A", "task_id": "t", "paths": []string{`C:\a`, ""}},
	} {
		out := callRefused(t, ctx, cs, "vm_file_info", args)
		if out["error"] != codeInvalidArgument || out["next"] == "" {
			t.Errorf("%s: %v", name, out)
		}
	}
	if len(f.ops) != 0 {
		t.Fatal("invalid arguments reached the agent")
	}
	var out map[string]any
	callJSON(t, ctx, cs, "vm_file_info", map[string]any{"vm": "A", "task_id": "t", "paths": tooMany[:64]}, &out)
	if len(f.ops) != 1 {
		t.Fatal("64 paths were not forwarded")
	}
}

func TestFileInfoErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		code string
		next string
	}{
		{"unknown op", errors.New(`unknown op "file_info"`), codeAgentOutdated, "vm_update_agent"},
		{"offline", errors.New("connect to agent: refused"), codeAgentRequired, "vm_install_agent"},
		{"unknown VM", errors.New(`VM "missing" not found`), codeInvalidArgument, "vm_list"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			f := &fakeCall{errs: map[string]error{proto.OpFileInfo: tt.err}}
			cs, _ := connectFileInfoMCP(t, ctx, f.call)
			out := callRefused(t, ctx, cs, "vm_file_info", map[string]any{"vm": "A", "task_id": "t", "paths": []string{`C:\a`}})
			if out["error"] != tt.code || !strings.Contains(out["reason"].(string), tt.err.Error()) || !strings.Contains(out["next"].(string), tt.next) {
				t.Errorf("error: %v", out)
			}
		})
	}
}
