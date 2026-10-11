package hyperv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	ole "github.com/go-ole/go-ole"
)

// Checkpoint is one VM checkpoint (Hyper-V snapshot). ID is the snapshot GUID (the second part of the
// Msvm_VirtualSystemSettingData InstanceID "Microsoft:<vm id>\<snapshot id>", also Get-VMSnapshot's Id); ParentID is
// the parent checkpoint's ID, "" for a root. CreatedAt is RFC 3339 with the host's offset. Kind is "standard" (the
// checkpoint may hold memory), "production" (application-consistent, Hyper-V's Recovery type; restores to Off), or
// another Hyper-V SnapshotType lower-cased ("planned", "missing", "replica", ...), which cannot be restored normally.
// State is the power state the checkpoint saved: "running", "off" or "saved".
type Checkpoint struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ParentID  string `json:"parent_id"`
	CreatedAt string `json:"created_at"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
}

// CheckpointList is a VM's checkpoint tree. CheckpointType is the VM's setting that decides what CreateCheckpoint
// makes: "Disabled", "Production", "ProductionOnly" or "Standard". CurrentParentID is the checkpoint the VM's current
// state branches from ("" when none). Checkpoints are in creation order, so parents come before children.
type CheckpointList struct {
	CheckpointType  string       `json:"checkpoint_type"`
	CurrentParentID string       `json:"current_parent_id"`
	Checkpoints     []Checkpoint `json:"checkpoints"`
}

// ErrCheckpointNotFound is returned (wrapped) when no checkpoint has the given ID.
var ErrCheckpointNotFound = errors.New("checkpoint not found")

// snapshotFields is the Select-Object list that turns a Microsoft.HyperV.PowerShell.VMSnapshot (Get-VMSnapshot,
// Checkpoint-VM -Passthru) into Checkpoint's JSON fields. Id and ParentSnapshotId are GUIDs (ParentSnapshotId null for
// a root, which [string] turns into ""); SnapshotType is Standard for a user checkpoint and Recovery/Planned/Missing/
// Replica... otherwise; State is the saved power state (Running, Off, Saved...).
// https://learn.microsoft.com/en-us/powershell/module/hyper-v/get-vmsnapshot
const snapshotFields = `@{n='id';e={[string]$_.Id}},@{n='name';e={$_.Name}},@{n='parent_id';e={[string]$_.ParentSnapshotId}},@{n='created_at';e={$_.CreationTime.ToString('yyyy-MM-ddTHH:mm:sszzz')}},@{n='kind';e={switch ([string]$_.SnapshotType) { 'Standard' {'standard'} 'Recovery' {'production'} default {$_.ToLower()} }}},@{n='state';e={([string]$_.State).ToLower()}}`

// checkpointScript is the PowerShell that lists a VM's checkpoint tree as one JSON object. vmScript sets $vm to the
// Microsoft.HyperV.PowerShell.VirtualMachine, whose CheckpointType is the Set-VM -CheckpointType setting (Disabled,
// Production, ProductionOnly, Standard) and whose ParentCheckpointId is the checkpoint the current state branches from
// (null when none). https://learn.microsoft.com/en-us/powershell/module/hyper-v/set-vm
const checkpointScript = `$c=@(Get-VMSnapshot -VM $vm | Sort-Object CreationTime | Select-Object ` + snapshotFields + `)
ConvertTo-Json -Compress -Depth 3 -InputObject @{checkpoint_type=[string]$vm.CheckpointType; current_parent_id=[string]$vm.ParentCheckpointId; checkpoints=$c}`

// ListCheckpoints returns the VM's checkpoint tree.
func ListCheckpoints(vm string) (CheckpointList, error) {
	out, err := vmScript(vm, checkpointScript)
	if err != nil {
		return CheckpointList{}, err
	}
	return parseCheckpoints(out)
}

// parseCheckpoints reads checkpointScript's JSON. ConvertTo-Json may write a one-element array as a bare object and an
// empty array as null or [], which all decode to the right slice.
func parseCheckpoints(out []byte) (CheckpointList, error) {
	out = bytes.TrimSpace(bytes.TrimPrefix(out, []byte("\xef\xbb\xbf")))
	var raw struct {
		CheckpointType  string          `json:"checkpoint_type"`
		CurrentParentID string          `json:"current_parent_id"`
		Checkpoints     json.RawMessage `json:"checkpoints"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return CheckpointList{}, fmt.Errorf("checkpoint list: %w", err)
	}
	l := CheckpointList{CheckpointType: raw.CheckpointType, CurrentParentID: raw.CurrentParentID, Checkpoints: []Checkpoint{}}
	c := bytes.TrimSpace(raw.Checkpoints)
	switch {
	case len(c) == 0 || bytes.Equal(c, []byte("null")):
	case c[0] == '{':
		l.Checkpoints = make([]Checkpoint, 1)
		if err := json.Unmarshal(c, &l.Checkpoints[0]); err != nil {
			return CheckpointList{}, fmt.Errorf("checkpoint list: %w", err)
		}
	default:
		if err := json.Unmarshal(c, &l.Checkpoints); err != nil {
			return CheckpointList{}, fmt.Errorf("checkpoint list: %w", err)
		}
		if l.Checkpoints == nil {
			l.Checkpoints = []Checkpoint{}
		}
	}
	return l, nil
}

// CheckpointTypes are the values of Set-VM -CheckpointType, which decides what CreateCheckpoint creates.
var CheckpointTypes = []string{"Standard", "Production", "ProductionOnly", "Disabled"}

// SetCheckpointType sets the VM's checkpoint setting to t, one of CheckpointTypes (Set-VM -CheckpointType; allowed
// while the VM runs).
// https://learn.microsoft.com/en-us/powershell/module/hyper-v/set-vm
func SetCheckpointType(vm, t string) error {
	if !slices.Contains(CheckpointTypes, t) {
		return fmt.Errorf("unknown checkpoint type %q", t)
	}
	_, err := vmScript(vm, `Set-VM -VM $vm -CheckpointType `+psq(t))
	return err
}

// CreateCheckpoint creates a checkpoint named name (the VM's CheckpointType decides its kind) and returns it; its
// ParentID is the checkpoint the VM's state branched from. A VM whose CheckpointType is Disabled is rejected with an
// error containing "checkpoints are disabled". Checkpoint-VM -Passthru returns the new VMSnapshot.
// https://learn.microsoft.com/en-us/powershell/module/hyper-v/checkpoint-vm
func CreateCheckpoint(vm, name string) (Checkpoint, error) {
	out, err := vmScript(vm, `if ([string]$vm.CheckpointType -eq 'Disabled') { throw ('checkpoints are disabled for VM ' + $vm.Name + ' (CheckpointType is Disabled; enable them in the VM settings)') }
Checkpoint-VM -VM $vm -SnapshotName `+psq(name)+` -Passthru | Select-Object `+snapshotFields+` | ConvertTo-Json -Compress`)
	if err != nil {
		return Checkpoint{}, err
	}
	var c Checkpoint
	if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(out, []byte("\xef\xbb\xbf"))), &c); err != nil {
		return Checkpoint{}, fmt.Errorf("created checkpoint: %w", err)
	}
	return c, nil
}

// RestoreCheckpoint applies the checkpoint with ID id. Hyper-V determines the restored power state; a running standard
// checkpoint resumes directly. A missing id fails with an error containing "checkpoint not found".
// https://learn.microsoft.com/en-us/powershell/module/hyper-v/restore-vmsnapshot
func RestoreCheckpoint(vm, id string) error {
	_, err := vmScript(vm, `$c=Get-VMSnapshot -VM $vm | Where-Object { [string]$_.Id -eq `+psq(id)+` } | Select-Object -First 1
if (-not $c) { throw ('checkpoint not found: ' + `+psq(id)+`) }
Restore-VMSnapshot -VMSnapshot $c -Confirm:$false`)
	return err
}

// isSnapshotInstance reports whether a snapshot Msvm_VirtualSystemSettingData.InstanceID ("Microsoft:<vm id>\<snapshot
// id>", upper-case GUIDs) names the checkpoint with ID id (any case).
func isSnapshotInstance(instanceID, id string) bool {
	return id != "" && strings.HasSuffix(strings.ToUpper(instanceID), "\\"+strings.ToUpper(id))
}

// snapshotSettings finds the Msvm_VirtualSystemSettingData of the checkpoint with ID id. A snapshot's settings are
// associated with the VM through several association classes, so the same object can appear more than once.
// https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/msvm-virtualsystemsettingdata
func snapshotSettings(s *session, o *ole.IDispatch, id string) (*ole.IDispatch, error) {
	settings, err := s.assoc(o, "Msvm_VirtualSystemSettingData")
	if err != nil {
		return nil, err
	}
	var seen []string
	for _, sd := range settings {
		if !strings.HasPrefix(fmt.Sprint(s.get(sd, "VirtualSystemType")), "Microsoft:Hyper-V:Snapshot:") {
			continue
		}
		instance, config := fmt.Sprint(s.get(sd, "InstanceID")), fmt.Sprint(s.get(sd, "ConfigurationID"))
		// Get-VMSnapshot's Id is the snapshot's ConfigurationID; the InstanceID ends with the same GUID.
		if isSnapshotInstance(instance, id) || strings.EqualFold(config, id) {
			return sd, nil
		}
		seen = append(seen, fmt.Sprintf("%s (%s, %q)", config, instance, fmt.Sprint(s.get(sd, "ElementName"))))
	}
	return nil, fmt.Errorf("%w: %s; snapshot settings of the VM: %s", ErrCheckpointNotFound, id, strings.Join(seen, "; "))
}

// DeleteCheckpoint removes the checkpoint with ID id through Msvm_VirtualSystemSnapshotService: DestroySnapshot
// (its children are re-parented and its disk differences merged) or, with subtree, DestroySnapshotTree (it and all
// its descendants). It waits for the merge job (up to 15 minutes).
// https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/destroysnapshot-msvm-virtualsystemsnapshotservice
// https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/destroysnapshottree-msvm-virtualsystemsnapshotservice
func DeleteCheckpoint(vm, id string, subtree bool) error {
	return withWMI(func(s *session) error {
		o, err := s.find(vm)
		if err != nil {
			return err
		}
		target, err := snapshotSettings(s, o, id)
		if err != nil {
			return err
		}
		svc, err := s.one("SELECT * FROM Msvm_VirtualSystemSnapshotService")
		if err != nil {
			return err
		}
		// The two methods name their snapshot reference differently: DestroySnapshot(AffectedSnapshot, Job) and
		// DestroySnapshotTree(SnapshotSettingData, Job).
		method, param := "DestroySnapshot", "AffectedSnapshot"
		if subtree {
			method, param = "DestroySnapshotTree", "SnapshotSettingData"
		}
		out, err := s.call(svc, method, param, s.path(target))
		if err != nil {
			return err
		}
		return s.awaitJob(out, method, 15*time.Minute)
	})
}

// RenameCheckpoint renames the checkpoint with ID id through Rename-VMSnapshot, the same PowerShell path as
// RestoreCheckpoint. A missing id fails with "checkpoint not found".
// https://learn.microsoft.com/en-us/powershell/module/hyper-v/rename-vmsnapshot
func RenameCheckpoint(vm, id, name string) error {
	if name == "" {
		return errors.New("checkpoint name required")
	}
	_, err := vmScript(vm, `$c=Get-VMSnapshot -VM $vm | Where-Object { [string]$_.Id -eq `+psq(id)+` } | Select-Object -First 1
if (-not $c) { throw ('checkpoint not found: ' + `+psq(id)+`) }
Rename-VMSnapshot -VMSnapshot $c -NewName `+psq(name)+` -Confirm:$false`)
	return err
}
