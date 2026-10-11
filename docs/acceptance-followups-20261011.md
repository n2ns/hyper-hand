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

## vm_evidence journal limits on the installed host

Build `dev-20261011-104854-28c6078`. Evidence: `build/evidence-limits-20261011/` (`limits.py`, `images.py`, results, export zips).

- **Both limits:** one task made 120 full-resolution screenshots (`vm_observe`, 367 MiB of PNG in total), then 2010 `vm_status` calls, then `vm_evidence`. The export held 2000 calls, the newest, and reported `omitted: {calls: 130, screenshots: 120}`: the 130 oldest calls held all 120 screenshots, each counted once. The zip was 42 KB and took 4.1 s.
- **Screenshot limit alone:** another task made 60 screenshots (183 MiB) and exported. It kept 60 calls and the newest 41 screenshots (about 125 MiB) and reported `omitted: {calls: 0, screenshots: 19}`. The zip was 131 MB and took 2.3 s.

## Saving a VM whose session is locked

Build `dev-20261011-104854-28c6078`. Evidence: `build/savelock-20261011/`.

The session was locked from the guest (`rundll32 user32.dll,LockWorkStation` through `vm_exec`); `vm_status` reported `session.locked: true`. `vm_save` took 4.1 s (`power: saved`). `vm_start` resumed the VM in 9.8 s with `previous_state: saved`, `unlocked: true` and `desktop: usable`, typing the stored unlock password as after any start. `vm_status` then reported `locked: false`, and `vm_exec` answered. Saving a VM with much more memory (the 5-minute job wait) needs a VM memory change and was not run.

## Interactive UAC consent in the guest

Build `dev-20261011-104854-28c6078`. Evidence: `build/uac-20261011/` (`uac*.py`, results, screenshots). Each run changed the guest policy to `ConsentPromptBehaviorAdmin = 2` (prompt for consent on the secure desktop) inside a temp checkpoint and restored the checkpoint afterwards; the policy read `0` again each time.

- **Waiting `vm_exec admin`:** the prompt appeared on the secure desktop. Its buttons read `是` and `否` with no access keys shown, and the focus was on `否`.
- **Calls during the wait:** while the call waited, `vm_status` reported the agent `busy`, and `vm_observe` waited behind it. One screenshot call returned after 49 s, at the call's timeout.
- **Why the first answers failed:** the first attempts sent their keys after such a screenshot, so they arrived after the prompt had timed out, and the call returned `elevation_timeout` correctly.
- **Raw input during the wait:** sent without a screenshot first, a raw `vm_key alt+y` returned in 1.1 s and approved the prompt. The waiting call then ran at high integrity (`High Mandatory Level`).
- **Documented flow:** the elevated program was started from a `background: true` job with `Start-Process -Verb RunAs`, so the agent stayed free and `vm_observe` showed the prompt in 2 to 4 s.
  - `alt+y`, `left` then `enter`, and a raw click on `是` each approved it, and the program ran at high integrity.
  - `esc` declined it: `Start-Process` reported "The operation was canceled by the user", and nothing ran.
- **Documentation added:** the queueing during a waiting admin call was undocumented. The `admin` parameter and 5.5 now describe it and name the raw answer.
- **Not run:** a mismatched stored unlock password. It needs the user's Credential Manager entry changed.
