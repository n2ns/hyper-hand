package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
)

func TestRunIDNaming(t *testing.T) {
	id := newRunID()
	if !regexp.MustCompile(`^run-\d{8}-\d{4}-[0-9a-f]{16}$`).MatchString(id) {
		t.Fatalf("run ID %q", id)
	}
	if id2 := newRunID(); id2 == id {
		t.Errorf("two run IDs are equal: %q", id)
	}
	name := checkpointName(id, "step3", false)
	if name != id+"-temp-step3" {
		t.Errorf("temp name %q", name)
	}
	if runID, typ, label := parseCheckpointName(name); runID != id || typ != checkpointTemp || label != "step3" {
		t.Errorf("parse(%q) = %q %q %q", name, runID, typ, label)
	}
	if keep := checkpointName(id, "golden", true); keep != id+"-keep-golden" {
		t.Errorf("keep name %q", keep)
	}
}

func TestParseCheckpointName(t *testing.T) {
	for _, c := range []struct{ name, runID, typ, label string }{
		{"run-20261010-0812-7f3a-temp-step3", "run-20261010-0812-7f3a", "temp", "step3"},
		{"run-20261010-0812-7f3a-keep-before-install", "run-20261010-0812-7f3a", "keep", "before-install"},
		{"run-20261010-0812-7f3a-keep-a-temp-b", "run-20261010-0812-7f3a", "keep", "a-temp-b"},
		{"run-20261010-0812-7f3a-temp-", "", "manual", ""},  // empty label
		{"run-20261010-0812-7f3a-old-x", "", "manual", ""},  // unknown type
		{"run-2026101-0812-7f3a-temp-x", "", "manual", ""},  // short date
		{"run-20261010-0812-7F3A-temp-x", "", "manual", ""}, // uppercase hex
		{"baseline", "", "manual", ""},
		{"干净", "", "manual", ""},
		{"", "", "manual", ""},
	} {
		runID, typ, label := parseCheckpointName(c.name)
		if runID != c.runID || typ != c.typ || label != c.label {
			t.Errorf("parseCheckpointName(%q) = %q %q %q, want %q %q %q", c.name, runID, typ, label, c.runID, c.typ, c.label)
		}
	}
}

// vmToolsBackend is an in-memory Hyper-V for the VM tools: one checkpoint tree (shared by its VMs) that create,
// delete, rename and restore change as Hyper-V would, plus a record of the operations; unexpected operations fail
// (nil Backend). A delete of an id ending in "-fails" fails.
type vmToolsBackend struct {
	Backend
	mu                 sync.Mutex
	created            []string
	deleted            []string
	renamed            []string
	restored           []string
	vms                []hyperv.VM
	listErr            error
	createErr          error
	tree               *hyperv.CheckpointList // nil: defaultTree on first use
	deleteErr          error                  // returned by DeleteCheckpoint and RestoreCheckpoint when set
	listCheckpointsErr error                  // returned by ListCheckpoints when set
}

// defaultTree is a manual root with one temp child, which is the current state's parent.
func defaultTree() hyperv.CheckpointList {
	return hyperv.CheckpointList{CheckpointType: "Standard", CurrentParentID: "id-2", Checkpoints: []hyperv.Checkpoint{
		{Name: "baseline", CreatedAt: "2026-10-04T10:00:00+08:00", ID: "id-1", Kind: "standard", State: "off"},
		{Name: "run-20261010-0812-7f3a-temp-step3", CreatedAt: "2026-10-10T08:15:00+08:00", ID: "id-2", ParentID: "id-1", Kind: "standard", State: "running"},
	}}
}

// current returns the tree (under b.mu).
func (b *vmToolsBackend) current() *hyperv.CheckpointList {
	if b.tree == nil {
		t := defaultTree()
		b.tree = &t
	}
	return b.tree
}

// node returns the checkpoint with ID id and its index in the tree (under b.mu).
func (b *vmToolsBackend) node(id string) (hyperv.Checkpoint, int, error) {
	for i, c := range b.current().Checkpoints {
		if c.ID == id {
			return c, i, nil
		}
	}
	return hyperv.Checkpoint{}, -1, fmt.Errorf("%w: %s", hyperv.ErrCheckpointNotFound, id)
}

func (b *vmToolsBackend) ListVMs() ([]hyperv.VM, error) { return b.vms, b.listErr }
func (b *vmToolsBackend) Find(name string) (hyperv.VM, error) {
	for _, v := range b.vms {
		if name == "" && v.State == "Running" || strings.EqualFold(v.Name, name) {
			return v, nil
		}
	}
	if name == "" {
		return hyperv.VM{}, errors.New("no running VM")
	}
	return hyperv.VM{}, errors.New("VM \"" + name + "\" not found")
}
func (b *vmToolsBackend) ListCheckpoints(vm string) (hyperv.CheckpointList, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listCheckpointsErr != nil {
		return hyperv.CheckpointList{}, b.listCheckpointsErr
	}
	t := *b.current()
	t.Checkpoints = append([]hyperv.Checkpoint{}, t.Checkpoints...)
	return t, nil
}

// CreateCheckpoint adds a running standard checkpoint under the current parent and makes it the current parent.
func (b *vmToolsBackend) CreateCheckpoint(vm, name string) (hyperv.Checkpoint, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.createErr != nil {
		return hyperv.Checkpoint{}, b.createErr
	}
	b.created = append(b.created, vm+"/"+name)
	t := b.current()
	c := hyperv.Checkpoint{ID: "id-" + name, Name: name, ParentID: t.CurrentParentID, CreatedAt: "2026-10-10T09:00:00+08:00", Kind: "standard", State: "running"}
	t.Checkpoints = append(t.Checkpoints, c)
	t.CurrentParentID = c.ID
	return c, nil
}

// DeleteCheckpoint removes the checkpoint (with subtree, its descendants too); surviving children and a removed
// current parent move to the deleted checkpoint's parent.
func (b *vmToolsBackend) DeleteCheckpoint(vm, id string, subtree bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deleteErr != nil {
		return b.deleteErr
	}
	if strings.HasSuffix(id, "-fails") {
		return errors.New("Hyper-V job failed")
	}
	target, _, err := b.node(id)
	if err != nil {
		return err
	}
	t := b.current()
	removed := map[string]bool{id: true}
	var kept []hyperv.Checkpoint
	for _, c := range t.Checkpoints {
		if c.ID == id || subtree && removed[c.ParentID] {
			removed[c.ID] = true
			continue
		}
		kept = append(kept, c)
	}
	for i := range kept {
		if removed[kept[i].ParentID] {
			kept[i].ParentID = target.ParentID
		}
	}
	if removed[t.CurrentParentID] {
		t.CurrentParentID = target.ParentID
	}
	t.Checkpoints = kept
	b.deleted = append(b.deleted, vm+"/"+id)
	return nil
}
func (b *vmToolsBackend) RenameCheckpoint(vm, id, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, i, err := b.node(id)
	if err != nil {
		return err
	}
	b.current().Checkpoints[i].Name = name
	b.renamed = append(b.renamed, vm+"/"+id+"="+name)
	return nil
}
func (b *vmToolsBackend) RestoreCheckpoint(vm, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deleteErr != nil {
		return b.deleteErr
	}
	if _, _, err := b.node(id); err != nil {
		return err
	}
	b.current().CurrentParentID = id
	b.restored = append(b.restored, vm+"/"+id)
	return nil
}

// connectTools builds a server on b and returns the client session and the server's deps.
func connectTools(t *testing.T, ctx context.Context, b Backend) (*mcp.ClientSession, *deps) {
	t.Helper()
	m := &Manager{Backend: b}
	s := mcp.NewServer(&mcp.Implementation{Name: "hyperhand", Version: "test"}, nil)
	d := &deps{s: s, m: m, raw: b, backend: lockedInput{b, &sync.Mutex{}}, input: &sync.Mutex{}, obs: newObservationStore(), runID: newRunID(), turn: newTurnState()}
	d.call = func(ctx context.Context, vm, op string, args any, payload []byte, result any) ([]byte, error) {
		c, err := m.Client(vm)
		if err != nil {
			return nil, err
		}
		return c.Call(ctx, op, args, payload, result)
	}
	d.u = newUnlocker(d.call, b, d.input)
	registerVM(d)
	registerTurn(d)
	registerLaunch(d)
	registerBatch(d)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	if vm := fixtureVM(b); vm != "" {
		fixtureVMs.Store(cs, vm)
		t.Cleanup(func() { fixtureVMs.Delete(cs) })
	}
	return cs, d
}

// fixtureVMs maps a connectTools session to its fixture VM name (the first running VM of its backend), which the
// call helpers pass as vm when a call omits it: every tool but vm_list requires vm.
var fixtureVMs sync.Map

// fixtureVM returns the name of b's first running VM, or "" when b lists none (or cannot list).
func fixtureVM(b Backend) (name string) {
	defer func() {
		if recover() != nil { // a fake without ListVMs
			name = ""
		}
	}()
	vms, err := b.ListVMs()
	if err != nil {
		return ""
	}
	for _, v := range vms {
		if v.State == "Running" {
			return v.Name
		}
	}
	return ""
}

// withFixtureVM returns args with vm set to the session's fixture VM when args has no vm key. vm_list takes no vm and
// vm_end_turn without vm ends the whole task, so both are left as given; an explicit vm (even "") is kept, so a test
// can still omit vm on purpose by passing "".
func withFixtureVM(cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	if name == "vm_list" || name == "vm_end_turn" {
		return args
	}
	if _, ok := args["vm"]; ok {
		return args
	}
	vm, ok := fixtureVMs.Load(cs)
	if !ok {
		return args
	}
	out := map[string]any{"vm": vm}
	for k, v := range args {
		out[k] = v
	}
	return out
}

// callJSON calls a tool (passing the fixture VM when args omit vm; see withFixtureVM) and decodes its JSON text item
// into out; it fails the test on an isError result.
func callJSON(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: withFixtureVM(cs, name, args)})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if r.IsError {
		t.Fatalf("%s refused: %s", name, resultText(r))
	}
	if err := json.Unmarshal([]byte(resultText(r)), out); err != nil {
		t.Fatalf("%s result %s: %v", name, resultText(r), err)
	}
}

// callRefused calls a tool (passing the fixture VM when args omit vm) expecting a refusal and returns the decoded
// error object.
func callRefused(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: withFixtureVM(cs, name, args)})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !r.IsError {
		t.Fatalf("%s succeeded: %s", name, resultText(r))
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(resultText(r)), &obj); err != nil {
		t.Fatalf("%s error %s: %v", name, resultText(r), err)
	}
	return obj
}

func TestVMListAndCheckpointsShapes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: "Running"}, {Name: "Win11", ID: "id-b", State: "Off"}}}
	cs, d := connectTools(t, ctx, b)
	var list struct {
		VMs   []hyperv.VM `json:"vms"`
		RunID string      `json:"run_id"`
	}
	callJSON(t, ctx, cs, "vm_list", nil, &list)
	if len(list.VMs) != 2 || list.VMs[0].Name != "Win10" || list.RunID != d.runID {
		t.Errorf("vm_list %+v", list)
	}
	var cps checkpointsOut
	callJSON(t, ctx, cs, "vm_checkpoints", nil, &cps)
	want := checkpointsOut{VM: "Win10", CheckpointType: "Standard", CurrentParent: ptr("id-2"), Checkpoints: []checkpointOut{
		{ID: "id-1", Name: "baseline", CreatedAt: "2026-10-04T10:00:00+08:00", Type: "manual", Kind: "standard", State: "off", Children: 1},
		{ID: "id-2", Name: "run-20261010-0812-7f3a-temp-step3", Parent: ptr("id-1"), CreatedAt: "2026-10-10T08:15:00+08:00", Type: "temp", RunID: ptr("run-20261010-0812-7f3a"), Label: ptr("step3"), Kind: "standard", State: "running", Current: true},
	}}
	if !reflect.DeepEqual(cps, want) {
		t.Errorf("vm_checkpoints %s", jsonString(cps))
	}
	// The null fields render as JSON null, not as "" or missing.
	raw := map[string]any{}
	callJSON(t, ctx, cs, "vm_checkpoints", nil, &raw)
	if first, ok := raw["checkpoints"].([]any)[0].(map[string]any); !ok || first["parent"] != nil || first["run_id"] != nil || first["label"] != nil {
		t.Errorf("manual checkpoint JSON %v", raw["checkpoints"])
	}
	// An unknown VM is an invalid argument that names the fix.
	e := callRefused(t, ctx, cs, "vm_status", map[string]any{"vm": "nope"})
	if e["error"] != codeInvalidArgument || e["run_id"] != d.runID || !strings.Contains(e["next"].(string), "vm_list") {
		t.Errorf("unknown VM: %v", e)
	}
	// Tool annotations follow the design.
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := map[string]bool{"vm_list": true, "vm_status": true, "vm_checkpoints": true, "vm_clipboard_get": true, "vm_pull": true, "vm_wait": true}
	destructive := map[string]bool{"vm_turn_off": true, "vm_restore": true, "vm_shutdown": true, "vm_update_agent": true, "vm_push": true, "vm_end_turn": true, "vm_checkpoint_delete": true, "vm_batch": true}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
		a := tool.Annotations
		if a == nil || a.DestructiveHint == nil || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s: annotations %+v", tool.Name, a)
			continue
		}
		if a.ReadOnlyHint != readOnly[tool.Name] || *a.DestructiveHint != destructive[tool.Name] {
			t.Errorf("%s: readOnly %v destructive %v", tool.Name, a.ReadOnlyHint, *a.DestructiveHint)
		}
	}
	for _, name := range []string{"vm_checkpoints", "vm_checkpoint", "vm_restore", "vm_checkpoint_delete", "vm_checkpoint_keep", "vm_end_turn"} {
		if !names[name] {
			t.Errorf("%s is not registered", name)
		}
	}
}

func ptr(s string) *string { return &s }

// jsonString renders v for test failure messages.
func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestVMWaitValidatesKind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, _ := connectTools(t, ctx, &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: "Running"}}})
	for _, args := range []map[string]any{
		{"kind": "window_exists", "name": "x"},
		{"kind": "process_exit"},
		{"kind": "file_exists"},
	} {
		if e := callRefused(t, ctx, cs, "vm_wait", args); e["error"] != codeInvalidArgument {
			t.Errorf("%v: %v", args, e)
		}
	}
}

func TestAgentErr(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{errors.New("connect to agent: dial failed"), codeAgentRequired},
		{errors.New("agent: EOF"), codeAgentRequired},
		{errors.New(`C:\x.txt: agent: EOF`), codeAgentRequired},
		{errors.New(`unknown op "launch"`), codeAgentOutdated},
		{errors.New(`VM "x" not found`), codeInvalidArgument},
		{errors.New("no running VM"), codeInvalidArgument},
		{errors.New("access denied"), codeFailed},
		{refuse(codeNoWindow, "", nil, "x"), codeNoWindow},
	} {
		if got := asToolError(agentErr(c.err)).Code; got != c.code {
			t.Errorf("agentErr(%v) = %s, want %s", c.err, got, c.code)
		}
	}
	if err := agentErr(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
}
