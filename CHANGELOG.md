# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

**Breaking:** the MCP tool surface is redesigned for AI callers. Tool names, parameters and result formats change without aliases, and the guest protocol moves to generation 2: the host refuses older agents with `agent_outdated` until `vm_update_agent` is run.

### Removed

- `vm_screenshot`: replaced by `vm_observe`. There is no `source`, `region` or coordinate metadata to apply; actions take image pixels of an observation.
- `vm_controls`: replaced by `vm_observe` with `controls: true`, which returns the tree as indexed text instead of JSON nodes.
- `vm_focus_window`: actions activate their target window themselves (`activate`, default `true`).
- Legacy title-selected window waits; the new UI conditions use HWND/PID or observation/control identity instead.
- Title selectors: the `window`, `title` and `exact` parameters of every tool. Windows are selected by `handle` (from `vm_windows`, `vm_find_controls`, `vm_observe`, `vm_launch` or an action result) or `pid` only.
- `vm_type` `mode`: the clipboard paste path is gone; text is injected as Unicode key events, with the Hyper-V keyboard for ASCII text when the agent is not available.
- `vm_click` `double` (use `count`), `vm_scroll` `delta` (use `delta_y`), `vm_drag` `x1`/`y1`/`x2`/`y2` (use `from`/`to`), `vm_checkpoint` `name` (use `label` and `keep`).
- Free-text results: `ok`, `handle: N` lines, `name<TAB>state` lists and `exit_code:` blocks.
- Guest op `screenshot`: all screenshots are taken on the host.

### Changed

- Success results that need a follow-up call carry `next`: a `vm_push` mirror plan (`status: "planned"`) says that nothing was written yet and names the apply call (`phase: "apply"`, its `plan_id`, the same paths, `force` and `task_id`, before `expires_at`); a background `vm_exec` names the `vm_job` call with its `job_id`. Callers had stopped after the plan, believing the mirror done.
- **Breaking:** `vm` is required on every tool except `vm_list`; there is no default VM any more ("the only running VM, or the only VM if none is running"). With several VMs on a host, a call meant for a VM that was off silently reached another one, including `vm_restore`, `vm_shutdown`, `vm_turn_off` and `vm_exec`. A call without `vm` is refused before anything runs with `invalid_argument`, reason `vm is required: there is no default VM`, `next` listing the VM names and field `vms`; the input schemas mark `vm` as required. `vm_end_turn` may still omit `vm` to end the whole task (only the task's own waits, temporary checkpoints and ownership), but `all_temp: true` requires `vm` (reason `vm is required with all_temp: ...`) instead of deleting temporary checkpoints on every VM.
  - Migration: pass `vm` (a name from `vm_list`) on every call; pass `vm` with every `vm_end_turn` that uses `all_temp: true`; Stop hooks that call `vm_end_turn` without `vm` (such as the examples in `docs/hooks/`) keep working.
- Observed controls can be re-located using their previous screen bounds as a read-only hint. The guest verifies the exact runtime ID and control-view ancestry to the requested window before accepting a hit; covered, moved or mismatched hints fall back to the existing traversal. Actions and wait conditions still read current values and state, and scoped reads retain password-subtree protection.
- Semantic actions return readable post-action `state` and verify Toggle, Expand, Collapse, Select and ScrollIntoView outcomes. An immediate read is followed by up to 250 ms of read-only polling when needed; actions are never replayed. Unknown outcomes, password values and unobservable Invoke effects stay `verified: null`.
- Every result is one JSON object in a single text item (`vm_observe` and actions with an after-action screenshot put a PNG image item before it). Every error is an `isError` result whose text is `{"error": <code>, "reason", "next", "run_id", ...fields}`; `next` names the call that makes progress. Codes: `invalid_argument`, `stale_observation`, `stale_element`, `session_unusable`, `activate_failed`, `target_disabled`, `covered`, `integrity_mismatch`, `target_not_responding`, `unsupported_pattern`, `partial_input`, `agent_required`, `agent_outdated`, `ambiguous_target`, `no_window`, `no_checkpoint`, `failed`.
- Tools carry MCP annotations: read-only (`vm_list`, `vm_status`, `vm_checkpoints`, `vm_pull`, `vm_clipboard_get`, `vm_wait`, `vm_windows`, `vm_find_controls`, `vm_observe`, `vm_doctor`), destructive (`vm_shutdown`, `vm_turn_off`, `vm_restore`, `vm_checkpoint_delete`, `vm_push`, `vm_update_agent`, `vm_end_turn`) and idempotent hints; `openWorldHint` is false.
- `vm_windows` returns `{windows, foreground, focused_control, session}`; each window gains `group_root` (the root of its owner chain within the process, at most 8 links) and `integrity` (`low`, `medium`, `high`, `system`); `focused_control` is the UI Automation focused element with `window`, `name`, `control_type`, `automation_id`, `class_name`, `runtime_id`, `rect`; `session` is the agent's session state.
- Actions (`vm_click`, `vm_drag`, `vm_scroll`, `vm_set_value`, `vm_invoke`, `vm_type`, `vm_key`) take `observation_id` with image pixels or a control `index`; the host maps pixels to the screen, re-finds controls by runtime ID and refuses an observation whose window moved, resized, minimized or closed (`stale_observation`). The check chain before input is: session, freshness, target, activation, enabled, hit (covered), integrity, each with its code and `next`. Actions default to `activate: true` and to `observe_after: screenshot` (`none` for `vm_type` and `vm_key`; `controls`, `both`; `settle_ms` default 300, max 5000), returning the receiving `window` and an `after` observation with a new `observation_id`. Raw input (pointer actions without `observation_id`, `vm_key` without `handle`/`pid`) goes to the console with no checks at all, so the sign-in screen, the lock screen and UAC prompts stay reachable; an `index` of a whole-screen observation's tree targets the foreground window the tree belongs to with the full chain.
- Window groups: a target's own windows (top-level windows of its process that it owns, directly or through other owned windows, up to 8 links) count as the target: an AutoCAD main-window handle reaches its untitled command line and command history popup, and the result names the window reached. Refusals name the window to act on next (`act_on`, `foreground`): a disabled window's own foreground dialog, another process's foreground window that probably blocks it, or the covering window described by class and process even when `vm_windows` does not list it (shell overlays).
- `vm_click` gains `count` (1 to 3) and `modifiers` (`ctrl`, `shift`, `alt`, held on the Hyper-V keyboard around the click); `vm_drag` takes `from`/`to` and `modifiers`; `vm_scroll` takes `delta_y` and `delta_x` (horizontal wheel through the agent).
- `vm_type` has one mode: Unicode key events through the agent (`type_keys`); without a target, the Hyper-V keyboard for ASCII text when the agent is absent or its session is locked or on the secure desktop (`agent_required` or `session_unusable` for other text); it accepts `handle`/`pid` or `observation_id` with `index` (clicks the control first and reads it back afterwards: `verified` says whether `value` contains the typed text), returns `{applied_chars, total_chars, window, after}` and reports a stop after some characters as `partial_input` with the same counts (`failed` with `applied_chars: 0` when nothing was typed).
- `vm_key` accepts the numeric keypad (`num0`-`num9`, `numenter`, `numdot`, `numplus`, `numminus`, `nummul`, `numdiv`), `numlock`, `f13`-`f20`, `printscreen`, `scrolllock`, `pause`, `apps`, `capslock` and X11-style aliases (`Return`, `Escape`, `Control_L`, `Shift_L`, `Alt_L`, `Super_L`, `KP_0`-`KP_9`, `KP_Enter`, `KP_Decimal`, `KP_Add`, `KP_Subtract`, `KP_Multiply`, `KP_Divide`, `period`, `comma`, `slash`, `minus`, `equal`, `Prior`, `Next`); a sequence stopped after at least one combination is `partial_input` with `applied` and `total` (a failure before the first keeps its own code); the result is `{ok, combinations, window, after}`.
- `vm_checkpoint` takes `label` (1 to 64 characters without `\ / : * ? " < > |` or line breaks; default `hhmmss`) and `keep` and names the checkpoint `<run_id>-temp-<label>` or `<run_id>-keep-<label>`, returning `{id, name, type, kind, state, parent, created_at}`; a VM whose checkpoint setting is `Disabled` is refused with `invalid_argument`.
- `vm_checkpoints` returns the tree: `{vm, checkpoint_type, current_parent, checkpoints: [{id, name, parent, created_at, type, run_id, label, kind, state, current, children}]}` in creation order; `parent` is the parent's id (`null` for a root), `kind` is `standard`, `production` or another Hyper-V snapshot type lower-cased, `state` the saved power state, `current` marks the checkpoint the VM's current state branches from, `created_at` is RFC 3339 with the host's offset.
- Checkpoints are selected by `id` (from `vm_checkpoints`; the stable selector, since names may repeat) or by `name` when exactly one checkpoint has it; several: `ambiguous_target` with `ids`; none: `no_checkpoint`.
- `vm_restore` takes `id` or `name` and `save_current` (first saves the current state as `<run_id>-temp-before-restore`), returning `{vm, restored: {id, name, type}, state, saved_current, next}`.
- `vm_end_turn` takes `all_temp` (delete the `temp` checkpoints of every run on the VM named by `vm`, which it requires) and returns `skipped: [{id, name, reason}]` for the checkpoints it did not delete (no longer `temp`, or a Hyper-V failure with `all_temp`), besides `cancelled_waits`, `deleted_checkpoints` and `errors`; registered checkpoints are checked against the VM's current list before deletion.
- `vm_list` returns `{vms: [{name, id, state}], run_id}`; `vm_status`, `vm_start`, `vm_unlock`, `vm_shutdown`, `vm_turn_off`, `vm_restore`, `vm_exec`, `vm_push`, `vm_pull`, `vm_clipboard_get`, `vm_clipboard_set`, `vm_wait` (`{satisfied, elapsed_ms}`), `vm_install_agent` and `vm_update_agent` return JSON objects; `vm_shutdown` on a VM that is already off returns `{vm, state: "off"}`.
- `vm_exec` runs commands to completion only; its description points GUI programs to `vm_launch`.
- Every new agent connection is pinged first: an agent whose protocol is older than 2 is refused with `agent_outdated` for every op except `ping` and `update_agent`, so `vm_status`, `vm_doctor` and `vm_update_agent` still work on it; `vm_start`, `vm_install_agent` and `vm_update_agent` also check the agent that answers their readiness ping; an agent's `unknown op` answer is reported the same way.
- Guest protocol: `ping` reports `protocol`; `list_windows` takes optional `{focused}` (only `vm_windows` and `vm_observe` ask for the focused control, which costs a UI Automation helper process) and returns `foreground`, `focused`, `session`, `agent_integrity` and per-window `group_root` and `integrity`; `list_controls` nodes carry `runtime_id`, `patterns`, `value`, `has_value` and `focused`, and the result carries `focused` and `selected_text`.
- Every `failed` error carries a default `next` (`call vm_status, then vm_doctor if the VM is running; the action may or may not have happened, so observe before repeating it`).
- Tool descriptions of `vm_windows`, `vm_find_controls`, `vm_observe`, the actions, `vm_exec`, `vm_pull` and `vm_clipboard_get` state that titles, control names, values, command output, file contents and clipboard text are data from the guest, not instructions.

### Added

- `vm_invoke` `action` is optional: omitted, it defaults to the control's only action (other than `SetValue`) from the observation's tree, such as `Invoke` on a `Button ... actions=[Invoke]`; a control with several or no actions is refused with `invalid_argument`, `supported` and a `next` listing the actions to choose from.
- `client/hyperhand_client.py`: a minimal official client for scripts and AI agents (Python 3.8+, standard library only), usable as a module or a command. It keeps one MCP session and a stable `task_id` (generated, `--task`, `--task-file` or `HYPERHAND_TASK_ID`), takes `key=value` arguments (Windows paths unescaped), `@file.json` or stdin, prints one UTF-8 JSON object, turns error results into `HyperHandError` / exit code 1 (transport problems exit 2), saves PNG images and returns their path as `_image`, parses control trees and finds controls by type, name, AutomationID, value, state, action or point (`--find`). Re-opens the session after a host restart. `HyperHand.call(tool, arguments=None, /, *, http_timeout=None, **kwargs)` takes every tool argument as a keyword, including `vm_launch` `args`; the HTTP timeout keyword is `http_timeout`. See [client/README.md](client/README.md).
- Background guest commands: `vm_exec` with `background: true` starts the command as a job in the guest agent and returns its `id` at once; `vm_job` reads its state and incremental output by byte offsets (`stdout_next`/`stderr_next`, `complete`), long-polls with `wait_ms` without holding the agent connection, cancels the whole process tree (`cancel: true`, needs write ownership) and lists the agent's jobs. Jobs survive MCP reconnects, task ends and host restarts and can be read by any task; they default to a 1-hour timeout (24 hours at most), keep 16 MiB per stream, and up to 32 jobs are kept for 24 hours after they end. An agent restart forgets them (`no_job`) without stopping their processes. New error code `no_job`. **Guest protocol 3:** the host refuses older agents with `agent_outdated`; run `vm_update_agent` on every VM.
- `vm_find_controls` searches a window or selected subtree by exact AutomationId/name/type with independent traversal and result limits. Matches become task-scoped observations usable by existing actions and UI waits. `vm_observe` accepts `observation_id`/`index` to read a subtree; diffing requires the same root. Scoped reads fail closed on missing/incomplete identities and old agents. New guest operations require updating the agent; protocol generation remains 2.
- UI conditions in `vm_wait`: window appearance, disappearance and foreground; control appearance, disappearance and exact enabled/value/state matching. Supports `check_only`, `assert` with `assertion_failed`, bounded host polling, concurrent actions, task cancellation and structured last-state evidence. Ambiguous, stale, truncated, password-redacted and unavailable provider results cannot silently satisfy an assertion.
- Directory mirroring through `vm_push mode: mirror`: read-only `phase: plan` lists copies, empty directories and deletions; `phase: apply` consumes a task/VM-bound plan after rechecking both inventories. Changed files are staged and verified before extra files and empty directories are removed. Drift, partial completion and unconfirmed responses are distinct results; failed applies require a new plan. Ordinary copy behavior is unchanged. Mirror rejects links, type conflicts and manifests exceeding 4096 entries or 1 MiB per side.
- `vm_invoke` supports control-targeted `ScrollUp`, `ScrollDown`, `ScrollLeft` and `ScrollRight` through UIA ScrollPattern, one small provider-defined step per call. Observations and results include supported axes and scroll percentages; verification reports observed directional movement, including `false` at an unchanged boundary.
- Control observations expose executable `actions` and readable semantic `state` (toggle, expansion, selection, read-only and offscreen); unknown properties stay absent and `controls_diff` includes capability/state changes. Unsupported actions report action names rather than UIA pattern names.
- Task ownership: optional `task_id` on every tool, independent observations, waits and checkpoint runs, and exclusive VM writes until task-scoped `vm_end_turn` cleanup. Persistent MCP sessions get a default task; short-lived connections and agents sharing a session can pass distinct IDs.
- Stronger observation freshness: shared VM revisions invalidate old coordinates after input and other state-changing calls; lifecycle changes invalidate control references too. Coordinate actions also check local screenshot changes, and window checks include process/class identity. Stable UIA runtime IDs can still be re-located after ordinary input.

- `vm_apps`: query launchable desktop applications from guest Start Menu shortcuts and App Paths. Returns stable IDs, preserved `vm_launch` arguments, current-session running state matched by executable path, visible window handles, result limits and partial-source warnings. No drive scan or UWP/MSIX launching.

- `vm_observe`: one call returns a PNG of the screen or of one window (`handle`, `pid`; `max_size`), the observed window, the focused control, `selected_text` and, with `controls: true`, the UI Automation tree as indexed text (`[index] ControlType "name" id=... (x,y wxh) disabled|offscreen|focused value="..."`; `max_depth`, `max_nodes`); `diff_from` replaces the tree by `controls_diff` (`added`, `removed`, `changed`; a node whose index shifted counts as changed). The result carries `observation_id`; the host keeps the 8 most recent observations per VM. The screenshot is the host console image, so it works while the guest is locked or shows a UAC prompt (`agent: "offline"` then); a window that does not answer UI Automation yields the screenshot with `stale_risk: "target not responding: control tree not read"`.
- `vm_set_value` (UI Automation `ValuePattern.SetValue`, read back as `verified`, `value` and `state`; `verified` is null when the read-back was cut at 512 UTF-16 units) and `vm_invoke` (`Invoke`, `Toggle`, `Expand`, `Collapse`, `Select`, `ScrollIntoView`, `ScrollUp`, `ScrollDown`, `ScrollLeft`, `ScrollRight`) by control `index`; `unsupported_pattern` lists the control's `supported` executable actions, `stale_element` reports a vanished control, `target_not_responding` a window that does not answer within 12 s on the host or the agent's 10 s UI Automation timeout.
- Index actions re-locate the control by runtime ID before acting (guest `control_action` action `Locate`, which performs nothing and returns the element's current `rect` and value): `vm_click` with `index` and `vm_type` with `index` click the centre of the control's current rectangle, so a control that moved since the observation is still hit; a vanished control is `stale_element`.
- `vm_launch`: start a program detached (not in the `vm_exec` job object, optionally `admin`) and wait up to `wait_window_ms` (default 60 s) for its first visible top-level window; returns `{pid, handle, title, class, elapsed_ms}`, or `no_window` with `pid` while the process keeps running.
- `vm_checkpoint_delete`: delete a checkpoint by `id` (`manual` checkpoints by `id` only) or by unique `name`, with `subtree: true` the checkpoint and all its descendants; waits for Hyper-V to merge the disk differences (up to 15 minutes) and returns `{deleted: [{id, name, type}], merged_into_current, elapsed_ms}`.
- `vm_checkpoint_keep`: rename a `temp` checkpoint to `<run_id>-keep-<label>` (optional new `label`) so that `vm_end_turn` no longer deletes it; returns `{id, name, type}`; a `keep` or `manual` checkpoint is `invalid_argument`.
- Error code `no_checkpoint`: no checkpoint has the given `id` or `name`.
- Broker operation `checkpoint_rename`; `checkpoint_restore` and `checkpoint_delete` take the checkpoint `id` (a GUID), `checkpoint_delete` also `subtree`.
- `vm_end_turn`: cancel this server's pending `vm_wait` calls and delete the `temp` checkpoints of this run; returns `{cancelled_waits, deleted_checkpoints, skipped, errors}`. Hook examples for Claude Code (`Stop`) and a Codex plugin (`Stop`, `Interrupt`, `SubagentStop`) in `docs/hooks/`.
- `vm_doctor`: read-only checks of the host service, MCP listener, service pipe, Hyper-V socket registration and enhanced session mode, and of the VM's power, agent version and protocol, session and agent integrity, each `ok`, `warn` or `fail` with a suggestion.
- Run ID `run-<yyyymmdd-hhmm>-<4 hex>` per server process, returned by `vm_list` and carried in every error object and checkpoint name.
- Integrity check: input to a window whose process runs at a higher integrity level than the agent is refused with `integrity_mismatch` instead of being dropped silently by Windows.
- Guest ops `control_action` (with `Locate` and a `rect` in its result), `launch` and `hscroll`.

### Fixed

- A client that opens and deletes one MCP session per call no longer leaves the VM owned by a task nobody can reach. Deleting a session (HTTP `DELETE`) ends its default task once none of its calls is in flight: waits cancelled, observations and mirror plans dropped, VM ownership released; its temporary checkpoints stay for `vm_end_turn` `all_temp`. Explicit `task_id`s keep their ownership across sessions as before. `vm_busy` adds `owner_idle_ms` and `owner_in_flight` and names `vm_end_turn {task_id: owner_task_id, vm}` for an abandoned owner; `vm_status` adds `owner: {task_id, idle_ms, in_flight, this_task}` (`null` when free).
- `vm_type` into AutoCAD no longer stops with `partial_input` after the first characters of a command name. AutoCAD's dynamic-input tooltip (`CAcDynInputWndControl`, a bare popup owned by the main window on its UI thread) takes the foreground and keyboard focus and forwards the keys to the command line; the agent now accepts such an input popup of the target (`WS_POPUP` without caption, sizing border or system menu, not a dialog, same process and thread, owner chain reaching the target) as the foreground while the target stays visible, enabled and not minimized. Modal dialogs, floating palettes (they have a sizing border and system menu), popups of other threads or processes and other windows still stop the input. Requires the updated guest agent (`vm_update_agent`); the protocol is unchanged.
- Window integrity inspection uses an aligned token-information buffer and preserves SID pointer provenance, avoiding a crash under Go's race/checkptr instrumentation.
- `vm_install_agent` confirms the per-installation ID returned by the newly launched agent, so an older running process (including the same version) cannot report success early. Readiness polling stops promptly on cancellation.
- UI Automation provider failures, including read-only or disabled controls, remain operation failures instead of reporting the responding agent as offline or falling back to raw keyboard input.
- `vm_invoke Select` detects the SelectionItem pattern on individual list/tree items using the correct UI Automation property, instead of checking the parent container's Selection pattern.
- Missing, unknown or mistyped tool arguments return structured JSON `invalid_argument` results before VM access, preserving identifiable task ownership instead of leaking the MCP SDK's plain-text validation errors. Omitted or null arguments remain compatible with tools that have no required fields.

## [0.2.0] - 2026-10-07

### Added

- `vm_type` supports `mode: keys` through guest Unicode `SendInput`, preserving the clipboard, limiting text to 16384 UTF-8 bytes and rechecking the selected foreground window between characters. `vm_type` and `vm_key` accept optional window selectors; `vm_key` also accepts an ordered `sequence` of up to 256 combinations. Partial input is reported without automatic retry.
- `vm_screenshot` supports a `region` crop and `max_size` downscaling, with JSON dimensions, crop origin, actual per-axis scales, VM identity and source/session coordinate metadata. Unknown or non-console agent captures are not marked compatible with console input.
- Read-only `vm_controls` returns a bounded UI Automation control-view tree for a selected window. A separate helper has a 10-second timeout; password names and subtrees are not read.
- `vm_focus_window`, window-relative `vm_click` and window conditions in `vm_wait` share window selection by handle, PID and case-insensitive title, with optional `exact` full-title matching. A PID can select a window alone or restrict a title or handle match.
- `vm_wait` conditions `window_exists`, `window_gone` and `window_foreground`, polled by the host without holding the agent connection between checks. Successful appearance and foreground waits return the window's handle and title; disappearing includes windows that become hidden or cloaked. These use the existing agent `list_windows` operation without a guest protocol change.
- Dedicated `HyperHandService` Windows service under `NT SERVICE\HyperHandService`, with Hyper-V rights assigned to the service account and an access-controlled local named pipe for specific VM operations and the guest socket tunnel.
- Host tray restart action: restarts the ordinary tray/MCP process without restarting the service or any VM; in-progress requests are interrupted.
- `vm_start` waits until the desktop is usable: the guest agent answers (up to 90 seconds) and the session is unlocked. A session Windows locked after signing in is unlocked with the password stored for the VM; otherwise the error says why the desktop is not usable.
- `vm_status` reports a VM's power state, the agent, the session lock state, a non-console session, an open UAC prompt and whether an unlock password is stored.
- `vm_unlock` unlocks a locked session with the stored password. The password is typed once, only after the agent confirms the session is locked, is the console session, shows the sign-in screen with keyboard input on its secure desktop and has no UAC prompt open.
- Host settings window, opened by clicking the tray icon or from its **Settings...** menu item: the MCP endpoint and port (the port can be changed and is saved per user; `-port` still overrides it), the service status and a **Virtual machines** list with the Hyper-V VMs and their state, with **Set unlock password...** and **Clear unlock password** per VM (stored in Windows Credential Manager for the current user).
- **Open console** per VM in the settings window opens Virtual Machine Connection without a UAC prompt, or brings an already open one to the front; **Open console when started** opens it whenever `vm_start` starts that VM. `install` registers the on-demand `HyperHand Console` task for this, which runs `vmconnect.exe` with the installing user's highest privileges; `uninstall` removes it.
- Agent op `session_state`.
- `vm_focus_window` and `vm_click` with a window name the locked session as the reason when they fail on a locked guest.
- `vm_windows` lists the guest's visible top-level windows with handle, title, class, process, position, enabled, foreground, owner and modal state.
- `vm_focus_window` accepts a window `handle` and returns the focused window's handle.
- `vm_click` accepts `window` or `handle`: coordinates are then relative to that window, and the click is refused unless it is the enabled foreground window and the point is inside it, on screen and not covered by another window.

### Changed

- `vm_status` reports `busy` when this host already has an agent request in progress, without queueing its probe behind that request. Connection and response failures leave the guest state unknown rather than claiming the agent is offline.
- `vm_screenshot` now returns JSON metadata instead of the former `<width>x<height>` text item. Cropped and scaled coordinates must be mapped before console input; old agents can still supply images but cannot establish coordinate compatibility.
- **Breaking:** `vm_focus_window` now rejects multiple matching windows instead of focusing the first match. All focus requests require an agent supporting `list_windows`; older agents must be updated, with no fallback to title-only focus. Window clicks and waits also reject ambiguous selection, including disappearance waits.
- **Breaking:** `vm_stop` is replaced by `vm_shutdown`, which shuts the guest down normally through the Hyper-V shutdown integration service and waits up to 3 minutes until the VM is off (failing, without turning it off, when a program blocks shutdown), and `vm_turn_off`, which turns the VM off immediately as `vm_stop` did.
- Host tray menu text is in English.
- `hyperhand.exe`, its tray icon and the settings window use a new gripper icon, from the executable's resources at the system's small and large icon sizes. `hyperhand-agent.exe` and its tray icon use it too.
- The guest agent tray uses the same notification icon code as the host instead of `fyne.io/systray`, so it also survives a taskbar that is not ready at logon; a left click opens its menu as before.
- `hyperhand-agent.exe uninstall` no longer starts a PowerShell helper to delete the folder it runs from after it exits: it moves its own executable to `%TEMP%` and deletes the folder at once.
- Normal host tray startup no longer requests elevation. Installation, updates and uninstallation still require administrator approval.
- `hyperhand.exe install` installs both executables under `%ProgramFiles%\HyperHand`, records the owner under `%ProgramData%\HyperHand`, and migrates the `HyperHand` logon task from highest privileges to least privilege. Repeating `install` updates the installation.
- Host file transfers use the ordinary tray user's permissions. The local MCP endpoint remains unauthenticated; the broker pipe ACL is not MCP authentication.
- `scripts/restart-tray.ps1` now invokes installation/update of the built executables instead of terminating every process named `hyperhand.exe`.
- `install` and `uninstall` no longer run an embedded PowerShell script: the elevated `hyperhand.exe` does the work itself through the service control manager, Task Scheduler, local group and security APIs. Uninstalling from the installed `hyperhand.exe` leaves `%ProgramData%\HyperHand\uninstall-cleanup.exe` until the next restart.

### Fixed

- Window click checks, focus changes, keyboard sequences and clipboard writes share the input lock, so another MCP input operation cannot invalidate a queued click check or replace text between clipboard setup and paste.
- Elevated command cleanup could hang when an early launcher failure raced a pending named-pipe accept, even after the request deadline. Pin `go-winio` to Microsoft's [listener shutdown fix](https://github.com/microsoft/go-winio/commit/7e8af9b09c4b3e8da1a8ba080484c8a84b71354a), which preserves the close signal; the fix is not yet in a tagged release. A repeated early-failure regression test covers this path.
- Reinstalling could stop with "Access is denied" after stopping the service, leaving the service stopped and no tray: setup tried to end the service's process, which runs as the service account. It now waits for that process to exit.
- The host tray icon no longer stays an empty placeholder without menu when the tray starts at logon before the taskbar is ready. The tray now uses its own notification icon code instead of `fyne.io/systray`: a failed add is retried every 5 seconds and whenever the taskbar is created, and every add carries the icon, tooltip and callback message and selects version 4 behaviour.
- VM start and stop wait up to 45 seconds for asynchronous Hyper-V jobs and report failures or timeouts without automatically resending the operation.
- `vm_focus_window` no longer puts ribbon programs such as AutoCAD into key-tip mode. When the window did not come to the front at once, the agent simulated an Alt press, which arrived after the switch in the focused window; it now injects a zero-distance mouse move instead.

## [0.1.2] - 2026-10-05

### Fixed

- Command cancellation and timeouts terminate descendants even after the original shell has exited, while successful commands can still launch background applications.
- Elevated commands include UAC consent in their timeout, cancel without a second elevation prompt, and reject workers that arrive after the request expires.
- Elevated CMD commands preserve UTF-8 text, Unicode filenames, percent expansion and exit codes.
- Requests cancelled while waiting for another agent call return promptly without disturbing the active connection.
- Saved and paused Hyper-V machines display the correct state.

## [0.1.1] - 2026-10-05

### Added

- `hyperhand.exe uninstall`: relaunches itself elevated if needed, stops the tray, deletes the `HyperHand` scheduled task, the Hyper-V socket service registration and `%LOCALAPPDATA%\HyperHand`, and shows what was removed. The exe and its folder are kept.
- `hyperhand-agent.exe uninstall`: without administrator rights, stops the running agent and deletes the `HyperHandAgent` Run value, `%LOCALAPPDATA%\HyperHand` and `C:\Users\Public\HyperHand`, and shows what was removed.

### Fixed

- File transfers use unique temporary files so existing files named `<destination>.hhpart` are preserved, including when a transfer fails.
- Uploads return an error promptly when the guest disk is full, without blocking subsequent agent requests.
- Commands no longer block the agent when completion and cancellation occur together.
- Guest uninstallation waits for the uninstall process to exit before deleting its executable, including when the result dialog stays open.

## [0.1.0] - 2026-10-05

Initial release.

### Added

- Host tray program `hyperhand.exe` serving MCP over Streamable HTTP at `http://127.0.0.1:8770/mcp`; `-port` selects another port. Relaunches itself elevated, runs as a single instance, and logs to `%LOCALAPPDATA%\HyperHand\hyperhand.log`.
- `hyperhand.exe install`: registers a scheduled task that starts the exe from its current location at logon with highest privileges, and starts it.
- Registration of the HyperHand Hyper-V socket service on the host at startup.
- VM control through Hyper-V WMI, without a guest agent: `vm_list`, `vm_start`, `vm_stop`, `vm_screenshot` (PNG at the guest resolution), `vm_click`, `vm_drag`, `vm_scroll`, `vm_key`.
- Checkpoints: `vm_checkpoints`, `vm_checkpoint`, `vm_restore` (exact name; starts the VM after restoring unless `start` is false).
- Optional `vm` argument on every tool; defaults to the only running VM, or the only VM if none is running.
- Guest agent `hyperhand-agent.exe`: tray app in the user's session that serves host requests over a Hyper-V socket; `install` subcommand copies it to `%LOCALAPPDATA%\HyperHand` and starts it at logon through the `HKCU` Run key, without administrator rights.
- `vm_install_agent`: copies the agent into the guest with `Copy-VMFile` (enabling the Guest Service Interface if needed), runs its installer through the keyboard, and waits for it to answer.
- `vm_update_agent`: replaces the running agent with the build next to `hyperhand.exe` and waits for it to restart.
- `vm_exec`: runs a PowerShell or cmd command as the logged-on user with a timeout and returns exit code, stdout and stderr; `admin: true` runs it elevated.
- `vm_push` and `vm_pull`: copy files or directories recursively between host and guest, streamed through `.hhpart` temporary files; `vm_push` skips files whose SHA-256 matches the guest copy unless `force` is true.
- `vm_type`: pastes text through the guest clipboard when the agent is available, so an IME cannot alter it; falls back to keyboard input for ASCII text.
- `vm_clipboard_get`, `vm_clipboard_set`, `vm_focus_window`, `vm_wait` (process exit, process running, file exists), and `vm_screenshot` with `source: agent`.
- `scripts/restart-tray.ps1`: swaps in a rebuilt `build\hyperhand.exe.new` and restarts the tray.
- Release builds through GitHub Actions: pushing a `vX.Y.Z` tag runs the tests, builds `hyperhand.exe` and `hyperhand-agent.exe` for Windows amd64, and publishes `hyperhand-X.Y.Z-windows-amd64.zip` on a GitHub Release with the version's changelog section as notes.
- Version stamped at build time (`-X hyperhand/internal/proto.Version`); reported by the MCP server and the agent ping, `dev` for builds without it.
