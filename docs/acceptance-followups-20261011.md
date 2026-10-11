# Follow-up acceptance on Win10 (2026-10-11)

Acceptance items from the TODO that run on `Win10` without the user, done after the 0.3.0 release. Builds are named per section. Evidence is under ignored `build/<item>-20261011/`.

## Cold boot without a signed-in user

Build `dev-20261011-101346-7c8bdf7-dirty` (0.3.0 plus the English guest tray). Evidence: `build/coldboot-20261011/` (`accept_coldboot.py`, `results.json`, screenshot).

A temp checkpoint was taken first. Then automatic sign-in was turned off in the guest (`AutoAdminLogon` = `0` under `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon`, set through `vm_exec admin`), and the VM was shut down with `vm_shutdown` (7.7 s).

- `vm_start` started the VM and, after 99.6 s, refused with `agent_required`: `VM Win10 is running, but its desktop is not usable: the guest agent did not answer within 1m30s: no user is signed in, or the agent is not installed. ...`. The VM stayed running.
- Two `vm_status` calls afterwards each answered in 5 s: `power: running`, `agent.state: not_answering`.
- `vm_doctor` reported every host check `ok`, `vm.power` `ok` and `guest.agent` `fail` with its suggestion.
- `vm_observe` returned a host screenshot of the Windows sign-in curtain with `agent: offline`.
- `vm_unlock` and `vm_exec` refused at once with `agent_required`.
- Restoring the checkpoint (`vm_restore`, then `vm_start` in 3.2 s) brought back the signed-in desktop. `AutoAdminLogon` read `1` again, and `vm_end_turn` deleted the temp checkpoint.

Signing a user in is out of scope (see the TODO's product decisions); HyperHand reports the state and keeps working for status, doctor and screenshots.

## AutoCAD 2015: focus fallback, UI Automation coverage and a busy UI thread

Builds `dev-20261011-101346-7c8bdf7-dirty` and, for the timeout reports, `dev-20261011-102954-1b778d4-dirty` with the change committed as `f1c461a`. AutoCAD 2015 (trial) on Win10. Evidence: `build/acad-20261011/` (scripts, JSON results, screenshots).

### Focus fallback branch

- A helper window took the foreground and called `LockSetForegroundWindow(LSFW_LOCK)`. A background probe process then got `SetForegroundWindow(AutoCAD) = False`, and the foreground stayed on the helper. Under this lock, the agent's first `SetForegroundWindow` fails in the same way.
- `vm_key {handle: AutoCAD main window, keys: esc}` still activated AutoCAD. `vm_type "_qnew\n"` then typed 6 of 6 characters, and the `Select template` dialog of `acad.exe` became the foreground, so the input reached AutoCAD.
- The agent has no log, so its first call failing is inferred from the lock and the probe; the fallback (attach to the foreground thread, zero mouse move, retry) is what activated the window.

### UI Automation coverage

- **Standard dialog (`Select template`, `#32770`):** a 120-line tree. `vm_set_value` on `File name:` wrote `acadiso.dwt` with `verified: true`, and `vm_invoke` on `Cancel` closed the dialog.
- **Custom-drawn drawing area** (the focused `Pane`): `vm_invoke Invoke` and `vm_set_value` were refused with `unsupported_pattern` and `supported: []`. The `next` then suggested `vm_set_value`/`vm_invoke`; it now offers only `vm_click` or keyboard input (fixed).
- **Elevated AutoCAD** (`vm_launch admin: true`): `vm_windows` reported its windows, including the Qt licensing dialog `LMU.exe`, at `high` integrity. `vm_observe` with controls read the dialog. `vm_type`, `vm_key` and `vm_click` with the target were refused with `integrity_mismatch`, and nothing was sent. A raw `vm_click` at the dialog's `Try` button (screen pixels, Hyper-V mouse) closed the dialog, and AutoCAD continued elevated. The old `next` (`vm_launch with admin:true`) pointed at the cause; it now names raw input (fixed).
- **Whole main window:** about 1,400 control-view nodes. HyperHand's walk reads about 120 nodes/s; a cached `FindAll` in .NET read 1,382 nodes in 4.9 s. `vm_observe` with `max_depth: 10, max_nodes: 1000` and `vm_find_controls` with the default `max_visited` ran into the 10 s helper limit, and so did `max_visited: 1500`. These reports said `target not responding` with a `next` to kill the program although AutoCAD responded; they now say `ui_automation_timeout` (fixed in `f1c461a`). Ribbon tabs (WPF) were therefore not reached; see the TODO's control-search performance item.

### Busy UI thread

- AutoLISP created 1.5 million circles. During creation AutoCAD's main window was hung (`IsHungAppWindow` `True`).
- `vm_find_controls` returned after 10.1 s, twice, with `target_not_responding (window hung)`.
- `vm_windows` (≤ 2.1 s), screenshots (≤ 6 s) and `vm_key` all returned. `vm_key` succeeded because AutoCAD already held the foreground and needed no activation: the key was queued and processed once AutoCAD recovered, so `ok` did not mean that AutoCAD had handled it.
- `REGEN` afterwards did not hang it. AutoCAD answered again 63 s after the command was typed, and every tool worked afterwards.

## Hyper-V keyboard and the Run dialog

Build `dev-20261011-104854-28c6078`. Evidence: `build/rawtype-20261011/` (`rawtype.py`, `runlen.py`, results).

Earlier the same day, a 248-character check command typed into the Run dialog on the Hyper-V keyboard did not run, while three shorter ones did (see [Not verified](acceptance-gaps-20261011.md#not-verified)).

- **Typing is intact.** With the agent stopped, `vm_type` without a target typed four lines into Notepad on the Hyper-V keyboard: that exact command, 248 letters, a line of every special ASCII character (`()&"%><|^!@#$*_+-=[]{};:',./?\~` and a backquote) and `(a & b)`. After `vm_install_agent`, the text read back through the clipboard equalled every line.
- **The Run dialog drops long commands.** Commands of exactly 250, 258 and 259 characters typed into the Run dialog ran; 260 and 265 did not. The dialog closed without any message either way.
- **Cause.** The earlier command had 248 characters, but `%LOCALAPPDATA%` expands from 14 to 26, so it became 260.
- **Effect on HyperHand.** None. `vm_install_agent` types a command of about 90 characters into the Run dialog. Callers who type into the Run dialog themselves must stay at 259 characters or fewer after variable expansion.
