package host

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

func TestConsoleReadyHookAfterStartAndTakeConsole(t *testing.T) {
	for _, tool := range []string{"vm_start", "vm_take_console"} {
		t.Run(tool, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var moved atomic.Bool
			b := newFakeAgentBackend(func(req proto.Request) (any, error) {
				switch req.Op {
				case proto.OpPing:
					return proto.PingResult{Version: "test", Protocol: proto.Protocol}, nil
				case proto.OpSessionState:
					return proto.SessionStateResult{Console: tool == "vm_start" || moved.Load()}, nil
				case proto.OpExec:
					moved.Store(true)
					return proto.ExecResult{}, nil
				}
				return nil, errors.New("unexpected " + req.Op)
			})
			cs, d := connectTools(t, ctx, b)
			var calls atomic.Int32
			d.m.AfterConsoleReady = func(vm string) {
				if vm != "Win10" {
					t.Errorf("VM %q", vm)
				}
				calls.Add(1)
			}
			var out map[string]any
			callJSON(t, ctx, cs, tool, map[string]any{"vm": "Win10"}, &out)
			if calls.Load() != 1 {
				t.Fatalf("hook calls %d", calls.Load())
			}
			if tool == "vm_take_console" {
				callJSON(t, ctx, cs, tool, map[string]any{"vm": "Win10"}, &out)
				if calls.Load() != 1 {
					t.Fatal("already_console repeated viewer recovery")
				}
			}
		})
	}
}

func TestConsoleReadyHookOnlyForRunningRestore(t *testing.T) {
	for _, state := range []string{"Running", "Off"} {
		t.Run(state, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			b := &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: state}}}
			cs, d := connectTools(t, ctx, b)
			var calls atomic.Int32
			d.m.AfterConsoleReady = func(string) { calls.Add(1) }
			var out restoreOut
			callJSON(t, ctx, cs, "vm_restore", map[string]any{"vm": "Win10", "id": "id-1", "start": false}, &out)
			if (calls.Load() == 1) != (state == "Running") {
				t.Fatalf("state %s calls %d", state, calls.Load())
			}
		})
	}
}
