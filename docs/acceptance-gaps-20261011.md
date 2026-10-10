# Automatable acceptance gaps on Win10 (2026-10-11)

Phase C of the TODO execution plan: hung UI threads, blocked graceful shutdown, mirror interruption and expiry, old-agent upgrade errors, guest uninstall and reinstall. All runs used `Win10` only, through `client/hyperhand_client.py`. Evidence: ignored `build/phaseC-20261011/` (scripts, JSON results, screenshots).

## Build

- Source `f29f004` plus the hung-window fix committed with this record, installed with `go run ./cmd/hyperhand dev-install` (no UAC) as `dev-20261011-031823-f29f004-dirty`. Build and installed `hyperhand.exe` SHA-256 `21ffa791affa…1909d824`, `hyperhand-agent.exe` `e0be349bad6b…87d66991`; Win10 was updated with `vm_update_agent` and its running agent has the same hash.
- `go vet ./...` and `go test -race ./...` pass. The fix was reviewed once (correctness and the requirement only); no defects.

## Hung UI thread

A WinForms window (`HH Hung Test`) whose UI thread sleeps 240 s, started 8 s after it is shown; its control tree was read before the hang. The guest's `IsHungAppWindow` reported `True` before and after the probes, and Windows showed a `Ghost` window titled `HH Hung Test (未响应)` in the foreground.

- **Before the fix (reproduced):** `vm_set_value` on the hung window did not return within the client's 120 s and `vm_invoke` took 77 s, returning only when the program woke up. While `vm_set_value` was pending, `vm_windows` from a second client also stalled for 60 s: every call to the VM queued behind it. With `activate: false` the same call returned `activate_failed` in 0.07 s, naming the `Ghost` window, which located the block in activation: the agent's `focus_window` restored and raised the window with calls that wait on its thread.
- **After the fix:** `vm_set_value`, `vm_invoke`, `vm_click`, `vm_type` and `vm_key` with the hung target each returned `target_not_responding` in 0.10–0.12 s with `handle`, `pid` and a `taskkill` `next`. `vm_find_controls` and `vm_wait control_exists` returned `target_not_responding` at the 10 s helper bound (previously `failed`). `vm_observe` with controls returned after 12.8 s with `stale_risk: target not responding: control tree not read` and a current screenshot. `vm_windows`, `vm_observe` (screenshot), `vm_wait window_exists` and `vm_exec` stayed under 5 s.

## Blocked graceful shutdown

Notepad with unsaved text, then `vm_shutdown`:

- After 183 s the call failed (`failed`, reason `VM Win10 is still Running 3m0s after the shutdown request ... It was not turned off`). `vm_status` showed `power: running`. HyperHand did not power the VM off.
- The guest was left in a pending sign-out: the agent had exited (`not_answering`), the console showed the lock screen and then the sign-in screen for the user, and neither the Hyper-V keyboard (the stored password, plain text, Ctrl+Alt+Del) nor clicks reached the password box over more than 8 minutes. The unsaved document could not be reached from inside the guest. The VM was recovered with `vm_turn_off` and `vm_start` (desktop usable, agent answering), losing the disposable document.
- Blocked: whether `vm_shutdown` should detect blocking programs or leave the guest recoverable is a product decision (see TODO).

## Directory mirror interruption and expiry

Source: a 3 GiB file and a small file; the guest target held one extra file, so a partial apply would be visible. The host's temporary payload reached full size about 4.5 s into the apply; each interruption came 2 s later, during the stream.

| Case | Apply outcome | Temporary files afterwards | Same plan again | Fresh plan |
|---|---|---|---|---|
| MCP `notifications/cancelled` | `mirror_unknown`, `status: unknown`, all three changes listed under `unknown`, after 6.7 s | none on the host (`%TEMP%\hyperhand-mirror-*.hhpart`) or the guest (`%TEMP%\hyperhand-mirror-*`, `*.hhpart` in the target); target unchanged | `plan_stale` | planned and applied `complete` (3 changes) |
| Host reinstall (`dev-install --no-build`) mid-stream | connection reset after 9.8 s | none on host or guest; target unchanged | `plan_stale` (plans are lost with the host) | `complete` |
| Plan applied after 10.5 minutes | `plan_stale`, nothing written | none | `plan_stale` | `complete` |

## Old agent upgrade errors

The v0.2.0 release agent was pushed and installed in the guest (`vm_launch ... install`); `vm_status` then reported agent `0.2.0`. A mirror plan was refused with `agent_outdated` (`agent_protocol: 0`, `host_protocol` the current one, `next` `call vm_update_agent`). `vm_update_agent` replaced it with the current agent, and the same mirror plan then succeeded.

## Guest uninstall and reinstall

The uninstall ran from the guest's Run dialog (`%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe uninstall`), not through `vm_exec`. Its message box listed the removed Run value and folders; `vm_status` then reported the agent `not_answering`. With no agent, three check commands were typed on the Hyper-V keyboard into the Run dialog (each screenshot before Enter); their output, read after the reinstall:

- `reg query ...\Run`: no `HyperHandAgent` value.
- `dir %LOCALAPPDATA%\HyperHand C:\Users\Public\HyperHand`: file not found for both.
- `tasklist /fi "imagename eq hyperhand-agent.exe"`: no matching process.

`vm_install_agent` (Copy-VMFile and the Run dialog, independent of the agent) then reinstalled it: the agent answered with the current version and protocol, the Run value points to `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe`, the running process uses that path, and both installed copies have the build's hash. The uninstalled executable remains as `%TEMP%\hyperhand-agent-uninstalled-<pid>.exe`, as documented.

## Blocked

- **Production-only checkpoints:** need `Set-VM -CheckpointType ProductionOnly` on Win10 (it is `Standard`). HyperHand has no tool for it and the developer account has no Hyper-V administrator rights on the host; changing either is the user's decision.
- **Long checkpoint merges at the timeout boundary:** HyperHand waits 15 minutes for a merge. A merge that long needs far more changed data than this VM and host can produce in a test, and a shorter test-only timeout would be a product change; the user's decision.

## Not verified

- A first attempt to type one long check command (248 characters, with parentheses and `&`) on the Hyper-V keyboard into the Run dialog did not run; the same checks as three shorter commands did. The cause (length, a character, or timing) was not isolated.
- AutoCAD regeneration as an unresponsive application was not exercised.
