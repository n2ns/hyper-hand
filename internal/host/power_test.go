package host

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
)

func TestWaitOff(t *testing.T) {
	states := []string{"Running", "Running", "Off"}
	find := func() (hyperv.VM, error) {
		s := states[0]
		if len(states) > 1 {
			states = states[1:]
		}
		return hyperv.VM{Name: "Win10", State: s}, nil
	}
	sleeps := 0
	if err := waitOff(context.Background(), find, func(time.Duration) { sleeps++ }, time.Minute); err != nil || sleeps != 2 {
		t.Fatalf("off: %v after %d sleeps", err, sleeps)
	}
	running := func() (hyperv.VM, error) { return hyperv.VM{Name: "Win10", State: "Running"}, nil }
	if err := waitOff(context.Background(), running, func(time.Duration) {}, 0); err == nil || !strings.Contains(err.Error(), "not turned off") {
		t.Errorf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitOff(ctx, running, func(time.Duration) {}, time.Minute); err == nil || !strings.Contains(err.Error(), "may still be in progress") {
		t.Errorf("cancelled: %v", err)
	}
}

type powerBackend struct {
	Backend // unexpected operations fail rather than reaching the real machine
	calls   []string
	state   string
}

func (b *powerBackend) Find(name string) (hyperv.VM, error) {
	return hyperv.VM{Name: "Win10", ID: "id", State: b.state}, nil
}
func (b *powerBackend) Shutdown(name string) error {
	b.calls = append(b.calls, "shutdown:"+name)
	b.state = "Off" // the guest shut down at once
	return nil
}
func (b *powerBackend) Stop(name string) error {
	b.calls = append(b.calls, "stop:"+name)
	b.state = "Off"
	return nil
}

func TestPowerTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connect := func(b Backend) *mcp.ClientSession {
		st, ct := mcp.NewInMemoryTransports()
		if _, err := NewServer(&Manager{Backend: b}).Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	for _, c := range []struct {
		tool, state string
		want        []string
	}{
		{"vm_shutdown", "Running", []string{"shutdown:Win10"}},
		{"vm_shutdown", "Off", nil},
		{"vm_turn_off", "Running", []string{"stop:Win10"}},
	} {
		b := &powerBackend{state: c.state}
		cs := connect(b)
		// No default VM: without vm the power tool is refused and nothing is shut down or turned off.
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: map[string]any{}})
		if err != nil || !r.IsError || !strings.Contains(resultText(r), `"error":"invalid_argument"`) || !strings.Contains(resultText(r), "vm is required") || len(b.calls) != 0 {
			t.Fatalf("%s from %s without vm: %v %s, calls %v", c.tool, c.state, err, resultText(r), b.calls)
		}
		r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: map[string]any{"vm": "Win10"}})
		cs.Close()
		if err != nil || r.IsError {
			t.Fatalf("%s from %s: %v %+v", c.tool, c.state, err, r)
		}
		if !reflect.DeepEqual(b.calls, c.want) {
			t.Errorf("%s from %s: calls %v, want %v", c.tool, c.state, b.calls, c.want)
		}
	}
	cs := connect(&powerBackend{state: "Running"})
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "vm_stop" {
			t.Error("vm_stop is still registered")
		}
	}
}
