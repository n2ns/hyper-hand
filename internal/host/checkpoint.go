package host

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"hyperhand/internal/hyperv"
)

// Checkpoint types. HyperHand names the checkpoints it creates <run_id>-temp-<label> (a rollback point for one run;
// vm_end_turn deletes it) or <run_id>-keep-<label> (a baseline kept across runs); any other name is a manual
// checkpoint made outside HyperHand, which only vm_checkpoint_delete removes, and only by id.
const (
	checkpointTemp   = "temp"
	checkpointKeep   = "keep"
	checkpointManual = "manual"
)

var checkpointNameRe = regexp.MustCompile(`^(run-\d{8}-\d{4}-(?:[0-9a-f]{4}|[0-9a-f]{16}))-(temp|keep)-(.+)$`)

// checkpointName builds the name of a checkpoint created in run runID.
func checkpointName(runID, label string, keep bool) string {
	typ := checkpointTemp
	if keep {
		typ = checkpointKeep
	}
	return runID + "-" + typ + "-" + label
}

// parseCheckpointName accepts legacy 4-hex and current 16-hex run suffixes; other names are manual checkpoints.
func parseCheckpointName(name string) (runID, typ, label string) {
	m := checkpointNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", checkpointManual, ""
	}
	return m[1], m[2], m[3]
}

// labelForbidden are the characters a label may not contain: the ones Windows forbids in file names (Hyper-V uses
// checkpoint names in paths) and line breaks.
const labelForbidden = "\\/:*?\"<>|\r\n"

// checkLabel validates a checkpoint label: 1 to 64 characters, none of labelForbidden.
func checkLabel(label string) error {
	if n := utf8.RuneCountInString(label); n == 0 || n > 64 || strings.ContainsAny(label, labelForbidden) {
		return refuse(codeInvalidArgument, `pass a label of 1 to 64 characters without \ / : * ? " < > | or line breaks`, nil,
			"label %q must be 1 to 64 characters and contain none of \\ / : * ? \" < > | or line breaks", label)
	}
	return nil
}

// defaultLabel is the label of a checkpoint created without one: the current time as hhmmss.
func defaultLabel() string { return time.Now().Format("150405") }

// nullable returns nil for "" so that an absent value renders as JSON null.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// checkpointRef names a checkpoint in results: its id (the stable selector), name and type.
type checkpointRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func checkpointRefOf(c hyperv.Checkpoint) checkpointRef {
	_, typ, _ := parseCheckpointName(c.Name)
	return checkpointRef{ID: c.ID, Name: c.Name, Type: typ}
}

// checkpointOut is one checkpoint in vm_checkpoints. RunID, Type and Label come from the name (see
// parseCheckpointName); RunID and Label are null for a manual checkpoint. Parent is the parent's id, null for a root.
// Current marks the checkpoint the VM's current state branches from; Children counts direct children.
type checkpointOut struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Parent    *string `json:"parent"`
	CreatedAt string  `json:"created_at"`
	Type      string  `json:"type"`
	RunID     *string `json:"run_id"`
	Label     *string `json:"label"`
	Kind      string  `json:"kind"`
	State     string  `json:"state"`
	// HoldsMemory: the checkpoint saved memory (state running or saved), so restoring it brings the programs back;
	// false for a production checkpoint and for one taken while the VM was off, which restore to off.
	HoldsMemory bool `json:"holds_memory"`
	Current     bool `json:"current"`
	Children    int  `json:"children"`
}

// holdsMemory reports whether a checkpoint saved in state holds memory: Hyper-V keeps a saved-state file for a
// checkpoint of a running or saved VM (Msvm_VirtualSystemSettingData.IsSaved), never for a production checkpoint.
func holdsMemory(state string) bool { return state == "running" || state == "saved" }

// checkpointTree converts l to vm_checkpoints' entries, in l's (creation) order.
func checkpointTree(l hyperv.CheckpointList) []checkpointOut {
	children := map[string]int{}
	for _, c := range l.Checkpoints {
		if c.ParentID != "" {
			children[c.ParentID]++
		}
	}
	out := make([]checkpointOut, 0, len(l.Checkpoints))
	for _, c := range l.Checkpoints {
		runID, typ, label := parseCheckpointName(c.Name)
		out = append(out, checkpointOut{
			ID: c.ID, Name: c.Name, Parent: nullable(c.ParentID), CreatedAt: c.CreatedAt,
			Type: typ, RunID: nullable(runID), Label: nullable(label), Kind: c.Kind, State: c.State,
			HoldsMemory: holdsMemory(c.State), Current: c.ID != "" && c.ID == l.CurrentParentID, Children: children[c.ID],
		})
	}
	return out
}

// subtreeOf returns the checkpoint with ID id and all its descendants in l, in l's order. Membership is computed to
// a fixed point, so it does not depend on parents being listed before children (a clock set back can order them
// otherwise).
func subtreeOf(l hyperv.CheckpointList, id string) []hyperv.Checkpoint {
	in := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for _, c := range l.Checkpoints {
			if !in[c.ID] && in[c.ParentID] {
				in[c.ID] = true
				changed = true
			}
		}
	}
	var out []hyperv.Checkpoint
	for _, c := range l.Checkpoints {
		if in[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// selectCheckpoint finds the checkpoint in l that id or name selects: id wins; name is accepted when exactly one
// checkpoint has it (several: ambiguous_target with their ids; none: no_checkpoint). byName reports that the name
// was used, so that callers can insist on an id for manual checkpoints.
func selectCheckpoint(l hyperv.CheckpointList, id, name string) (c hyperv.Checkpoint, byName bool, err error) {
	if id == "" && name == "" {
		return c, false, refuse(codeInvalidArgument, "call vm_checkpoints and pass an id", nil, "id (or name) is required")
	}
	if id != "" {
		for _, c := range l.Checkpoints {
			if strings.EqualFold(c.ID, id) {
				return c, false, nil
			}
		}
		return c, false, refuse(codeNoCheckpoint, "call vm_checkpoints and pass a listed id", map[string]any{"id": id}, "no checkpoint has id %q", id)
	}
	var found []hyperv.Checkpoint
	for _, c := range l.Checkpoints {
		if c.Name == name {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return c, true, refuse(codeNoCheckpoint, "call vm_checkpoints and pass a listed id", map[string]any{"name": name}, "no checkpoint is named %q", name)
	case 1:
		return found[0], true, nil
	}
	ids := make([]string, len(found))
	for i, c := range found {
		ids[i] = c.ID
	}
	return c, true, refuse(codeAmbiguousTarget, "pass id instead of name (vm_checkpoints shows each one's parent and created_at)", map[string]any{"ids": ids}, "%d checkpoints are named %q", len(ids), name)
}

// isNoCheckpoint reports whether err says the checkpoint does not exist: hyperv.ErrCheckpointNotFound, or its text
// once the error has crossed the broker pipe.
func isNoCheckpoint(err error) bool {
	return errors.Is(err, hyperv.ErrCheckpointNotFound) || strings.Contains(err.Error(), hyperv.ErrCheckpointNotFound.Error())
}

// checkpointErr maps a backend error about the checkpoint with ID id: a missing checkpoint (deleted since it was
// listed) is no_checkpoint, anything else passes through.
func checkpointErr(err error, id string) error {
	if isNoCheckpoint(err) {
		return refuse(codeNoCheckpoint, "call vm_checkpoints and pass a listed id", map[string]any{"id": id}, "%v", err)
	}
	return err
}

// checkpointsDisabled reports whether err is Hyper-V refusing because the VM's checkpoint setting is Disabled.
func checkpointsDisabled(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "checkpoints are disabled")
}

// disabledNext is the next step when the VM's checkpoint setting is Disabled.
const disabledNext = "enable checkpoints for the VM in Hyper-V Manager (Settings > Checkpoints) or with Set-VM -CheckpointType Standard, then retry"
