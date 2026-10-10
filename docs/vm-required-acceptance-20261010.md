# Required `vm` acceptance (2026-10-10)

## Change

Every tool except `vm_list` now requires an explicit `vm`; the default ("the only running VM, or the only VM if none is running") is gone. `vm_end_turn` may still omit `vm` to end the whole task (only the task's own waits, temporary checkpoints and ownership), but `all_temp: true` requires `vm`, and a whitespace-only `vm` is refused. Contract: [features.md 2.3](features/results-errors.md#23-vm-selection); migration: [CHANGELOG](../CHANGELOG.md#unreleased).

Reason: the host now has two VMs, `Win10` (HyperHand development) and `Win10-Example` (Example testing). With a default, a call meant for a VM that was off reached the other one, including `vm_restore`, `vm_shutdown`, `vm_turn_off` and `vm_exec`; `vm_end_turn` with `all_temp` and no `vm` deleted temporary checkpoints on every VM.

Commits: `3cc8a2f` (core), `06cee63` (docs), `569bd9e` and `51a7d03` (test adaptation and `vm_required_test.go`), `5f3a5c4`, `e961113`, `6a44f22`, `18a9cbb`, `45cf65f` (fixes from tests and two review rounds).

## Verification

- `go vet ./cmd/... ./internal/...` and `go test -race ./cmd/... ./internal/...`: all packages pass.
- `vm_required_test.go` with a two-VM recording backend (one Off / both Running): every registered tool except `vm_list` and `vm_end_turn` refuses `{}`, `""`, blank and `null` `vm` with `invalid_argument`, `fields.vms` and task identity, and makes no backend call besides listing the names; a non-string `vm` is the schema type error; schemas mark `vm` required except `vm_list` and `vm_end_turn`; `vm_end_turn` without `vm` cleans only the task's own checkpoints, with `all_temp` and no or a blank `vm` is refused and deletes nothing, with `vm` acts on that VM only.
- Review: two independent rounds; no high findings; the medium findings (documentation of the refusal order) and the low ones (`vm_list` schema text, blank `vm` on `vm_end_turn`, test coverage) are fixed.
- Installed host (`v0.1.1-86-g18a9cbb` build plus the docs-only `45cf65f`; installed and build `hyperhand.exe` SHA-256 `C4E4B633…E407B809` identical), both VMs running, guest agents unchanged (`control-search-hint-20261010`, protocol 2):
  - `vm_list` without `vm`: lists `Win10` and `Win10-Example`.
  - `vm_status {}`, `vm_status {"vm": "  "}`, `vm_exec {"command": "hostname"}`, `vm_checkpoints {}`: `invalid_argument`, reason `vm is required: there is no default VM`, `next` `pass vm with one of: Win10, Win10-Example`, `vms`, `task_id` and `run_id`.
  - `vm_end_turn {"all_temp": true}`: refused with the all_temp reason.
  - `vm_status` with `vm: "Win10"` and `vm: "Win10-Example"`: both answer (agent ok, console session).
  - `tools/list`: 33 tools have a `vm` property; all but `vm_list` and `vm_end_turn` list it as required.
  - `vm_end_turn {}`: ends the task, nothing deleted.

## Not covered on the installed host

The "one VM off" case was covered by the unit tests only: no VM was shut down for this acceptance, because both are in use.
