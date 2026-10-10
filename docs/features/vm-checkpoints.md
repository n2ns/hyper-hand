# 3. VM and Checkpoint Tools

Part of [HyperHand features](../features.md). Section numbers such as 8.6 refer to the chapters listed there.

These tools use Hyper-V on the host and do not need the agent, except for the readiness and unlock steps in 3.5.

### 3.1 vm_list

`vm_list` lists all VMs and the task's run ID. The `vm` argument is ignored.

```json
{"vms": [{"name": "Win10", "id": "2F0A9B3C-...", "state": "Running"}], "run_id": "run-20261010-0812-7f3a"}
```

`vms` is `[]` when the host has no VMs.

### 3.2 vm_start, vm_shutdown, vm_turn_off, vm_save and vm_pause

- `vm_start` requests state Running (`RequestStateChange` 2) unless the VM is already Running, then waits until the desktop is usable (see 3.5). Result: `{"vm": "Win10", "state": "running", "desktop": "usable", "agent": {"version": "0.3.0", "hostname": "WIN10", "user": "WIN10\\tester", "protocol": <n>}, "unlocked": false, "previous_state": "saved"}`; `unlocked` is `true` when the session was locked and `vm_start` unlocked it; `previous_state` is the power state `vm_start` found (`running`, `off`, `saved` or `paused`). A saved or paused VM is resumed the same way as an off one is started: Running is requested, then the same readiness wait applies, so the result means the agent answers and the desktop is usable again. When the desktop is not usable the refusal's `reason` starts with `VM <name> is running, but its desktop is not usable:` and its code is `agent_required` (no agent answer within 90 s), `agent_outdated` (fields `agent_protocol`, `host_protocol`) or `session_unusable` (field `locked: true`; the reason says why the unlock failed, see 3.5).
- `vm_shutdown` asks the guest to shut down through the Hyper-V shutdown integration service (`Msvm_ShutdownComponent.InitiateShutdown` with `Force` false), then checks the VM state every 2 seconds for up to 3 minutes until it is Off. Because shutdown is not forced, a program with unsaved work can keep Windows from shutting down; the tool then fails (`failed`, the reason names `vm_observe` and `vm_turn_off`) and the VM keeps running. If the integration service refuses the request (not running, guest not booted, disabled in the VM settings), the tool fails. It never turns the VM off itself. MCP clients and scripts must allow calls of more than 3 minutes for the wait to finish. Result, also for a VM that is already off: `{"vm": "Win10", "state": "off"}`.
- `vm_turn_off` requests state Off (`RequestStateChange` 3), which turns the VM off at once like pulling the plug; unsaved guest work is lost. Result: `{"vm": "Win10", "state": "off"}`.
- `vm_save` requests state Saved (`RequestStateChange` 6): Hyper-V writes the VM's memory and device state to disk and stops it; programs and unsaved work survive. It waits up to 5 minutes for the job. A running or paused VM can be saved. Result: `{"vm": "Win10", "state": "saved"}`, also for a VM that is already saved.
- `vm_pause` requests state Paused (`RequestStateChange` 9): Hyper-V freezes the VM in memory at once. Only a running VM can be paused. Result: `{"vm": "Win10", "state": "paused"}`, also for a VM that is already paused.
- `vm_save` of an off VM and `vm_pause` of an off or saved VM are refused before anything is requested: `failed`, fields `vm` and `state`, `next` `call vm_start first if the VM should run, then retry`.
- While a VM is saved or paused nothing runs in the guest: tools that need the agent fail at once with `agent_required` (`next` names `vm_start`); host screenshots still work. Hyper-V reports a saved VM as `EnabledState` 6 (or 32769) and a paused one as 9 (or 32768); `vm_list`, `vm_status` and the tray show them as Saved and Paused.
- All of them close the VM's agent client afterwards (see 1.3).
- For `vm_start` and `vm_turn_off`, WMI return value 0 means completed. When WMI returns 4096 (job started), the tool waits up to 45 seconds for the asynchronous job and checks its result; a job failure or timeout is reported rather than returning success on acceptance. The operation is not resent automatically.

### 3.3 Checkpoints

Five tools manage Hyper-V checkpoints: `vm_checkpoints`, `vm_checkpoint`, `vm_restore`, `vm_checkpoint_delete` and `vm_checkpoint_keep`; `vm_end_turn` (see 7.4) deletes the temporary ones. None of them needs the agent.

#### Hyper-V facts the tools rely on

- A VM's checkpoints form a tree. Every checkpoint has a GUID id (the `Id` of [`Get-VMSnapshot`](https://learn.microsoft.com/en-us/powershell/module/hyper-v/get-vmsnapshot)) and an optional parent; the VM's current state branches from one checkpoint (the VM's `ParentCheckpointId`), which the tools report as `current_parent` and `current`.
- Names may repeat; Hyper-V does not prevent it. Only the id is unique, so the id is the selector the tools prefer.
- Two kinds: a **standard** checkpoint may hold the memory of a running VM and resumes running when restored; a **production** checkpoint is application-consistent and restores to off. The VM's checkpoint setting (`Set-VM -CheckpointType`: `Disabled`, `Production`, `ProductionOnly` or `Standard`) decides which kind [`Checkpoint-VM`](https://learn.microsoft.com/en-us/powershell/module/hyper-v/checkpoint-vm) creates; it cannot be chosen per checkpoint. HyperHand reports Hyper-V's `Standard` snapshot type as `standard`, `Recovery` as `production` and any other snapshot type lower-cased (`planned`, `missing`, `replica`, ...); those are not checkpoints to restore to. See [Microsoft's checkpoint guide](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/checkpoints).
- Deleting a checkpoint ([`DestroySnapshot`](https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/destroysnapshot-msvm-virtualsystemsnapshotservice)) merges its disk differences into its children or, when it is the current state's parent, into the VM's current disk; its children are re-parented to its parent. Deleting a subtree ([`DestroySnapshotTree`](https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/destroysnapshottree-msvm-virtualsystemsnapshotservice)) removes the checkpoint and all its descendants. The merge takes time in proportion to the differences, minutes for large ones; HyperHand waits for the Hyper-V job for up to 15 minutes.
- Restoring (applying) a checkpoint replaces the VM's current state. The replaced state is lost unless a checkpoint of it is created first; the restored checkpoint itself stays.
- Renaming ([`Rename-VMSnapshot`](https://learn.microsoft.com/en-us/powershell/module/hyper-v/rename-vmsnapshot)) changes the name only; the id stays.

#### Names and types

HyperHand names the checkpoints it creates after the task's run ID (see 2.1) and classifies every checkpoint by its name:

| Type | Name | Meaning | Deleted by |
|---|---|---|---|
| `temp` | `<run_id>-temp-<label>` | A rollback point for one run; disposable when the turn ends | `vm_end_turn` (this run's), `vm_end_turn` with `all_temp: true` (any run's), `vm_checkpoint_delete` |
| `keep` | `<run_id>-keep-<label>` | A baseline kept across runs | `vm_checkpoint_delete` only |
| `manual` | any other name | Made outside HyperHand, for example in Hyper-V Manager | `vm_checkpoint_delete` only, and only by `id` |

- Names match `^(run-\d{8}-\d{4}-(?:[0-9a-f]{4}|[0-9a-f]{16}))-(temp|keep)-(.+)$`, accepting legacy 4-hex and current 16-hex suffixes; `run_id` and `label` are parsed from the name and are `null` for a `manual` checkpoint.
- A `label` is 1 to 64 characters and contains none of `\ / : * ? " < > |` or line breaks (Hyper-V uses checkpoint names in file paths). Anything else is `invalid_argument` with `next` `pass a label of 1 to 64 characters without \ / : * ? " < > | or line breaks`. The default label is the current time as `hhmmss`.

#### Selecting a checkpoint

`vm_restore`, `vm_checkpoint_delete` and `vm_checkpoint_keep` select their checkpoint with `id` (from `vm_checkpoints`; preferred) or `name`.

- `id` wins when both are given and is compared case-insensitively. An unknown id is `no_checkpoint` with field `id` and `next` `call vm_checkpoints and pass a listed id`.
- `name` is compared exactly (case-sensitive) and accepted when exactly one checkpoint has it. Several checkpoints with that name: `ambiguous_target` with `ids` (their ids) and `next` `pass id instead of name (vm_checkpoints shows each one's parent and created_at)`. None: `no_checkpoint` with field `name`.
- Neither: `invalid_argument` with reason `id (or name) is required` and `next` `call vm_checkpoints and pass an id`.
- A checkpoint that disappeared between the listing and the operation (Hyper-V answers `checkpoint not found`) is `no_checkpoint` with field `id`.

#### vm_checkpoints

`vm_checkpoints` (read-only) returns the VM's checkpoint tree in creation order, parents before children:

```json
{"vm": "Win10", "checkpoint_type": "Standard", "current_parent": "c85ca8fb-...", "checkpoints": [
  {"id": "76221ce2-...", "name": "clean install", "parent": null, "created_at": "2026-10-04T23:49:55+08:00", "type": "manual", "run_id": null, "label": null, "kind": "standard", "state": "off", "current": false, "children": 1},
  {"id": "c85ca8fb-...", "name": "run-20261010-0812-7f3a-temp-step3", "parent": "76221ce2-...", "created_at": "2026-10-10T08:15:00+08:00", "type": "temp", "run_id": "run-20261010-0812-7f3a", "label": "step3", "kind": "standard", "state": "running", "current": true, "children": 0}
]}
```

- `checkpoint_type` is the VM's Hyper-V checkpoint setting (`Standard`, `Production`, `ProductionOnly` or `Disabled`), which decides what `vm_checkpoint` will create.
- `current_parent` is the id of the checkpoint the VM's current state branches from, `null` when there is none; the same checkpoint has `current: true`.
- Per checkpoint: `id`; `name`; `parent` (the parent's id, `null` for a root); `created_at` (RFC 3339 with the host's offset); `type`, `run_id` and `label` (see above); `kind` (`standard`, `production` or another Hyper-V snapshot type lower-cased, see above); `state`, the power state the checkpoint saved (`running`, `off` or `saved`; `running` means it holds memory and resumes directly); `current`; `children`, the number of direct children (deleting the checkpoint re-parents them).
- `checkpoints` is `[]` when the VM has none.

#### vm_checkpoint

`vm_checkpoint` creates a checkpoint of the VM's current state with `Checkpoint-VM`. It takes `label` (default `hhmmss`) and `keep` (default `false`) and names the checkpoint `<run_id>-temp-<label>`, or `<run_id>-keep-<label>` with `keep: true`. The new checkpoint becomes the current state's parent.

- Result: `{"id": "c85ca8fb-...", "name": "run-20261010-0812-7f3a-temp-step3", "type": "temp", "kind": "standard", "state": "running", "parent": "76221ce2-...", "created_at": "2026-10-10T08:15:00+08:00"}`. Pass `id` to `vm_restore`, `vm_checkpoint_keep` or `vm_checkpoint_delete`.
- A `temp` checkpoint is registered with the server, and `vm_end_turn` deletes it; `vm_checkpoint_keep` turns it into a `keep` one. A `keep` checkpoint is not registered and stays until `vm_checkpoint_delete`.
- Refusals, all before anything is created: an invalid `label` (`invalid_argument`, see above); the VM's checkpoint setting is `Disabled` (`invalid_argument` with field `checkpoint_type` and `next` `enable checkpoints for the VM in Hyper-V Manager (Settings > Checkpoints) or with Set-VM -CheckpointType Standard, then retry`). Hyper-V refusing at creation time because checkpoints are disabled is reported the same way, without the field.

#### vm_restore

`vm_restore` applies a checkpoint selected by `id` or `name` (see above), then starts the VM unless it is already running or `start` is `false`.

- `save_current` (default `false`): first saves the current state as the `temp` checkpoint `<run_id>-temp-before-restore` (registered for `vm_end_turn`) and reports it as `saved_current`. If that creation fails, nothing is restored. Without it the current state is replaced and lost.
- The VM is resolved before the restore. After the restore the VM's agent client is discarded (see 1.3).
- `start` (default `true`): if the VM is not running after the restore, HyperHand starts it. With `start: false` it leaves the state Hyper-V produced; it does not force Off or Saved. A running standard checkpoint resumes directly; a production checkpoint or one saved while off comes back off and needs the start.
- Result: `{"vm": "Win10", "restored": {"id": "c85ca8fb-...", "name": "run-20261010-0812-7f3a-keep-golden", "type": "keep"}, "state": "running", "saved_current": {"id": "3d9e...", "name": "run-20261010-0812-7f3a-temp-before-restore", "type": "temp"}, "next": "call vm_start to make sure the desktop is usable"}`. `state` is the VM's power state afterwards, read from Hyper-V (lowercase, see 2.3); `saved_current` is `null` without `save_current`; `next` is a hint in the successful result, not an error.
- The restore does not wait for a usable desktop: call `vm_start` (or `vm_status`) afterwards. A restore to a checkpoint taken before the agent was installed needs `vm_install_agent` again.
- Refusals: the selection refusals above; a Hyper-V failure is `failed`. When the restore fails after `save_current` saved the state, the error object carries `saved_current` so that the saved checkpoint can be found.

#### vm_checkpoint_delete

`vm_checkpoint_delete` (destructive) deletes a checkpoint selected by `id` or `name`, with `subtree` (default `false`).

- A `manual` checkpoint is deleted by `id` only. Selected by name, it is refused with `invalid_argument`, field `id` (the checkpoint's id) and `next` `pass id <id> instead of name`.
- Without `subtree`, `DestroySnapshot` deletes the one checkpoint: its disk differences are merged into its children, or into the VM's current disk when it is the current state's parent, and its children are re-parented to its parent. With `subtree: true`, `DestroySnapshotTree` deletes it and every descendant.
- The call waits for the merge, up to 15 minutes. Result: `{"deleted": [{"id": "c85ca8fb-...", "name": "run-20261010-0812-7f3a-temp-step3", "type": "temp"}], "merged_into_current": true, "elapsed_ms": 41873}`. `deleted` lists every deleted checkpoint, computed from the tree before the deletion, in the tree's listing order; `merged_into_current` is `true` when the current state's parent was among them (the current state's parent then moves up); `elapsed_ms` is how long Hyper-V took.
- Deleted `temp` checkpoints of this run are unregistered from `vm_end_turn`. A `keep` checkpoint is deleted like any other; delete it only when the baseline is no longer needed.
- Refusals: the selection refusals above, and `failed` with `elapsed_ms` and `next` `call vm_checkpoints; a merge may still be running in Hyper-V` when Hyper-V reports an error or the 15-minute wait expires.

#### vm_checkpoint_keep

`vm_checkpoint_keep` keeps a `temp` checkpoint across runs by renaming `<run_id>-temp-<label>` to `<run_id>-keep-<label>` with `Rename-VMSnapshot`. It takes `id` or `name` and an optional new `label` (same rules as above; default: the checkpoint's current label). The `run_id` in the name stays, even for a temp checkpoint of another run.

- Result: `{"id": "c85ca8fb-...", "name": "run-20261010-0812-7f3a-keep-step3", "type": "keep"}`; the id is unchanged. The checkpoint is unregistered from `vm_end_turn`.
- Only `temp` checkpoints are accepted. A `keep` one: `invalid_argument` with fields `id` and `name`, reason `"<name>" is already a keep checkpoint`, `next` `nothing to do; vm_end_turn does not delete keep checkpoints`. A `manual` one: `invalid_argument` with `id` and `name`, `next` `nothing to do; vm_end_turn does not delete manual checkpoints (pass a temp checkpoint's id to keep one)`.
- An invalid `label` is `invalid_argument` and is checked before the checkpoint is looked up; an unknown selector is `no_checkpoint` or `ambiguous_target` as above.

#### vm_end_turn and checkpoints

`vm_end_turn` (see 7.4) is the turn's cleanup.

- By default it deletes, by id, the `temp` checkpoints this run registered: those `vm_checkpoint` created without `keep` and the `before-restore` ones of `vm_restore`, minus those `vm_checkpoint_keep` renamed or `vm_checkpoint_delete` deleted. `vm` (case-insensitive) limits this to one VM. Each registered checkpoint is checked against the VM's current list first: one that no longer exists is forgotten silently; one whose name is no longer a `temp` name (renamed outside this server) is not deleted and listed in `skipped` with reason `no longer a temp checkpoint (now <type>); not deleted`; one whose deletion fails is listed in `errors` as `<vm>/<name>: <error>` and stays registered for the next call, as do all of a VM's checkpoints when its list could not be read (`errors`: `<vm>: <error>`). `keep` and `manual` checkpoints are never touched.
- `all_temp: true` deletes every checkpoint whose name parses as `temp`, whatever its `run_id`, on the VM named by `vm` (required with `all_temp`, see 2.3), to clean up after a crashed or restarted server. Another task's write ownership blocks this with `vm_busy`. One that could not be deleted is listed in `skipped` as `{"id", "name", "reason"}`; a VM that could not be found or whose checkpoints could not be listed is listed in `errors`.
- Each deletion merges disk differences and can take minutes.
- Result: `{"cancelled_waits": 0, "deleted_checkpoints": ["run-20261010-0812-7f3a-temp-step3", "run-20261009-2200-aaaa-temp-x"], "skipped": [{"id": "9a2f...", "name": "run-20261009-2200-aaaa-temp-stuck", "reason": "Hyper-V job failed"}], "errors": []}`; `deleted_checkpoints` holds names.

#### Annotations

`vm_checkpoints` is read-only and idempotent. `vm_restore`, `vm_checkpoint_delete` and `vm_end_turn` are destructive (`vm_end_turn` also idempotent). `vm_checkpoint` and `vm_checkpoint_keep` are neither read-only nor destructive and carry no idempotent hint (`vm_checkpoint_keep` refuses an already kept checkpoint instead of repeating the rename).

### 3.4 PowerShell-based operations

`vm_checkpoints`, `vm_checkpoint`, `vm_restore`, the rename of `vm_checkpoint_keep` and the file copy of `vm_install_agent` (see 8.1) run a hidden, non-interactive Windows PowerShell on the host with `$ErrorActionPreference = 'Stop'`, the VM looked up by ID and UTF-8 output. A failure is reported as `failed` with reason `powershell: <error>: <stderr and stdout>`. Checkpoint deletion (`vm_checkpoint_delete`, `vm_end_turn`) goes through WMI (`Msvm_VirtualSystemSnapshotService`, see 3.3) instead.

### 3.5 Session readiness and unlock: vm_start, vm_status, vm_unlock

These use the agent's `session_state` (see 10.2), which reports for the agent's own session: the lock state from `WTSQuerySessionInformation` (`WTSSessionInfoEx` SessionFlags), whether it is the console session (`WTSGetActiveConsoleSessionId`), whether keyboard input goes to a secure desktop the user cannot open (`OpenInputDesktop` fails with access denied: the sign-in screen's password box or a UAC prompt), and whether `LogonUI.exe` and `consent.exe` run in it (`WTSEnumerateProcesses`).

- When `vm_start` started a VM that was not running and **Open console when started** is checked for it in the tray (see 9.1), the tray opens its console in the background; `vm_start` does not wait for it.
- After the VM runs, `vm_start` pings the agent every 2 seconds for up to 90 seconds, refuses an agent whose protocol is older than the host's (`agent_outdated`, see 8.6; the same check guards every agent connection), and if the session is not locked checks again 3 seconds later, because Windows can lock a session right after an automatic sign-in. A locked session is unlocked as below. The VM keeps running in every case.
- `vm_status` reports the power state, whether an unlock password is stored, the task that holds the VM's write ownership and, for a running VM, the agent (5-second ping) and its session:

  ```json
  {"vm": "Win10", "power": "running", "unlock_password": "stored", "owner": {"task_id": "deploy-1", "idle_ms": 5300, "in_flight": 0, "this_task": false}, "agent": {"state": "ok", "version": "0.3.0", "hostname": "WIN10", "user": "WIN10\\tester", "protocol": <n>}, "session": {"locked": false, "console": true, "uac_prompt": false}}
  ```

  `owner` is `null` when no task owns the VM; otherwise `task_id`, `idle_ms` and `in_flight` as in `vm_busy` (see 2.2), and `this_task` whether it is the caller's own task (then `in_flight` includes this `vm_status` call). `unlock_password` is `stored`, `not stored` or `unknown` (Credential Manager could not be read). `agent.state` is `ok`, `busy` or `not_answering`; `busy` means this host already has a request in progress on the agent connection and the probe did not queue behind it (the session is then not queried); `not_answering` carries the error in `agent.error` and does not prove the agent is offline. `agent` and `session` are absent for a VM that is not running; `session_error` replaces `session` when the session query failed. Status changes nothing.
- `vm_unlock` unlocks a running VM's locked session. Result: `{"vm": "Win10", "state": "unlocked"}`, or `"state": "not_locked"` when nothing had to be typed. A VM that is not running is refused (`failed`, fields `vm` and `state`, `next` `call vm_start`).
- Unlocking reads the VM's unlock password from Windows Credential Manager (generic credential `HyperHand:<VM name>`, stored from the tray, see 9.1). It refuses before any input when no password is stored, the password is not ASCII, or the session is not the console session (all `failed`, with the reason saying so). It presses `ctrl` to dismiss the lock screen curtain and waits 1.5 seconds. Then, holding the input lock (see 4.5), it presses `ctrl+a`, checks once more that the session is locked, is the console session, runs `LogonUI.exe`, has keyboard input on the secure desktop and shows no UAC prompt, and only then types the password and `enter`. It waits up to 15 seconds for the session to report unlocked. The password is typed once per call: a wrong password is reported, not retried, so that the account is not locked out. The password never appears in results, errors or logs.
- An agent too old to know `session_state` is reported (`vm_unlock`: `failed` with a request to run `vm_update_agent`; `vm_start`: `agent_outdated`), and nothing is typed.
- Targeted actions refuse a locked session with `session_unusable` and `next` `call vm_unlock` (see 4.4); raw screen input is not checked, so the lock screen can be operated.
