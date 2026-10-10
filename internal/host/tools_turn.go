package host

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
)

type endTurnIn struct {
	VM      string `json:"vm,omitempty" jsonschema:"clean only this task's waits, observations, temporary checkpoints and ownership on this VM, leaving the task active on other VMs; omit to end the entire task"`
	AllTemp bool   `json:"all_temp,omitempty" jsonschema:"also delete temp checkpoints of other runs (every checkpoint named <run_id>-temp-<label>) on the VM; requires vm; default false"`
}

// endTurnOut is vm_end_turn's result. DeletedCheckpoints are the names of the deleted checkpoints; Skipped lists
// checkpoints that were not deleted, with the reason: registered temps that are no longer temp-named (renamed to keep
// or by hand) and, with all_temp, temps whose deletion failed; Errors are this run's registered checkpoints whose
// deletion failed (kept for the next call) and VMs whose checkpoints could not be listed.
type endTurnOut struct {
	CancelledWaits     int                 `json:"cancelled_waits"`
	DeletedCheckpoints []string            `json:"deleted_checkpoints"`
	Skipped            []skippedCheckpoint `json:"skipped"`
	Errors             []string            `json:"errors"`
}

// skippedCheckpoint is a checkpoint vm_end_turn did not delete, with the reason.
type skippedCheckpoint struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// takeVMWaits removes matching pending waits for cancellation outside the lock.
func (t *turnState) takeVMWaits(vm string) (waits []context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, cancel := range t.waits {
		if vm != "" && !strings.EqualFold(t.waitVMs[id], vm) {
			continue
		}
		waits = append(waits, cancel)
		delete(t.waits, id)
		delete(t.waitVMs, id)
	}
	return waits
}

// takeCheckpoints removes and returns the temporary checkpoints of vm (compared case-insensitively, as Hyper-V
// names VMs), or of every VM when vm is "".
func (t *turnState) takeCheckpoints(vm string) map[string][]tempCheckpoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	checkpoints := map[string][]tempCheckpoint{}
	for name, cps := range t.checkpoints {
		if vm == "" || strings.EqualFold(name, vm) {
			checkpoints[name] = cps
			delete(t.checkpoints, name)
		}
	}
	return checkpoints
}

// deleteRegisteredTemp deletes this run's registered temp checkpoints of one VM after checking each against the
// VM's current checkpoint list: one that no longer exists is forgotten, one whose current name is no longer a temp
// name (renamed to keep by another client or by hand) is forgotten and reported in Skipped, one whose deletion fails
// stays registered for the next call and is reported in Errors.
func deleteRegisteredTemp(d *deps, vm string, cps []tempCheckpoint, out *endTurnOut) {
	l, err := d.raw.ListCheckpoints(vm)
	if err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", vm, err))
		for _, c := range cps {
			d.turn.addTempCheckpoint(vm, c)
		}
		return
	}
	byID := map[string]hyperv.Checkpoint{}
	for _, c := range l.Checkpoints {
		byID[strings.ToUpper(c.ID)] = c
	}
	for _, c := range cps {
		cur, ok := byID[strings.ToUpper(c.ID)]
		if !ok {
			continue // already gone: nothing to delete, nothing to remember
		}
		if _, typ, _ := parseCheckpointName(cur.Name); typ != checkpointTemp {
			out.Skipped = append(out.Skipped, skippedCheckpoint{ID: cur.ID, Name: cur.Name, Reason: "no longer a temp checkpoint (now " + typ + "); not deleted"})
			continue
		}
		if err := d.raw.DeleteCheckpoint(vm, c.ID, false); err != nil {
			if isNoCheckpoint(err) {
				continue
			}
			out.Errors = append(out.Errors, fmt.Sprintf("%s/%s: %v", vm, cur.Name, err))
			d.turn.addTempCheckpoint(vm, c) // keep it for the next vm_end_turn
			continue
		}
		out.DeletedCheckpoints = append(out.DeletedCheckpoints, cur.Name)
	}
}

// registerTurn registers vm_end_turn.
func registerTurn(d *deps) {
	addToolIn(d, toolSpec{name: "vm_end_turn", desc: "End this task: cancel its pending waits, wait for its in-flight calls to finish, delete its temporary checkpoints, clear its observations and release its VM write ownership. Pass vm to clean only that VM and keep the task active; omit vm to end the task and reject late calls with this task_id. Repeated full cleanup is harmless. Cleanup failures retain ownership so the same task can retry. Keep and manual checkpoints, running programs and VM power are preserved. A Stop hook on another connection must pass the original task_id. all_temp: true additionally deletes every temp checkpoint of any run (names <run_id>-temp-<label>) on the VM named by vm (required with all_temp), to clean up after a crashed or restarted server; this crosses runs, so use it only when no other HyperHand client is working on the VM. Deleting merges disk differences and can take minutes per checkpoint; registered temps that were renamed to keep (or by hand) in the meantime are not deleted and listed in skipped, as are temps all_temp could not delete.", destructive: true, idempotent: true}, func(ctx context.Context, in endTurnIn) (*mcp.CallToolResult, error) {
		var task *taskState
		if t, ok := ctx.Value(taskContextKey{}).(*taskState); ok {
			task = t
			task.mu.Lock()
			ended := task.ended
			task.mu.Unlock()
			if ended {
				return jsonResult(endTurnOut{DeletedCheckpoints: []string{}, Skipped: []skippedCheckpoint{}, Errors: []string{}})
			}
		}
		scoped := *d
		scoped.turn = d.taskTurn(ctx)
		d := &scoped
		cancelled := 0
		cleanupSucceeded := false
		if task != nil {
			if in.VM != "" {
				v, err := d.raw.Find(in.VM)
				if err != nil {
					return nil, err
				}
				in.VM = v.Name
			}
			var err error
			cancelled, err = task.beginEnd(ctx, in.VM)
			if errors.Is(err, errTaskEnded) {
				return jsonResult(endTurnOut{DeletedCheckpoints: []string{}, Skipped: []skippedCheckpoint{}, Errors: []string{}})
			}
			if err != nil {
				return nil, err
			}
			defer func() { task.finishEnd(in.VM, cleanupSucceeded) }()
		}
		waits := d.turn.takeVMWaits(in.VM)
		for _, cancel := range waits {
			cancel()
		}
		out := endTurnOut{CancelledWaits: cancelled + len(waits), DeletedCheckpoints: []string{}, Skipped: []skippedCheckpoint{}, Errors: []string{}}
		if in.AllTemp {
			if task != nil {
				d.tasks.mu.Lock()
				for vm, owner := range d.tasks.owners {
					if owner != task.id && (in.VM == "" || strings.EqualFold(vm, in.VM)) {
						d.tasks.mu.Unlock()
						return nil, refuse("vm_busy", "omit all_temp to clean only this task, or wait for the other task to finish", map[string]any{"vm": vm, "owner_task_id": owner}, "all_temp would touch another active task")
					}
				}
				// Reserve all VMs while enumerating/deleting to prevent another task claiming one mid-cleanup.
				vms, err := d.raw.ListVMs()
				if err != nil {
					d.tasks.mu.Unlock()
					return nil, err
				}
				for _, vm := range vms {
					if in.VM == "" || strings.EqualFold(vm.Name, in.VM) {
						d.tasks.owners[strings.ToUpper(vm.Name)] = task.id
					}
				}
				d.tasks.mu.Unlock()
			}
			deleteAllTemp(d, in.VM, &out)
		} else {
			for vm, cps := range d.turn.takeCheckpoints(in.VM) {
				deleteRegisteredTemp(d, vm, cps, &out)
			}
		}
		if task != nil && len(out.Errors) == 0 && (!in.AllTemp || len(out.Skipped) == 0) {
			if d.mirrors != nil {
				d.mirrors.discard(task.runID, in.VM)
			}
			d.tasks.release(task, in.VM)
			task.obs.clear(in.VM)
			cleanupSucceeded = true
		}
		return jsonResult(out)
	})
}

// deleteAllTemp deletes, by id, every checkpoint whose name parses as a temp one of any run on vm (or on every VM
// when vm is ""), unregistering the deleted ones from turnState. Checkpoints it could not delete go to out.Skipped,
// VMs it could not list or resolve to out.Errors.
func deleteAllTemp(d *deps, vm string, out *endTurnOut) {
	var vms []string
	if vm != "" {
		v, err := d.raw.Find(vm)
		if err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", vm, err))
			return
		}
		vms = []string{v.Name}
	} else {
		all, err := d.raw.ListVMs()
		if err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("list VMs: %v", err))
			return
		}
		for _, v := range all {
			vms = append(vms, v.Name)
		}
	}
	for _, name := range vms {
		l, err := d.raw.ListCheckpoints(name)
		if err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		for _, c := range l.Checkpoints {
			if _, typ, _ := parseCheckpointName(c.Name); typ != checkpointTemp {
				continue
			}
			if err := d.raw.DeleteCheckpoint(name, c.ID, false); err != nil {
				out.Skipped = append(out.Skipped, skippedCheckpoint{ID: c.ID, Name: c.Name, Reason: err.Error()})
				continue
			}
			d.turn.removeTempCheckpoint(name, c.ID)
			out.DeletedCheckpoints = append(out.DeletedCheckpoints, c.Name)
		}
	}
}
