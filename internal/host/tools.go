package host

import (
	"context"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/proto"
)

// GuestAgentPath is where vm_install_agent puts the agent in the guest.
const GuestAgentPath = `C:\Users\Public\HyperHand\hyperhand-agent.exe`

type vmIn struct {
	VM string `json:"vm,omitempty" jsonschema:"VM name from vm_list (required)"`
}

// deps is what every tool registration needs. Tools live in tools_*.go, one register function per file:
//
//	tools_vm.go       VM power, status, unlock, checkpoints, exec, files, clipboard, wait, agent install/update
//	tools_observe.go  vm_windows, vm_observe (sets d.observe)
//	tools_actions.go  vm_click, vm_drag, vm_scroll, vm_type, vm_key, vm_set_value, vm_invoke
//	tools_checkpoint.go vm_checkpoint_delete, vm_checkpoint_keep (registered from registerVM)
//	tools_launch.go   vm_launch
//	tools_apps.go     vm_apps
//	tools_turn.go     vm_end_turn
//	tools_doctor.go   vm_doctor
type deps struct {
	s       *mcp.Server
	m       *Manager
	raw     Backend     // the backend without input serialisation
	backend lockedInput // the backend with every input call under input
	input   *sync.Mutex // held around every tool that sends keyboard or mouse input (see features.md 4.5)
	call    agentCall   // one agent op on a VM by name
	u       unlocker
	obs     *observationStore
	runID   string
	// observe is vm_observe's implementation, set by registerObserve; actions call it for observe_after.
	observe observeFunc
	// turn records state vm_end_turn cleans up: pending waits to cancel and temporary checkpoints by VM.
	turn    *turnState
	tasks   *taskRegistry
	mirrors *mirrorPlans
}

// turnState is what vm_end_turn cleans up. Tools register cancellable waits with addWait and temporary checkpoints
// with addTempCheckpoint; registerTurn implements the tool.
type turnState struct {
	mu          sync.Mutex
	waits       map[int]context.CancelFunc
	waitVMs     map[int]string
	nextWait    int
	checkpoints map[string][]tempCheckpoint // VM name -> temporary checkpoints created in this run
}

// tempCheckpoint is a temporary checkpoint vm_end_turn deletes: its Hyper-V ID and the name it was created with.
type tempCheckpoint struct {
	ID   string
	Name string
}

func newTurnState() *turnState {
	return &turnState{waits: map[int]context.CancelFunc{}, waitVMs: map[int]string{}, checkpoints: map[string][]tempCheckpoint{}}
}

// addWait registers a wait's cancel function; the returned function unregisters it.
func (t *turnState) addWait(cancel context.CancelFunc) func() {
	return t.addVMWait("", cancel)
}

func (t *turnState) addVMWait(vm string, cancel context.CancelFunc) func() {
	t.mu.Lock()
	id := t.nextWait
	t.nextWait++
	t.waits[id] = cancel
	t.waitVMs[id] = vm
	t.mu.Unlock()
	return func() { t.mu.Lock(); delete(t.waits, id); delete(t.waitVMs, id); t.mu.Unlock() }
}

// addTempCheckpoint remembers a temporary checkpoint created for vm.
func (t *turnState) addTempCheckpoint(vm string, c tempCheckpoint) {
	t.mu.Lock()
	t.checkpoints[vm] = append(t.checkpoints[vm], c)
	t.mu.Unlock()
}

// removeTempCheckpoint forgets the temporary checkpoint with ID id (e.g. after vm_checkpoint_keep renamed it).
func (t *turnState) removeTempCheckpoint(vm, id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cps := t.checkpoints[vm]
	for i, c := range cps {
		if c.ID == id {
			t.checkpoints[vm] = append(cps[:i:i], cps[i+1:]...)
			return
		}
	}
}

// NewServer builds the MCP server with all HyperHand tools.
func NewServer(m *Manager) *mcp.Server {
	raw := m.backend()
	input := &sync.Mutex{}
	s := mcp.NewServer(&mcp.Implementation{Name: "hyperhand", Version: proto.Version}, nil)
	call := func(ctx context.Context, vm, op string, args any, payload []byte, result any) ([]byte, error) {
		c, err := m.Client(vm)
		if err != nil {
			return nil, err
		}
		return c.Call(ctx, op, args, payload, result)
	}
	d := &deps{
		s:       s,
		m:       m,
		raw:     raw,
		backend: lockedInput{raw, input},
		input:   input,
		call:    call,
		u:       newUnlocker(call, raw, input),
		obs:     newObservationStore(),
		runID:   newRunID(),
		turn:    newTurnState(),
		tasks:   newTaskRegistry(),
		mirrors: &mirrorPlans{},
	}
	registerVM(d)
	registerObserve(d)
	registerActions(d)
	registerLaunch(d)
	registerApps(d)
	registerTurn(d)
	registerJobs(d)
	registerDoctor(d)
	return s
}

// notImplemented is the handler body of tools whose implementation is pending.
func notImplemented(name string) error {
	return refuse(codeFailed, "", nil, "%s is not implemented yet", name)
}
