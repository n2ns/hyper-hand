# VM save and pause: acceptance (2026-10-11)

## Build

- Source `3987d1d` plus the save/pause change committed with this record, installed with `go run ./cmd/hyperhand dev-install` (no UAC) as `dev-20261011-040054-3987d1d-dirty`; build and installed `hyperhand.exe` `81d3a1d5a1e1…`, `hyperhand-agent.exe` `52fa60d15f2d…` (equal). Win10 was updated with `vm_update_agent`.
- `go vet ./...` passes. `go test -race ./...`: `cmd`, `host`, `hyperv`, `broker`, `mirror` and `tray` pass; three desktop hit-test tests of `internal/agent` (`TestWindowAt`, `TestFocusedHelper`, `TestControlHintNativeIdentityAndFallback`) failed because the host desktop was locked (their hits landed on `LockApp.exe`), as recorded before. This change does not touch the agent package, which passed earlier the same day. `python -m unittest discover -s client` passes.

## Win10 runs

Evidence: ignored `build/phaseD-20261011/` (`accept_d.py`, `accept-results.json`). Notepad held unsaved text throughout.

- **First run (reproduced defect):** `vm_pause` and `vm_save` worked, but `vm_status` reported `power: "9"` while paused and `"6"` while saved, `vm_start` reported `previous_state` `"9"`/`"6"`, and a second `vm_pause`/`vm_save` was refused because the state did not read as Paused/Saved. Hyper-V on this host reports `EnabledState` 9 and 6 for paused and saved VMs, not the 32768/32769 HyperHand mapped. Fixed and reinstalled.
- **Pause:** `vm_pause` 0.06 s, `state: paused`; `vm_status` `power: paused`; a second `vm_pause` returned `paused` without a request. While paused, `vm_exec` failed in 0.07 s with `agent_required` (`next` names `vm_start`), and `vm_observe` returned a host screenshot with `agent: offline`. `vm_start` resumed in 3.2 s with `previous_state: paused`, `desktop: usable` and the agent's protocol; Notepad's text was intact.
- **Save:** `vm_save` 3.6 s, `state: saved`; `vm_status` `power: saved`; a second `vm_save` returned `saved` without a request; `vm_pause` of the saved VM was refused (`failed`, `VM Win10 is saved and cannot be paused`, `next` naming `vm_start`); `vm_exec` failed in 0.04 s with `agent_required`. `vm_start` resumed in 5.2 s with `previous_state: saved`, `desktop: usable`; Notepad's text was intact and `vm_exec hostname` answered in 0.2 s.

## Not covered

- Saving a VM with much more memory (the 5-minute job wait) and a session that locks while saved (the readiness wait unlocks it as after any start; not exercised here).
