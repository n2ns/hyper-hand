# HyperHand Features

This document describes the behaviour of HyperHand as implemented: the host tray program, the MCP tools it exposes, the guest agent and the protocol between them.

The exact behavior is split by chapter, one file each. Read the chapter of the tool or area you work on; section numbers are global (8.6 is in chapter 8).

- [1. Architecture](features/architecture.md): 1.1 Host side, 1.2 Hyper-V socket, 1.3 Connections and request ordering, 1.4 Cancellation, 1.5 Agent service loop, 1.6 Task ownership
- [2. Results, Errors and VM Selection](features/results-errors.md): 2.1 Result format, 2.2 Error object and codes, 2.3 VM selection
- [3. VM and Checkpoint Tools](features/vm-checkpoints.md): 3.1 vm_list, 3.2 vm_start, vm_shutdown, vm_turn_off, vm_save and vm_pause, 3.3 Checkpoints, 3.4 PowerShell-based operations, 3.5 Session readiness and unlock: vm_start, vm_status, vm_unlock
- [4. Observation and Input](features/observation-input.md): 4.1 Observations, 4.2 vm_windows, 4.3 vm_observe, 4.4 Actions: targets, check chain, activation and observe_after, 4.5 Input serialisation, 4.6 Mouse: vm_click, vm_drag, vm_scroll, 4.7 Control actions: vm_set_value, vm_invoke, 4.8 Keyboard: vm_key, 4.9 Text: vm_type
- [5. Commands](features/commands.md): 5.1 vm_exec, 5.2 Shells, 5.3 Working directory, 5.4 Timeout, 5.5 Elevated execution (admin), 5.6 Output decoding, 5.7 Background jobs: vm_exec background, vm_job
- [6. Files](features/files.md): 6.1 vm_push, 6.2 Unchanged-file skipping, 6.3 vm_pull, 6.4 Temporary .hhpart files, 6.5 Directory mirror, 6.6 vm_file_info
- [7. Clipboard, Launching, Waiting, Turn End, Batches and Evidence](features/clipboard-launch-wait.md): 7.1 vm_clipboard_get and vm_clipboard_set, 7.2 vm_launch, 7.3 vm_wait, 7.4 vm_end_turn, 7.5 vm_apps, 7.6 vm_batch, 7.7 vm_evidence
- [8. Guest Agent Installation, Update and Diagnostics](features/agent.md): 8.1 vm_install_agent, 8.2 Agent install command, 8.3 Single instance and startup, 8.4 Agent readiness check, 8.5 vm_update_agent, 8.6 Protocol version, 8.7 vm_doctor
- [9. Host Tray](features/host-tray.md): 9.1 Running, 9.2 install, 9.3 Log
- [10. Wire Protocol](features/wire-protocol.md): 10.1 Frame format, 10.2 Operations, 10.3 Error behaviour, 10.4 Broker checkpoint operations

## Tools

Every tool except `vm_list` requires `vm`; see 2.3.

| Tool | What it does |
|---|---|
| `vm_list`, `vm_start` | List VMs with their state and the task's `run_id`; start and wait until the desktop is usable (unlocking it with the stored password) |
| `vm_shutdown`, `vm_turn_off` | Shut the guest down normally and wait until the VM is off (fails, without turning it off, if a program blocks shutdown); turn the VM off immediately, like pulling the plug |
| `vm_save`, `vm_pause` | Save the VM's memory to disk and stop it (saved), or freeze it in memory (paused); programs and unsaved work survive, and `vm_start` resumes it and waits until the desktop is usable |
| `vm_status`, `vm_unlock`, `vm_doctor` | Report power state, agent, session lock state and whether an unlock password is stored; unlock a locked session with the stored password; run read-only host and guest checks with a suggestion per problem |
| `vm_checkpoints`, `vm_checkpoint` | List the checkpoint tree (`id`, `name`, `parent`, `type`, `kind`, `state`, `current`, `children`, plus the VM's `checkpoint_type` and `current_parent`); create one named `<run_id>-temp-<label>` (or `-keep-` with `keep: true`) and return its `id` |
| `vm_restore` | Restore a checkpoint by `id` (or by `name` when it is unique) and start the VM unless `start` is false; `save_current: true` first saves the current state as a `temp` checkpoint |
| `vm_checkpoint_keep`, `vm_checkpoint_delete` | Rename a `temp` checkpoint to `keep` so that `vm_end_turn` leaves it alone; delete a checkpoint by `id` (`manual` ones by `id` only), with `subtree: true` its whole branch, waiting for Hyper-V to merge the disks |
| `vm_windows` | List visible windows: handle, title, class, process, rect, enabled, foreground, owner, `group_root`, `integrity`; plus the foreground handle, the focused control and the session state |
| `vm_observe` | The observation entry point: PNG of the screen or of one window (`handle`), the focused control, `selected_text` and with `controls: true` the indexed control tree (`diff_from` for changes only); returns an `observation_id` |
| `vm_find_controls` | Bounded control search by AutomationId, name or type; returns actionable indexes and explicit unique/multiple/not-found/incomplete status |
| `vm_click`, `vm_drag`, `vm_scroll` | Mouse at image pixels of an `observation_id`, or at a control `index` (`vm_click`); `button`, `count`, `modifiers`, `delta_y`/`delta_x`; without an observation, raw screen pixels |
| `vm_set_value`, `vm_invoke` | Set a control's value, Invoke, Toggle, Expand, Collapse, Select, ScrollIntoView, or ScrollUp/Down/Left/Right by its observation `index`; returns actual `state` and read-back `verified` (unknown outcomes remain null) |
| `vm_type`, `vm_key` | Type Unicode text into a window (`handle`/`pid`, or an observation `index` to focus first) as key events, never through the clipboard; press one key combination or a `sequence` (numeric keypad and X11-style names included) |
| `vm_apps` | Find launchable desktop applications by name or executable path; return stable IDs, `launch` arguments for `vm_launch`, running state and visible window handles |
| `vm_launch` | Start a program detached and return its `pid` and first window's `handle`, `title` and `class` |
| `vm_exec` | Run a command to completion in the guest; `shell`, `cwd`, `timeout_ms`, `admin`; `background: true` starts it as a job and returns its `id` at once |
| `vm_job` | Read a background job's state and output incrementally (`stdout_offset`/`stderr_offset`, `wait_ms`), cancel its process tree, or list the agent's jobs; jobs survive reconnects and host restarts |
| `vm_push`, `vm_pull` | Copy files or directories host to guest and back; `vm_push` skips unchanged files unless `force` is true. `mode: mirror` synchronizes exact directory contents with `phase: plan` then `phase: apply` and the returned `plan_id` |
| `vm_file_info` | Check up to 64 guest paths in one call (environment variables such as `%APPDATA%` expanded): existence, file or directory, size, SHA-256, PE `ProductVersion` and modification time; read-only |
| `vm_clipboard_get`, `vm_clipboard_set` | Read or write the guest clipboard |
| `vm_wait` | Wait for a process, file or UI condition; check or assert window/control state |
| `vm_batch` | Run up to 64 tool steps in order in one call, passing values of earlier results (`${0.handle}`) and checking each result with assertions; stops at the first failure and reports the last completed step |
| `vm_evidence` | Export this task's calls on a VM (arguments, results, assertions, screenshots, file hashes, host and agent versions) as a reviewable zip on the host, with the stored unlock password and given strings redacted |
| `vm_end_turn` | End this task's work: cancel its waits, clean up its temporary checkpoints and release its VM ownership; `vm` limits cleanup to one VM |
| `vm_install_agent`, `vm_update_agent` | Install or replace the guest agent |
