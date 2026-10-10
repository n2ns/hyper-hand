package host

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"hyperhand/internal/hyperv"
	"hyperhand/internal/proto"
)

type inputMCPBackend struct {
	*windowMCPBackend
	press    func(string, string) error
	typeText func(string, string) error
}

func (b *inputMCPBackend) PressKeys(vm, keys string) error { return b.press(vm, keys) }
func (b *inputMCPBackend) TypeText(vm, text string) error  { return b.typeText(vm, text) }

func connectInputMCP(t *testing.T, ctx context.Context, b Backend) *mcp.ClientSession {
	t.Helper()
	manager := &Manager{Backend: b}
	t.Cleanup(func() { manager.Drop("A"); manager.Drop("B") })
	st, ct := mcp.NewInMemoryTransports()
	s, err := NewServer(manager).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c, err := mcp.NewClient(&mcp.Implementation{Name: "input-test", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// vm_type acts on the named VM (never resolving a default), types through the agent only, and with a background target
// either activates it (default) or refuses (activate: false).
func TestInputMCPTypePinsTargetAndActivates(t *testing.T) {
	for _, mode := range []string{"success", "old-agent", "background", "background-strict"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			inputText := "汉字😀"
			var defaults, sent, focused, forbidden atomic.Int32
			b := &inputMCPBackend{windowMCPBackend: &windowMCPBackend{
				find: func(name string) (hyperv.VM, error) {
					id := "A"
					if name == "" && defaults.Add(1) > 1 {
						id = "B"
					}
					return hyperv.VM{ID: id, Name: id}, nil
				},
				respond: func(id string, req proto.Request) (any, error) {
					if id != "A" {
						return nil, fmt.Errorf("changed VM to %s", id)
					}
					switch req.Op {
					case proto.OpListWindows:
						fg := mode == "success" || mode == "old-agent" || focused.Load() > 0
						return proto.WindowsResult{Windows: []proto.WindowInfo{{Handle: 17, PID: 42, Title: "Editor", Enabled: true, Foreground: fg}}, Session: &proto.SessionStateResult{Console: true}}, nil
					case proto.OpFocusWindow:
						var a proto.TitleArgs
						if err := json.Unmarshal(req.Args, &a); err != nil {
							return nil, err
						}
						if a != (proto.TitleArgs{Handle: 17}) {
							return nil, fmt.Errorf("wrong focus target: %+v", a)
						}
						focused.Add(1)
						return proto.FocusResult{Handle: 17}, nil
					case proto.OpTypeKeys:
						sent.Add(1)
						var a proto.TypeKeysArgs
						if err := json.Unmarshal(req.Args, &a); err != nil {
							return nil, err
						}
						if a != (proto.TypeKeysArgs{Text: inputText, Handle: 17, PID: 42}) {
							return nil, fmt.Errorf("wrong input target: %+v", a)
						}
						if mode == "old-agent" {
							return nil, fmt.Errorf("unknown op type_keys")
						}
						return proto.TypeKeysResult{Events: 8}, nil
					case proto.OpPing:
						return proto.PingResult{Version: "test", Protocol: proto.Protocol}, nil
					default:
						forbidden.Add(1)
						return nil, fmt.Errorf("unexpected operation %s", req.Op)
					}
				},
			}, press: func(string, string) error { forbidden.Add(1); return nil }, typeText: func(string, string) error { forbidden.Add(1); return nil }}
			cs := connectInputMCP(t, ctx, b)
			args := map[string]any{"vm": "A", "text": inputText, "handle": 17, "pid": 42}
			if mode == "background-strict" {
				args["activate"] = false
			}
			r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_type", Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			m := resultJSON(t, r)
			wantErr := map[string]string{"old-agent": codeAgentOutdated, "background-strict": codeActivateFailed}[mode]
			if r.IsError != (wantErr != "") || m["error"] != any(nil) && m["error"] != wantErr {
				t.Fatalf("result: %v", m)
			}
			if !r.IsError && (m["applied_chars"] != float64(3) || m["total_chars"] != float64(3) || m["window"].(map[string]any)["handle"] != float64(17)) {
				t.Fatalf("result: %v", m)
			}
			wantSent, wantFocused := int32(1), int32(0)
			if mode == "background-strict" {
				wantSent = 0
			}
			if mode == "background" {
				wantFocused = 1
			}
			if defaults.Load() != 0 || sent.Load() != wantSent || focused.Load() != wantFocused || forbidden.Load() != 0 {
				t.Fatalf("default resolutions=%d sends=%d focused=%d forbidden=%d", defaults.Load(), sent.Load(), focused.Load(), forbidden.Load())
			}
		})
	}
}

func TestInputMCPSequenceValidatesBeforeSending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var operations atomic.Int32
	b := &inputMCPBackend{windowMCPBackend: &windowMCPBackend{
		find: func(string) (hyperv.VM, error) { return hyperv.VM{ID: "A", Name: "A"}, nil },
	}, press: func(string, string) error { operations.Add(1); return nil }}
	cs := connectInputMCP(t, ctx, b)
	for _, args := range []map[string]any{
		{"vm": "A", "sequence": []string{"ctrl+a", "not-a-key"}},
		{"vm": "A", "keys": "enter", "sequence": []string{"ctrl+a"}},
		{"vm": "A", "sequence": []string{}},
		{"vm": "A", "keys": "enter", "observe_after": "video"},
	} {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_key", Arguments: args})
		if err == nil && !r.IsError {
			t.Fatalf("accepted invalid input: %+v", args)
		}
		if err == nil && resultJSON(t, r)["error"] != codeInvalidArgument {
			t.Fatalf("%+v: %s", args, resultText(r))
		}
	}
	if operations.Load() != 0 {
		t.Fatalf("invalid sequence performed %d operations", operations.Load())
	}
}

func TestInputMCPSequenceOrderAndChangedTarget(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var mu sync.Mutex
			var pressed []string
			var polls, defaults atomic.Int32
			b := &inputMCPBackend{windowMCPBackend: &windowMCPBackend{
				find: func(name string) (hyperv.VM, error) {
					if name == "" {
						defaults.Add(1)
					}
					return hyperv.VM{ID: "A", Name: "A"}, nil
				},
				respond: func(_ string, req proto.Request) (any, error) {
					if req.Op != proto.OpListWindows {
						return nil, fmt.Errorf("unexpected operation %s", req.Op)
					}
					pid := uint32(42)
					if polls.Add(1) > 1 && changed {
						pid = 99 // the handle now belongs to another process: the pinned window is gone
					}
					return proto.WindowsResult{Windows: []proto.WindowInfo{{Handle: 17, PID: pid, Title: "Editor", Enabled: true, Foreground: true}}, Session: &proto.SessionStateResult{Console: true}}, nil
				},
			}, press: func(vm, key string) error {
				if vm != "A" {
					return fmt.Errorf("target VM was not pinned")
				}
				mu.Lock()
				defer mu.Unlock()
				pressed = append(pressed, key)
				return nil
			}}
			cs := connectInputMCP(t, ctx, b)
			r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vm_key", Arguments: map[string]any{"vm": "A", "handle": 17, "pid": 42, "sequence": []string{"ctrl+a", "tab", "enter"}}})
			if err != nil {
				t.Fatal(err)
			}
			if r.IsError != changed {
				t.Fatalf("result: %s", resultText(r))
			}
			want := []string{"ctrl+a", "tab", "enter"}
			if changed {
				want = want[:1]
				if m := resultJSON(t, r); m["error"] != codePartialInput || m["applied"] != float64(1) || m["total"] != float64(3) {
					t.Fatalf("missing partial progress: %v", m)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(pressed, want) || defaults.Load() != 0 {
				t.Fatalf("pressed=%v default resolutions=%d", pressed, defaults.Load())
			}
		})
	}
}
