# 10. Wire Protocol

Part of [HyperHand features](../features.md). Section numbers such as 8.6 refer to the chapters listed there.

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
| `exec` | `{command, shell, cwd, timeout_ms, admin}` | `{exit_code, stdout, stderr, timed_out, elevation_pending}`; `elevation_pending` (omitted when false): with `admin`, the timeout came before the elevated worker received the command, so it did not run (see 5.5) |
| `job_start` | `{command, shell, cwd, timeout_ms}` | `{id, pid, command, shell, cwd, state, exit_code, started_at, ended_at, elapsed_ms, timeout_ms, stdout_bytes, stderr_bytes, stdout_dropped, stderr_dropped}`; starts a background job (5.7); `admin` is refused |
| `job_read` | `{id, stdout_offset, stderr_offset, max_bytes}` | the job's fields plus `{stdout, stderr, stdout_next, stderr_next}`; error `no such job "<id>": ...` for an unknown ID |
| `job_cancel` | `{id}` | the job's fields after its shell exited; terminates its Job Object |
| `job_list` | none | `{jobs: [...]}`, the job fields of every kept job |
| `list_apps` | `{query, limit}` | `{apps: [{id, name, launch: {path, args, cwd}, running, windows: [{handle, pid, title}]}], total, truncated, warnings}` (see 7.5) |
| `write_file` | `{path}` + payload (file contents) | none |
| `read_file` | `{path}` | payload (file contents) |
| `list_dir` | `{path}` | `{entries: [{name, is_dir}]}`, links skipped |
| `hash_files` | `{paths}` | `{hashes}`, lowercase hex SHA-256 in order, `""` when unavailable |
| `file_info` | `{paths}` (1 to 64) | `{files: [{path, resolved, exists, type, size, sha256, version, modified, error}]}`, fields omitted when they do not apply (see 6.6) |
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
