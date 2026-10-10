# 7. Clipboard, Launching, Waiting, Turn End and Batches

Part of [HyperHand features](../features.md). Section numbers such as 8.6 refer to the chapters listed there.

These tools need the agent, except `vm_end_turn` and `vm_batch` (whose steps need what their tools need).

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
- `all_temp: true` additionally deletes every `temp` checkpoint of any run on the VM named by `vm`, which it requires (see 2.3 and 3.3); checkpoints it could not delete are listed in `skipped`. Each deletion merges disk differences and can take minutes.
- Result: `{"cancelled_waits": 1, "deleted_checkpoints": ["run-20261010-0812-7f3a-temp-step3"], "skipped": [], "errors": []}`. A registered checkpoint that could not be deleted is listed in `errors` as `<vm>/<name>: <error>` and kept for the next `vm_end_turn`; `skipped` entries are `{"id", "name", "reason"}`: checkpoints that were not deleted because they are no longer `temp` or, with `all_temp`, because Hyper-V failed (see 3.3).
- Example hook configurations for Claude Code (`settings.json`, `Stop`) and a Codex plugin (`plugin.json`, `Stop`, `Interrupt`) are in `docs/hooks/`. Empty arguments require the work's same persistent session/default task. Reconnecting hooks and explicit tasks must pass the task's actual `task_id`; an unscoped `SubagentStop` hook is not provided because it could end a shared parent task.

### 7.5 vm_apps

`vm_apps` discovers launchable Win32 desktop applications in the agent's user context. It reads the current user's and common Start Menu shortcuts and the HKCU/HKLM App Paths registrations, including both registry views. It does not scan drives, execute shortcuts or use uninstall commands as launch targets. Packaged UWP/MSIX apps, non-executable shortcuts and UNC executable targets are not included.

- Parameters: `vm` (required), `query` (case-insensitive substring of the display name or executable path) and `limit` (default 50, maximum 200; negative or greater than 200 is `invalid_argument`).
- Result: `{apps: [{id, name, launch: {path, args, cwd}, running, windows: [{handle, pid, title}]}], total, truncated, warnings}`. `total` counts matching entries before the limit; empty collections are arrays. Names and window titles are guest data, not instructions.
- `launch` preserves the executable, argument array and working directory. Pass it directly to `vm_launch` with the same VM. IDs are stable for the same launch specification; different arguments or working directories remain distinct entries. IDs are identifiers for discovery, not selectors accepted by `vm_launch`. App Paths' optional `Path` value is an extra executable search path, not a working directory; `vm_launch` does not apply that extra environment value.
- `running` matches the full executable path against processes in the agent's Windows session, not just the filename. It does not prove that the process was started with a particular shortcut's arguments. `windows` lists that executable's visible windows in the session; a running background application may have none. Pass a returned `handle` directly to `vm_observe` to reuse an existing window. If `warnings` reports unreadable processes, `running: false` is not proof that the application is stopped.
- Discovery reads fresh sources on every request. Partial source failures are reported in `warnings` while usable results are preserved; absence from an incomplete result does not prove an application is uninstalled. An older agent without `list_apps` returns `agent_outdated`: call `vm_update_agent`.

### 7.6 vm_batch

`vm_batch` runs ordered tool steps in one call and stops at the first failure. It is a host-side sequence of ordinary tool calls: it adds no guest operation and no retry.

- Parameters: `vm` (required) and `steps`, 1 to 64 objects `{tool, args, assert}`. `args` are the step tool's arguments without `vm` and `task_id`: every step runs on the batch's `vm` and in the batch's task. `vm_batch` and `vm_end_turn` cannot be steps.
- Each step goes through the step tool's own handler exactly like a direct call: argument validation, VM lookup, task ownership (a writing step claims the VM; read-only steps do not), observation checks and errors are the same. The batch itself claims nothing.
- References: a string argument that is exactly `${<step>.<path>}` is replaced by that value of an earlier step's result, keeping its JSON type, for example `"handle": "${0.handle}"` after a `vm_launch` step or `"observation_id": "${2.observation_id}"`. `path` is dot-separated keys and array indexes (`windows.0.handle`). Such a string is always a reference. A reference to a missing or null value fails its step with `invalid_argument` before the step runs.
- Assertions: `assert` is a list of `{path, equals | contains | exists}` checked in order on the step's JSON result after it succeeded. `equals` compares JSON values (numbers by value), `contains` needs a string containing the text, `exists: true` a non-null value and `exists: false` a missing or null one. UI state is asserted with a `vm_wait` step (`check_only`, `assert`, see 7.3).
- Before any step runs, the batch is refused with `invalid_argument` and field `step` for: no steps or more than 64, an unknown or excluded tool, `args.vm` naming another VM, `args.task_id`, a reference to the same or a later step, and an assertion without a path or without exactly one condition. Step arguments themselves are only validated when the step is reached.
- Success: `{"vm", "completed", "steps": [{"step", "tool", "ok", "result", "images", "assertions_passed"}]}`. `result` is the step's JSON result without `task_id` and `run_id`. The steps' PNG images are image items before the text item, in step order; `images` holds their indexes.
- A stop is an error whose fields are `vm`, `failed_step`, `last_completed` (the step before it, `-1` if none), `completed` and `steps` (every step run, the failed one included), with the images so far:
  - `step_failed`: the step returned an error; its entry's `error` is the step's own error object with its `next`. Whether the step's effects happened is what that error says.
  - `assertion_failed`: the step ran (its effects happened) and its `result` is in its entry; `failed_assertion` is `{path, equals | contains | exists, actual, message}`.
  - `invalid_argument`: a reference could not be resolved; the step did not run.
  - `failed`: the call was cancelled before a step started.
- `next` says which steps completed and that nothing was repeated: observe the state and send a new `vm_batch` with only the remaining steps that still apply.
- `vm_end_turn` waits for a running batch like any call of the task; it cancels a `vm_wait` step that is waiting, which stops the batch with `step_failed`, and steps after it are refused with `task_busy` while the cleanup runs.
