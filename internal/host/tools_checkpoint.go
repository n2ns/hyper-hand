package host

import (
	"context"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hyperhand/internal/hyperv"
)

type checkpointDeleteIn struct {
	VM      string `json:"vm,omitempty" jsonschema:"VM name from vm_list (required)"`
	ID      string `json:"id,omitempty" jsonschema:"checkpoint id from vm_checkpoints (the stable selector; required for manual checkpoints)"`
	Name    string `json:"name,omitempty" jsonschema:"name of a temp or keep checkpoint, accepted when exactly one checkpoint has it"`
	Subtree bool   `json:"subtree,omitempty" jsonschema:"also delete every descendant; default false (children are re-parented to the deleted checkpoint's parent)"`
}
type checkpointKeepIn struct {
	VM    string `json:"vm,omitempty" jsonschema:"VM name from vm_list (required)"`
	ID    string `json:"id,omitempty" jsonschema:"id of a temp checkpoint from vm_checkpoints (preferred)"`
	Name  string `json:"name,omitempty" jsonschema:"name of a temp checkpoint, accepted when exactly one checkpoint has it"`
	Label string `json:"label,omitempty" jsonschema:"new label (1 to 64 characters without \\ / : * ? \" < > | or line breaks); default: the temp checkpoint's label"`
}

// checkpointDeleteOut is vm_checkpoint_delete's result: every checkpoint that was deleted (the selected one and,
// with subtree, its descendants), whether the current state's parent was among them (its disk differences were
// merged into the current disk) and how long Hyper-V took.
type checkpointDeleteOut struct {
	Deleted           []checkpointRef `json:"deleted"`
	MergedIntoCurrent bool            `json:"merged_into_current"`
	ElapsedMs         int64           `json:"elapsed_ms"`
}

// checkpointTypeIn is vm_set_checkpoint_type's input.
type checkpointTypeIn struct {
	VM   string `json:"vm,omitempty" jsonschema:"VM name from vm_list (required)"`
	Type string `json:"type" jsonschema:"Standard (disk and memory; restores to the running state), Production (application-consistent through the guest's VSS, restores to off; falls back to Standard when VSS fails), ProductionOnly (like Production but fails instead of falling back) or Disabled (vm_checkpoint refused); case-insensitive"`
}

// checkpointTypeOut is vm_set_checkpoint_type's result: the setting before and after.
type checkpointTypeOut struct {
	VM             string `json:"vm"`
	CheckpointType string `json:"checkpoint_type"`
	Previous       string `json:"previous"`
}

// registerCheckpoint registers vm_set_checkpoint_type, vm_checkpoint_delete and vm_checkpoint_keep (vm_checkpoints,
// vm_checkpoint and vm_restore are in registerVM).
func registerCheckpoint(d *deps) {
	backend := d.backend
	addToolIn(d, toolSpec{name: "vm_set_checkpoint_type", desc: "Set the VM's checkpoint type, the Hyper-V setting (Set-VM -CheckpointType) that decides what kind of checkpoint vm_checkpoint creates: Standard, Production, ProductionOnly or Disabled. Existing checkpoints keep their kind. Allowed while the VM runs; changes nothing in the guest. Returns the new checkpoint_type and the previous one; an unchanged setting is not written again. vm_checkpoints reports the current setting.", idempotent: true}, func(ctx context.Context, in checkpointTypeIn) (*mcp.CallToolResult, error) {
		want := ""
		for _, t := range hyperv.CheckpointTypes {
			if strings.EqualFold(t, strings.TrimSpace(in.Type)) {
				want = t
			}
		}
		if want == "" {
			return nil, refuse(codeInvalidArgument, "pass type as one of "+strings.Join(hyperv.CheckpointTypes, ", "), map[string]any{"types": hyperv.CheckpointTypes}, "unknown checkpoint type %q", in.Type)
		}
		v, err := backend.Find(in.VM)
		if err != nil {
			return nil, vmErr(err)
		}
		l, err := backend.ListCheckpoints(v.Name)
		if err != nil {
			return nil, err
		}
		if l.CheckpointType != want {
			if err := backend.SetCheckpointType(v.Name, want); err != nil {
				return nil, err
			}
		}
		return jsonResult(checkpointTypeOut{VM: v.Name, CheckpointType: want, Previous: l.CheckpointType})
	})
	addToolIn(d, toolSpec{name: "vm_checkpoint_delete", desc: "Delete a checkpoint selected by id (from vm_checkpoints; preferred, and the only selector a manual checkpoint accepts) or by name (temp and keep checkpoints, when exactly one has it). Its disk differences are merged into its children or, when it is the current state's parent (merged_into_current), into the VM's current disk; children are re-parented to its parent. With subtree: true it and every descendant are deleted. Merging can take minutes for large differences; the call waits for it. Types: temp (this or another run's rollback point), keep (a baseline; delete only when it is no longer needed), manual (made outside HyperHand).", destructive: true}, func(ctx context.Context, in checkpointDeleteIn) (*mcp.CallToolResult, error) {
		v, err := backend.Find(in.VM)
		if err != nil {
			return nil, vmErr(err)
		}
		l, err := backend.ListCheckpoints(v.Name)
		if err != nil {
			return nil, err
		}
		target, byName, err := selectCheckpoint(l, in.ID, in.Name)
		if err != nil {
			return nil, err
		}
		ref := checkpointRefOf(target)
		if byName && ref.Type == checkpointManual {
			return nil, refuse(codeInvalidArgument, "pass id "+ref.ID+" instead of name", map[string]any{"id": ref.ID}, "%q is a manual checkpoint (made outside HyperHand); manual checkpoints are deleted by id only", ref.Name)
		}
		nodes := subtreeOf(l, target.ID)
		if !in.Subtree {
			nodes = nodes[:1]
		}
		out := checkpointDeleteOut{Deleted: make([]checkpointRef, 0, len(nodes))}
		for _, c := range nodes {
			out.Deleted = append(out.Deleted, checkpointRefOf(c))
			out.MergedIntoCurrent = out.MergedIntoCurrent || c.ID == l.CurrentParentID
		}
		start := time.Now()
		if err := backend.DeleteCheckpoint(v.Name, target.ID, in.Subtree); err != nil {
			if isNoCheckpoint(err) {
				return nil, checkpointErr(err, target.ID)
			}
			return nil, refuse(codeFailed, "call vm_checkpoints; a merge may still be running in Hyper-V", map[string]any{"elapsed_ms": time.Since(start).Milliseconds()}, "%v", err)
		}
		out.ElapsedMs = time.Since(start).Milliseconds()
		for _, c := range out.Deleted {
			d.taskTurn(ctx).removeTempCheckpoint(v.Name, c.ID)
		}
		return jsonResult(out)
	})
	addToolIn(d, toolSpec{name: "vm_checkpoint_keep", desc: "Keep a temp checkpoint across runs: rename <run_id>-temp-<label> to <run_id>-keep-<label> (same run_id; label replaces the label) so that vm_end_turn no longer deletes it. Select it by id (from vm_checkpoints; preferred) or by name (when exactly one checkpoint has it). Only temp checkpoints are accepted: a keep one is already kept and a manual one is never deleted by vm_end_turn (both invalid_argument). Returns the id (unchanged), the new name and type keep."}, func(ctx context.Context, in checkpointKeepIn) (*mcp.CallToolResult, error) {
		v, err := backend.Find(in.VM)
		if err != nil {
			return nil, vmErr(err)
		}
		if in.Label != "" {
			if err := checkLabel(in.Label); err != nil {
				return nil, err
			}
		}
		l, err := backend.ListCheckpoints(v.Name)
		if err != nil {
			return nil, err
		}
		target, _, err := selectCheckpoint(l, in.ID, in.Name)
		if err != nil {
			return nil, err
		}
		runID, typ, label := parseCheckpointName(target.Name)
		switch typ {
		case checkpointKeep:
			return nil, refuse(codeInvalidArgument, "nothing to do; vm_end_turn does not delete keep checkpoints", map[string]any{"id": target.ID, "name": target.Name}, "%q is already a keep checkpoint", target.Name)
		case checkpointManual:
			return nil, refuse(codeInvalidArgument, "nothing to do; vm_end_turn does not delete manual checkpoints (pass a temp checkpoint's id to keep one)", map[string]any{"id": target.ID, "name": target.Name}, "%q is a manual checkpoint (made outside HyperHand), not a temp one", target.Name)
		}
		if in.Label != "" {
			label = in.Label
		}
		name := checkpointName(runID, label, true)
		if err := backend.RenameCheckpoint(v.Name, target.ID, name); err != nil {
			return nil, checkpointErr(err, target.ID)
		}
		d.taskTurn(ctx).removeTempCheckpoint(v.Name, target.ID)
		return jsonResult(checkpointRef{ID: target.ID, Name: name, Type: checkpointKeep})
	})
}
