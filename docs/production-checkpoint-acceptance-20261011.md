# Production-only checkpoints and vm_set_checkpoint_type: acceptance (2026-10-11)

## Build

- Source `8a2d575` plus `vm_set_checkpoint_type` (committed with this record), installed with `go run ./cmd/hyperhand dev-install` (no UAC) as `dev-20261011-084949-8a2d575-dirty`; Win10 was updated with `vm_update_agent`.
- `go vet ./internal/...` and `go test -race` of `host`, `broker` and `hyperv` pass.

## Win10 run

Evidence: ignored `build/prodcp-20261011/` (`accept_prod.py`, `results.json`). One task; marker files under `C:\Users\Public` showed what the disk held.

| Step | Result |
|---|---|
| `vm_set_checkpoint_type ProductionOnly` | `previous: Standard`, `checkpoint_type: ProductionOnly` (7.1 s); `vm_checkpoints` then reported `ProductionOnly` |
| `vm_checkpoint label=prod1` on the running VM | created in 8.5 s; `state: off` (no memory saved, as a production checkpoint); **`kind: standard`** (see below) |
| `vm_restore start: false` | `state: off`; `vm_status` `power: off` |
| `vm_start` | 28.2 s, `previous_state: off`, `desktop: usable`, agent answering: reconnection after the cold start |
| disk | the marker written before the checkpoint existed, the one written after it did not |
| `vm_restore` (default start) | `state: running`; `vm_start` then 25.6 s to a usable desktop; disk restored again |
| `vm_end_turn` | deleted `…-temp-prod1`; `vm_checkpoints` no longer listed it |
| `vm_set_checkpoint_type Standard` | `previous: ProductionOnly`; the VM is back on `Standard` |

With `ProductionOnly` a production checkpoint cannot fall back to a standard one, and its saved state `off` on a running VM confirms it held no memory.

## Finding: `kind` does not identify production checkpoints

HyperHand derives `kind` from Hyper-V's `SnapshotType` and maps `Recovery` to `production`. A production checkpoint made by `Checkpoint-VM` has `SnapshotType` `Standard`, so it is reported as `kind: standard`. `Recovery` is the type of checkpoints made by backup applications. The documented `kind: production` therefore never appears for `vm_checkpoint` checkpoints. What a caller can rely on is `state`: `running` means the checkpoint holds memory and resumes running, `off` means disk only and restores to off. Resolved after checking Microsoft's documentation (`Msvm_VirtualSystemSettingData`: no snapshot type for production checkpoints; `Recovery` is a recovery VM's snapshot, made by backup software): `kind` now reports the snapshot type as is, and a new `holds_memory` field states whether the checkpoint saved memory.

## holds_memory on Win10

After the fix (`dev-20261011-090727-d16ad62-dirty`; evidence `build/prodcp-20261011/holds.json`), one checkpoint of the running VM under each setting, as created and as listed:

- `ProductionOnly`: `kind: standard`, `state: off`, `holds_memory: false`.
- `Standard`: `kind: standard`, `state: saved`, `holds_memory: true`. A standard checkpoint of a running VM reports `saved`, not `running`, on this host.

Both were deleted by `vm_end_turn` and the setting is `Standard` again.
