package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

type windowMCPBackend struct {
	Backend
	find    func(string) (hyperv.VM, error)
	respond func(string, proto.Request) (any, error)
	click   func(string, int, int, int, int, []string) error
}

func (b *windowMCPBackend) Find(name string) (hyperv.VM, error) { return b.find(name) }
func (b *windowMCPBackend) Click(vm string, x, y, button, count int, modifiers []string) error {
	return b.click(vm, x, y, button, count, modifiers)
}
func (b *windowMCPBackend) Dial(_ context.Context, id string) (net.Conn, error) {
	client, guest := net.Pipe()
	go func() {
		defer guest.Close()
		for {
			var req proto.Request
			if _, err := proto.ReadFrame(guest, &req); err != nil {
				return
			}
			result, err := b.respond(id, req)
			if err != nil && req.Op == proto.OpPing { // the client's protocol check on a new connection
				result, err = proto.PingResult{Version: "test", Protocol: proto.Protocol}, nil
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

func connectWindowMCP(t *testing.T, ctx context.Context, b *windowMCPBackend) *mcp.ClientSession {
	t.Helper()
	manager := &Manager{Backend: b}
	t.Cleanup(func() { manager.Drop("A"); manager.Drop("B") })
	st, ct := mcp.NewInMemoryTransports()
	server, err := NewServer(manager).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "window-test", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// A raw click (no observation_id) through the full server goes straight to the Hyper-V mouse at screen pixels, with
// the session check as the only agent call.
func TestWindowMCPRawClick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var clicks atomic.Int32
	b := &windowMCPBackend{
		find: func(string) (hyperv.VM, error) { return hyperv.VM{ID: "A", Name: "CAD"}, nil },
		respond: func(_ string, req proto.Request) (any, error) {
			if req.Op != proto.OpListWindows {
				return nil, fmt.Errorf("unexpected guest op %q", req.Op)
			}
			return proto.WindowsResult{Windows: testWindows(), Session: &proto.SessionStateResult{Console: true}}, nil
		},
		click: func(_ string, x, y, button, count int, mods []string) error {
			if x != 101 || y != 52 || button != 3 || count != 1 || len(mods) != 0 {
				return fmt.Errorf("unexpected click: %d %d %d %d %v", x, y, button, count, mods)
			}
			clicks.Add(1)
			return nil
		},
	}
	cs := connectWindowMCP(t, ctx, b)
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_click", Arguments: map[string]any{"vm": "CAD", "x": 101, "y": 52, "button": "middle", "observe_after": "none"}})
	if err != nil {
		t.Fatal(err)
	}
	if m := resultJSON(t, r); r.IsError || m["ok"] != true || m["window"] != nil {
		t.Fatalf("result: %v", m)
	}
	if clicks.Load() != 1 {
		t.Errorf("click count: %d", clicks.Load())
	}
}

// Without vm an action is refused before anything runs: no VM lookup (default or named), no agent call, no input. With
// vm every agent check and the final input go to that VM, and no default is ever resolved.
func TestWindowMCPActionsPinVM(t *testing.T) {
	for _, tool := range []string{"vm_type", "vm_key"} {
		t.Run(tool, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var finds, defaultFinds, actions atomic.Int32
			b := &inputMCPBackend{windowMCPBackend: &windowMCPBackend{
				find: func(name string) (hyperv.VM, error) {
					finds.Add(1)
					id := name
					if name == "" {
						id = "A"
						if defaultFinds.Add(1) > 1 {
							id = "B"
						}
					}
					return hyperv.VM{ID: id, Name: id}, nil
				},
				respond: func(id string, req proto.Request) (any, error) {
					if id != "A" {
						return nil, fmt.Errorf("window operation moved to VM %s", id)
					}
					switch req.Op {
					case proto.OpListWindows:
						return proto.WindowsResult{Windows: testWindows(), Foreground: 10, Session: &proto.SessionStateResult{Console: true}}, nil
					case proto.OpTypeKeys:
						actions.Add(1)
						return proto.TypeKeysResult{Events: 2}, nil
					default:
						return nil, fmt.Errorf("unexpected op %q", req.Op)
					}
				},
			}, press: func(vm, _ string) error {
				if vm != "A" {
					return fmt.Errorf("keys did not pin VM: %q", vm)
				}
				actions.Add(1)
				return nil
			}}
			cs := connectInputMCP(t, ctx, b)
			args := map[string]any{"handle": 10, "pid": 100}
			if tool == "vm_type" {
				args["text"] = "x"
			} else {
				args["keys"] = "enter"
			}
			r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			if m := resultJSON(t, r); !r.IsError || m["error"] != codeInvalidArgument || m["vms"] == nil {
				t.Fatalf("%s without vm: %s", tool, resultText(r))
			}
			if finds.Load() != 0 || actions.Load() != 0 {
				t.Fatalf("refused call had side effects: finds=%d actions=%d", finds.Load(), actions.Load())
			}
			args["vm"] = "A"
			r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			if r.IsError {
				t.Fatalf("%s: %s", tool, resultText(r))
			}
			if defaultFinds.Load() != 0 || actions.Load() != 1 {
				t.Errorf("default resolutions=%d actions=%d", defaultFinds.Load(), actions.Load())
			}
		})
	}
}
