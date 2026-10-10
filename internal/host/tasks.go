package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A task survives individual HTTP connections. Session defaults are only for
// persistent MCP sessions; callers sharing a session use explicit task_id values.
type taskRegistry struct {
	mu       sync.Mutex
	tasks    map[string]*taskState
	sessions map[*mcp.ServerSession]string
	owners   map[string]string // canonical VM name, folded -> task ID
}

type taskState struct {
	id, runID      string
	obs            *observationStore
	turn           *turnState
	mu             sync.Mutex
	active         map[int]taskCall
	next           int
	closing, ended bool
	closingVM      string
	changed        chan struct{}
	// session is the MCP session whose default task this is (no task_id); nil for an explicit task_id. Deleting
	// that session ends the task once no call of it is in flight.
	session *mcp.ServerSession
	// lastUsed is when a call of the task last started or returned.
	lastUsed time.Time
}

type taskCall struct {
	vm     string
	wait   bool
	cancel context.CancelFunc
}
type taskContextKey struct{}

var errTaskEnd = errors.New("the wait was cancelled by vm_end_turn")

// errTaskEnded: vm_end_turn found its task already ended; repeated cleanup is harmless and returns an empty result.
var errTaskEnded = errors.New("task already ended")

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{tasks: map[string]*taskState{}, sessions: map[*mcp.ServerSession]string{}, owners: map[string]string{}}
}

func (d *deps) taskObs(ctx context.Context) *observationStore {
	if t, ok := ctx.Value(taskContextKey{}).(*taskState); ok {
		return t.obs
	}
	return d.obs
}
func (d *deps) taskTurn(ctx context.Context) *turnState {
	if t, ok := ctx.Value(taskContextKey{}).(*taskState); ok {
		return t.turn
	}
	return d.turn
}
func (d *deps) taskRunID(ctx context.Context) string {
	if t, ok := ctx.Value(taskContextKey{}).(*taskState); ok {
		return t.runID
	}
	return d.runID
}

// Serialize only the revision boundaries with input dispatch. Holding input for
// the external call itself would deadlock lifecycle operations that inject keys.
func (d *deps) beginExternalMutation(ctx context.Context, vm string, lifecycle bool) func() {
	d.input.Lock()
	end := d.taskObs(ctx).beginMutation(vm, lifecycle)
	d.input.Unlock()
	return func() { d.input.Lock(); defer d.input.Unlock(); end() }
}

func (d *deps) resolveTask(req *mcp.CallToolRequest, id string, ending bool) (*taskState, error) {
	r := d.tasks
	if r == nil {
		return nil, nil
	}
	if len(id) > 128 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\r\n\t") {
		return nil, refuse("invalid_argument", "use a nonblank task_id of at most 128 characters", nil, "invalid task_id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	explicit := id != ""
	if !explicit {
		if req.Session == nil || (req.Extra != nil && req.Session.ID() == "") {
			return nil, refuse("task_required", "pass the same unique task_id on every tool call in this AI task", nil, "this transport has no persistent MCP session; task_id is required")
		}
		id = r.sessions[req.Session]
		if t := r.tasks[id]; t != nil {
			t.mu.Lock()
			ended := t.ended
			t.mu.Unlock()
			if ended && !ending {
				id = ""
			}
		}
		if id == "" {
			if _, watched := r.sessions[req.Session]; !watched {
				go d.watchSession(req.Session)
			}
			id = "task-" + newRunID()
			r.sessions[req.Session] = id
		}
	}
	t := r.tasks[id]
	if t == nil {
		t = &taskState{id: id, runID: newRunID(), obs: newObservationStoreWithRevisions(d.obs.revisions), turn: newTurnState(), active: map[int]taskCall{}, changed: make(chan struct{}), lastUsed: time.Now()}
		if !explicit {
			t.session = req.Session
		}
		r.tasks[id] = t
	}
	return t, nil
}

// watchSession ends the session's current default task when the MCP session is closed (an HTTP DELETE; there is no
// idle timeout),
// so clients that open one session per call do not leave a VM owned by a task nobody can reach.
func (d *deps) watchSession(ss *mcp.ServerSession) {
	_ = ss.Wait()
	r := d.tasks
	r.mu.Lock()
	t := r.tasks[r.sessions[ss]]
	delete(r.sessions, ss)
	r.mu.Unlock()
	if t != nil && t.session == ss {
		d.abandonTask(t)
	}
}

// abandonTask waits until t has no call in flight and no vm_end_turn running, then ends it the way vm_end_turn
// without vm does, except that its temporary checkpoints are left in place (nobody can ask for them any more;
// vm_end_turn with all_temp deletes them): pending waits are cancelled, mirror plans and observations dropped, VM
// ownership released, and the task forgotten.
func (d *deps) abandonTask(t *taskState) {
	t.mu.Lock()
	for len(t.active) > 0 || t.closing {
		changed := t.changed
		t.mu.Unlock()
		<-changed
		t.mu.Lock()
	}
	ended := t.ended
	t.ended = true
	t.signalLocked()
	t.mu.Unlock()
	if !ended {
		for _, cancel := range t.turn.takeVMWaits("") {
			cancel()
		}
		if d.mirrors != nil {
			d.mirrors.discard(t.runID, "")
		}
		d.tasks.release(t, "")
		t.obs.clear("")
	}
	d.tasks.mu.Lock()
	if d.tasks.tasks[t.id] == t {
		delete(d.tasks.tasks, t.id)
	}
	d.tasks.mu.Unlock()
}

func (t *taskState) signalLocked() { close(t.changed); t.changed = make(chan struct{}) }

func (t *taskState) enter(ctx context.Context, vm string, wait bool) (context.Context, func(), error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return ctx, nil, refuse("task_ended", "use a new task_id for a new task", nil, "task %q has ended", t.id)
	}
	if t.closing && (t.closingVM == "" || strings.EqualFold(t.closingVM, vm)) {
		return ctx, nil, refuse("task_busy", "wait for vm_end_turn to finish", nil, "task %q is being cleaned up", t.id)
	}
	id := t.next
	t.next++
	t.lastUsed = time.Now()
	cctx, cancel := context.WithCancelCause(ctx)
	t.active[id] = taskCall{vm: vm, wait: wait, cancel: func() { cancel(errTaskEnd) }}
	return cctx, func() {
		cancel(nil)
		t.mu.Lock()
		delete(t.active, id)
		t.lastUsed = time.Now()
		t.signalLocked()
		t.mu.Unlock()
	}, nil
}

func (r *taskRegistry) claim(t *taskState, vm string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.ToUpper(vm)
	if owner := r.owners[key]; owner != "" && owner != t.id {
		o := r.ownerLocked(key, t)
		fields := map[string]any{"vm": vm, "owner_task_id": owner, "owner_idle_ms": o.IdleMS, "owner_in_flight": o.InFlight}
		return refuse("vm_busy", "wait for the owning task to call vm_end_turn; read-only tools remain available. If the owner is abandoned (owner_in_flight 0 and a long owner_idle_ms, e.g. a crashed script), end it with vm_end_turn {task_id: owner_task_id, vm}: that deletes its temp checkpoints on the VM and releases the VM", fields, "VM %q is owned by task %q (idle %d ms, %d calls in flight)", vm, owner, o.IdleMS, o.InFlight)
	}
	r.owners[key] = t.id
	return nil
}

// vmOwner describes the task that holds a VM's write ownership.
type vmOwner struct {
	TaskID   string `json:"task_id"`
	IdleMS   int64  `json:"idle_ms"`   // since its last call started or returned; 0 while a call is in flight
	InFlight int    `json:"in_flight"` // its calls in progress, on any VM
	ThisTask bool   `json:"this_task"` // the caller's own task
}

// owner reports the owner of vm, nil when no task owns it. self is the caller's task (may be nil).
func (r *taskRegistry) owner(vm string, self *taskState) *vmOwner {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.ToUpper(vm)
	if r.owners[key] == "" {
		return nil
	}
	o := r.ownerLocked(key, self)
	return &o
}

func (r *taskRegistry) ownerLocked(key string, self *taskState) vmOwner {
	o := vmOwner{TaskID: r.owners[key], ThisTask: self != nil && self.id == r.owners[key]}
	if t := r.tasks[o.TaskID]; t != nil {
		t.mu.Lock()
		o.InFlight = len(t.active)
		if o.InFlight == 0 {
			o.IdleMS = time.Since(t.lastUsed).Milliseconds()
		}
		t.mu.Unlock()
	}
	return o
}

func (r *taskRegistry) release(t *taskState, vm string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, owner := range r.owners {
		if owner == t.id && (vm == "" || strings.EqualFold(key, vm)) {
			delete(r.owners, key)
		}
	}
}

// beginEnd blocks new calls before inspecting active operations, and waits for
// already accepted writes to finish before any checkpoint cleanup or release.
func (t *taskState) beginEnd(ctx context.Context, vm string) (int, error) {
	t.mu.Lock()
	if t.ended {
		// Ended meanwhile (its session was deleted): cleaning up now would reserve VMs for a forgotten task.
		t.mu.Unlock()
		return 0, errTaskEnded
	}
	if t.closing {
		t.mu.Unlock()
		return 0, refuse("task_busy", "retry vm_end_turn after the pending cleanup completes", nil, "task cleanup is already running")
	}
	t.closing = true
	t.closingVM = vm
	cancelled := 0
	for _, call := range t.active {
		if call.wait && (vm == "" || strings.EqualFold(vm, call.vm)) {
			call.cancel()
			cancelled++
		}
	}
	for {
		pending := false
		for _, call := range t.active {
			if vm == "" || strings.EqualFold(vm, call.vm) {
				pending = true
				break
			}
		}
		if !pending {
			t.mu.Unlock()
			return cancelled, nil
		}
		changed := t.changed
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			t.mu.Lock()
			t.closing = false
			t.signalLocked()
			t.mu.Unlock()
			return cancelled, ctx.Err()
		case <-changed:
		}
		t.mu.Lock()
	}
}

func (t *taskState) finishEnd(vm string, success bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if success && vm == "" {
		t.ended = true
	}
	t.closing = false
	t.signalLocked()
}

func taskResult(r *mcp.CallToolResult, t *taskState) *mcp.CallToolResult {
	if r == nil || t == nil {
		return r
	}
	for _, c := range r.Content {
		text, ok := c.(*mcp.TextContent)
		if !ok {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(text.Text), &obj) != nil || obj == nil {
			continue
		}
		obj["task_id"], _ = json.Marshal(t.id)
		obj["run_id"], _ = json.Marshal(t.runID)
		b, _ := json.Marshal(obj)
		text.Text = string(b)
	}
	return r
}
