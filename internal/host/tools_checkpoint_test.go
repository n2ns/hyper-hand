package host

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"hyperhand/internal/hyperv"
)

func TestCheckLabel(t *testing.T) {
	for _, c := range []struct {
		label string
		ok    bool
	}{
		{"step3", true}, {"before-restore", true}, {"干净环境", true}, {strings.Repeat("x", 64), true},
		{"", false}, {strings.Repeat("x", 65), false}, {"a/b", false}, {`a\b`, false}, {"a:b", false}, {"a*b", false},
		{"a?b", false}, {`a"b`, false}, {"a<b", false}, {"a>b", false}, {"a|b", false}, {"a\nb", false}, {"a\rb", false},
	} {
		err := checkLabel(c.label)
		if (err == nil) != c.ok {
			t.Errorf("checkLabel(%q) = %v, want ok=%v", c.label, err, c.ok)
		}
		if err != nil && asToolError(err).Code != codeInvalidArgument {
			t.Errorf("checkLabel(%q) code %s", c.label, asToolError(err).Code)
		}
	}
}

// registeredTemps returns the ids of vm's temp checkpoints registered with d.turn.
func registeredTemps(d *deps, vm string) []string {
	d.turn.mu.Lock()
	defer d.turn.mu.Unlock()
	var ids []string
	for _, c := range d.turn.checkpoints[vm] {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestCheckpointCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: "Running"}}}
	cs, d := connectTools(t, ctx, b)
	var out checkpointCreatedOut
	callJSON(t, ctx, cs, "vm_checkpoint", map[string]any{"label": "step4"}, &out)
	name := d.runID + "-temp-step4"
	want := checkpointCreatedOut{ID: "id-" + name, Name: name, Type: "temp", Kind: "standard", State: "running", Parent: ptr("id-2"), CreatedAt: "2026-10-10T09:00:00+08:00"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("vm_checkpoint %s", jsonString(out))
	}
	if got := registeredTemps(d, "Win10"); !reflect.DeepEqual(got, []string{"id-" + name}) {
		t.Errorf("registered temps %v", got)
	}
	// The new checkpoint is the current state's parent and its parent gained a child.
	var cps checkpointsOut
	callJSON(t, ctx, cs, "vm_checkpoints", nil, &cps)
	if *cps.CurrentParent != "id-"+name || !cps.Checkpoints[2].Current || cps.Checkpoints[1].Current || cps.Checkpoints[1].Children != 1 {
		t.Errorf("after create %s", jsonString(cps))
	}
	// keep is not registered for vm_end_turn.
	callJSON(t, ctx, cs, "vm_checkpoint", map[string]any{"label": "golden", "keep": true}, &out)
	if out.Type != "keep" || out.Name != d.runID+"-keep-golden" || len(registeredTemps(d, "Win10")) != 1 {
		t.Errorf("keep %s, temps %v", jsonString(out), registeredTemps(d, "Win10"))
	}
	// No label: hhmmss.
	callJSON(t, ctx, cs, "vm_checkpoint", nil, &out)
	if !regexp.MustCompile(`^` + d.runID + `-temp-\d{6}$`).MatchString(out.Name) {
		t.Errorf("default label name %q", out.Name)
	}
	// Bad labels are refused before anything is created.
	n := len(b.created)
	for _, label := range []string{"a/b", strings.Repeat("x", 65)} {
		if e := callRefused(t, ctx, cs, "vm_checkpoint", map[string]any{"label": label}); e["error"] != codeInvalidArgument || !strings.Contains(e["next"].(string), "64") {
			t.Errorf("label %q: %v", label, e)
		}
	}
	if len(b.created) != n {
		t.Errorf("created %v", b.created)
	}
	// Checkpoints disabled in the VM's settings: refused before creating, with the setting to change.
	b.mu.Lock()
	b.current().CheckpointType = "Disabled"
	b.mu.Unlock()
	e := callRefused(t, ctx, cs, "vm_checkpoint", map[string]any{"label": "x"})
	if e["error"] != codeInvalidArgument || e["checkpoint_type"] != "Disabled" || !strings.Contains(e["next"].(string), "Hyper-V") || len(b.created) != n {
		t.Errorf("disabled: %v, created %v", e, b.created)
	}
	// Hyper-V refusing at creation time maps the same way.
	b.mu.Lock()
	b.current().CheckpointType = "Standard"
	b.createErr = errors.New("powershell: Checkpoint-VM: checkpoints are disabled for this virtual machine")
	b.mu.Unlock()
	if e := callRefused(t, ctx, cs, "vm_checkpoint", map[string]any{"label": "x"}); e["error"] != codeInvalidArgument || !strings.Contains(e["next"].(string), "Hyper-V") {
		t.Errorf("create error: %v", e)
	}
}

// selectorTree has a manual root, a temp and a keep child, and two manual roots sharing a name.
func selectorTree() hyperv.CheckpointList {
	return hyperv.CheckpointList{CheckpointType: "Standard", CurrentParentID: "id-2", Checkpoints: []hyperv.Checkpoint{
		{ID: "id-1", Name: "baseline", CreatedAt: "2026-10-04T10:00:00+08:00", Kind: "standard", State: "off"},
		{ID: "id-2", Name: "run-20261010-0812-7f3a-temp-step3", ParentID: "id-1", CreatedAt: "2026-10-10T08:15:00+08:00", Kind: "standard", State: "running"},
		{ID: "id-3", Name: "run-20261010-0812-7f3a-keep-golden", ParentID: "id-1", CreatedAt: "2026-10-10T08:20:00+08:00", Kind: "production", State: "off"},
		{ID: "id-4", Name: "dup", CreatedAt: "2026-10-10T08:30:00+08:00", Kind: "standard", State: "off"},
		{ID: "id-5", Name: "dup", CreatedAt: "2026-10-10T08:31:00+08:00", Kind: "standard", State: "off"},
	}}
}

func TestRestoreSelectorsAndSaveCurrent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tree := selectorTree()
	b := &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: "Running"}}, tree: &tree}
	cs, d := connectTools(t, ctx, b)
	// By unique name.
	var out restoreOut
	callJSON(t, ctx, cs, "vm_restore", map[string]any{"name": "run-20261010-0812-7f3a-keep-golden"}, &out)
	want := restoreOut{VM: "Win10", Restored: checkpointRef{ID: "id-3", Name: "run-20261010-0812-7f3a-keep-golden", Type: "keep"}, State: "running", Next: "call vm_start to make sure the desktop is usable"}
	if !reflect.DeepEqual(out, want) || !reflect.DeepEqual(b.restored, []string{"Win10/id-3"}) {
		t.Errorf("restore by name %s, restored %v", jsonString(out), b.restored)
	}
	// By id with save_current: the current state becomes a registered temp checkpoint first.
	callJSON(t, ctx, cs, "vm_restore", map[string]any{"id": "id-1", "save_current": true}, &out)
	savedName := d.runID + "-temp-before-restore"
	if out.Restored.ID != "id-1" || out.Restored.Type != "manual" || out.SavedCurrent == nil || *out.SavedCurrent != (checkpointRef{ID: "id-" + savedName, Name: savedName, Type: "temp"}) {
		t.Errorf("save_current %s", jsonString(out))
	}
	if !reflect.DeepEqual(b.created, []string{"Win10/" + savedName}) || !reflect.DeepEqual(b.restored, []string{"Win10/id-3", "Win10/id-1"}) {
		t.Errorf("created %v restored %v", b.created, b.restored)
	}
	if got := registeredTemps(d, "Win10"); !reflect.DeepEqual(got, []string{"id-" + savedName}) {
		t.Errorf("registered temps %v", got)
	}
	// The saved checkpoint branches from where the state was (id-3, after the first restore).
	var cps checkpointsOut
	callJSON(t, ctx, cs, "vm_checkpoints", nil, &cps)
	if last := cps.Checkpoints[len(cps.Checkpoints)-1]; last.ID != "id-"+savedName || *last.Parent != "id-3" || *cps.CurrentParent != "id-1" {
		t.Errorf("after save_current %s", jsonString(cps))
	}
	// Refusals: ambiguous and unknown names, unknown id, no selector.
	e := callRefused(t, ctx, cs, "vm_restore", map[string]any{"name": "dup"})
	if e["error"] != codeAmbiguousTarget || !reflect.DeepEqual(e["ids"], []any{"id-4", "id-5"}) || !strings.Contains(e["next"].(string), "id") {
		t.Errorf("ambiguous: %v", e)
	}
	if e := callRefused(t, ctx, cs, "vm_restore", map[string]any{"name": "nope"}); e["error"] != codeNoCheckpoint || e["name"] != "nope" {
		t.Errorf("unknown name: %v", e)
	}
	if e := callRefused(t, ctx, cs, "vm_restore", map[string]any{"id": "nope"}); e["error"] != codeNoCheckpoint || e["id"] != "nope" {
		t.Errorf("unknown id: %v", e)
	}
	if e := callRefused(t, ctx, cs, "vm_restore", nil); e["error"] != codeInvalidArgument {
		t.Errorf("no selector: %v", e)
	}
	// A failing save_current does not restore.
	b.mu.Lock()
	b.createErr = errors.New("Hyper-V job failed")
	b.mu.Unlock()
	if e := callRefused(t, ctx, cs, "vm_restore", map[string]any{"id": "id-1", "save_current": true}); e["error"] != codeFailed || len(b.restored) != 2 {
		t.Errorf("failed save_current: %v, restored %v", e, b.restored)
	}
}

// deleteTree: manual root, keep child, two temp grandchildren (the first is the current parent), two manual "dup"
// roots and a temp whose deletion fails.
func deleteTree() hyperv.CheckpointList {
	return hyperv.CheckpointList{CheckpointType: "Standard", CurrentParentID: "id-3", Checkpoints: []hyperv.Checkpoint{
		{ID: "id-1", Name: "baseline", CreatedAt: "2026-10-04T10:00:00+08:00", Kind: "standard", State: "off"},
		{ID: "id-2", Name: "run-20261010-0812-7f3a-keep-golden", ParentID: "id-1", CreatedAt: "2026-10-10T08:10:00+08:00", Kind: "standard", State: "running"},
		{ID: "id-3", Name: "run-20261010-0812-7f3a-temp-step3", ParentID: "id-2", CreatedAt: "2026-10-10T08:15:00+08:00", Kind: "standard", State: "running"},
		{ID: "id-4", Name: "run-20261010-0812-7f3a-temp-step4", ParentID: "id-2", CreatedAt: "2026-10-10T08:20:00+08:00", Kind: "standard", State: "running"},
		{ID: "id-5", Name: "dup", CreatedAt: "2026-10-10T08:30:00+08:00", Kind: "standard", State: "off"},
		{ID: "id-6", Name: "dup", CreatedAt: "2026-10-10T08:31:00+08:00", Kind: "standard", State: "off"},
		{ID: "id-fails", Name: "run-20261009-2200-aaaa-temp-stuck", ParentID: "id-1", CreatedAt: "2026-10-09T22:05:00+08:00", Kind: "standard", State: "running"},
	}}
}

func TestCheckpointDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tree := deleteTree()
	b := &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: "Running"}}, tree: &tree}
	cs, d := connectTools(t, ctx, b)
	d.turn.addTempCheckpoint("Win10", tempCheckpoint{ID: "id-3", Name: "run-20261010-0812-7f3a-temp-step3"})
	d.turn.addTempCheckpoint("Win10", tempCheckpoint{ID: "id-4", Name: "run-20261010-0812-7f3a-temp-step4"})
	// A manual checkpoint is deleted by id only; the refusal names the id.
	e := callRefused(t, ctx, cs, "vm_checkpoint_delete", map[string]any{"name": "baseline"})
	if e["error"] != codeInvalidArgument || e["id"] != "id-1" || !strings.Contains(e["next"].(string), "id-1") {
		t.Errorf("manual by name: %v", e)
	}
	if e := callRefused(t, ctx, cs, "vm_checkpoint_delete", map[string]any{"name": "dup"}); e["error"] != codeAmbiguousTarget || !reflect.DeepEqual(e["ids"], []any{"id-5", "id-6"}) {
		t.Errorf("ambiguous: %v", e)
	}
	if e := callRefused(t, ctx, cs, "vm_checkpoint_delete", map[string]any{"id": "nope"}); e["error"] != codeNoCheckpoint {
		t.Errorf("unknown id: %v", e)
	}
	if len(b.deleted) != 0 {
		t.Fatalf("deleted %v", b.deleted)
	}
	// A temp that is not the current parent, by name.
	var out checkpointDeleteOut
	callJSON(t, ctx, cs, "vm_checkpoint_delete", map[string]any{"name": "run-20261010-0812-7f3a-temp-step4"}, &out)
	if !reflect.DeepEqual(out.Deleted, []checkpointRef{{ID: "id-4", Name: "run-20261010-0812-7f3a-temp-step4", Type: "temp"}}) || out.MergedIntoCurrent || out.ElapsedMs < 0 {
		t.Errorf("delete temp %s", jsonString(out))
	}
	if got := registeredTemps(d, "Win10"); !reflect.DeepEqual(got, []string{"id-3"}) {
		t.Errorf("registered temps %v", got)
	}
	// The current parent: merged into the current state; the current parent moves up.
	callJSON(t, ctx, cs, "vm_checkpoint_delete", map[string]any{"id": "id-3"}, &out)
	if len(out.Deleted) != 1 || out.Deleted[0].ID != "id-3" || !out.MergedIntoCurrent {
		t.Errorf("delete current parent %s", jsonString(out))
	}
	var cps checkpointsOut
	callJSON(t, ctx, cs, "vm_checkpoints", nil, &cps)
	if *cps.CurrentParent != "id-2" || len(cps.Checkpoints) != 5 || len(registeredTemps(d, "Win10")) != 0 {
		t.Errorf("after deletes %s, temps %v", jsonString(cps), registeredTemps(d, "Win10"))
	}
	// A manual checkpoint by id with subtree: the whole branch is listed, computed from the tree before deleting, and
	// the current parent (id-2) is among the deleted, so the current state absorbs the merge.
	callJSON(t, ctx, cs, "vm_checkpoint_delete", map[string]any{"id": "id-1", "subtree": true}, &out)
	wantDeleted := []checkpointRef{
		{ID: "id-1", Name: "baseline", Type: "manual"},
		{ID: "id-2", Name: "run-20261010-0812-7f3a-keep-golden", Type: "keep"},
		{ID: "id-fails", Name: "run-20261009-2200-aaaa-temp-stuck", Type: "temp"},
	}
	if !reflect.DeepEqual(out.Deleted, wantDeleted) || !out.MergedIntoCurrent {
		t.Errorf("subtree %s", jsonString(out))
	}
	if !reflect.DeepEqual(b.deleted, []string{"Win10/id-4", "Win10/id-3", "Win10/id-1"}) {
		t.Errorf("backend deletes %v", b.deleted)
	}
	callJSON(t, ctx, cs, "vm_checkpoints", nil, &cps)
	if len(cps.Checkpoints) != 2 || cps.CurrentParent != nil {
		t.Errorf("after subtree %s", jsonString(cps))
	}
	// A Hyper-V failure says how to check on the merge.
	b.mu.Lock()
	b.tree.Checkpoints = append(b.tree.Checkpoints, hyperv.Checkpoint{ID: "id-fails", Name: "run-20261009-2200-aaaa-temp-stuck"})
	b.mu.Unlock()
	if e := callRefused(t, ctx, cs, "vm_checkpoint_delete", map[string]any{"id": "id-fails"}); e["error"] != codeFailed || !strings.Contains(e["next"].(string), "vm_checkpoints") || e["elapsed_ms"] == nil {
		t.Errorf("failed delete: %v", e)
	}
}

func TestCheckpointKeep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: "Running"}}}
	cs, d := connectTools(t, ctx, b)
	d.turn.addTempCheckpoint("Win10", tempCheckpoint{ID: "id-2", Name: "run-20261010-0812-7f3a-temp-step3"})
	var out checkpointRef
	callJSON(t, ctx, cs, "vm_checkpoint_keep", map[string]any{"name": "run-20261010-0812-7f3a-temp-step3"}, &out)
	if out != (checkpointRef{ID: "id-2", Name: "run-20261010-0812-7f3a-keep-step3", Type: "keep"}) {
		t.Errorf("keep %s", jsonString(out))
	}
	if !reflect.DeepEqual(b.renamed, []string{"Win10/id-2=run-20261010-0812-7f3a-keep-step3"}) || len(registeredTemps(d, "Win10")) != 0 {
		t.Errorf("renamed %v, temps %v", b.renamed, registeredTemps(d, "Win10"))
	}
	var cps checkpointsOut
	callJSON(t, ctx, cs, "vm_checkpoints", nil, &cps)
	if cps.Checkpoints[1].Type != "keep" || *cps.Checkpoints[1].Label != "step3" {
		t.Errorf("after keep %s", jsonString(cps))
	}
	// Already keep, manual, bad label, unknown.
	if e := callRefused(t, ctx, cs, "vm_checkpoint_keep", map[string]any{"id": "id-2"}); e["error"] != codeInvalidArgument || !strings.Contains(e["reason"].(string), "already") {
		t.Errorf("keep again: %v", e)
	}
	if e := callRefused(t, ctx, cs, "vm_checkpoint_keep", map[string]any{"id": "id-1"}); e["error"] != codeInvalidArgument || !strings.Contains(e["reason"].(string), "manual") {
		t.Errorf("manual: %v", e)
	}
	if e := callRefused(t, ctx, cs, "vm_checkpoint_keep", map[string]any{"id": "id-1", "label": "a:b"}); e["error"] != codeInvalidArgument || !strings.Contains(e["reason"].(string), "label") {
		t.Errorf("bad label: %v", e)
	}
	if e := callRefused(t, ctx, cs, "vm_checkpoint_keep", map[string]any{"id": "nope"}); e["error"] != codeNoCheckpoint {
		t.Errorf("unknown: %v", e)
	}
	// A temp of this run kept under a new label is no longer deleted by vm_end_turn.
	var created checkpointCreatedOut
	callJSON(t, ctx, cs, "vm_checkpoint", map[string]any{"label": "step5"}, &created)
	callJSON(t, ctx, cs, "vm_checkpoint_keep", map[string]any{"id": created.ID, "label": "golden"}, &out)
	if out.Name != d.runID+"-keep-golden" || out.ID != created.ID {
		t.Errorf("keep with label %s", jsonString(out))
	}
	var turn endTurnOut
	callJSON(t, ctx, cs, "vm_end_turn", nil, &turn)
	if len(turn.DeletedCheckpoints) != 0 || len(b.deleted) != 0 {
		t.Errorf("end_turn after keep %s, deleted %v", jsonString(turn), b.deleted)
	}
}

func TestEndTurnAllTemp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tree := hyperv.CheckpointList{CheckpointType: "Standard", CurrentParentID: "id-2", Checkpoints: []hyperv.Checkpoint{
		{ID: "id-1", Name: "baseline"},
		{ID: "id-2", Name: "run-20261010-0812-7f3a-temp-step3", ParentID: "id-1"},
		{ID: "id-3", Name: "run-20261009-2200-aaaa-temp-x", ParentID: "id-1"},
		{ID: "id-4", Name: "run-20261009-2200-aaaa-keep-y", ParentID: "id-1"},
		{ID: "id-fails", Name: "run-20261009-2200-aaaa-temp-stuck", ParentID: "id-1"},
	}}
	b := &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: "Running"}, {Name: "Win11", ID: "id-b", State: "Off"}}, tree: &tree}
	cs, d := connectTools(t, ctx, b)
	d.turn.addTempCheckpoint("Win10", tempCheckpoint{ID: "id-2", Name: "run-20261010-0812-7f3a-temp-step3"})
	// The default deletes only this run's registered temps.
	var out endTurnOut
	callJSON(t, ctx, cs, "vm_end_turn", nil, &out)
	if !reflect.DeepEqual(out, endTurnOut{DeletedCheckpoints: []string{"run-20261010-0812-7f3a-temp-step3"}, Skipped: []skippedCheckpoint{}, Errors: []string{}}) {
		t.Errorf("default end_turn %s", jsonString(out))
	}
	// all_temp crosses runs: every temp on the VM goes, keep and manual stay, failures are skipped with the reason.
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"vm": "Win10", "all_temp": true}, &out)
	want := endTurnOut{DeletedCheckpoints: []string{"run-20261009-2200-aaaa-temp-x"}, Skipped: []skippedCheckpoint{{ID: "id-fails", Name: "run-20261009-2200-aaaa-temp-stuck", Reason: "Hyper-V job failed"}}, Errors: []string{}}
	if !reflect.DeepEqual(out, want) || !reflect.DeepEqual(b.deleted, []string{"Win10/id-2", "Win10/id-3"}) {
		t.Errorf("all_temp %s, deleted %v", jsonString(out), b.deleted)
	}
	var cps checkpointsOut
	callJSON(t, ctx, cs, "vm_checkpoints", nil, &cps)
	if len(cps.Checkpoints) != 3 || cps.Checkpoints[0].Type != "manual" || cps.Checkpoints[1].Type != "keep" {
		t.Errorf("after all_temp %s", jsonString(cps))
	}
	// Without vm, all_temp is refused: it would sweep every VM. Nothing is deleted.
	b.mu.Lock()
	b.tree.Checkpoints = append(b.tree.Checkpoints, hyperv.Checkpoint{ID: "id-5", Name: "run-20261009-2200-aaaa-temp-z", ParentID: "id-1"})
	deletedBefore := len(b.deleted)
	b.mu.Unlock()
	if refused := callRefused(t, ctx, cs, "vm_end_turn", map[string]any{"all_temp": true}); refused["error"] != codeInvalidArgument {
		t.Errorf("all_temp without vm: %v", refused)
	}
	b.mu.Lock()
	if len(b.deleted) != deletedBefore {
		t.Errorf("all_temp without vm deleted %v", b.deleted[deletedBefore:])
	}
	b.mu.Unlock()
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"vm": "Win10", "all_temp": true}, &out)
	if !reflect.DeepEqual(out.DeletedCheckpoints, []string{"run-20261009-2200-aaaa-temp-z"}) || len(out.Skipped) != 1 || len(out.Errors) != 0 {
		t.Errorf("all_temp on Win10 %s", jsonString(out))
	}
	if !reflect.DeepEqual(b.deleted, []string{"Win10/id-2", "Win10/id-3", "Win10/id-5"}) {
		t.Errorf("deleted %v", b.deleted)
	}
	// An unknown VM is reported in errors.
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"vm": "nope", "all_temp": true}, &out)
	if len(out.Errors) != 1 || !strings.Contains(out.Errors[0], "nope") {
		t.Errorf("unknown VM %s", jsonString(out))
	}
}

func TestEndTurnDefaultPathRechecksRegisteredTemps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tree := hyperv.CheckpointList{CheckpointType: "Standard", CurrentParentID: "id-1", Checkpoints: []hyperv.Checkpoint{
		{ID: "id-1", Name: "baseline"},
		{ID: "id-2", Name: "run-20261010-0812-7f3a-temp-step1", ParentID: "id-1"},
		{ID: "id-3", Name: "run-20261010-0812-7f3a-keep-step2", ParentID: "id-1"}, // registered as temp, renamed to keep by hand
		{ID: "id-fails", Name: "run-20261010-0812-7f3a-temp-stuck", ParentID: "id-1"},
	}}
	b := &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: "Running"}}, tree: &tree}
	cs, d := connectTools(t, ctx, b)
	for _, c := range []tempCheckpoint{
		{ID: "id-2", Name: "run-20261010-0812-7f3a-temp-step1"},
		{ID: "id-3", Name: "run-20261010-0812-7f3a-temp-step2"},
		{ID: "id-gone", Name: "run-20261010-0812-7f3a-temp-deleted-by-hand"},
		{ID: "id-fails", Name: "run-20261010-0812-7f3a-temp-stuck"},
	} {
		d.turn.addTempCheckpoint("Win10", c)
	}
	// The VM name is matched case-insensitively, as Hyper-V does.
	var out endTurnOut
	callJSON(t, ctx, cs, "vm_end_turn", map[string]any{"vm": "win10"}, &out)
	want := endTurnOut{
		DeletedCheckpoints: []string{"run-20261010-0812-7f3a-temp-step1"},
		Skipped:            []skippedCheckpoint{{ID: "id-3", Name: "run-20261010-0812-7f3a-keep-step2", Reason: "no longer a temp checkpoint (now keep); not deleted"}},
		Errors:             []string{"Win10/run-20261010-0812-7f3a-temp-stuck: Hyper-V job failed"},
	}
	if !reflect.DeepEqual(out, want) || !reflect.DeepEqual(b.deleted, []string{"Win10/id-2"}) {
		t.Errorf("end_turn %s, deleted %v", jsonString(out), b.deleted)
	}
	// Only the failed one stays registered: the renamed and the vanished ones are forgotten.
	if got := registeredTemps(d, "Win10"); !reflect.DeepEqual(got, []string{"id-fails"}) {
		t.Errorf("registered after end_turn: %v", got)
	}
	// Once it can be deleted it goes and nothing stays registered.
	b.mu.Lock()
	for i := range b.tree.Checkpoints {
		if b.tree.Checkpoints[i].ID == "id-fails" {
			b.tree.Checkpoints[i].ID = "id-4"
		}
	}
	b.mu.Unlock()
	d.turn.addTempCheckpoint("Win10", tempCheckpoint{ID: "id-4", Name: "run-20261010-0812-7f3a-temp-stuck"})
	callJSON(t, ctx, cs, "vm_end_turn", nil, &out)
	if !reflect.DeepEqual(out.DeletedCheckpoints, []string{"run-20261010-0812-7f3a-temp-stuck"}) || len(out.Errors) != 0 || len(registeredTemps(d, "Win10")) != 0 {
		t.Errorf("second end_turn %s, registered %v", jsonString(out), registeredTemps(d, "Win10"))
	}
	// A listing failure deletes nothing and keeps everything registered.
	d.turn.addTempCheckpoint("Win10", tempCheckpoint{ID: "id-2", Name: "x"})
	b.mu.Lock()
	b.listCheckpointsErr = errors.New("PowerShell failed")
	b.mu.Unlock()
	callJSON(t, ctx, cs, "vm_end_turn", nil, &out)
	if len(out.DeletedCheckpoints) != 0 || len(out.Errors) != 1 || !reflect.DeepEqual(registeredTemps(d, "Win10"), []string{"id-2"}) {
		t.Errorf("list failure %s, registered %v", jsonString(out), registeredTemps(d, "Win10"))
	}
}

func TestDeleteAfterListNoCheckpoint(t *testing.T) {
	// The checkpoint disappears between the list and the delete: the backend's "checkpoint not found" (as text after
	// the broker pipe) is no_checkpoint.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := &vmToolsBackend{vms: []hyperv.VM{{Name: "Win10", ID: "id-a", State: "Running"}}}
	b.deleteErr = errors.New("checkpoint not found: id-2")
	cs, _ := connectTools(t, ctx, b)
	e := callRefused(t, ctx, cs, "vm_checkpoint_delete", map[string]any{"id": "id-2"})
	if e["error"] != codeNoCheckpoint || e["id"] != "id-2" {
		t.Errorf("vanished: %v", e)
	}
	// Likewise for restore, where the state saved by save_current is reported in the refusal.
	e = callRefused(t, ctx, cs, "vm_restore", map[string]any{"id": "id-2", "save_current": true})
	if e["error"] != codeNoCheckpoint || e["saved_current"] == nil {
		t.Errorf("restore after save_current: %v", e)
	}
}

func TestSubtreeOfIgnoresOrder(t *testing.T) {
	l := hyperv.CheckpointList{Checkpoints: []hyperv.Checkpoint{
		{ID: "c", ParentID: "b"}, // listed before its parent (clock set back)
		{ID: "a"},
		{ID: "b", ParentID: "a"},
		{ID: "x", ParentID: "a"},
	}}
	got := subtreeOf(l, "b")
	if len(got) != 2 || got[0].ID != "c" || got[1].ID != "b" {
		t.Errorf("subtree of b: %+v", got)
	}
}
