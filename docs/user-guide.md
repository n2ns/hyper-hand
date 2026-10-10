# User Guide

This guide covers downloading or building HyperHand, installing the host tray and the guest agent, connecting an MCP client (Claude Code, Codex or another client), updating, releasing, uninstalling, and troubleshooting.

HyperHand has two executables and three roles:

- `hyperhand.exe` runs as an ordinary user tray program on the Hyper-V host. It serves MCP over Streamable HTTP at `http://127.0.0.1:8770/mcp` and reads and writes host files using that user's permissions.
- `HyperHandService` runs the same executable as a Windows service under `NT SERVICE\HyperHandService`. The tray requests specific Hyper-V operations over an access-controlled local named pipe. Screenshots, mouse, keyboard, VM state and checkpoints work without anything installed in the guest; the service also tunnels the guest agent connection.
- `hyperhand-agent.exe` runs inside the guest, in the logged-on user's desktop session. It answers requests from the host over a Hyper-V socket and provides command execution, program launching, file transfer, clipboard, window and UI Automation control inspection, control actions, Unicode text input and waits.

## Requirements

- Host: Windows 10 or Windows 11 Pro or Enterprise with Hyper-V enabled. Installation, updates and uninstallation require administrator approval; normal tray startup and restart do not.
- Guest: a Windows VM with a user logged on to the desktop.
- Go 1.27 or later, only to build from source.
- VMConnect in basic session mode. In an enhanced session, the guest user session moves to RDP, and the host-side screenshot and input reach the console session, which shows the lock screen. Switch with View > Enhanced Session in VMConnect, or close VMConnect while HyperHand is working.

## 1. Download or build

Download `hyperhand-X.Y.Z-windows-amd64.zip` from [Releases](https://github.com/n2ns/hyper-hand/releases). It contains `hyperhand.exe`, `hyperhand-agent.exe` (both Windows amd64, with the version stamped in), `README.md` and `CHANGELOG.md`. Extract both executables to the same folder. The installer copies them to `%ProgramFiles%\HyperHand`; the logon task uses that installed copy.

To build from source instead, from the repository root:

```
go build -ldflags "-H windowsgui" -o build\hyperhand.exe .\cmd\hyperhand
go build -ldflags "-H windowsgui" -o build\hyperhand-agent.exe .\cmd\hyperhand-agent
```

`-H windowsgui` builds both as GUI programs, so no console window appears. A source build reports its version as `dev`.

Keep `hyperhand-agent.exe` in the same directory as `hyperhand.exe` when installing or updating. Guest installation and update use the installed agent executable.

## 2. Install the host service and tray

From the folder with `hyperhand.exe` (`build` for a source build):

```
hyperhand.exe install
```

This shows one UAC prompt, then:

1. Copies both executables to `%ProgramFiles%\HyperHand` and records the installing user's SID in `%ProgramData%\HyperHand\config.json`.
2. Installs the automatic Windows service `HyperHandService`, running as `NT SERVICE\HyperHandService`. Only this dedicated account is added to Hyper-V Administrators. It does not add your user account or run the service as LocalSystem.
3. Registers the HyperHand Hyper-V socket service under `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\3ce544e1-2645-4383-b332-fedf8a18736b`.
4. Creates or replaces the `HyperHand` logon task for the installing user, using the installed executable with least privilege. This replaces the old highest-privilege task.
5. Creates or replaces the `HyperHand Console` task for the installing user: it has no trigger, runs `vmconnect.exe localhost "<VM>"` with that user's highest privileges, and is run by the tray to open a VM's console (see [Watch a VM](#watch-a-vm)).
5. Starts the service and ordinary tray.

Run `install` again from a new release or build to update the installed copies. Keep the service installation separate from normal tray startup: launching the installed executable without `install` does not request UAC or change machine configuration.

Only one tray instance runs at a time. Its connection to the service uses a local named pipe restricted to the configured owner, SYSTEM, administrators and the service account. This does not add authentication to the local MCP HTTP endpoint.

Click the HyperHand tray icon, or right-click it and choose **Settings...**, to open the settings window. It shows the MCP URL (with **Copy**), or the error if the port could not be opened, and whether the background service is running.

### Restarting the tray

Use the tray's restart action to restart the tray and MCP listener as the ordinary user, without a UAC prompt. It preserves the selected port. This disconnects MCP clients and interrupts in-progress requests; wait for the tray to return, then reconnect or retry from your MCP client. It does not restart the Windows service, guest agent or any VM. Use `install` to deploy updated binaries instead.

### Using a different port

In the settings window, check **Change port**, enter a port from 1024 to 65535 and click **Apply and restart**. The port is saved for your user and also used at logon. Use the matching URL when adding the server to your MCP client.

For a single run, start the tray with a port instead; it then ignores the saved port:

```
hyperhand.exe -port 8771
```

The server always listens on `127.0.0.1`.

## 3. Add HyperHand to an MCP client

Claude Code:

```
claude mcp add --transport http hyperhand http://127.0.0.1:8770/mcp
```

Codex:

```powershell
codex mcp add hyperhand --url http://127.0.0.1:8770/mcp
```

or in `~/.codex/config.toml`:

```toml
[mcp_servers.hyperhand]
url = "http://127.0.0.1:8770/mcp"
```

Other clients: add a Streamable HTTP server with the URL `http://127.0.0.1:8770/mcp`. No token or other authentication is needed.

Start a new session in the client afterwards. The tools are loaded when the session starts.

Every tool except `vm_list` requires a `vm` argument (the VM name); there is no default VM. `vm_list` shows names, states and IDs. A call without `vm` is refused with `invalid_argument` (reason `vm is required: there is no default VM`), and its `next` and `vms` field list the VM names. The one exception is `vm_end_turn`, which may omit `vm` to end the whole task (see [Ending a turn](#ending-a-turn-vm_end_turn-and-hooks)); with `all_temp: true` it requires `vm` too.

## 4. Install the guest agent

Before you start:

- The VM is running and a user is logged on to the desktop.
- The guest keyboard layout and IME are in English mode. The install command is typed on the keyboard, and a non-English IME can swallow or alter the keystrokes. See [Set the guest default input method to English](#set-the-guest-default-input-method-to-english), or [install the agent manually](#install-the-guest-agent-manually).

Ask the AI to call `vm_install_agent`. It:

1. Enables the Guest Service Interface integration service if it is off (required by `Copy-VMFile`; no guest password is needed).
2. Copies `hyperhand-agent.exe` to `C:\Users\Public\HyperHand\hyperhand-agent.exe` in the guest.
3. Opens the Run dialog (`win+r`) and types `C:\Users\Public\HyperHand\hyperhand-agent.exe install`, then Enter.
4. Waits up to 30 seconds for the agent to answer, and reports its version, the guest host name and the user it runs as.

The agent installer, running as the logged-on user:

- Copies itself to `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe`.
- Adds the value `HyperHandAgent` under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` so it starts at that user's logon. No administrator rights are needed.
- Starts the agent. A tray icon appears in the guest; its menu shows whether the host is connected.

If the install fails, call `vm_observe` to see what the guest shows, fix the cause (for example the IME mode or a dialog in the way) and run `vm_install_agent` again.

Tools that need the agent: `vm_exec`, `vm_launch`, `vm_push`, `vm_pull`, `vm_file_info`, `vm_clipboard_get`, `vm_clipboard_set`, `vm_windows`, `vm_find_controls`, `vm_wait`, `vm_unlock`, `vm_update_agent`, `vm_set_value`, `vm_invoke`, `vm_observe` with a window/subtree selector or with `controls: true`, and every action with an `observation_id`, `handle`, `pid` or `index`. Without the agent, `vm_observe` still returns the host screenshot (with `"agent": "offline"`), and `vm_click`, `vm_drag`, `vm_scroll`, `vm_key` and ASCII `vm_type` without a target still reach the VM console. `vm_start` starts the VM without the agent but reports that the desktop is not usable. An agent older than the host is refused with `agent_outdated` until `vm_update_agent` is run.

### Install the guest agent manually

Instead of `vm_install_agent`, you can install the agent yourself. This does not type anything on the keyboard, so the guest input method does not matter.

1. Copy `hyperhand-agent.exe` into the guest by any means: drag and drop in VMConnect, a shared folder, an ISO.
2. In the guest, as the logged-on user, run `hyperhand-agent.exe install`. No administrator rights and no UAC prompt are needed.

The installer does the same as above: it copies itself to `%LOCALAPPDATA%\HyperHand\`, adds the `HyperHandAgent` value under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` and starts the agent. Running `hyperhand-agent.exe` without `install` starts the agent for this session only, without autostart.

## 5. Optional guest setup

### Get a usable desktop after `vm_start`

`vm_start` waits until the agent answers and the session is unlocked. The agent starts when the user signs in, so the guest must reach a signed-in session on its own:

- **Automatic sign-in** (recommended): configure the guest to sign the user in at boot, for example with [Sysinternals Autologon](https://learn.microsoft.com/en-us/sysinternals/downloads/autologon). HyperHand cannot sign a user in at the sign-in screen.
- **Unlock password**: Windows can sign the user in and then lock the session, for example after an update. To let `vm_start` and `vm_unlock` unlock it, open the HyperHand settings window from the tray icon, select the VM under **Virtual machines** and click **Set unlock password...**, then enter the password or PIN the guest lock screen asks for (ASCII only). It is stored in Windows Credential Manager for your user as `HyperHand:<VM name>`; **Clear unlock password** removes it. HyperHand types it on the VM's keyboard only after the agent confirms that the sign-in screen's password box has the input, and types it once per call.

Use `vm_status` to see whether the agent answers, whether the session is locked and whether a password is stored.

### Watch a VM

HyperHand works on the VM console without any window open, so `vm_start` starts VMs in the background. To watch what the AI does, open the HyperHand settings window from the tray icon, select the VM under **Virtual machines** and click **Open console**. Check **Open console when started** to have Virtual Machine Connection open whenever `vm_start` starts that VM; the setting is per VM and per user (`%LOCALAPPDATA%\HyperHand\settings.json`). If a console for the VM is already open, it is brought to the front instead of opening a second one, which would ask to take over the connection.

- Virtual Machine Connection needs Hyper-V rights that your everyday (non-elevated) account does not have. The tray opens it through the `HyperHand Console` task, which `install` registered with your highest privileges, so there is no UAC prompt. Your account, its groups and the VM's permissions are not changed.
- Use a basic session. If the host allows enhanced session mode, the settings window warns you: an enhanced session moves the guest session away from the console, and HyperHand's screenshots and input reach the lock screen instead. Turn it off with **View > Enhanced Session** in Virtual Machine Connection, or turn off **Allow enhanced session mode** in the Hyper-V host settings.
- Typing or clicking in the console window while the AI works mixes your input with the AI's.

### Allow `admin` exec without a prompt

`vm_exec` with `admin: true` starts the command elevated through the `runas` verb. If UAC prompts administrators for consent, the command waits for that prompt in the guest until it times out. To elevate without a prompt, set the following in the guest (from an elevated PowerShell):

```
Set-ItemProperty -Path HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System -Name ConsentPromptBehaviorAdmin -Value 0
```

The logged-on user must be a member of the Administrators group.

The change itself needs elevation, and it can be made through HyperHand once the agent is installed:

1. Request the change elevated; this opens a UAC prompt in the guest. Give the call a timeout long enough to answer the prompt, since the command waits for it:

   ```
   vm_exec  command: Start-Process reg.exe -Verb RunAs -Wait -ArgumentList 'add HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System /v ConsentPromptBehaviorAdmin /t REG_DWORD /d 0 /f'
            timeout_ms: 60000
   ```

2. While it waits, answer the prompt from the host: `vm_observe` (without `handle`) shows it on the secure desktop; `vm_key shift+tab` moves the focus from No to Yes, then `vm_key enter` confirms. Host-side input reaches the secure desktop, unlike the agent; use `vm_key` and `vm_click` without `observation_id` or `handle` there.
3. Check: `vm_exec` with `(Get-ItemProperty HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System).ConsentPromptBehaviorAdmin` returns `0`.

See [UAC](#uac) for every place a prompt can appear.

### Set the guest default input method to English

If the guest uses a non-English input method by default, add the en-US language and make the US keyboard the default input method (in the guest, as the logged-on user):

```
$list = Get-WinUserLanguageList
$list.Add("en-US")
Set-WinUserLanguageList $list -Force
Set-WinDefaultInputMethodOverride -InputTip "0409:00000409"
```

Sign out and back in for the change to apply.

## UAC

Where UAC prompts appear, and where they do not:

| When | Where | Prompt |
|---|---|---|
| `hyperhand.exe install` (including updates) | Host | Once per install/update, for the service, protected files and logon task. |
| `hyperhand.exe uninstall` | Host | Once; it relaunches itself elevated. |
| Starting the installed `hyperhand.exe`, at logon or by hand | Host | None. The tray runs as the ordinary user. |
| Restarting from the tray menu | Host | None. Only the tray/MCP process restarts; active requests are interrupted. |
| Development installs with `go run ./cmd/hyperhand dev-install` | Host | None, through the preauthorized task `HyperHand Dev Install`. Registering that task once (`scripts\dev-install-setup.ps1`, elevated) is the only prompt. |
| `vm_install_agent`, `hyperhand-agent.exe install`, `hyperhand-agent.exe uninstall`, `vm_update_agent`, the agent at logon | Guest | None. The agent runs as the logged-on user, not elevated. |
| `vm_exec` with `admin: true` | Guest | None if `ConsentPromptBehaviorAdmin` is `0` (see [Allow `admin` exec without a prompt](#allow-admin-exec-without-a-prompt)); otherwise a prompt that the command waits for. |
| A program started in the guest that asks for elevation | Guest | As configured in the guest; answer it from the host with `vm_observe` and `vm_key` (or `vm_click` with raw screen pixels). |

Host-side screenshots and input work on the guest's secure desktop, so a guest UAC prompt can always be answered through HyperHand. The agent cannot see or answer it.

## Using the tools

Every result is one JSON object; `vm_observe` and actions that observe afterwards put a PNG before it. Every refusal is an error result whose text is `{"error": "<code>", "reason": "...", "next": "...", "run_id": "...", ...}`: `next` names the call that makes progress, the other fields carry the handles and facts it needs. The codes are listed in [features.md](features.md#22-error-object-and-codes).

### Observing and acting

Scripts and agents that have no MCP connection of their own should call HyperHand through the official [Python client](../client/README.md) (`client/hyperhand_client.py`): it keeps a stable `task_id`, passes Windows paths without quoting problems, prints UTF-8 JSON, turns error results into failures, saves screenshots and finds controls.

Every tool accepts an optional `task_id`. A persistent MCP session uses its own default task when it is omitted. For scripts that reconnect for each call, or multiple agents sharing one session, choose a unique `task_id` and pass it on every call, including `vm_observe`, actions and `vm_end_turn`. Results return the task and its `run_id`; do not reuse another task's observations. A task keeps exclusive write ownership of a VM until cleanup, while other tasks can still read its state. A conflicting write returns `vm_busy` with the owner's `owner_task_id`, `owner_idle_ms` and `owner_in_flight` (`vm_status` shows the same as `owner`); when the owner is clearly abandoned, `vm_end_turn` with its `task_id` and `vm` releases the VM. Deleting an MCP session ends its default task once its calls have returned, so a script that opens one session per call and omits `task_id` never leaves a VM owned, but also cannot keep ownership, observations or temporary checkpoints between calls. A stateless call without an explicit ID returns `task_required`. Task state is held in memory until the host exits.

1. Get a window handle: `vm_windows` lists the visible windows with `handle`, `pid`, `process`, `title`, `rect`, `group_root` (windows with the same group root belong together, such as AutoCAD's main window and its command line) and `integrity`, plus the foreground handle, the focused control and the session state; `vm_launch` starts a program and returns its first window's handle. Titles are not selectors: pick the handle yourself from the list.
2. Observe: `vm_observe` with `handle` returns a PNG cropped to the window, the window, the focused control (`focused`) and an `observation_id`. Add `controls: true` for the UI Automation tree as indexed text, one control per line: `[12] Edit "" id=cmdline (0,980 1920x60) focused value="LINE"`. `diff_from` with the previous `observation_id` returns only what changed. Without `handle` it observes the whole screen, which also works on the lock screen and UAC prompts (the agent is then reported `offline`). `max_size` shrinks the image.
3. Act with the observation: `vm_click` with `observation_id` and `x`, `y` (pixels of that image) or `index` (a control of that tree); `vm_set_value` and `vm_invoke` with `index`; `vm_type` with `handle` or with `observation_id` and `index`; `vm_key` with `handle`. The host maps the pixels back to the screen and re-finds the control by its runtime ID, so a control that moved is still hit where it is now. It refuses a locked or non-console session (`session_unusable`) and an observation whose window moved, resized, minimized or closed (`stale_observation`), activates the window (`activate: true` by default), then refuses a disabled window (`target_disabled`), a point covered by another window (`covered`) or a window running at higher integrity than the agent (`integrity_mismatch`); each refusal names the window to act on next. Whole-screen pixels also receive revision and local screenshot checks; an `index` from its tree targets the window the tree belongs to. After input or command execution, obtain a new observation before using coordinates again. Stable runtime-ID controls can still be re-located after ordinary input; VM lifecycle changes invalidate them too.
4. Read the result: `window` is the window that received the input, and `after` is a new observation taken 300 ms later (`observe_after`: `screenshot` by default for mouse and control actions, `none` for `vm_type` and `vm_key`; `controls` or `both` include the tree; `settle_ms` changes the wait). Decide the next step from `after`; call `vm_observe` again only when you need more than it shows.

Prefer control actions where the tree has the control: `vm_set_value` sets a text box through UI Automation and reports `verified`; `vm_invoke` presses buttons and menu items (`Invoke`), toggles check boxes (`Toggle`), opens and closes nodes (`Expand`, `Collapse`), selects list and tab items (`Select`) or scrolls an item into view. Each control's `actions=[...]` lists its supported operations (`SetValue` uses `vm_set_value`); when a control has exactly one action other than `SetValue`, `vm_invoke` may omit `action` and uses it; `state={...}` reports readable toggle, expansion, selection, read-only and offscreen state. Missing fields mean unknown, and capability lists do not override disabled/read-only state. Custom-drawn surfaces such as a CAD drawing area expose no controls; click them by image pixels.

For large windows, use `vm_find_controls` with a window `handle` and an exact `automation_id`, `control_name` or `control_type` (such as `Edit`). Supplied filters use AND. The result reports `unique`, `multiple`, `not_found` or `incomplete`; incomplete searches do not prove absence or uniqueness. Each match has an `index` under the returned `observation_id`, ready for `vm_set_value`, `vm_invoke`, `vm_wait` or other control actions. To inspect just that control's subtree, call `vm_observe` with its `observation_id` and `index`, optionally `screenshot: false`; controls are implied. The subtree root is index 0, and the screenshot, if requested, still shows the owning window. `vm_find_controls` also accepts this pair to search within a known subtree. If a control is removed, find it again; an old index never silently selects a replacement. New search/subtree operations require an updated guest (`vm_update_agent` on `agent_outdated`).

Control actions return the actual post-action `state`. `verified:true` means the read-back matches the action, `false` means a readable mismatch, and `null` means it could not be verified. Toggle compares before/after state; Expand, Collapse, Select and ScrollIntoView check their target states. The agent polls for up to 250 ms when needed without replaying the action. Invoke remains `null` because a button's business effect cannot be inferred generically; inspect its result with `observe_after` or another observation.

For a scrollable container, use its control `index` with `vm_invoke` action `ScrollUp`, `ScrollDown`, `ScrollLeft` or `ScrollRight`. Each call requests one small UIA step. Available directions appear in `actions`; `state` reports readable axis support and scroll percentages from 0 to 100. `verified:true` confirms movement in that direction; an unchanged boundary returns `false` with its actual position. `vm_scroll` remains available for coordinate-based wheel input on surfaces without ScrollPattern.

Without `observation_id`, `handle` or `pid`, `vm_click`, `vm_drag`, `vm_scroll`, `vm_key` and ASCII `vm_type` send raw input to the VM console at screen pixels, with no checks and without the agent. Use that only for the sign-in screen and UAC prompts.

### Other tools

For a deployment directory that must exactly match a host build, use `vm_push` in two calls:

```json
{"task_id":"deploy-1","vm":"Win10","host_path":"V:\\Build\\App","guest_path":"C:\\Test\\App","mode":"mirror","phase":"plan"}
```

Inspect the returned `changes` and `summary`, including every deletion, then repeat those arguments with `"phase":"apply","plan_id":"<returned plan_id>"`. This mirrors the directory contents, including empty directories; an empty source removes everything inside the target. The target's parent directory must already exist. Keep logs, drawings and user settings outside a mirrored build directory.

The plan is read-only and does not take VM write ownership. Apply takes ownership like ordinary uploads. Use the same task ID, paths and `force` value; plans expire after 10 minutes, are lost on host restart or task cleanup, and only the newest 16 plans are retained across tasks. Applying consumes the plan, even on failure. `plan_stale` means the plan can no longer be used. `mirror_partial` includes `completed`, `failed` and `pending`; `mirror_unknown` means the response was lost or unrecognized and the listed changes have unknown outcomes. In every case, create a new plan before continuing. Mirror requires an updated guest agent; `agent_outdated` asks for `vm_update_agent`, never silently falls back to copying.

Mirror stages changed file bytes on the host and guest, so leave temporary disk space available. It is not a whole-directory transaction or a file-lock workaround: close applications using the deployment files before applying. Details and limits are in [Directory mirror](features.md#65-directory-mirror).

- `vm_type` types Unicode text as key events in the user's session (`\n` presses Enter, `\t` Tab) and never touches the clipboard. Without a target it falls back to the Hyper-V keyboard for ASCII text when the agent is absent or its session is locked or on the secure desktop. With `observation_id` and `index` it reads the control back afterwards: `verified` says whether its `value` contains the typed text. The result has `applied_chars` and `total_chars`; a stop midway is `partial_input` with the same counts. Inspect the window before retyping; never retry blindly. Input popups of the target that take the foreground while you type, such as AutoCAD's dynamic-input tooltip at the cursor, keep receiving the text; a dialog, palette or other window stops it. A dialog opened by an Enter appears after the agent has already typed what follows, and the application may drop those characters: end the call at an Enter that opens a dialog, then act on the dialog.
- `vm_key` takes either `keys`, such as `ctrl+s`, or `sequence`, such as `["ctrl+a", "backspace"]` (up to 256 combinations). Key names include the numeric keypad (`num0`, `numenter`, ...), `f1` to `f20` and X11-style aliases (`Return`, `Escape`, `KP_Enter`); use `plus` for the `+`/`=` key, for example `ctrl+plus`. With `handle` the sequence stops as soon as another window takes the foreground (`partial_input`).
- `vm_apps` finds desktop programs without guessing their install paths. For example, call `vm_apps {"vm":"Win10","query":"AutoCAD"}`; reuse a returned `windows[].handle` with `vm_observe`, or pass an entry's `launch` object (`path`, `args`, `cwd`) to `vm_launch` with the same VM. `limit` defaults to 50 (maximum 200); `total`, `truncated` and `warnings` show whether the result is complete. Discovery covers Start Menu executable shortcuts and App Paths, not packaged UWP/MSIX apps or every executable on disk.
- `vm_exec` runs a command to completion as the logged-on user with `powershell` (default) or `cmd`; `timeout_ms` defaults to 60 seconds and `timed_out` says whether the process tree was killed. It is not for GUI programs: `vm_launch` starts one detached and waits up to `wait_window_ms` (60 seconds) for its window.
- For commands that run minutes (test suites, capability checks), call `vm_exec` with `background: true`: it returns a job `id` at once (default timeout 1 hour, at most 24 hours). Then call `vm_job {"vm":"Win10","job_id":"<id>","wait_ms":30000}` repeatedly, passing the previous result's `stdout_next` and `stderr_next` as `stdout_offset` and `stderr_offset`, until `complete` is `true`; `state` and `exit_code` give the outcome. `vm_job` with `cancel: true` terminates the job's process tree. A job survives a lost connection, a new task and a host restart, so a script can collect the result later with the same `id` (reading needs no write ownership); restarting the agent (`vm_update_agent`, reboot) forgets the job (`no_job`) without stopping it. `vm_job` without `job_id` lists the agent's jobs.
- `vm_push` and `vm_pull` copy a file or a directory recursively. A single file pushed to a guest path ending in `\` goes into that directory under its own name; a single file pulled to an existing host directory, or to a path ending in `\`, goes into it under the guest file's name. `vm_push` skips files whose SHA-256 already matches the guest copy unless `force` is true. Files are written to unique `.hyperhand-*.hhpart` temporary files in the destination directory and renamed when complete.
- `vm_file_info` checks guest files without copying them, for example after an installation: `vm_file_info {"vm":"Win10","paths":["%ProgramFiles%\App\app.exe","%APPDATA%\App\settings.json"]}` returns per path `exists`, `type` (`file` or `dir`), `size`, `sha256`, the PE `version` (ProductVersion) when the file has one, and `modified` (UTC). Environment variables are expanded as the logged-on guest user; `resolved` shows the result. A missing path is `exists: false`, not an error; a locked or unreadable file has `exists: true` and an `error` for that entry. Up to 64 paths per call; it is read-only and needs no write ownership.
- `vm_wait` waits for `process_exit` or `process_running` (with `name`, for example `notepad`) or `file_exists` (with `path`, for example `C:\temp\app\done.txt`). UI kinds are `window_exists`, `window_gone`, `window_foreground`, `control_exists`, `control_gone` and `control_matches`. Select a window by `handle`/`pid`; select a control by `observation_id`/`index` or by `handle`/`pid` plus exact `automation_id`/`control_name`. For example, `{"vm":"Win10","kind":"control_matches","handle":123,"automation_id":"status","enabled":true,"value":"Ready","timeout_ms":10000,"assert":true}` waits for the selected control to become enabled with the exact value `Ready`. Use actual handles and control properties from your VM. `check_only: true` samples once; `assert: true` returns `assertion_failed` if unsatisfied. The default timeout is 60 seconds; ordinary expiration returns `satisfied: false`. UI results include `last` with actual state or an uncertainty reason. Unknown or truncated data does not establish a match or disappearance. See the [full wait contract](features.md#73-vm_wait).
- `vm_start` returns once the desktop is usable; if it fails, the VM may still be running, and the error says why (`agent_required`: no agent answer; `agent_outdated`: run `vm_update_agent`; `session_unusable`: locked without a stored password, or the unlock failed). `vm_status` reports the state without changing anything (`agent.state` `busy` means another request of this host holds the agent connection; `not_answering` does not prove the agent is offline); `vm_unlock` unlocks a session that was locked later.
- Checkpoints have their own section below.
- `vm_doctor` runs read-only checks on the host (service, MCP listener, service pipe, Hyper-V socket registration, enhanced session mode) and the VM (power, agent version and protocol, session, agent integrity) and gives a suggestion for every `warn` or `fail`. Run it first when anything looks wrong.

### Checkpoints

Checkpoints are the AI's safety net for risky tests. HyperHand names the ones it creates `<run_id>-temp-<label>` (a rollback point for this run; `vm_end_turn` deletes it) or `<run_id>-keep-<label>` (a baseline kept across runs); anything else is a `manual` checkpoint made in Hyper-V Manager, which HyperHand never deletes on its own. The workflow:

1. Before a risky step (installing a build, changing settings, a destructive test), call `vm_checkpoint` with a `label` that says what the state is, for example `before-install`. The result's `id` is the stable selector; names may repeat in Hyper-V, so keep the id rather than the name.
2. Run the test and verify the result.
3. Passed and worth keeping? `vm_checkpoint_keep` with that `id` renames the checkpoint from `temp` to `keep`, optionally with a new `label` such as `golden`. It then survives `vm_end_turn` until you delete it with `vm_checkpoint_delete`.
4. Failed, or you want to try again from the same point? `vm_restore` with the `id` rolls back. Restoring replaces the VM's current state, so pass `save_current: true` when that state still matters; it is saved first as `<run_id>-temp-before-restore` and reported as `saved_current`. The result tells you the VM's power state and reminds you to call `vm_start` so that the desktop is usable; a standard checkpoint of a running VM resumes directly, a production or off one is started.
5. Clean up: `vm_end_turn` (usually from the Stop hook, see below) deletes this run's `temp` checkpoints. After a crashed or restarted server, `vm_end_turn` with `all_temp: true` deletes the `temp` checkpoints of every run on the VM named by `vm`, which `all_temp` requires; use it only when no other HyperHand client is working on that VM. Checkpoints it could not delete are listed in `skipped`.

`vm_checkpoints` shows the tree: `checkpoint_type` (the VM's Hyper-V setting, which decides whether a new checkpoint is `standard` or `production`), `current_parent` (the checkpoint the VM's current state branches from) and, per checkpoint, `id`, `name`, `parent`, `created_at`, `type`, `kind`, `state`, `current` and `children`. If `checkpoint_type` is `Disabled`, enable checkpoints in the VM's settings (Hyper-V Manager, Settings > Checkpoints) before `vm_checkpoint` can work.

Deleting a checkpoint (`vm_checkpoint_delete`, `vm_end_turn`) merges its disk differences into its children or into the current disk and re-parents its children; with `subtree: true` the whole branch goes. The merge can take minutes for large differences, and the call waits for it. A `manual` checkpoint can be deleted by `id` only; a name that several checkpoints share is refused with `ambiguous_target` and their `ids`.

### Ending a turn: vm_end_turn and hooks

`vm_end_turn` cancels this task's pending `vm_wait` calls, waits for its active calls to finish, deletes its temporary checkpoints and releases its VM ownership. With `vm`, cleanup is limited to that VM and the task can continue on others; without `vm`, the whole task ends. `keep` and manual checkpoints, detached programs and VM power state are untouched. Ended explicit task IDs cannot be used for new work: choose a new ID for the next task. Cleanup failures retain ownership for retry. `all_temp: true` also removes older runs' temporary checkpoints on the VM named by `vm` (required with `all_temp`), but refuses to cross another task's active write ownership.

The examples in [docs/hooks/](hooks/) call `vm_end_turn` without arguments, so without `vm` they end the whole task; they apply only when the hook uses the same persistent MCP session and default task as the work. `claude-code-settings.json` provides `Stop`; `codex-plugin.json` provides `Stop` and `Interrupt`. There is no unscoped `SubagentStop` hook: it could clean up the parent task when agents share a session. For explicit tasks or hooks that reconnect, the caller must pass the matching `task_id`; a hook adapter must obtain it from the task that actually ended, rather than guessing from a VM name or cleaning every task.

## Updating

### From a release

1. Download the new zip from [Releases](https://github.com/n2ns/hyper-hand/releases) and extract both executables together.
2. Finish active VM tool requests, then run the extracted `hyperhand.exe install` and approve UAC. The installer updates the protected installation and restarts the host components. Existing highest-privilege tray tasks are migrated to the ordinary logon task.
3. Reconnect the MCP client after the tray returns.
4. Ask the AI to call `vm_update_agent` for each VM. It sends the installed `hyperhand-agent.exe` to the running agent, which replaces itself and restarts. Until then, tools refuse an agent that speaks an older protocol with `agent_outdated`.

### From source

1. Once per machine and developer, register the development install task from an elevated PowerShell (the only UAC prompt):

   ```
   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev-install-setup.ps1
   ```

   It registers `HyperHand Dev Install`: an on-demand task without triggers that runs `build\dev-install\hyperhand.exe install --quiet` of this checkout with your elevated token. Anyone who can replace that file and start the task runs code as an administrator; remove the task with `-Mode Remove` when you no longer develop HyperHand on this machine. An existing task of that name that runs something else is never replaced.

2. Build and install without UAC, from the repository root:

   ```
   go run ./cmd/hyperhand dev-install
   ```

   It builds both executables into `build\dev-install` (version `dev-<yyyyMMdd-HHmmss>-<commit>`, with `-dirty` when `cmd`, `internal`, `go.mod` or `go.sum` have uncommitted changes), starts the task, waits for it (`--timeout`, default 180 seconds), and checks that the installed files equal the build, the service runs and one installed tray process is up. It prints progress and a final result line, writes errors to stderr and exits with a nonzero code on failure. `--no-build` installs the files already there. On failure it prints the installer's last log lines; if the service is left stopped, running it again with `--no-build` restores it. Installing interrupts active MCP connections. It does not terminate processes by executable name.

3. Ask the AI to call `vm_update_agent`. The host sends the `hyperhand-agent.exe` next to `hyperhand.exe` to the running agent, which replaces itself, restarts, and is pinged until it answers (up to 30 seconds). Repeat for each VM.

`vm_update_agent` needs a running agent. If the agent does not answer, use `vm_install_agent` instead.

## Releasing

For maintainers. Add a section for the version to `CHANGELOG.md`, commit, and push a tag:

```
git tag v0.1.0
git push origin v0.1.0
```

Pushing a `vX.Y.Z` tag runs a GitHub Actions workflow that runs the tests, builds `hyperhand.exe` and `hyperhand-agent.exe` for Windows amd64 with the version stamped in, and publishes a GitHub Release with `hyperhand-X.Y.Z-windows-amd64.zip` (both exes, `README.md` and `CHANGELOG.md`). The release notes are the version's section of `CHANGELOG.md`.

## Uninstalling

Uninstall the guest agent first, then the host.

In each guest, as the user the agent was installed for, run:

```
%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe uninstall
```

Run it in the guest itself: by hand, or from the host with `vm_key` `win+r`, `vm_type` for the command and `vm_key` `enter`. Do not run it through `vm_exec`: the uninstall stops the other running agent instances, including the one executing the command. It needs no elevation. It:

- stops the other running agent instances;
- deletes the `HyperHandAgent` value under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`;
- deletes `%LOCALAPPDATA%\HyperHand` and `C:\Users\Public\HyperHand`. A running program's file cannot be deleted, so it first moves its own executable out of the folder to `%TEMP%\hyperhand-agent-uninstalled-<number>.exe`, (or to the folder above if `%TEMP%` is on another drive), which you can delete afterwards;
- shows a message box listing what was removed.

On the host:

1. Run:

   ```
   hyperhand.exe uninstall
   ```

   It requests elevation if needed, stops the installed tray and service, removes `HyperHandService` and its Hyper-V Administrators membership, the `HyperHand` logon task, the `HyperHand Console` task and the Hyper-V socket registration. A protected cleanup helper waits for the installed executable to exit and removes the installed host and agent executables only if their hashes still match. The helper, `%ProgramData%\HyperHand\uninstall-cleanup.exe`, cannot delete itself; it is deleted at the next restart.

   User logs under `%LOCALAPPDATA%\HyperHand`, files inside guests and nonempty `%ProgramData%\HyperHand\service-data` are preserved. When service data remains, `config.json` is retained as the owner marker for reinstallation. Otherwise it removes the owner configuration and empty installation/data directories. It does not claim to remove directories containing other files.
2. Delete the folder with `hyperhand.exe` (the extracted release or the build directory).
3. Remove the server from your MCP client: `claude mcp remove hyperhand` (Claude Code) or `codex mcp remove hyperhand` (Codex).

## Troubleshooting

### Start with vm_doctor

`vm_doctor` checks the host (`HyperHandService`, the MCP listener, the service pipe, the Hyper-V socket registration, enhanced session mode) and the VM (power, agent version and protocol, session, agent integrity) without changing anything. Every `warn` or `fail` comes with a suggestion: the command or tool call that fixes it. Run it before anything below; the sections name the checks that cover them.

### The agent does not answer

Tools that need the agent refuse with `agent_required`, or `vm_install_agent` reports that the agent did not answer within 30 seconds (`vm_doctor`: `guest.agent`, `host.hvsocket`).

- Check that a user is logged on in the guest and that the HyperHand tray icon is present there. The agent runs only in a logged-on desktop session.
- Check that the host tray and `HyperHandService` are running. Socket registration happens during installation, not normal tray startup; rerun `hyperhand.exe install` to repair an incomplete installation.
- If the VM was restored to a checkpoint taken before the agent was installed, install it again with `vm_install_agent`.
- Call `vm_observe` (without `handle`; it works without the agent) to see whether the install command reached the Run dialog intact.

### Tools refuse with agent_outdated

The guest agent speaks an older protocol than the host (`vm_doctor`: `guest.agent`). The host pings every new agent connection first and refuses every request except the ping and the agent update, so only `vm_status`, `vm_doctor` and `vm_update_agent` work until the agent is replaced. Call `vm_update_agent`; if the agent does not answer at all, `vm_install_agent`. There is no compatibility mode for older agents.

### Typed text is garbled or missing

The host keyboard sends key strokes, and a non-English IME in the guest can swallow or convert them. This affects `vm_install_agent` and `vm_type` without the agent. Switch the guest IME to English mode, or set English as the default input method (see above). [Installing the agent manually](#install-the-guest-agent-manually) avoids the problem for the install. With the agent installed, `vm_type` injects Unicode key events in the user's session and is not affected.

### An action is refused

Read the error object: `next` says what to do and the fields name the window involved.

- `stale_observation`: the observation belongs to another task, was evicted (8 per VM per task), the window identity or geometry changed, or its revision/local screenshot check failed. Call `vm_observe` again with the same `task_id` and use the new ID. Coordinate checks compare only the target neighbourhood; they do not prove the whole page is unchanged.
- `activate_failed`, `target_disabled`, `covered`: another window holds the foreground or covers the point, usually a dialog; `foreground`, `act_on` or `window` names it. Act on that handle first (or `vm_key esc` for a shell overlay), then observe again.
- `session_unusable`: the session is locked (`vm_unlock`), is not the console session (see the next section) or shows a UAC prompt (answer it with `vm_key` or `vm_click` without `observation_id`).
- `integrity_mismatch`: the program runs elevated and the agent does not. Start it without administrator rights, or with `vm_launch` and `admin: true`.
- `partial_input`: some text or keys arrived. Observe the window before sending the rest; do not repeat the whole input.

### The screenshot is black or shows the lock screen

- The VM must be running. A stopped or saved VM has no video output.
- VMConnect must be in basic session mode. In an enhanced session, the host-side screenshot and input go to the console session, which is locked, while the agent sees the user's desktop; actions then refuse with `session_unusable` (`vm_doctor`: `guest.session`, `host.enhanced_session`).
- If the guest screen is locked, run `vm_unlock` with an unlock password stored in the tray (see [Get a usable desktop after `vm_start`](#get-a-usable-desktop-after-vm_start)), or unlock it in VMConnect. If the display is off, wake it with `vm_click` or `vm_key` without `observation_id`.

### Port 8770 is already in use

The settings window shows the error as the server status, the tray icon's tooltip says that the MCP server is not running, and the log records it. Choose another port as described in [Using a different port](#using-a-different-port), then re-add the server in your MCP client with the new URL.

### `admin` exec hangs or times out

The elevation is waiting for a UAC consent prompt in the guest. Set `ConsentPromptBehaviorAdmin` to `0` as described in [Allow `admin` exec without a prompt](#allow-admin-exec-without-a-prompt). Commands still pending at the timeout are terminated.

### `vm is required: there is no default VM`

Every tool except `vm_list` needs `vm`, whatever the number of VMs or which of them are running. Pass one of the names listed in the error's `next` and `vms` field (or from `vm_list`). `vm_end_turn` without `vm` still ends the whole task; with `all_temp: true` it needs `vm`.

### The host service is unavailable

Check `HyperHandService` in Windows Services. Normal tray startup does not repair or elevate the service. Rerun `hyperhand.exe install` with administrator approval to repair the installation. Use the configured owner's account for the tray; another account is not automatically granted broker access. Do not add the human user to Hyper-V Administrators as a workaround.

### Log file

The host tray writes its log to `%LOCALAPPDATA%\HyperHand\hyperhand.log` for the user it runs as. It records the listening address and startup, connection and install errors.

For service startup or broker failures, inspect `%ProgramData%\HyperHand\service-data\broker.log` with administrator access. The service limits it to approximately 1 MiB. Errors may also appear in Event Viewer under **Windows Logs > Application**, with source `HyperHandService`; use the file log if that source is unavailable.
