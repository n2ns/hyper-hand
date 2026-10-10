package host

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

func connectAppsMCP(t *testing.T, ctx context.Context, call agentCall) *mcp.ClientSession {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "apps-test", Version: "test"}, nil)
	registerApps(&deps{s: s, call: call, runID: "apps-test"})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "apps-client", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestAppsRegisteredSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectObserveMCP(t, ctx, newObserveBackend(t))
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "vm_apps" {
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
			Properties map[string]struct{ Type string }
			Required   []string
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(schema.Required, []string{"vm"}) || len(schema.Properties) != 4 || schema.Properties["task_id"].Type != "string" || schema.Properties["vm"].Type != "string" || schema.Properties["query"].Type != "string" || schema.Properties["limit"].Type != "integer" {
			t.Fatalf("schema: %s", b)
		}
		return
	}
	t.Fatal("vm_apps is not registered")
}

func TestAppsForwardsLaunchAndWindows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	want := proto.ListAppsResult{
		Apps: []proto.AppInfo{
			{ID: "app-1", Name: "CAD", Launch: proto.AppLaunch{Path: `C:\Program Files\CAD\cad.exe`, Args: []string{"/profile", "Test Profile"}, Cwd: `C:\Drawings`}, Running: true, Windows: []proto.AppWindow{{Handle: 123, PID: 45, Title: "Drawing"}}},
			{ID: "app-2", Name: "CAD viewer", Launch: proto.AppLaunch{Path: `C:\Viewer\viewer.exe`, Args: []string{}, Cwd: ""}, Windows: []proto.AppWindow{}},
		},
		Total: 3, Truncated: true, Warnings: []string{"one shortcut could not be read"},
	}
	calls := 0
	cs := connectAppsMCP(t, ctx, func(callCtx context.Context, vm, op string, args any, payload []byte, result any) ([]byte, error) {
		calls++
		if callCtx.Err() != nil || vm != "Win10" || op != proto.OpListApps || !reflect.DeepEqual(args, proto.ListAppsArgs{Query: "CaD", Limit: 2}) || payload != nil {
			t.Errorf("forwarded vm=%q op=%q args=%+v payload=%v", vm, op, args, payload)
		}
		*result.(*proto.ListAppsResult) = want
		return nil, nil
	})
	var got proto.ListAppsResult
	callJSON(t, ctx, cs, "vm_apps", map[string]any{"vm": "Win10", "query": "CaD", "limit": 2}, &got)
	if calls != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%d result=%+v", calls, got)
	}
}

func TestAppsLimitsAndEmptyResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := &fakeCall{results: map[string]any{proto.OpListApps: proto.ListAppsResult{Apps: []proto.AppInfo{}, Warnings: []string{}}}}
	cs := connectAppsMCP(t, ctx, f.call)
	for _, limit := range []int{-1, 201} {
		out := callRefused(t, ctx, cs, "vm_apps", map[string]any{"vm": "A", "limit": limit})
		if out["error"] != codeInvalidArgument || out["next"] == "" {
			t.Errorf("limit %d: %v", limit, out)
		}
	}
	if len(f.ops) != 0 {
		t.Fatal("invalid limits reached agent")
	}
	for _, input := range []map[string]any{{"vm": "A"}, {"vm": "A", "limit": 0}, {"vm": "A", "limit": 1}, {"vm": "A", "limit": 200}} {
		var out map[string]any
		callJSON(t, ctx, cs, "vm_apps", input, &out)
		apps, appsOK := out["apps"].([]any)
		warnings, warningsOK := out["warnings"].([]any)
		if !appsOK || len(apps) != 0 || !warningsOK || len(warnings) != 0 || out["total"] != float64(0) || out["truncated"] != false {
			t.Errorf("empty result: %v", out)
		}
	}
	for i, want := range []int{50, 50, 1, 200} {
		if got := f.args[i].(proto.ListAppsArgs).Limit; got != want {
			t.Errorf("call %d limit=%d want=%d", i, got, want)
		}
	}
}

func TestAppsErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		code string
		next string
	}{
		{"unknown op", errors.New(`unknown op "list_apps"`), codeAgentOutdated, "vm_update_agent"},
		{"protocol", refuse(codeAgentOutdated, "call vm_update_agent", nil, "old protocol"), codeAgentOutdated, "vm_update_agent"},
		{"offline", errors.New("connect to agent: refused"), codeAgentRequired, "vm_install_agent"},
		{"unknown VM", errors.New(`VM "missing" not found`), codeInvalidArgument, "vm_list"},
		{"cancel", context.Canceled, codeFailed, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			f := &fakeCall{errs: map[string]error{proto.OpListApps: tt.err}}
			cs := connectAppsMCP(t, ctx, f.call)
			out := callRefused(t, ctx, cs, "vm_apps", map[string]any{"vm": "A"})
			reason := tt.err.Error()
			var te *toolError
			if errors.As(tt.err, &te) {
				reason = te.Reason
			}
			if out["error"] != tt.code || out["run_id"] != "apps-test" || !strings.Contains(out["reason"].(string), reason) || !strings.Contains(out["next"].(string), tt.next) {
				t.Errorf("error: %v", out)
			}
		})
	}
}
