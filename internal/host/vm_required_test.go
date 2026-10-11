package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
)

// twoVMBackend is a Hyper-V with two VMs (Win10 and Win10-PipeSifu) and per-VM checkpoint lists that records every
// backend call as "Op:args". No guest agent is reachable (Dial fails).
type twoVMBackend struct {
	mu    sync.Mutex
	calls []string
	vms   []hyperv.VM
	cps   map[string][]hyperv.Checkpoint // by VM name
}

// otherRunTemp is a temp checkpoint left behind by another (crashed) run.
const otherRunTemp = "run-20261001-0800-0123456789abcdef-temp-old"

func newTwoVMBackend(secondState string) *twoVMBackend {
	return &twoVMBackend{
		vms: []hyperv.VM{{Name: "Win10", ID: "id-win10", State: "Running"}, {Name: "Win10-PipeSifu", ID: "id-pipesifu", State: secondState}},
		cps: map[string][]hyperv.Checkpoint{
			"Win10":          {{ID: "w-base", Name: "baseline", Kind: "standard", State: "off"}, {ID: "w-old", Name: otherRunTemp, ParentID: "w-base", Kind: "standard", State: "off"}},
			"Win10-PipeSifu": {{ID: "p-base", Name: "baseline", Kind: "standard", State: "off"}, {ID: "p-old", Name: otherRunTemp, ParentID: "p-base", Kind: "standard", State: "off"}},
		},
	}
}

func (b *twoVMBackend) record(op string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	parts := []string{op}
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	b.calls = append(b.calls, strings.Join(parts, ":"))
}

// recorded returns the recorded calls whose op is not in skip.
func (b *twoVMBackend) recorded(skip ...string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, c := range b.calls {
		if !slices.Contains(skip, strings.SplitN(c, ":", 2)[0]) {
			out = append(out, c)
		}
	}
	return out
}

func (b *twoVMBackend) vm(name string) (int, error) {
	for i, v := range b.vms {
		if strings.EqualFold(v.Name, name) {
			return i, nil
		}
	}
	return -1, fmt.Errorf("VM %q not found", name)
}

// checkpointIDs returns the checkpoint IDs of a VM.
func (b *twoVMBackend) checkpointIDs(vm string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for _, c := range b.cps[vm] {
		ids = append(ids, c.ID)
	}
	return ids
}

func (b *twoVMBackend) ListVMs() ([]hyperv.VM, error) {
	b.record("ListVMs")
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]hyperv.VM{}, b.vms...), nil
}
func (b *twoVMBackend) Find(name string) (hyperv.VM, error) {
	b.record("Find", name)
	b.mu.Lock()
	defer b.mu.Unlock()
	if name == "" { // the old default: the only running VM
		for _, v := range b.vms {
			if v.State == "Running" {
				return v, nil
			}
		}
		return hyperv.VM{}, errors.New("no running VM")
	}
	i, err := b.vm(name)
	if err != nil {
		return hyperv.VM{}, err
	}
	return b.vms[i], nil
}
func (b *twoVMBackend) setState(name, state string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	i, err := b.vm(name)
	if err == nil {
		b.vms[i].State = state
	}
	return err
}
func (b *twoVMBackend) Start(name string) error {
	b.record("Start", name)
	return b.setState(name, "Running")
}
func (b *twoVMBackend) Stop(name string) error {
	b.record("Stop", name)
	return b.setState(name, "Off")
}
func (b *twoVMBackend) Save(name string) error {
	b.record("Save", name)
	return b.setState(name, "Saved")
}
func (b *twoVMBackend) Pause(name string) error {
	b.record("Pause", name)
	return b.setState(name, "Paused")
}
func (b *twoVMBackend) SetCheckpointType(name, t string) error {
	b.record("SetCheckpointType", name)
	return nil
}
func (b *twoVMBackend) Shutdown(name string) error {
	b.record("Shutdown", name)
	return b.setState(name, "Off")
}
func (b *twoVMBackend) ListCheckpoints(vm string) (hyperv.CheckpointList, error) {
	b.record("ListCheckpoints", vm)
	b.mu.Lock()
	defer b.mu.Unlock()
	cps := b.cps[vm]
	l := hyperv.CheckpointList{CheckpointType: "Standard", Checkpoints: append([]hyperv.Checkpoint{}, cps...)}
	if len(cps) > 0 {
		l.CurrentParentID = cps[len(cps)-1].ID
	}
	return l, nil
}
func (b *twoVMBackend) CreateCheckpoint(vm, name string) (hyperv.Checkpoint, error) {
	b.record("CreateCheckpoint", vm, name)
	b.mu.Lock()
	defer b.mu.Unlock()
	cps := b.cps[vm]
	c := hyperv.Checkpoint{ID: "new-" + vm, Name: name, Kind: "standard", State: "running", CreatedAt: "2026-10-10T09:00:00+08:00"}
	if len(cps) > 0 {
		c.ParentID = cps[len(cps)-1].ID
	}
	b.cps[vm] = append(cps, c)
	return c, nil
}
func (b *twoVMBackend) RestoreCheckpoint(vm, id string) error {
	b.record("RestoreCheckpoint", vm, id)
	return nil
}
func (b *twoVMBackend) DeleteCheckpoint(vm, id string, subtree bool) error {
	b.record("DeleteCheckpoint", vm, id)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cps[vm] = slices.DeleteFunc(b.cps[vm], func(c hyperv.Checkpoint) bool { return c.ID == id })
	return nil
}
func (b *twoVMBackend) RenameCheckpoint(vm, id, name string) error {
	b.record("RenameCheckpoint", vm, id, name)
	return nil
}
func (b *twoVMBackend) Screenshot(vm string) ([]byte, int, int, error) {
	b.record("Screenshot", vm)
	return nil, 0, 0, errors.New("no screen")
}
func (b *twoVMBackend) Click(vm string, x, y, button, count int, modifiers []string) error {
	b.record("Click", vm)
	return nil
}
func (b *twoVMBackend) Drag(vm string, x1, y1, x2, y2 int, modifiers []string) error {
	b.record("Drag", vm)
	return nil
}
func (b *twoVMBackend) Scroll(vm string, x, y, delta int) error {
	b.record("Scroll", vm)
	return nil
}
func (b *twoVMBackend) PressKeys(vm, keys string) error   { b.record("PressKeys", vm); return nil }
func (b *twoVMBackend) TypeText(vm, text string) error    { b.record("TypeText", vm); return nil }
func (b *twoVMBackend) CopyToGuest(vm, h, g string) error { b.record("CopyToGuest", vm); return nil }
func (b *twoVMBackend) Dial(_ context.Context, id string) (net.Conn, error) {
	b.record("Dial", id)
	return nil, errors.New("connect to agent: no agent in this fake")
}

// connectTwoVM serves the full HyperHand tool set on b over an in-memory transport.
func connectTwoVM(t *testing.T, ctx context.Context, b Backend) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := NewServer(&Manager{Backend: b}).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// callRaw calls a tool with exactly args and returns the decoded JSON object and whether it was an error result.
func callRaw(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(resultText(r)), &obj); err != nil {
		t.Fatalf("%s result %s: %v", name, resultText(r), err)
	}
	return obj, r.IsError
}

// toolSchema is the part of a tool's input schema these tests read.
type toolSchema struct {
	Properties map[string]any `json:"properties"`
	Required   []string       `json:"required"`
}

func schemaOf(t *testing.T, tool *mcp.Tool) toolSchema {
	t.Helper()
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var s toolSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s schema %s: %v", tool.Name, raw, err)
	}
	return s
}

func TestVMRequiredEveryToolRefusesMissingVM(t *testing.T) {
	for _, second := range []string{"Off", "Running"} {
		t.Run("Win10-PipeSifu "+second, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			b := newTwoVMBackend(second)
			cs := connectTwoVM(t, ctx, b)
			tools, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			checked := 0
			for _, tool := range tools.Tools {
				if tool.Name == "vm_list" || tool.Name == "vm_end_turn" {
					continue
				}
				checked++
				for _, args := range []map[string]any{{}, {"vm": ""}, {"vm": "   "}, {"vm": "\t"}, {"vm": nil}} {
					e, isErr := callRaw(t, ctx, cs, tool.Name, args)
					vms, _ := e["vms"].([]any)
					next, _ := e["next"].(string)
					if !isErr || e["error"] != codeInvalidArgument || !strings.Contains(fmt.Sprint(e["reason"]), "vm is required") ||
						!reflect.DeepEqual(vms, []any{"Win10", "Win10-PipeSifu"}) || !strings.Contains(next, "Win10") || !strings.Contains(next, "Win10-PipeSifu") {
						t.Errorf("%s %v: %v", tool.Name, args, e)
					}
					// Like any other refusal, it carries the task identity.
					if e["task_id"] == nil || e["task_id"] == "" || e["run_id"] == nil || e["run_id"] == "" {
						t.Errorf("%s %v: refusal without task identity: %v", tool.Name, args, e)
					}
				}
				// A vm of the wrong type is the input schema's type error, not a missing vm.
				if e, isErr := callRaw(t, ctx, cs, tool.Name, map[string]any{"vm": 7}); !isErr || e["error"] != codeInvalidArgument ||
					strings.Contains(fmt.Sprint(e["reason"]), "vm is required") {
					t.Errorf("%s with vm 7: %v", tool.Name, e)
				}
				// Only the VM names were listed for the refusal: no Find, power, checkpoint, input or agent call ran.
				if calls := b.recorded("ListVMs"); len(calls) != 0 {
					t.Fatalf("%s without vm reached the backend: %v", tool.Name, calls)
				}
			}
			if checked < 20 {
				t.Fatalf("only %d tools checked", checked)
			}
			for _, vm := range []string{"Win10", "Win10-PipeSifu"} {
				if ids := b.checkpointIDs(vm); len(ids) != 2 {
					t.Errorf("%s checkpoints changed: %v", vm, ids)
				}
				if i, _ := b.vm(vm); b.vms[i].State != map[string]string{"Win10": "Running", "Win10-PipeSifu": second}[vm] {
					t.Errorf("%s state changed: %s", vm, b.vms[i].State)
				}
			}
		})
	}
}

func TestVMRequiredSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectTwoVM(t, ctx, newTwoVMBackend("Off"))
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range tools.Tools {
		s := schemaOf(t, tool)
		if _, ok := s.Properties["vm"]; !ok {
			continue
		}
		seen[tool.Name] = true
		required := slices.Contains(s.Required, "vm")
		switch tool.Name {
		case "vm_list":
			// covered by TestVMListNeedsNoVM
		case "vm_end_turn":
			if required {
				t.Errorf("vm_end_turn requires vm: %v", s.Required)
			}
		default:
			if !required {
				t.Errorf("%s does not require vm: %v", tool.Name, s.Required)
			}
		}
	}
	for _, name := range []string{"vm_end_turn", "vm_restore", "vm_shutdown", "vm_turn_off", "vm_exec", "vm_observe", "vm_click", "vm_checkpoints"} {
		if !seen[name] {
			t.Errorf("%s has no vm property", name)
		}
	}
}

// vm_list is the one tool without vm: it is how a caller learns the names.
func TestVMListNeedsNoVM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := connectTwoVM(t, ctx, newTwoVMBackend("Off"))
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "vm_list" {
			if s := schemaOf(t, tool); slices.Contains(s.Required, "vm") {
				t.Errorf("vm_list schema requires vm: %v", s.Required)
			}
		}
	}
	out, isErr := callRaw(t, ctx, cs, "vm_list", map[string]any{})
	if vms, _ := out["vms"].([]any); isErr || len(vms) != 2 {
		t.Errorf("vm_list without vm: %v", out)
	}
}

func TestVMRequiredEndTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("without vm ends the task and cleans only its own checkpoints", func(t *testing.T) {
		b := newTwoVMBackend("Running")
		cs := connectTwoVM(t, ctx, b)
		created, isErr := callRaw(t, ctx, cs, "vm_checkpoint", map[string]any{"vm": "Win10", "label": "mine"})
		if isErr {
			t.Fatalf("vm_checkpoint: %v", created)
		}
		out, isErr := callRaw(t, ctx, cs, "vm_end_turn", map[string]any{})
		deleted, _ := out["deleted_checkpoints"].([]any)
		if isErr || len(deleted) != 1 || deleted[0] != created["name"] {
			t.Fatalf("vm_end_turn: %v", out)
		}
		if w, p := b.checkpointIDs("Win10"), b.checkpointIDs("Win10-PipeSifu"); !reflect.DeepEqual(w, []string{"w-base", "w-old"}) || !reflect.DeepEqual(p, []string{"p-base", "p-old"}) {
			t.Fatalf("other runs' checkpoints touched: Win10 %v, Win10-PipeSifu %v", w, p)
		}
		if dels := slices.DeleteFunc(b.recorded(), func(c string) bool { return !strings.HasPrefix(c, "DeleteCheckpoint") }); !reflect.DeepEqual(dels, []string{"DeleteCheckpoint:Win10:new-Win10"}) {
			t.Fatalf("deletes %v", dels)
		}
	})

	t.Run("all_temp without vm is refused and deletes nothing", func(t *testing.T) {
		b := newTwoVMBackend("Running")
		cs := connectTwoVM(t, ctx, b)
		for _, args := range []map[string]any{{"all_temp": true}, {"all_temp": true, "vm": " "}} {
			e, isErr := callRaw(t, ctx, cs, "vm_end_turn", args)
			vms, _ := e["vms"].([]any)
			if !isErr || e["error"] != codeInvalidArgument || !strings.Contains(fmt.Sprint(e["reason"]), "vm is required with all_temp") || !reflect.DeepEqual(vms, []any{"Win10", "Win10-PipeSifu"}) {
				t.Errorf("%v: %v", args, e)
			}
		}
		if calls := b.recorded("ListVMs"); len(calls) != 0 {
			t.Fatalf("all_temp without vm reached the backend: %v", calls)
		}
		if w, p := b.checkpointIDs("Win10"), b.checkpointIDs("Win10-PipeSifu"); len(w) != 2 || len(p) != 2 {
			t.Fatalf("checkpoints deleted: Win10 %v, Win10-PipeSifu %v", w, p)
		}
	})

	t.Run("a blank vm is refused, not treated as ending the whole task", func(t *testing.T) {
		b := newTwoVMBackend("Running")
		cs := connectTwoVM(t, ctx, b)
		created, isErr := callRaw(t, ctx, cs, "vm_checkpoint", map[string]any{"vm": "Win10", "label": "mine"})
		if isErr {
			t.Fatalf("vm_checkpoint: %v", created)
		}
		e, isErr := callRaw(t, ctx, cs, "vm_end_turn", map[string]any{"vm": "  "})
		if !isErr || e["error"] != codeInvalidArgument || !strings.Contains(fmt.Sprint(e["reason"]), "vm must not be blank") {
			t.Fatalf("blank vm: %v", e)
		}
		if ids := b.checkpointIDs("Win10"); len(ids) != 3 {
			t.Fatalf("blank vm cleaned up anyway: %v", ids)
		}
	})

	t.Run("all_temp with vm acts on that VM only", func(t *testing.T) {
		b := newTwoVMBackend("Off")
		cs := connectTwoVM(t, ctx, b)
		out, isErr := callRaw(t, ctx, cs, "vm_end_turn", map[string]any{"vm": "Win10-PipeSifu", "all_temp": true})
		deleted, _ := out["deleted_checkpoints"].([]any)
		if isErr || !reflect.DeepEqual(deleted, []any{otherRunTemp}) {
			t.Fatalf("vm_end_turn all_temp: %v", out)
		}
		if w, p := b.checkpointIDs("Win10"), b.checkpointIDs("Win10-PipeSifu"); !reflect.DeepEqual(w, []string{"w-base", "w-old"}) || !reflect.DeepEqual(p, []string{"p-base"}) {
			t.Fatalf("checkpoints after: Win10 %v, Win10-PipeSifu %v", w, p)
		}
		for _, c := range b.recorded("ListVMs") {
			if strings.Contains(c, ":Win10:") || strings.HasSuffix(c, ":Win10") {
				t.Errorf("Win10 touched: %s", c)
			}
		}
	})
}

func TestVMRequiredExplicitVMUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("write tool", func(t *testing.T) {
		b := newTwoVMBackend("Running")
		cs := connectTwoVM(t, ctx, b)
		out, isErr := callRaw(t, ctx, cs, "vm_shutdown", map[string]any{"vm": "Win10-PipeSifu"})
		if isErr || out["vm"] != "Win10-PipeSifu" || out["state"] != "off" {
			t.Fatalf("vm_shutdown: %v", out)
		}
		var power []string
		for _, c := range b.recorded() {
			if strings.HasPrefix(c, "Shutdown") || strings.HasPrefix(c, "Stop") {
				power = append(power, c)
			}
		}
		if !reflect.DeepEqual(power, []string{"Shutdown:Win10-PipeSifu"}) {
			t.Fatalf("power calls %v", power)
		}
		if i, _ := b.vm("Win10"); b.vms[i].State != "Running" {
			t.Fatalf("Win10 state %s", b.vms[i].State)
		}
	})

	t.Run("read-only tool", func(t *testing.T) {
		b := newTwoVMBackend("Off")
		cs := connectTwoVM(t, ctx, b)
		out, isErr := callRaw(t, ctx, cs, "vm_checkpoints", map[string]any{"vm": "Win10-PipeSifu"})
		cps, _ := out["checkpoints"].([]any)
		if isErr || out["vm"] != "Win10-PipeSifu" || len(cps) != 2 || cps[0].(map[string]any)["id"] != "p-base" {
			t.Fatalf("vm_checkpoints: %v", out)
		}
		if lists := slices.DeleteFunc(b.recorded(), func(c string) bool { return !strings.HasPrefix(c, "ListCheckpoints") }); !reflect.DeepEqual(lists, []string{"ListCheckpoints:Win10-PipeSifu"}) {
			t.Fatalf("checkpoint lists %v", lists)
		}
	})
}
