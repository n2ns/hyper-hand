# HyperHand Features

This document describes the behaviour of HyperHand as implemented: the host tray program, the MCP tools it exposes, the guest agent and the protocol between them.

## 1. Architecture

HyperHand consists of two Windows executables.

- `hyperhand.exe` (host) runs as an ordinary user tray program and serves MCP over Streamable HTTP. The same executable runs separately as `HyperHandService`, under the dedicated virtual account `NT SERVICE\HyperHandService`, to perform WMI (`root\virtualization\v2`) and PowerShell Hyper-V operations.
- `hyperhand-agent.exe` (guest agent) runs inside the VM in the logged-on user's desktop session, shows a tray icon and answers requests from the host over a Hyper-V socket.

### 1.1 Host side

- The MCP endpoint is `http://127.0.0.1:<port>/mcp`, default port 8770 (see 9.1). It listens on the loopback interface only.
- One MCP server instance (name `hyperhand`, version stamped at build time, `dev` for source builds) serves all HTTP sessions.
- Screen capture, mouse, keyboard, VM state and checkpoints use Hyper-V through the service and work without the guest agent (see 3, 4).
- Commands, files, clipboard, window focus and waiting go through the guest agent (see 5, 6, 7).
- The tray requests specific broker operations over a local named pipe whose ACL permits the configured owner, SYSTEM, administrators and the service account. There is no arbitrary host command execution broker operation. Host file reads and writes stay in the ordinary tray process.
- The MCP HTTP endpoint remains unauthenticated. Its callers can use all exposed tools through the tray; the pipe ACL is not MCP authentication.

### 1.2 Hyper-V socket

- The agent listens on the Hyper-V socket service GUID `3ce544e1-2645-4383-b332-fedf8a18736b`, accepting connections from the parent partition.
- The installer registers this GUID under `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\<GUID>` with `ElementName` = `HyperHand`. Ordinary tray startup does not write this registration.
- The service dials the VM by its VM ID (`Msvm_ComputerSystem.Name`) and tunnels the connection to the tray. The guest service GUID is fixed; the broker does not accept an arbitrary guest socket service ID.

### 1.3 Connections and request ordering

- The host keeps one agent client per VM ID, holding at most one connection.
- Each client sends one request at a time and waits for its response; concurrent tool calls for the same VM queue on the client.
- A queued call can be cancelled or reach its context deadline without waiting for the active request to finish. It is not sent and does not interrupt the active request.
- Before reusing an idle connection the client probes it with a 1 ms read. A timeout means the connection is alive; EOF, any other error, or unexpected data marks it dead, and the client redials.
- If a request could not be sent, the client reconnects and sends it once more. The resend happens only if the request payload can be rewound (no payload, or a seekable source). A request that was sent is never resent, so a command cannot run twice.
- The client for a VM is closed and discarded after `vm_start`, `vm_shutdown`, `vm_turn_off` and `vm_restore` (see 3).

### 1.4 Cancellation

- When an MCP call is cancelled, the host forces the connection's deadline, which aborts the pending read or write, and drops the connection.
- The agent reads frames on a separate goroutine. The host sends nothing while it waits for a response, so a read error while a request runs means the host went away; the agent then cancels that request's context.
- Cancellation stops `exec` (the process tree is killed, see 5.4), `hash_files` and `wait`. The agent does not send a response after the host disconnected.

### 1.5 Agent service loop

- The agent serves one host connection at a time. When it ends, the agent waits for the next one.
- If the Hyper-V socket listener cannot be created, or accepting fails, the agent retries after 1 s.
- The agent tray icon (the gripper icon, also the executable's icon) opens a menu on a left or right click: `等待宿主机连接` (waiting for the host) or `宿主机已连接` (host connected), and a `退出` (quit) item. Like the host tray (see 9.1), it is added at once at logon and retried every 5 seconds and whenever the taskbar is created; the agent serves the host whether or not the icon could be shown.

### 1.6 Task ownership

- Every tool accepts optional `task_id`. A dedicated persistent MCP session gets a default task; stateless HTTP requires an explicit ID (`task_required`). Scripts that reconnect and agents sharing a session must pass a unique, consistent ID on every call. An explicit task survives reconnects while this host process lives.
- Observations, pending waits and temporary checkpoints belong to that task. Resolved tool results include `task_id` and the task's `run_id`. Observations cannot be shared between tasks.
- The first write call reserves the VM for that task until successful `vm_end_turn` cleanup. Another task's write receives `vm_busy` with `owner_task_id`; read-only calls remain available. There is no automatic idle takeover. IDs express ownership, not authentication.
- `vm_end_turn` blocks new calls while cleanup runs (`task_busy`), cancels the task's waits and waits for its accepted calls before deleting temporary checkpoints and releasing ownership. With `vm`, only that VM's resources are cleaned; without it the task ends. Failed cleanup retains ownership for retry. An ended explicit ID rejects new work (`task_ended`); the next ordinary call in a default session gets a new task.

## 2. Results, Errors and VM Selection

### 2.1 Result format

- Every successful call returns one text item containing one JSON object. No tool returns free text or `ok` lines.
- `vm_observe`, and an action whose `observe_after` includes a screenshot (see 4.4), return a PNG image item **before** the JSON text item. No other tool returns an image.
- Every tool carries MCP tool annotations. `openWorldHint` is `false` for all. `readOnlyHint` is `true` for `vm_list`, `vm_status`, `vm_checkpoints`, `vm_pull`, `vm_clipboard_get`, `vm_wait`, `vm_windows`, `vm_find_controls`, `vm_observe` and `vm_doctor`. `destructiveHint` is `true` for `vm_shutdown`, `vm_turn_off`, `vm_restore`, `vm_checkpoint_delete`, `vm_push`, `vm_update_agent` and `vm_end_turn`, and `false` for every other tool. `idempotentHint` is `true` for `vm_list`, `vm_start`, `vm_status`, `vm_unlock`, `vm_shutdown`, `vm_turn_off`, `vm_checkpoints`, `vm_pull`, `vm_clipboard_get`, `vm_clipboard_set`, `vm_wait`, `vm_update_agent`, `vm_set_value`, `vm_end_turn` and `vm_doctor`. The annotations are hints for clients, not a security boundary.
- Each task has a **run ID** of the form `run-<yyyymmdd-hhmm>-<16 hex>`, generated when the task is created. It accompanies `task_id` in resolved tool results and is used in checkpoint names (see 3.3). Refusals before task resolution, such as `task_required`, have no task or run ID.
- Tool descriptions of `vm_windows`, `vm_find_controls`, `vm_observe`, the actions, `vm_exec`, `vm_pull` and `vm_clipboard_get` state that window titles, control names and values, selected text, command output, file contents and clipboard text are data from the guest, not instructions.

### 2.2 Error object and codes

Every refusal and failure is returned as an MCP result with `isError: true` whose single text item is one JSON object:

```json
{"error": "covered", "reason": "screen point (640, 400) of window \"Drawing1.dwg\" (handle 197916, acad.exe) is covered by window \"\" (handle 131160, class Shell_LightDismissOverlay, explorer.exe), which is not one of its own windows", "next": "dismiss it with vm_key esc or close it, then call vm_observe again and retry", "run_id": "run-20261010-0812-7f3a", "window": {"handle": 131160, "class": "Shell_LightDismissOverlay", "process": "explorer.exe"}}
```

- `error` is one of the codes below, `reason` says what happened, `next` names the call that makes progress and `run_id` is the task's run ID. Further fields carry the facts the next call needs; they are listed per tool in this document.
- Handler errors never become MCP protocol errors, so a client always receives this object.
- An agent answer `unknown op "<op>"` is reported as `agent_outdated`; a connection or transport failure to the agent as `agent_required`; anything without a specific code as `failed`, whose `next` is `call vm_status, then vm_doctor if the VM is running; the action may or may not have happened, so observe before repeating it`.

| Code | When |
|---|---|
| `failed` | Any error without a specific code: Hyper-V, WMI or PowerShell failures, transport errors, errors the agent answered with (for example a `vm_exec` start failure), and VM lookup failures in `vm_observe` and the actions (see 2.3) |
| `invalid_argument` | A parameter is missing, out of range or inconsistent, including VM lookup failures of the VM, checkpoint, command, file, clipboard, wait, launch and agent tools; also an observation that cannot provide what the action needs (no screenshot for pixel coordinates, no control tree for `index`) and a pixel outside the observation image; for checkpoints (see 3.3) an invalid `label`, a VM whose checkpoint setting is `Disabled`, a `manual` checkpoint selected by `name` for deletion, and `vm_checkpoint_keep` on a `keep` or `manual` checkpoint |
| `task_required` | The transport has no persistent MCP session and `task_id` was omitted |
| `task_ended` | An explicit task ID was already ended; choose a new ID |
| `task_busy` | Cleanup is running for this task; wait for it to finish |
| `vm_busy` | Another task owns writes to this VM; its `vm_end_turn` must release ownership |
| `stale_observation` | The observation is unknown, evicted or belongs to another task/VM; its window identity or geometry changed; a HyperHand lifecycle operation invalidated it; or its coordinates fail revision or local screenshot checks (see 4.1) |
| `stale_element` | `index` is not in the observation's tree, or the control's runtime ID no longer resolves in the window when the host re-locates it before acting |
| `session_unusable` | A targeted action: the agent reports its session locked, not the VM console session, or keyboard input on the secure desktop (see 4.4 step 2); untargeted `vm_type` of non-ASCII text while the session is locked or on the secure desktop |
| `activate_failed` | The target window (or one of its own windows) is not in the foreground and could not be brought there, or `activate` is `false` |
| `target_disabled` | The target window, or the window of its group that would receive input, is disabled or minimized (typically a modal dialog is open) |
| `covered` | The screen point of a pointer action reaches a top-level window outside the target's group |
| `integrity_mismatch` | The target window's process runs at a higher integrity level than the agent, so Windows would drop the input (UIPI) |
| `target_not_responding` | A UI Automation action on the window did not finish within the host's 12 s, or the agent's own UI Automation helper timed out (10 s) |
| `unsupported_pattern` | The control does not support the requested pattern action; `supported` lists the patterns it does support |
| `partial_input` | `vm_type` or `vm_key` stopped after some input was injected; `applied_chars` / `total_chars` or `applied` / `total` say how much. A failure before anything was injected keeps the underlying code (`failed` with `applied_chars: 0`, or the refusal of the first combination) |
| `agent_required` | The operation needs the guest agent, which did not answer or is not installed |
| `agent_outdated` | The agent's protocol is older than the host's (`protocol` 2): every new agent connection is pinged first and every op except `ping` and `update_agent` is then refused (see 8.6); also an agent that answered `unknown op`. `next` is `call vm_update_agent` |
| `ambiguous_target` | `pid` alone selects a process with several visible windows; `handles` lists them. A checkpoint `name` that several checkpoints have; `ids` lists them (see 3.3) |
| `no_window` | No visible window has the given `handle` or `pid`, no window is in the foreground when one is needed, or `vm_launch` saw no window in time |
| `no_checkpoint` | No checkpoint has the given `id` or `name` (field `id` or `name`), or the selected checkpoint disappeared before the operation; `next` is `call vm_checkpoints and pass a listed id` (see 3.3) |

### 2.3 VM selection

Every tool except `vm_list` takes an optional `vm` argument.

- A non-empty `vm` is matched case-insensitively against VM names (`ElementName`).
  - No match: `VM "<name>" not found`.
  - Several matches: `several VMs are named "<name>"`.
- An empty `vm` selects the only running VM (`EnabledState` = 2).
  - If no VM is running and the host has exactly one VM, that VM is selected (so `vm_start` works without a name).
  - If no VM is running and there are several VMs: `no running VM`.
  - If several VMs are running: `several VMs are running; specify one by name`.
- The VM, checkpoint, command, file, clipboard, wait, launch and agent tools report these as `invalid_argument` with `next` `call vm_list and pass one of its names as vm` (or `call vm_start with the VM's name, or pass vm` when no VM runs, `pass vm` when several run). `vm_observe` reports them as `failed` with `next` `call vm_list and pass vm`; the actions as `failed` with the default `next` of 2.2.
- Only `Msvm_ComputerSystem` objects whose `Name` is GUID-shaped are treated as VMs; the host computer itself is excluded.
- State names are `Running`, `Off`, `Saved` and `Paused`; any other state is reported as its numeric `EnabledState`. `vm_list` reports them as Hyper-V names them; `vm_status`, `vm_start`, `vm_shutdown`, `vm_turn_off` and `vm_restore` report lowercase (`running`, `off`, `saved`, `paused`).

## 3. VM and Checkpoint Tools

These tools use Hyper-V on the host and do not need the agent, except for the readiness and unlock steps in 3.5.

### 3.1 vm_list

`vm_list` lists all VMs and the task's run ID. The `vm` argument is ignored.

```json
{"vms": [{"name": "Win10", "id": "2F0A9B3C-...", "state": "Running"}], "run_id": "run-20261010-0812-7f3a"}
```

`vms` is `[]` when the host has no VMs.

### 3.2 vm_start, vm_shutdown and vm_turn_off

- `vm_start` requests state Running (`RequestStateChange` 2) unless the VM is already Running, then waits until the desktop is usable (see 3.5). Result: `{"vm": "Win10", "state": "running", "desktop": "usable", "agent": {"version": "0.3.0", "hostname": "WIN10", "user": "WIN10\\tester", "protocol": 2}, "unlocked": false}`; `unlocked` is `true` when the session was locked and `vm_start` unlocked it. When the desktop is not usable the refusal's `reason` starts with `VM <name> is running, but its desktop is not usable:` and its code is `agent_required` (no agent answer within 90 s), `agent_outdated` (fields `agent_protocol`, `host_protocol`) or `session_unusable` (field `locked: true`; the reason says why the unlock failed, see 3.5).
- `vm_shutdown` asks the guest to shut down through the Hyper-V shutdown integration service (`Msvm_ShutdownComponent.InitiateShutdown` with `Force` false), then checks the VM state every 2 seconds for up to 3 minutes until it is Off. Because shutdown is not forced, a program with unsaved work can keep Windows from shutting down; the tool then fails (`failed`, the reason names `vm_observe` and `vm_turn_off`) and the VM keeps running. If the integration service refuses the request (not running, guest not booted, disabled in the VM settings), the tool fails. It never turns the VM off itself. MCP clients and scripts must allow calls of more than 3 minutes for the wait to finish. Result, also for a VM that is already off: `{"vm": "Win10", "state": "off"}`.
- `vm_turn_off` requests state Off (`RequestStateChange` 3), which turns the VM off at once like pulling the plug; unsaved guest work is lost. Result: `{"vm": "Win10", "state": "off"}`.
- All three close the VM's agent client afterwards (see 1.3).
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
- The VM is resolved before the restore, so an empty `vm` still refers to the same VM after it stops running. After the restore the VM's agent client is discarded (see 1.3).
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
- `all_temp: true` deletes every checkpoint whose name parses as `temp`, whatever its `run_id`, on `vm` or on every VM when `vm` is omitted, to clean up after a crashed or restarted server. Another task's write ownership blocks this with `vm_busy`. One that could not be deleted is listed in `skipped` as `{"id", "name", "reason"}`; a VM that could not be found or whose checkpoints could not be listed is listed in `errors`.
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
- `vm_status` reports the power state, whether an unlock password is stored and, for a running VM, the agent (5-second ping) and its session:

  ```json
  {"vm": "Win10", "power": "running", "unlock_password": "stored", "agent": {"state": "ok", "version": "0.3.0", "hostname": "WIN10", "user": "WIN10\\tester", "protocol": 2}, "session": {"locked": false, "console": true, "uac_prompt": false}}
  ```

  `unlock_password` is `stored`, `not stored` or `unknown` (Credential Manager could not be read). `agent.state` is `ok`, `busy` or `not_answering`; `busy` means this host already has a request in progress on the agent connection and the probe did not queue behind it (the session is then not queried); `not_answering` carries the error in `agent.error` and does not prove the agent is offline. `agent` and `session` are absent for a VM that is not running; `session_error` replaces `session` when the session query failed. Status changes nothing.
- `vm_unlock` unlocks a running VM's locked session. Result: `{"vm": "Win10", "state": "unlocked"}`, or `"state": "not_locked"` when nothing had to be typed. A VM that is not running is refused (`failed`, fields `vm` and `state`, `next` `call vm_start`).
- Unlocking reads the VM's unlock password from Windows Credential Manager (generic credential `HyperHand:<VM name>`, stored from the tray, see 9.1). It refuses before any input when no password is stored, the password is not ASCII, or the session is not the console session (all `failed`, with the reason saying so). It presses `ctrl` to dismiss the lock screen curtain and waits 1.5 seconds. Then, holding the input lock (see 4.5), it presses `ctrl+a`, checks once more that the session is locked, is the console session, runs `LogonUI.exe`, has keyboard input on the secure desktop and shows no UAC prompt, and only then types the password and `enter`. It waits up to 15 seconds for the session to report unlocked. The password is typed once per call: a wrong password is reported, not retried, so that the account is not locked out. The password never appears in results, errors or logs.
- An agent too old to know `session_state` is reported (`vm_unlock`: `failed` with a request to run `vm_update_agent`; `vm_start`: `agent_outdated`), and nothing is typed.
- Targeted actions refuse a locked session with `session_unusable` and `next` `call vm_unlock` (see 4.4); raw screen input is not checked, so the lock screen can be operated.

## 4. Observation and Input

### 4.1 Observations

An observation is what the host remembers about one `vm_observe` result so that later actions can be checked against it and mapped back to the screen.

- Each `vm_observe` call stores: the VM, the observed window (its handle, PID and `rect` at capture time; none for a whole-screen observation), the window list at capture time, the screenshot geometry (the captured screen region and the output scale per axis), the control tree nodes when one was read, and the **tree window** the nodes belong to (the observed window, or the foreground window whose tree a whole-screen observation read). It returns the observation's `observation_id`, `o-` followed by 8 hex digits.
- The host keeps the 8 most recent observations per VM per task. An `observation_id` that is unknown to this task or evicted is refused with `stale_observation` (`call vm_observe again and use its observation_id`).
- **Coordinate mapping.** Host screenshots are taken at the guest's screen resolution, so screenshot pixels are guest screen pixels. For an image pixel `(u, v)` of an observation with crop origin `(x, y)`, crop size `w x h` and per-axis scales `scale_x`, `scale_y` (output pixels per screen pixel; integer rounding of the output size can make them differ slightly, and the result's `screenshot.scale` reports `scale_x`), the host computes the screen point `x + min(w - 1, floor((u + 0.5) / scale_x))`, `y + min(h - 1, floor((v + 0.5) / scale_y))`. Callers pass image pixels and never convert. A pixel outside the output image, or coordinates for an observation taken with `screenshot: false`, is `invalid_argument`.
- **Index resolution.** A control `index` names a node of the observation's tree. Every index action asks the agent to re-find the element by its `runtime_id` in the tree window (`control_action` `Locate`, which performs nothing and returns the element's current `rect` and value): pointer actions then click the centre of the **current** rectangle (`(left + right) / 2`, `(top + bottom) / 2`), so a control that moved since the observation (a scrolled list, a re-laid-out dialog) is hit where it is now; `vm_set_value` and `vm_invoke` act on the re-found element. A node without a runtime ID uses its old rectangle only after revision and local screenshot checks; without a screenshot that fallback is refused. A vanished element is `stale_element`; an index outside the tree is `stale_element`; an observation without a tree is `invalid_argument` (`call vm_observe with controls: true`).
- **Freshness.** The host rechecks the window's handle, PID, process and class, minimized state and geometry. Shared VM revisions invalidate old coordinates after dispatched input (including failed or partial attempts) and command/launch operations. HyperHand lifecycle operations also invalidate old control references. A stable UI Automation runtime ID may still be re-located after ordinary input; an index without one uses the coordinate checks.
- Before using observation coordinates, including whole-screen pixels, the host compares a fresh screenshot with the stored original-resolution capture around the target point. The region is up to 64 by 64 pixels, clipped to the captured region; over 5% of pixels with a channel change greater than 24/255 causes `stale_observation`. Drag checks both endpoints. Input revisions and window identity are checked again after capture. `vm_observe` serializes with input and refuses to register a capture whose revision or window identity/geometry changed during acquisition. A running external operation can still be observed, with `stale_risk`; actions using observations are refused until it finishes.
- These checks are a local visual heuristic, not a page identity guarantee: changes elsewhere, small changes, identical-looking controls, externally initiated lifecycle changes that reuse the same identity, or changes after the check can escape detection. Local animation can cause a refusal. Raw input without an observation remains available for sign-in and UAC; it bypasses observation freshness checks.
- The `screenshot` object of the result (`origin_x`, `origin_y`, `scale`) documents the mapping for auditing; actions do not need it.

### 4.2 vm_windows

`vm_windows` lists the visible top-level windows, from the top of the Z order down. It needs the agent (`agent_required` otherwise). Windows hidden by DWM (cloaked, such as suspended store apps) are left out.

```json
{"windows": [{"handle": 197916, "title": "AutoCAD 2015 - Drawing1.dwg", "class": "Afx:400000:...", "pid": 4120, "process": "acad.exe", "rect": {"left": 0, "top": 0, "right": 1920, "bottom": 1040}, "enabled": true, "foreground": false, "minimized": false, "modal": false, "group_root": 197916, "integrity": "medium"},
             {"handle": 2818992, "title": "", "class": "HwndWrapper[...]", "pid": 4120, "process": "acad.exe", "rect": {"left": 0, "top": 980, "right": 1920, "bottom": 1040}, "enabled": true, "foreground": true, "minimized": false, "owner": 197916, "modal": false, "group_root": 197916, "integrity": "medium"}],
 "foreground": 2818992,
 "focused_control": {"window": 2818992, "name": "", "control_type": "Edit", "class_name": "Edit", "runtime_id": "42.2818992.4", "rect": {"left": 0, "top": 980, "right": 1920, "bottom": 1040}},
 "session": {"locked": false, "console": true, "secure_desktop": false, "logonui": false, "consent": false}}
```

- Each window has `handle`, `title`, `class`, `pid`, `process` (executable name; empty if the agent cannot query the process), `rect` (`left`, `top`, `right`, `bottom` of the visible frame without invisible resize borders, in physical screen pixels), `enabled`, `foreground`, `minimized`, `owner` (the owner window's handle, omitted when there is none), `modal` (the owner window is disabled, as it is while a modal dialog runs), `group_root` and `integrity`.
- `group_root` is the handle reached by following `owner` links through listed windows of the same process, at most 8 links; the window's own handle when it has no such owner. Windows with the same `group_root` form a **group** (the "own windows" of the root): in AutoCAD 2015 the untitled command line is owned by the main window and its command history popup by the command line, so all three share the main window's `group_root`. The actions treat a group as one target (see 4.4). Windows of other processes are never in a group.
- `integrity` is the process token's integrity level, `low`, `medium`, `high` or `system`, omitted when it cannot be read (for example a protected process).
- `foreground` is the foreground window's handle, `0` when none.
- `focused_control` describes the UI Automation element with keyboard focus (`IUIAutomation::GetFocusedElement`): `window` (the top-level window containing it), `name` (empty for password controls), `control_type` (the UIA control type name), `automation_id` and `class_name` (omitted when empty), `runtime_id` and `rect`. It is `null` when nothing has focus or the lookup did not answer within 2 s. Only `vm_windows` and `vm_observe` ask the agent for it (`list_windows` with `focused: true`, which starts a UI Automation helper process); the window lists the actions and `vm_launch` fetch do not.
- `session` is the agent's `session_state` (see 3.5), `null` when it could not be read.
- `windows` is `[]` when no window is visible.

### 4.3 vm_observe

`vm_observe` is the observation entry point: one call returns a screenshot, the observed window, the focused control and, on request, the window's control tree as indexed text. It stores the observation (see 4.1) and returns a PNG image item followed by the JSON text item.

| Parameter | Default | Meaning |
|---|---|---|
| `vm` | the only running VM | see 2.3 |
| `handle`, `pid` | none | observe this window (`handle` from `vm_windows`, a previous observation, `vm_launch` or an action result; `pid` restricts it, or alone selects the process's only visible window). Without both, the whole screen |
| `screenshot` | `true` | include the PNG |
| `controls` | `false` | include the control tree |
| `observation_id`, `index` | none | select a control subtree instead of `handle`/`pid`; implies controls, with root index 0 and depth 0; screenshot still covers its owning window |
| `max_depth` | 4 | tree depth, 1 to 10 (0 means the default) |
| `max_nodes` | 200 | tree size, 1 to 1000 (0 means the default) |
| `max_size` | 0 | longest side of the output image in pixels; 0 keeps the original size; never upscales |
| `diff_from` | none | a previous tree observation of the same window and subtree root: `controls_diff` replaces `controls`; search result sets cannot be diff bases |

```json
{"observation_id": "o-7f3a9c21", "vm": "Win10", "captured_at": "2026-10-10T08:12:03Z",
 "window": {"handle": 197916, "pid": 4120, "process": "acad.exe", "title": "AutoCAD 2015 - Drawing1.dwg", "class": "Afx:400000:...", "rect": {"left": 0, "top": 0, "right": 1920, "bottom": 1040}, "foreground": true, "enabled": true, "group_root": 197916, "integrity": "medium"},
 "screenshot": {"width": 1280, "height": 693, "origin_x": 0, "origin_y": 0, "scale": 0.6666666666666666},
 "focused": {"index": 12, "name": "", "control_type": "Edit", "class_name": "Edit", "rect": {"left": 0, "top": 980, "right": 1920, "bottom": 1040}},
 "selected_text": "",
 "controls": "[0] Window \"AutoCAD 2015 - Drawing1.dwg\" (0,0 1920x1040)\n  [1] Pane \"Ribbon\" (0,52 1920x150)\n    [2] Button \"Line\" id=ID_Line (12,80 48x48)\n  [12] Edit \"\" id=cmdline (0,980 1920x60) focused value=\"LINE\"\n",
 "controls_truncated": false}
```

Behaviour, in order:

1. Parameter checks: `max_depth` outside 0 to 10, `max_nodes` outside 0 to 1000 or a negative `max_size` is `invalid_argument`.
2. The VM is resolved once (see 2.3).
3. The host asks the agent for the window list with the focused control (`list_windows` with `focused: true`). When the agent cannot be reached: with `handle` or `pid` the call is refused with `agent_required`; without them it continues with the screenshot only and the result has `"agent": "offline"` and no `window`, `focused` or `controls`.
4. With `handle` or `pid` the window is selected as in 4.4 step 4; `no_window` and `ambiguous_target` refusals carry `candidates`, the windows the selector could have meant (`[{handle, pid, process, title, class}]`, those of `pid` when given, else all). Without them and with `controls`, the tree is read from the foreground window, which `window` then names and which becomes the observation's tree window (index actions target it; coordinate actions on the whole-screen observation use revision and local image checks, see 4.1); with no foreground window the tree is not read and `stale_risk` says `no foreground window: control tree not read`.
5. Screenshot (`screenshot: true`): the image is taken from the Hyper-V console via WMI `GetVirtualSystemThumbnailImage`, at the current resolution of the VM's first video head, without the agent, so it also shows the lock screen and UAC prompts. Hyper-V delivers RGB565, which is converted to 8-bit RGBA, so colours are quantised. For a window observation the image is cropped to the window's `rect` intersected with the screen; a window that is entirely off screen or minimized is `invalid_argument` (`restore the window first (vm_key, or act on it by index), or observe without handle`). `max_size` shrinks with nearest-neighbour sampling at pixel centres, preserving aspect ratio subject to integer rounding and a minimum of one pixel per axis; an uncropped, unscaled image keeps the original PNG bytes. A capture failure is `failed` (`call vm_status and make sure the VM is running`).
6. Control tree (`controls: true` or `diff_from` set, and a target window): the agent reads the UI Automation control-view tree rooted at the window (`list_controls`) in a disposable helper process with a 10-second timeout. A timeout or interrupted helper is not an error: the result has no `controls` and `stale_risk` says `target not responding: control tree not read`, and the screenshot is still current. An agent that stops answering at this point is `agent_required` for a window observation and `"agent": "offline"` otherwise. Other agent errors are `failed`.
7. The VM revision and window identity/geometry are checked again. A revision or window change during capture is refused as `stale_observation`; otherwise the observation is stored for this task and its ID returned. When an external operation remains in progress, `stale_risk` advises waiting and observing again before acting.

Result fields (absent when empty):

- `window`: `handle`, `pid`, `process`, `title`, `class`, `rect`, `foreground`, `enabled`, `group_root`, `integrity` of the observed window, or of the foreground window whose tree a whole-screen observation read.
- `screenshot`: `width` and `height` of the returned image, `origin_x` and `origin_y` (its top-left corner in guest screen pixels) and `scale` (output pixels per screen pixel; 1 when not scaled). See 4.1.
- `focused`: the control with keyboard focus. When the tree was read and contains it, `index` is its index there; otherwise `index` is `-1` and the data comes from `list_windows`, only when the focused control belongs to the observed window's group (or for a whole-screen observation). `control_type` is a name (`Edit`, `Button`, ...).
- `selected_text`: the first selected `TextPattern` range of the window, or of the focused element when it belongs to the same process; at most 512 UTF-16 units; only when the tree was read.
- `controls`: the tree as text, one node per line, indented two spaces per depth: `[index] ControlType "name" id=automation_id (x,y wxh)`, then the state words `disabled`, `offscreen` and `focused` when they apply, then `value="..."` when the control supports `ValuePattern`. `id=` appears only when the control has an automation ID; `x,y` and `wxh` are the control's `rect` in physical screen pixels. The root has index 0 and depth 0. Names, automation IDs, class names and values longer than 512 UTF-16 units are cut, and `controls_truncated` is then `true`.
- `controls_truncated`: `true` when the tree hit `max_depth` or `max_nodes`, a password control's subtree was skipped, or a text was cut. Password controls have their `name` not read, their `value` omitted and their descendants not traversed; this is not a general secret detector.
- `controls_diff` (with `diff_from`): `{"added": [lines], "removed": [old indexes], "changed": [lines]}` and `controls` is omitted. Nodes are matched by `runtime_id`, or by their path of (control type, name) pairs from the root when the agent reports none; a node is `changed` when its index, name, rect, enabled, offscreen, focused or value differs (an inserted node therefore lists every node whose index shifted, so that the caller learns the new indexes); `removed` lists indexes of the earlier observation, sorted. When the earlier observation is unknown, of another VM or window, or has no tree, the full `controls` is returned and `stale_risk` says `diff_from ignored: <reason>`.
- `stale_risk`: the explanations above, joined with `; `.
- `agent`: `offline` when the agent could not be reached and only the screenshot is present.

Control lines also include `actions=[SetValue,...]` and compact JSON `state={...}` when available. Actions name supported operations: use `vm_set_value` for `SetValue` and `vm_invoke` for the others. They describe UIA capabilities, not a guarantee that a disabled or read-only control accepts the action. State fields are `toggle` (`off`, `on`, `indeterminate`), `expand_collapse` (`collapsed`, `expanded`, `partially_expanded`, `leaf`), `selected`, `read_only` and `offscreen`. Unreadable or unsupported properties are omitted, never replaced by false; password text stays hidden. `controls_diff` also reports changed actions and semantic state. Older agents' pattern names are translated to actions when possible; update the agent for semantic state.

Scrollable controls also expose readable axis support and scroll percentages in `state`; see 4.7 for field names and directional actions.

Tree coverage depends on the application's UI Automation provider; custom-drawn controls (common in CAD programs) may be absent. See Microsoft's [UI Automation tree views](https://learn.microsoft.com/en-us/windows/win32/winauto/uiauto-treeoverview), [UI Automation security boundaries](https://learn.microsoft.com/en-us/windows/win32/winauto/uiauto-securityoverview) and [physical pixels and DPI awareness](https://learn.microsoft.com/en-us/windows/win32/hidpi/high-dpi-desktop-application-development-on-windows); the depth, node, text and timeout limits are HyperHand choices.

#### Control search and subtree observation

`vm_find_controls` searches the UIA control view without first returning a whole-window snapshot. Select a window by `handle`/`pid`, or a known control subtree with `observation_id` and `index` (alternative selectors). Supply at least one of `automation_id`, `control_name`, or `control_type`. Names and AutomationIds match exactly and case-sensitively; type names such as `Button`, `Edit`, `Group` and `Pane` are case-insensitive. All supplied filters must match. The scoped root itself is included in the search.

Search defaults are `max_depth: 32`, `max_visited: 5000`, `max_matches: 20`; maxima are 64, 20000 and 100. Zero selects the default. Unlike the ordinary 200-node observation, nonmatching nodes need only selector properties; full actions/state/value are read for matches. Provider calls still use the guest's 10-second helper timeout and a 12-second host budget. Search cannot expose custom-drawn controls that the application does not provide through UIA.

The result contains `{observation_id, vm, window, matches, visited, truncated, truncation, status}`. Match indexes are contiguous from zero and include the available control identity, rectangle, actions, value and state. `status` is `unique`, `multiple`, `not_found`, or `incomplete`. Limits, password-subtree suppression, clipped properties and observed property changes mark incomplete data explicitly; even one match in an incomplete result does not prove uniqueness. Multiple matches are returned for the caller to choose, without acting on the first. Match indexes refer to actual runtime identities, so the caller can still select a returned match explicitly when the search was incomplete.

The returned observation belongs to the current task and VM. Pass its `observation_id` plus a match `index` to existing actions or `vm_wait`. Those waits read the selected runtime-ID root directly, so a match beyond ordinary snapshot limits remains reachable. They never turn an incomplete root lookup into `control_gone`. Search reads do not activate windows or reserve VM writes; window identity, session and VM revision are checked before retaining results.

`vm_observe` with `observation_id`/`index` re-finds that control and reads its subtree. Controls are implied, the root has index 0/parent -1/depth 0, and the ordinary 4/200 defaults and 10/1000 limits now apply relative to that root. Selected text and focused-control metadata stay within the subtree; a requested screenshot remains a crop of the owning window. Root disappearance returns `stale_element`; a bounded or privacy-limited lookup that cannot establish absence returns `search_incomplete`. There is no whole-window fallback. Subtree provider failures remain errors, unlike the best-effort whole-window screenshot path above. `diff_from` only compares trees with the same window and runtime-ID root; a different root or a search result set returns the full tree with a `stale_risk` explanation.

The guest uses separate `find_controls` and `list_control_subtree` operations, so an older generation-2 agent returns `agent_outdated` and directs the caller to `vm_update_agent` instead of silently ignoring the scope. Root identity lookup is bounded by 20000 visited nodes, depth 64 and the helper timeout. Window movement still invalidates action observations under the existing freshness rules.

For a previously observed control, the host supplies its old screen bounds as an internal lookup hint. The guest uses a read-only UIA point lookup, then requires an exact runtime-ID match and verifies the control-view parent chain back to the requested window, including the password-ancestor checks for scoped reads. The hint does not move the pointer, activate a window, or select a different control. Invalid, covered, moved or unmatched hints fall back to the existing traversal and cannot establish absence. Actions and wait conditions still read current values and state. This can avoid a full traversal for visible known controls; installed performance verification is still incomplete (see the [performance record](control-search-performance-20261010.md)). The initial property search, covered/offscreen controls and complete absence checks can still require a full bounded traversal. Older agents ignore the optional hint and retain their existing lookup behavior.

### 4.4 Actions: targets, check chain, activation and observe_after

The actions are `vm_click`, `vm_drag`, `vm_scroll`, `vm_set_value`, `vm_invoke`, `vm_type` and `vm_key`. They share these parameters:

- `observation_id`: pixels and indexes refer to this observation (see 4.1). `vm_click`, `vm_drag` and `vm_scroll` without it take raw guest screen pixels.
- `handle`, `pid` (`vm_type`, `vm_key`): the window that must receive the input; a group root is accepted and the input goes to the window of its group that is in the foreground.
- `activate` (default `true`): bring the target window to the foreground first when neither it nor a window of its group is there.
- `observe_after`: `none`, `screenshot`, `controls` or `both`; any other value is `invalid_argument`. The default is `screenshot` for `vm_click`, `vm_drag`, `vm_scroll`, `vm_set_value` and `vm_invoke`, and `none` for `vm_type` and `vm_key`.
- `settle_ms`: the wait before the after-action observation, default 300, 0 to 5000 (`invalid_argument` otherwise).

**Check chain.** A targeted action (one with `observation_id`, `index`, `handle` or `pid`) runs these steps under the input lock (see 4.5), in this order (session, freshness, target, activation, enabled, hit, integrity, execution), and stops at the first refusal. Activation may already have changed focus before a later check refuses; a dispatched mutation invalidates old coordinates even on failure:

1. **Window list.** The host asks the agent for the current windows, session and integrity facts. An agent that does not answer is `agent_required` (`next` suggests `vm_status`, `vm_install_agent`, or raw screen input without `observation_id` and `handle`).
2. **Session** (`session_unusable`, field `session`): the session is locked (`next` `call vm_unlock`); it is not the VM console session (`call vm_doctor; ...`); or keyboard input goes to the secure desktop (`answer the prompt first with vm_key or vm_click without observation_id (raw screen input), then retry`).
3. **Freshness** (`stale_observation`), see 4.1.
4. **Target resolution.** `index` is resolved to its node (`stale_element`, `invalid_argument`) and the target is the observation's tree window (the observed window, or for a whole-screen observation the foreground window whose tree was read); image pixels are mapped to the screen (`invalid_argument`) and the target is the observed window, or none for a whole-screen observation, which still receives revision and local image checks (see 4.1). `handle`/`pid` select a window: `no_window` when none matches (`call vm_windows and use a listed handle`), `ambiguous_target` with `handles` when `pid` alone matches several.
5. **Activation.** When neither the target nor a window of its group is in the foreground and `activate` is `true`, the host asks the agent to focus the target (`focus_window`: a minimized window is restored; `SetForegroundWindow`; if that does not work, the agent attaches to the foreground thread's input, injects a zero-distance mouse move and retries; it injects no key, so ribbon programs such as AutoCAD do not enter key-tip mode), lists the windows again and repeats step 3. If the target's group still does not hold the foreground, `activate_failed` with `foreground` (`{handle, pid, class, process, title}` of the window that does, or `null`) and `next` `act on handle <n> first` (adding `it belongs to <process> and probably is a dialog that blocks the target` when it is another process's). With `activate: false` the strict rule applies: a background target is `activate_failed` with `next` `call again with activate: true, or act on handle <n>`.
6. **Enabled** (`target_disabled`): the target is disabled, as an owner window is while its modal dialog runs. Fields `act_on` and `foreground` name the window to act on: the target's own foreground window (usually the dialog), or another process's foreground window that probably blocks it; when no window explains it, `next` is `call vm_observe (whole screen, controls: true) to find what blocks it`. For keyboard input, the receiving window of the group must also be enabled and not minimized.
7. **Hit** (pointer actions only). For an `index` the control is first re-located by runtime ID (`Locate`, see 4.1; `stale_element` when it vanished) and the point is the centre of its current rectangle. The agent then reports which top-level window is at the screen point (`window_at`). A window outside the target's group is `covered` with `window` (`{handle, class, process}`, described from `window_at` even when `vm_windows` does not list it, such as a shell overlay) and `next` `act on handle <n> first, or close it, ...` (listed) or `dismiss it with vm_key esc or close it, ...` (not listed). A point off screen is `invalid_argument`.
8. **Integrity** (`integrity_mismatch`, fields `integrity`, `agent_integrity`): the window that receives the input (the window hit, the keyboard target, or the control's window for `vm_set_value` and `vm_invoke`) runs at a higher integrity level than the agent, so Windows would drop injected input (UIPI). `next`: `launch the application without admin, or use vm_launch with admin:true`. An unknown level on either side never refuses.
9. **Execution.** UI Automation actions (`Locate` included) have a hard 12-second timeout on the host, and the agent's helper its own 10-second timeout; either is `target_not_responding` (`next` `call vm_observe on the window to see whether it answers; close it with vm_key alt+f4 or vm_exec if it hangs`). The check and the input are two steps, so a window that appears in between can still receive the input.
10. **observe_after.** After a successful action the host waits `settle_ms`, then performs the equivalent of `vm_observe` on the observed window (or the whole screen when the action had no window observation) with `screenshot` and `controls` as the mode says, and embeds the result as `after` with a new `observation_id`; the PNG, when included, is the result's first item. A failed action never observes. When the observation itself fails, the action's result is still returned with `after_error` (the error text) instead of `after`.

**Raw screen input.** `vm_click`, `vm_drag` and `vm_scroll` without `observation_id`, and `vm_key` without `handle` and `pid`, send their input to the Hyper-V console at screen pixels or to whatever has the focus, with no checks at all and without contacting the agent: the sign-in screen, the lock screen and UAC prompts are operated this way. `vm_type` without `handle`, `pid` and `index` asks the agent for the window list: when the agent answers and its session can receive injected input (not locked, not on the secure desktop, console session), the text goes to the foreground window through the agent after steps 6 and 8 on it (`no_window` when nothing is in the foreground); when the agent is unreachable, or its session is locked or on the secure desktop, ASCII text is typed on the Hyper-V keyboard with no checks, and text with a non-ASCII character is refused with `agent_required` (unreachable) or `session_unusable` (`next` `call vm_unlock, or type ASCII text`, field `session`).

**Result.** Every action returns its own fields (below) plus `window`, the window that received the input as `{handle, pid, class, process, title}` (`null` for raw screen input), and `after` (or `after_error`) when `observe_after` is not `none`:

```json
{"ok": true, "window": {"handle": 2818992, "pid": 4120, "class": "HwndWrapper[...]", "process": "acad.exe", "title": ""}, "after": {"observation_id": "o-1c88d0e4", "vm": "Win10", "captured_at": "2026-10-10T08:12:05Z", "window": {...}, "screenshot": {...}, "focused": {...}}}
```

Microsoft documents foreground activation restrictions in [SetForegroundWindow](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-setforegroundwindow), UIPI in [SendInput](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-sendinput), and DPI and invisible-border differences in [GetWindowRect](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-getwindowrect). The group rule, the check order and the codes are HyperHand interface choices.

### 4.5 Input serialisation

All mouse and keyboard operations in the host process are serialised by one lock, so concurrent tool calls never interleave their input events. This includes the keyboard steps of `vm_install_agent`. The MCP process holds a second input lock around the check chain and input of every action, around `vm_clipboard_set`, and around the final check and password typing of an unlock (see 3.5), so no other HyperHand tool sends input in between. The after-action observation runs after the lock is released.

### 4.6 Mouse: vm_click, vm_drag, vm_scroll

Mouse input goes through the VM's synthetic mouse (`Msvm_SyntheticMouse`) with absolute positions. If the device is not found the call fails with `Msvm_SyntheticMouse not found (VM not running?)`.

- `vm_click` targets `observation_id` with `x`, `y` (image pixels) or with `index` (the centre of the control's current rect, re-located by runtime ID); or `x`, `y` alone as raw screen pixels. `index` without `observation_id` is `invalid_argument`.
  - `button`: `left` (default), `right` or `middle`; anything else is `invalid_argument`.
  - `count`: 1 (default) to 3 clicks; anything else is `invalid_argument`.
  - `modifiers`: any of `ctrl`, `shift`, `alt`, without repeats (`invalid_argument` otherwise). They are pressed on the synthetic keyboard (`Msvm_Keyboard`) before the mouse moves and released in reverse order afterwards, even if the click fails.
  - The mouse moves to the point, waits 100 ms and clicks `count` times.
  - Result: `{"ok": true, "window": {...}, "after": {...}}`.
- `vm_drag` takes `from: {x, y}` and `to: {x, y}` (both image pixels of `observation_id`, or raw screen pixels without it) and `modifiers`. The check chain runs for the `from` point. It moves to `from`, waits 100 ms, presses the left button, moves to `to` in 8 equal steps 50 ms apart, waits 100 ms and releases the button. The button is released even if a move fails. Result: `{"ok": true, "window": {...}, "after": {...}}`.
- `vm_scroll` takes `x`, `y` (image pixels of `observation_id`, or raw screen pixels) and `delta_y` and/or `delta_x` in wheel notches (120 units each); both zero is `invalid_argument`. A positive `delta_y` scrolls up, a negative one down, through the synthetic mouse after a move and 100 ms wait. A positive `delta_x` scrolls right, a negative one left; the Hyper-V mouse has no horizontal wheel, so the agent sends it with `SendInput` at that screen point (`hscroll`), and `delta_x` is `agent_required` without the agent. Result: `{"ok": true, "window": {...}, "after": {...}}`.

### 4.7 Control actions: vm_set_value, vm_invoke

Both act on a control by its `index` in the tree of an observation taken with `controls: true` (`observation_id` required); the target window is the observation's tree window (for a whole-screen observation, the foreground window whose tree was read). The agent re-finds the element in that window by its runtime ID and performs one UI Automation pattern action in its helper process; the host bounds the call to 12 s.

- `vm_set_value` sets `value` through `ValuePattern.SetValue`.
- `vm_invoke` performs `action`: `Invoke` (`InvokePattern`), `Toggle` (`TogglePattern`), `Expand` or `Collapse` (`ExpandCollapsePattern`), `Select` (`SelectionItemPattern`), `ScrollIntoView` (`ScrollItemPattern`), or `ScrollUp`, `ScrollDown`, `ScrollLeft`, `ScrollRight` (`ScrollPattern`), case-insensitive. Use `vm_set_value` for `SetValue`.
- Refusals: an observation without a tree is `invalid_argument`; an element that no longer exists is `stale_element` (`call vm_observe with controls: true and use an index from its tree`); a control without the pattern is `unsupported_pattern` with `supported`, the control's executable action names from the agent's fresh capability check; use `vm_set_value` for `SetValue`, `vm_invoke` for other supported actions, or `vm_click` with this index. A hung window is `target_not_responding`.
- Result: `{"ok": true, "verified": true, "value": "abc", "state": {...}, "window": {...}, "after": {...}}`. `state` contains readable post-action semantic properties (as in observations); missing properties are unknown. `value` is the re-read `ValuePattern` text, `null` when unreadable, unsupported or a password. `verified` compares SetValue's requested value, Toggle's changed before/after state (including three-state controls), Expand's `expanded`, Collapse's `collapsed`, Select's `selected:true`, or ScrollIntoView's `offscreen:false`. It is `true` for a matching read-back, `false` for a readable non-matching result, and `null` when the required state is unknown. Invoke and Locate have no generic postcondition and remain `null`; this does not assert application-level completion. A value cut at 512 UTF-16 units cannot verify SetValue. The agent reads immediately and, if needed, polls for up to 250 ms; it never repeats the action. A later asynchronous change can occur after this verification window, so observe again before deciding to repeat an action.

Directional scrolling targets the scrollable container's `index` and requests one provider-defined small step, without pointer coordinates or wheel notch conversion. `state.horizontally_scrollable` and `state.vertically_scrollable` report readable axis support; `horizontal_scroll_percent` and `vertical_scroll_percent` are readable positions in `[0,100]` (absent for unsupported/unreadable axes). Actions for an explicitly unsupported axis are excluded from the control's `actions`; the agent checks support again before acting and refuses with `unsupported_pattern`. Both directions remain available at an axis boundary. For directional scrolling, `verified:true` requires a percentage change in the requested direction, `false` means no such observed movement (including an unchanged boundary), and `null` means a required before/after reading is unavailable. Inspect the returned percentage before repeating. `vm_scroll` retains its raw wheel behavior.

### 4.8 Keyboard: vm_key

`vm_key` presses a key or a `+`-separated combination through the VM's synthetic keyboard (`Msvm_Keyboard`).

- Pass exactly one of `keys` or `sequence`, an ordered array of up to 256 combinations such as `["ctrl+a", "backspace"]`; both or neither, more than 256, or an unknown name is `invalid_argument` before any key is sent.
- `handle` (optionally restricted by `pid`) or `pid` pins the window that must receive the keys (see 4.4): the keys go to the window of its group that is in the foreground. The window list is fetched again before every further combination and the sequence stops as soon as the pinned group loses the foreground, a dialog disables it, or a combination fails: after at least one combination was sent this is `partial_input` with `applied` and `total` (`call vm_observe to see the state before sending the rest`); a failure before the first one keeps its own code (`target_disabled`, `activate_failed`, `failed`, ...). Earlier input is not undone. Without a selector the keys go wherever the focus is, with no checks (see 4.4).
- Result: `{"ok": true, "combinations": 2, "window": {...}, "after": ...}`; `window` is `null` for untargeted input.
- Names are case-insensitive and surrounding spaces are ignored. Supported names:
  - Modifiers: `ctrl` / `control` / `control_l` / `control_r`, `shift` / `shift_l` / `shift_r`, `alt` / `alt_l` / `alt_r`, `win` / `super_l`.
  - Keys: `enter` / `return`, `esc` / `escape`, `tab`, `space`, `backspace`, `delete` / `del`, `insert`, `home`, `end`, `pageup` / `prior`, `pagedown` / `next`, `up`, `down`, `left`, `right`, `printscreen`, `scrolllock`, `pause`, `apps`, `capslock`, `numlock`, `f1` to `f20`.
  - Numeric keypad: `num0` to `num9` / `kp_0` to `kp_9`, `numenter` / `kp_enter`, `numdot` / `kp_decimal`, `numplus` / `kp_add`, `numminus` / `kp_subtract`, `nummul` / `kp_multiply`, `numdiv` / `kp_divide`. The Hyper-V keyboard takes a virtual-key code without an extended flag, so `numenter` is the same key as `enter`, and the left/right modifier aliases are the plain modifier.
  - Single letters `a`-`z` and digits `0`-`9`.
  - Punctuation keys `;` `=` / `equal` / `plus` (the `+`/`=` key; `+` itself is the separator), `,` / `comma`, `-` / `minus`, `.` / `period`, `/` / `slash`, `` ` ``, `[`, `\`, `]`, `'`.
- A single key is sent as one key press. A combination presses the keys in order and releases them in reverse order; keys already pressed are released even if a later press fails.

### 4.9 Text: vm_type

`vm_type` enters `text` into a window. It has one mode and never touches the guest clipboard.

- Target: `handle` (optionally restricted by `pid`) or `pid` selects the window (a group root is accepted; the text goes to the window of its group in the foreground); `observation_id` with `index` first clicks that control (re-located by runtime ID, with the full check chain, in the observation's tree window) to give it the focus; with none of them the text goes to the foreground window (see 4.4, raw screen input). `index` without `observation_id` (or vice versa), or `index` together with `handle`/`pid`, and an empty `text` are `invalid_argument`.
- With the agent, the text is injected into its session as Unicode key events (`type_keys`: UTF-16 `KEYEVENTF_UNICODE` / `VK_PACKET` through `SendInput`, including surrogate pairs). CRLF becomes one Enter; a lone CR or LF becomes Enter; Tab uses the Tab key. Enter and Tab can submit a command or move focus. Before every character the agent checks that its session is unlocked and not on the secure desktop and that the target is still the visible, enabled, non-minimized foreground window with the requested PID, and it refuses when a modifier or Windows key, or the key the character needs, is already held. The text must be valid UTF-8 of at most 16384 bytes.
- Without a target and without the agent (it does not answer), or when the agent's session is locked or on the secure desktop, ASCII text is typed on the Hyper-V keyboard (`TypeText`) into whatever has the focus; an IME in Chinese mode may swallow it. Text with a non-ASCII character is then `agent_required` or `session_unusable`.
- Result: `{"applied_chars": 8, "total_chars": 8, "window": {...}, "after": ...}`. With `index` the result also has `verified` and `value`: after typing, the control is read back (`Locate`); `value` is its current value and `verified` says whether it contains the typed text (without trailing newlines and tabs); both are `null` when the control has no readable value. A failure after some events were injected is `partial_input` with `applied_chars` and `total_chars` (`call vm_observe with controls: true to see what arrived; do not retype blindly`); a failure before any event is `failed` with `applied_chars: 0`. Earlier input is not undone.
- Windows UIPI blocks injection into higher-integrity applications (refused beforehand as `integrity_mismatch`, see 4.4). Successful injection does not prove the application consumed the text, and a foreground window does not establish which child control has the focus; without `index` nothing is read back. Observe the result (`observe_after`, or `vm_observe` with `controls: true`).

See Microsoft's [SendInput restrictions](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-sendinput) and [KEYBDINPUT Unicode semantics](https://learn.microsoft.com/en-us/windows/win32/api/winuser/ns-winuser-keybdinput).

## 5. Commands

### 5.1 vm_exec

`vm_exec` runs `command` in the guest as the logged-on user (the agent's user), waits for it to exit and returns:

```json
{"exit_code": 0, "stdout": "WIN10\r\n", "stderr": "", "timed_out": false}
```

A non-zero exit code is not a tool error. An empty `command` or an unknown `shell` is `invalid_argument`; an agent that does not answer is `agent_required`; errors from the agent (start failure, cancellation) are `failed`. `vm_exec` is not for starting GUI programs, whose process would belong to the request: use `vm_launch` (see 7.2).

### 5.2 Shells

- `shell` = `powershell` (default, also for an empty value): `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command` with `[Console]::OutputEncoding` set to UTF-8 before the command.
- `shell` = `cmd`: the command line is exactly `cmd.exe /d /s /c "<command>"` (AutoRun disabled, outer quotes stripped by `/s`).
- Any other value is `invalid_argument` (`shell: expected powershell or cmd, got "<value>"`).
- The process is started hidden, without a console window.

### 5.3 Working directory

`cwd` sets the working directory. When empty, the command inherits the agent's working directory.

### 5.4 Timeout

- `timeout_ms` defaults to 60000; zero or a negative value also means 60 s.
- For non-elevated commands, the shell is started suspended, assigned to a Windows Job Object and then resumed. On timeout the job's process tree is terminated, even if the shell has already exited, and the result has `timed_out: true`, with the exit code and whatever output was captured. Elevated commands use the separate path in 5.5.
- If the host disconnects while the command runs (see 1.4), the process tree is killed in the same way and no result is returned.
- After the process exits, the agent waits at most 5 s for its output pipes to close (for example when a child process still holds them).

### 5.5 Elevated execution (admin)

`admin` = `true` runs the command elevated. UAC may ask for consent or administrator credentials; waiting for it counts toward `timeout_ms`.

- A hidden, ordinary launcher process requests elevation of a one-shot worker. The launcher can be stopped even while UAC is waiting, leaving the agent available for the next request.
- The worker receives the command over a random, single-use local named pipe. It verifies the pipe server's PID and the original agent process's creation time. A late UAC approval after cancellation or timeout cannot retrieve the expired command.
- `cwd` defaults to the agent's working directory. `cmd` uses a temporary `%TEMP%\hh-admin-*.cmd` wrapper to select UTF-8 before a nested `cmd /d /s /c` parses the command. PowerShell uses a temporary `%TEMP%\hh-admin-*.ps1` file with a UTF-8 BOM, preserving support for long scripts. Both files are deleted afterwards. Explicit `exit N` is preserved; for PowerShell, a failed last statement otherwise returns exit code 1.
- The worker captures stdout/stderr and manages the command tree with a Windows Job Object. Pipe disconnection cancels the command; the original absolute deadline also terminates it. No second elevation prompt is needed to stop the command.
- A timeout before execution returns `timed_out: true`, exit code `-1` and empty output. Once execution starts, the agent allows up to 5 seconds for job cleanup and the captured result to return. Cancellation is reported as a tool error.

### 5.6 Output decoding

stdout and stderr are decoded independently.

- A leading UTF-8 BOM is removed.
- Valid UTF-8 is kept as is.
- Otherwise the bytes are decoded from the guest's OEM code page (the default for `cmd` output).

## 6. Files

`vm_push` and `vm_pull` need the agent (`agent_required` otherwise). Missing `host_path` or `guest_path` is `invalid_argument`. File contents are streamed in 1 MB buffers on both sides and are not held in memory.

### 6.1 vm_push

`vm_push` copies `host_path` (a file, or a directory recursively) into the guest at `guest_path`.

- Single file:
  - If `guest_path` ends in `\` or `/`, the file is written into that directory under its own name.
  - Otherwise `guest_path` is the destination file path.
- Directory: every file below `host_path` is written to `guest_path\<relative path>`. Only files are transferred; empty directories are not created. The host walk does not descend into symbolic links to directories.
- Parent directories in the guest are created as needed.
- Result: `{"copied": 12, "skipped": 30, "bytes": 48213760}` (files copied, unchanged files skipped, bytes sent).
- On failure the error's reason names the guest path and the field `copied` says how many files were copied before it; those files remain.

### 6.2 Unchanged-file skipping

Unless `force` is `true` (default `false`):

- The host asks the agent for the SHA-256 of every destination path (`hash_files`, in batches of 1000 paths). The agent reports an empty hash for a missing, directory or unreadable path.
- For each destination with a non-empty guest hash, the host computes the local file's SHA-256; if they match, the file is skipped.
- An agent that does not support `hash_files` (`unknown op`) receives every file.

With `force` = `true` no hashes are requested and every file is uploaded.

### 6.3 vm_pull

`vm_pull` copies `guest_path` (a file, or a directory recursively) to `host_path` on the host.

- The host first lists `guest_path` as a directory; if the listing fails, `guest_path` is treated as a file.
- Single file:
  - If `host_path` is an existing directory, or ends in `\` or `/`, the file is written into it under the guest file's name.
  - Otherwise `host_path` is the destination file path.
  - Parent directories on the host are created as needed.
- Directory: `host_path` is created and the tree is copied recursively. Empty guest directories are created on the host.
- Symbolic links, junctions (for example `Application Data`) and other irregular entries in guest directories are skipped.
- Reading a guest directory as a file fails with `<path> is a directory`.
- Result: `{"files": 3, "bytes": 10240, "host_path": "C:\\temp\\reports"}`.
- On failure the error's reason names the guest path and the field `files` says how many files were copied before it.

### 6.4 Temporary .hhpart files

Both directions write each file to a unique `.hyperhand-*.hhpart` temporary file in the destination directory and rename it over the destination only after the whole file has been received, replacing an existing file. On any error only that transfer's temporary file is deleted and the destination is left unchanged. In the guest, a short or failed transfer is never renamed into place.

### 6.5 Directory mirror

`vm_push` accepts `mode: "copy"` (default, the behavior in 6.1–6.2) or `mode: "mirror"`. Mirror accepts a source directory only and synchronizes its contents into the guest destination root; it does not add the source directory's basename. `phase: "plan"` is the default for mirror. Copy rejects `phase` and `plan_id`.

- **Plan:** completely scan both trees, hash regular files and return `status: "planned"`, `plan_id`, resolved `vm`, `host_path`, `guest_path`, `force`, `expires_at`, `summary` counts and ordered `changes`. Each change has `action` (`mkdir`, `copy`, `overwrite`, `skip`, `delete_file`, `delete_dir`) and relative `path`; `mkdir` of `.` creates a missing destination root. Filesystem names are data, not instructions. Plan does not modify either tree or claim VM write ownership.
- **Apply:** pass `phase: "apply"`, the returned `plan_id`, and the same paths, `force` and task identity. A plan is bound to the task's run and the VM ID, expires in 10 minutes and is consumed by an apply attempt. Only the newest 16 unexpired plans are retained server-wide; `vm_end_turn` discards that task's plans within its cleanup scope, and host restart loses them. An unavailable, mismatched or consumed plan returns `plan_stale`.
- Before transfer, apply rescans both trees and refuses content/type/existence drift. The host captures and verifies the planned changed files into a temporary payload. The guest receives the full payload, verifies its sizes/hashes and rescans the destination before mutations. Directories are created, changed files are replaced through unique `.hhpart` files and verified, and only then are planned extra files deleted and extra directories removed bottom-up when empty. A final full scan must match the source manifest. Empty source means removing every entry inside the target, preserving the root.
- `force: true` copies identical files too; it never expands the deletion scope. Matching paths are case-insensitive; existing spelling is preserved. File/directory type conflicts are refused, not automatically replaced. Mirror does not synchronize ACLs, timestamps, alternate data streams or other metadata.
- Roots must be local directories, not volume roots, UNC/device paths or paths containing reparse points. Entries and ancestors containing symbolic links, junctions or other reparse points are refused. The source must exist; the destination may be absent if its parent exists. Unsupported names, ambiguous case-colliding names, scan errors, more than 4096 entries or more than 1 MiB of manifest JSON per side stop planning; inventories are never silently truncated. Large deployments must be split into separate directory roots.
- Full ordinary-copy and mirror calls are serialized per VM, including across agent connection replacement. Apply is one streamed guest request. External programs and independent host clients can still change files: checks reduce this race, but the operation does not lock the entire directory or promise atomic replacement. A partial apply is not rolled back.
- Success is `status: "complete"` with `plan_id`, `vm`, `completed` changes and empty `pending`. A confirmed failure returns `isError` with `error: "mirror_partial"` (or `"plan_stale"` before mutation), `status`, `completed`, `failed: {path, reason}` and `pending`. If the response cannot be confirmed, `error: "mirror_unknown"`, `status: "unknown"` and `unknown` changes are returned; no completion is inferred. Create a new plan after a failure or uncertain outcome; never replay the consumed apply.
- Changed bytes use temporary disk space: one payload on the host, the received payload plus staged files on the guest, and a per-file replacement alongside the target. Handled errors/cancellation clean temporary files; abrupt process termination can leave temporary artifacts. A locked destination can fail without deleting later extras. Ordinary copy/pull keep their existing results. A guest without `mirror_scan`/`mirror_apply` returns `agent_outdated`; mirror never falls back to copy.

The same inventory limits also apply to the intermediate tree after copying but before deletion (the union of source entries and target-only entries). Planning refuses a union that would exceed those limits even if each side separately fits.

Because mirror apply plans are single-use, the `vm_push` tool no longer advertises `idempotentHint`, even though ordinary copy still has its previous semantics. Its static annotation remains destructive; mirror plan is read-only at execution time.

## 7. Clipboard, Launching, Waiting and Turn End

These tools need the agent, except `vm_end_turn`.

### 7.1 vm_clipboard_get and vm_clipboard_set

- `vm_clipboard_get` returns the guest clipboard as Unicode text: `{"text": "..."}`, an empty string when the clipboard holds no text.
- `vm_clipboard_set` empties the clipboard and puts `text` on it as Unicode text; it holds the input lock (see 4.5) meanwhile. Result: `{"ok": true}`.
- Opening the clipboard is retried 20 times, 50 ms apart; if it stays locked by another program the call fails with `clipboard busy`.
- No other tool uses the clipboard: `vm_type` injects key events.

### 7.2 vm_launch

Use `vm_apps` first when the program's path is unknown (see 7.5). Pass the returned `launch` object's `path`, `args` and `cwd`, plus the same `vm`.

`vm_launch` starts a program in the guest as a detached process and waits for its first visible top-level window.

- Parameters: `path` (required; `invalid_argument` when empty), `args` (array), `cwd`, `wait_window_ms` (default 60000; zero or negative also means 60 s) and `admin`.
- The agent starts `path` with `args` in `cwd` in a new process group, outside the job object `vm_exec` uses and without captured output, so it outlives the request (`launch`). With `admin: true` it is started elevated through the same worker as `vm_exec` (see 5.5).
- The host then lists the guest's windows every 300 ms until a visible top-level window of that PID appears: the first one with a title is preferred, else the first one. A splash screen or dialog of the process counts.
- Result: `{"pid": 4120, "handle": 197916, "title": "AutoCAD 2015 - Drawing1.dwg", "class": "Afx:400000:...", "elapsed_ms": 8120}`. Pass `handle` to `vm_observe` and the actions.
- No window within `wait_window_ms` is `no_window` with the field `pid` and `next` `call vm_windows later, or vm_observe without handle to see a splash screen or dialog`; the process keeps running.
- The VM is resolved once, so the polling cannot move to another VM.

### 7.3 vm_wait

`vm_wait` waits until a condition holds or `timeout_ms` (default 60000) expires. A normal timeout returns `"satisfied": false`; UI conditions also support one-shot checks and assertions.

- `kind` = `process_running`: a process named `name` exists.
- `kind` = `process_exit`: no process named `name` exists (true immediately if none was running).
- Process names are compared case-insensitively on the executable name; a directory part is ignored and `.exe` is added if missing, so `notepad`, `Notepad.exe` and `C:\Windows\notepad.exe` are equivalent.
- `kind` = `file_exists`: `path` exists (a file or a directory).
- These process/file conditions keep their guest-side 300 ms polling and result `{"satisfied": true, "elapsed_ms": 1200}`. An empty `name` or `path` for its kind is `invalid_argument`; UI-only arguments are refused for these kinds.
- `vm_end_turn` cancels pending waits: the cancelled call returns `failed` with reason `the wait was cancelled by vm_end_turn` and `elapsed_ms`. Cancellation by the MCP client is an error too. If the host disconnects, the wait stops (see 1.4).

UI conditions use host-side polling with a 300 ms interval between samples. They release the agent connection between samples and do not hold the input lock, so other tools can act while a wait is pending. They are read-only and do not activate windows or change controls.

- `window_exists`, `window_gone`, `window_foreground`: select a top-level window with `handle` or `pid` (or both). Multiple matches are `ambiguous_target`. There are no title selectors.
- `control_exists`, `control_gone`, `control_matches`: select a control with `observation_id` and `index`, or with `handle`/`pid` plus an exact `automation_id` and/or `control_name`. Both properties, when supplied, must match. Multiple matches are `ambiguous_target`; no first-match fallback is used.
- Observation selectors retain the observed window and control runtime identity. Property selectors can wait for a control that does not exist yet. VM lifecycle changes and reuse of the selected window handle for a different identity are `stale_observation`.
- `control_matches` requires one or more expected fields: `enabled`, `value`, or a `state` object using the control-state fields returned by `vm_observe`. All supplied fields must match exactly. `false`, zero and an empty `value` are real expectations, not omitted values. Other UI kinds refuse these predicates.
- Missing ValuePattern/state, password redaction, or incomplete trees cannot prove a match or disappearance. `last.unknown` explains why the result is inconclusive. Property lookup in a truncated tree cannot prove uniqueness even when a matching node is present. An observation's known runtime ID may still be found in a bounded tree, but missing nodes do not prove disappearance. Increase `max_depth`/`max_nodes` when appropriate (defaults 4/200, caps 10/1000).
- `check_only: true` takes one sample. `assert: true` makes an unsatisfied check or timeout return `assertion_failed` as an MCP error. Without `assert`, the result is successful tool execution with `satisfied: false`.
- Completed UI checks and waits contain `{satisfied, elapsed_ms, last}`; `last` carries the observed window/control identity, available actual state and uncertainty. Assertion failures and sampling errors include the same fields. Session/provider failures remain errors rather than being interpreted as a missing window/control. A timeout during an unfinished provider call is also an error. Argument and observation-validation errors can occur before the first sample.
- UI `timeout_ms` accepts 0 for the 60000 ms default, or 1..600000. Each guest read is bounded by 12 seconds and the remaining wait budget; a one-shot check is bounded by 12 seconds. These conditions use the existing generation-2 guest operations.

### 7.4 vm_end_turn

`vm_end_turn` ends an AI turn. It does not need the agent and changes nothing in the guest.

- It cancels this task's pending `vm_wait` calls, waits for its accepted calls, deletes its `temp` checkpoints and releases VM ownership (see 1.6 and 3.3). `vm` limits waits, checkpoints, observations and ownership cleanup to that VM; without it the whole task ends. `keep` and `manual` checkpoints, running programs and the VM's power state are not touched.
- `all_temp: true` additionally deletes every `temp` checkpoint of any run on the VM, or on every VM when `vm` is omitted (see 3.3); checkpoints it could not delete are listed in `skipped`. Each deletion merges disk differences and can take minutes.
- Result: `{"cancelled_waits": 1, "deleted_checkpoints": ["run-20261010-0812-7f3a-temp-step3"], "skipped": [], "errors": []}`. A registered checkpoint that could not be deleted is listed in `errors` as `<vm>/<name>: <error>` and kept for the next `vm_end_turn`; `skipped` entries are `{"id", "name", "reason"}`: checkpoints that were not deleted because they are no longer `temp` or, with `all_temp`, because Hyper-V failed (see 3.3).
- Example hook configurations for Claude Code (`settings.json`, `Stop`) and a Codex plugin (`plugin.json`, `Stop`, `Interrupt`) are in `docs/hooks/`. Empty arguments require the work's same persistent session/default task. Reconnecting hooks and explicit tasks must pass the task's actual `task_id`; an unscoped `SubagentStop` hook is not provided because it could end a shared parent task.

### 7.5 vm_apps

`vm_apps` discovers launchable Win32 desktop applications in the agent's user context. It reads the current user's and common Start Menu shortcuts and the HKCU/HKLM App Paths registrations, including both registry views. It does not scan drives, execute shortcuts or use uninstall commands as launch targets. Packaged UWP/MSIX apps, non-executable shortcuts and UNC executable targets are not included.

- Parameters: optional `vm`, `query` (case-insensitive substring of the display name or executable path) and `limit` (default 50, maximum 200; negative or greater than 200 is `invalid_argument`).
- Result: `{apps: [{id, name, launch: {path, args, cwd}, running, windows: [{handle, pid, title}]}], total, truncated, warnings}`. `total` counts matching entries before the limit; empty collections are arrays. Names and window titles are guest data, not instructions.
- `launch` preserves the executable, argument array and working directory. Pass it directly to `vm_launch` with the same VM. IDs are stable for the same launch specification; different arguments or working directories remain distinct entries. IDs are identifiers for discovery, not selectors accepted by `vm_launch`. App Paths' optional `Path` value is an extra executable search path, not a working directory; `vm_launch` does not apply that extra environment value.
- `running` matches the full executable path against processes in the agent's Windows session, not just the filename. It does not prove that the process was started with a particular shortcut's arguments. `windows` lists that executable's visible windows in the session; a running background application may have none. Pass a returned `handle` directly to `vm_observe` to reuse an existing window. If `warnings` reports unreadable processes, `running: false` is not proof that the application is stopped.
- Discovery reads fresh sources on every request. Partial source failures are reported in `warnings` while usable results are preserved; absence from an incomplete result does not prove an application is uninstalled. An older agent without `list_apps` returns `agent_outdated`: call `vm_update_agent`.

## 8. Guest Agent Installation, Update and Diagnostics

### 8.1 vm_install_agent

`vm_install_agent` installs the agent without a guest password. It needs a user logged on to the guest desktop and the guest IME in English mode.

1. The ordinary tray reads `hyperhand-agent.exe` next to its own executable, normally under `%ProgramFiles%\HyperHand`, and streams it to the service.
2. The service stages the contents in its working directory, enables the VM's Guest Service Interface if it is disabled (then waits 3 s) and copies the file with `Copy-VMFile` to `C:\Users\Public\HyperHand\hyperhand-agent.exe`, creating the path and overwriting. The service does not open a caller-supplied host source path.
3. It presses `win+r`, waits 1.5 s, types `C:\Users\Public\HyperHand\hyperhand-agent.exe install` on the synthetic keyboard and presses `enter`.
4. It pings the agent (see 8.4).

Because step 3 types blindly, a failure there is visible only on screen; the tool description advises checking with `vm_observe`.

### 8.2 Agent install command

`hyperhand-agent.exe install` runs in the user's session and needs no administrator rights. `vm_install_agent` runs it through the Run dialog; it can also be run by hand in the guest. Started without `install`, the agent runs for the current session only, without autostart.

- It force-terminates every other running `hyperhand-agent.exe` and waits 500 ms.
- Unless it already runs from there, it copies itself to `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe`, retrying up to 20 times 250 ms apart while the old image is still locked.
- It sets the `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` value `HyperHandAgent` to the quoted installed path, so the agent starts at logon.
- It starts the installed copy.
- On error it shows a message box and exits with code 1.

`hyperhand-agent.exe uninstall` reverses the install. It runs as the logged-on user without administrator rights. Run it in the guest directly, not through `vm_exec`, because it stops the agent that would be executing it.

- It stops every other running `hyperhand-agent.exe`.
- It deletes the `HyperHandAgent` value under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`.
- It deletes `%LOCALAPPDATA%\HyperHand` and `C:\Users\Public\HyperHand`. The file of a running program cannot be deleted but can be renamed on its volume, so before deleting the folder it is running from it moves its own executable to `%TEMP%\hyperhand-agent-uninstalled-<pid>.exe`, where it remains (or to the parent of that folder if `%TEMP%` is on another volume), and leaves the folder as its working directory. No script or helper process is started.
- It shows a message box listing what was removed.

### 8.3 Single instance and startup

- The agent uses the mutex `HyperHandAgent`; a second instance exits immediately.
- At startup it marks itself DPI-aware and deletes a leftover `hyperhand-agent.exe.old` next to itself, retrying for up to 10 s while the previous version is still exiting.

### 8.4 Agent readiness check

`vm_install_agent` and `vm_update_agent` ping the agent every 2 s for up to 30 s, each ping limited to 5 s.

- Success returns `{"agent": {"version": "0.3.0", "hostname": "WIN10", "user": "WIN10\\tester", "protocol": 2}}`. An agent that answers with an older protocol is refused with `agent_outdated` (see 8.6).
- Otherwise the call is refused with `agent_required`, reason `the agent did not answer within 30 s: <last error>` and `next` `look at the screen with vm_observe, then call vm_install_agent again`.

### 8.5 vm_update_agent

`vm_update_agent` replaces the running agent with the installed `hyperhand-agent.exe` next to the host executable. The agent must already be running (`agent_required` otherwise).

1. The host sends the whole executable as the `update_agent` payload.
2. The agent writes `<exe>.new`, deletes any `<exe>.old`, renames the running `<exe>` to `<exe>.old` and `<exe>.new` to `<exe>`. If the last rename fails, the old file is renamed back and the update fails. An empty payload fails with `empty payload`.
3. After a successful update the agent sends its response, releases its mutex, starts the new executable with the same arguments and exits. It restarts even if the response could not be sent.
4. The host closes its connection, waits 2 s and pings the new agent (see 8.4).

The update replaces the file the agent is running from; the HKCU Run entry is unchanged.

### 8.6 Protocol version

The guest protocol has a generation number, `protocol` 2 in this version, reported by the agent's `ping`. There is no compatibility path for older agents:

- Every new connection to an agent is checked once: before the first op other than `ping` or `update_agent`, the host pings the agent and refuses an older `protocol` with `agent_outdated` (fields `agent_protocol`, `host_protocol`; `next` `call vm_update_agent`) for that op and every later one on the connection. `vm_status` (which only pings) and `vm_update_agent` therefore still work on an old agent, so that it can be reported and replaced; `vm_start`, `vm_install_agent` and `vm_update_agent` additionally check the protocol of the agent that answers their readiness ping.
- An agent that answers `unknown op` to a request is reported as `agent_outdated` with the same `next`.
- `vm_status` and `vm_doctor` report the protocol without refusing.

### 8.7 vm_doctor

`vm_doctor` diagnoses the host and the selected VM. It is read-only and changes nothing. Result: `{"checks": [{"check": "host.service", "status": "ok", "detail": "HyperHandService is running"}, ...]}`; `status` is `ok`, `warn` or `fail`, and every `warn` or `fail` has a `suggestion` naming a command or tool call.

| Check | Looks at | fail / warn |
|---|---|---|
| `host.service` | `HyperHandService` in the service control manager | fail: not installed or not running (`run hyperhand.exe install (elevated)`, `Start-Service HyperHandService`); warn: access denied when querying |
| `host.mcp` | this process answered the call; `detail` names the URL it serves | always ok |
| `host.pipe` | dialling `\\.\pipe\HyperHandService` (2 s) | fail: not reachable |
| `host.hvsocket` | the registry registration of the Hyper-V socket service ID (see 1.2) | fail: not registered |
| `host.enhanced_session` | `EnhancedMode` under `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization` | warn: the host allows enhanced session mode |
| `vm.power` | the selected VM | fail: the VM cannot be resolved (see 2.3) or is not running; the guest checks are then skipped |
| `guest.agent` | a 5 s ping: version, protocol, hostname, user | fail: no answer (`call vm_start ...; ... vm_install_agent`), or protocol older than the host's (`call vm_update_agent`) |
| `guest.session` | `session_state` | fail: not available, or not the console session; warn: a UAC prompt is open, or the session is locked |
| `guest.integrity` | the agent's integrity level from `list_windows` | warn: the window list or the level is unavailable |
## 9. Host Tray

### 9.1 Running

`hyperhand.exe [-port <n>]` starts the tray and the MCP server. The port is `-port` if given, else the port saved in the settings window, else 8770.

- The tray starts without requesting elevation. Hyper-V operations require the separately installed and running `HyperHandService`.
- A second instance exits immediately (mutex `Local\HyperHandTray`).
- It listens on `127.0.0.1:<port>`; protected machine configuration is performed only by the installer.
- The tray icon's tooltip is `HyperHand`, or says that the MCP server is not running or the background service is unavailable. A left click, or **Settings...** in its right-click menu, opens the settings window; the menu also has **Restart** and **Quit**.
- The icon is added at once, without waiting for the taskbar. If the taskbar does not accept it yet (for example at logon, while Explorer is still starting), the tray keeps running and retries every 5 seconds and whenever the taskbar is created; it adds the icon again whenever Explorer recreates the taskbar.
- The settings window shows the MCP endpoint (with **Copy**), the port, the server status, the background service status, the version and **Open log folder**. **Change port** and **Apply and restart** save a new port (1024 to 65535, which must be free) in `%LOCALAPPDATA%\HyperHand\settings.json` and restart the tray; this is unavailable when `-port` was given. Its **Virtual machines** list shows the Hyper-V VMs with their state (running, off, saved, paused), whether an unlock password is stored and whether the console opens when started, refreshed every 5 seconds. For the selected VM: **Open console**, which brings an open Virtual Machine Connection window for that VM to the front (a visible window of `<system directory>\vmconnect.exe` whose title, such as `Win10 on localhost - Virtual Machine Connection` or a localized `localhost 上的 Win10 - 虚拟机连接`, names exactly that VM; a title in another layout is not matched, and a new console is opened) or else runs the `HyperHand Console` task with the VM name (see 9.2), after checking that the name is an existing VM's exact name without quotes, line breaks or a trailing backslash; **Open console when started**, a per-VM check box saved in `%LOCALAPPDATA%\HyperHand\settings.json` (see 3.5); the window warns when the host allows enhanced session mode (`EnhancedMode` under `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization`); **Set unlock password...**, which asks in the Windows credential dialog for the password or PIN the guest lock screen asks for and stores it in Windows Credential Manager for the current user (non-ASCII passwords are refused), and **Clear unlock password**. The user name in the dialog is only a note. Restart replaces only this ordinary tray/MCP process, without UAC, preserving the selected port. Active MCP connections and requests are interrupted; the service, guest agent and VMs are not restarted.

### 9.2 install

`hyperhand.exe install` installs or updates the host service and ordinary tray. Installation and uninstallation run in the elevated `hyperhand.exe` itself, through the Windows service control manager, Task Scheduler, local group and security APIs; no script is run.

- If not elevated, it relaunches itself elevated with `install`.
- It installs `hyperhand.exe` and `hyperhand-agent.exe` under `%ProgramFiles%\HyperHand`.
- It records the installing user's SID in the protected `%ProgramData%\HyperHand\config.json` and provides a service-writable `service-data` directory beneath it.
- It stops an installed service and waits for its process to exit before replacing the executables, and ends installed tray processes.
- It configures automatic Windows service `HyperHandService` under `NT SERVICE\HyperHandService` and adds that service account to Hyper-V Administrators. It does not add the human user or use LocalSystem.
- It creates or replaces the `HyperHand` logon task for the installing user with least privilege and the installed executable. Reinstalling migrates the older highest-privilege task.
- It creates or replaces the `HyperHand Console` task for the installing user: no trigger, highest privileges, one action `<system directory>\vmconnect.exe localhost "$(Arg0)"` (the system directory from `GetSystemDirectory`, not an environment variable). An existing task of that name must belong to the installing user and run `vmconnect.exe`, or installation stops.
- It registers the fixed guest socket service (see 1.2) and starts the host components. Repeating `install` deploys an update; normal tray restart does not update binaries.
- On error it logs the error and shows it in a message box.

`hyperhand.exe uninstall` reverses the install.

- If not elevated, it relaunches itself elevated with `uninstall` (one UAC prompt).
- It stops the installed tray and service and removes `HyperHandService`, its Hyper-V Administrators membership, the `HyperHand` logon task, the `HyperHand Console` task and the fixed guest socket registration.
- Run from another copy, it removes the installed host and agent executables itself, only if their hashes still match. Run as the installed `hyperhand.exe`, which cannot delete its own file, it copies itself to the administrators-only `%ProgramData%\HyperHand\uninstall-cleanup.exe`, which waits for it to exit and then does the same. That copy cannot delete itself either: it and the then empty data directory are deleted at the next restart (`MoveFileEx` with `MOVEFILE_DELAY_UNTIL_REBOOT`); a later `install` deletes a leftover copy that is not running. A cleanup failure is written to `%ProgramData%\HyperHand\uninstall-error.log`. Only empty directories are removed.
- User logs, guest files and nonempty service working data are preserved. If working data remains, the owner configuration is retained for reinstallation; otherwise the configuration and empty data directory are removed.
- It does not uninstall guest agents or change host UAC policy.

### 9.3 Log

The tray appends its log to `%LOCALAPPDATA%\HyperHand\hyperhand.log` (the directory is created if needed). The service writes errors to `%ProgramData%\HyperHand\service-data\broker.log`, with an approximately 1 MiB size limit. It also attempts to report errors to the Windows Application event log under `HyperHandService`; the file log remains available if that event source is unavailable.

## 10. Wire Protocol

The host and agent exchange frames over the Hyper-V socket, tunneled through the local broker pipe. 10.1 to 10.3 describe the guest protocol; 10.4 lists the broker's checkpoint operations.

### 10.1 Frame format

```
uint32 header length | uint64 payload length | header JSON | payload bytes
```

- Both lengths are big-endian.
- A request header is `{"op": "<op>", "args": {...}}` (`args` omitted when empty).
- A response header is `{"error": "<message>", "result": {...}}`; both fields are omitted when empty. A response with an error carries no payload.
- Payloads carry file contents and the agent executable; they are empty otherwise. File payloads are streamed rather than buffered.
- If a header is not valid JSON, the receiver skips its payload so the stream stays in sync.

### 10.2 Operations

| Op | Args | Result / payload |
|---|---|---|
| `ping` | none | `{version, protocol, hostname, user}`; `protocol` is the generation the agent speaks (see 8.6) |
| `exec` | `{command, shell, cwd, timeout_ms, admin}` | `{exit_code, stdout, stderr, timed_out}` |
| `list_apps` | `{query, limit}` | `{apps: [{id, name, launch: {path, args, cwd}, running, windows: [{handle, pid, title}]}], total, truncated, warnings}` (see 7.5) |
| `write_file` | `{path}` + payload (file contents) | none |
| `read_file` | `{path}` | payload (file contents) |
| `list_dir` | `{path}` | `{entries: [{name, is_dir}]}`, links skipped |
| `hash_files` | `{paths}` | `{hashes}`, lowercase hex SHA-256 in order, `""` when unavailable |
| `mirror_scan` | `{path}` | `{exists, entries: [{path, kind, size, sha256}]}`; strict bounded directory manifest (6.5) |
| `mirror_apply` | `{path, source, target, force}` plus payload | `{status, completed, failed?, pending}`; source/target are manifests. Payload concatenates `copy`/`overwrite` file bytes in the deterministic diff order; streamed to temporary storage before target changes (6.5) |
| `type_keys` | `{text, handle, pid}` | `{events}`, the number of injected input events |
| `list_controls` | `{handle, pid, max_depth, max_nodes}` | `{nodes: [{index, parent, depth, name, control_type, automation_id, class_name, pid, enabled, offscreen, rect, runtime_id, patterns, actions, state, value, has_value, focused}], truncated, truncation, focused, selected_text}`, a bounded UIA control-view snapshot; `control_type` is the numeric UIA ID (the host renders names), `focused` the index of the focused node or -1 |
| `control_action` | `{handle, pid, runtime_id, action, value}` | `{rect, value, has_value, state, verified}`: one UIA pattern action (`SetValue`, `Invoke`, `Toggle`, `Expand`, `Collapse`, `Select`, `ScrollIntoView`, `ScrollUp`, `ScrollDown`, `ScrollLeft`, `ScrollRight`) on the element with that runtime ID, or `Locate`, which performs nothing; then the element's current `rect`, re-read value and semantic state, with verification as in 4.7; errors `element not found` and `unsupported pattern: <action>; supported: <list>` |
| `launch` | `{path, args, cwd, admin}` | `{pid}`; the process is started detached, outside the `exec` job object |
| `hscroll` | `{x, y, delta}` | none; horizontal wheel notches at a screen point through `SendInput` |
| `clipboard_get` | none | `{text}` |
| `clipboard_set` | `{text}` | none |
| `focus_window` | `{title, handle}` | `{text, handle}`, the focused window's title and handle; the host passes a handle (used by the actions' activation step) |
| `window_at` | `{x, y}` | `{handle, class, pid, process}`, the top-level window a click at that screen point reaches; handle 0 off screen |
| `list_windows` | `{focused}` (optional) | `{windows: [{handle, title, class, pid, process, rect, enabled, foreground, minimized, owner, modal, group_root, integrity}], foreground, focused, session, agent_integrity}`; `focused` is the UIA focused control (see 4.2), read through a helper process only when the args ask for it (`vm_windows`, `vm_find_controls`, `vm_observe`), `session` the `session_state` result, `agent_integrity` the agent's own integrity level |
| `wait` | `{kind, name, path, timeout_ms}` | `{satisfied}` |
| `session_state` | none | `{locked, console, secure_desktop, logonui, consent}` for the agent's session (see 3.5) |
| `update_agent` | payload (new executable) | none; the agent then restarts (see 8.5) |

The former `screenshot` op is gone: all screenshots are taken on the host.

### 10.3 Error behaviour

- An unknown op returns the error `unknown op "<op>"`.
- A handler that panics returns the error `<op> panicked: <value>`; the agent keeps running.
- For `write_file`, the agent always consumes the full payload, so a failed write (for example an invalid path) is reported as an error without breaking the connection. Only a failed read from the socket ends the connection.

### 10.4 Broker checkpoint operations

The tray requests checkpoint operations from `HyperHandService` over the broker pipe (see 1.1) in the same frame format; a request header is `{"op", "vm", "name", "id", "subtree"}` (unused fields omitted), a response `{"error", "result"}`.

| Op | Request fields | Result |
|---|---|---|
| `checkpoints` | `{vm}` | `{checkpoint_type, current_parent_id, checkpoints: [{id, name, parent_id, created_at, kind, state}]}`, the tree in creation order (`Get-VMSnapshot` sorted by `CreationTime`) |
| `checkpoint_create` | `{vm, name}` | the created checkpoint `{id, name, parent_id, created_at, kind, state}` (`Checkpoint-VM -Passthru`) |
| `checkpoint_restore` | `{vm, id}` | none (`Restore-VMSnapshot`) |
| `checkpoint_delete` | `{vm, id, subtree}` | none (`DestroySnapshot`, or `DestroySnapshotTree` with `subtree`; waits for the job up to 15 minutes) |
| `checkpoint_rename` | `{vm, id, name}` | none (`Rename-VMSnapshot`) |

The service rejects `checkpoint_restore`, `checkpoint_delete` and `checkpoint_rename` whose `id` is not a GUID in `8-4-4-4-12` form, and `checkpoint_create` or `checkpoint_rename` without `name`. `checkpoint_create`, `checkpoint_restore`, `checkpoint_delete` and `checkpoint_rename` have a 15-minute timeout like `copy`; `checkpoints` has one minute. A missing `id` fails with `checkpoint not found: <id>`, which the tools report as `no_checkpoint`.
