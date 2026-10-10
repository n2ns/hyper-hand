# User Guide

This guide covers downloading HyperHand, installing the host tray and the guest agent, connecting an MCP client (Claude Code, Codex or another client), using the tools, updating, uninstalling, and troubleshooting. Building from source, development installs and releasing are in [Building HyperHand](building.md).

HyperHand consists of a tray program and a Windows service on the host, and a small agent inside each guest; see [How it works](../README.md#how-it-works).

## Requirements

- Host: Windows 10 or Windows 11 Pro or Enterprise with Hyper-V enabled. Installation, updates and uninstallation require administrator approval; normal tray startup and restart do not.
- Guest: a Windows VM with a user logged on to the desktop.
- VMConnect in basic session mode. In an enhanced session, the guest user session moves to RDP, and the host-side screenshot and input reach the console session, which shows the lock screen. Switch with View > Enhanced Session in VMConnect, or close VMConnect while HyperHand is working.

## 1. Download

Download `hyperhand-X.Y.Z-windows-amd64.zip` from [Releases](https://github.com/n2ns/hyper-hand/releases). It contains `hyperhand.exe`, `hyperhand-agent.exe` (both Windows amd64, with the version stamped in), `README.md` and `CHANGELOG.md`. Extract both executables to the same folder. The installer copies them to `%ProgramFiles%\HyperHand`; the logon task uses that installed copy.

To build from source instead, see [Building HyperHand](building.md#build).

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
6. Starts the service and ordinary tray.

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

The logged-on user must be a member of the Administrators group. Run the command in the guest from a PowerShell started as administrator, or ask the AI to make the change: it can request it elevated and answer the UAC prompt from the host.

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
| `vm_install_agent`, `hyperhand-agent.exe install`, `hyperhand-agent.exe uninstall`, `vm_update_agent`, the agent at logon | Guest | None. The agent runs as the logged-on user, not elevated. |
| `vm_exec` with `admin: true` | Guest | None if `ConsentPromptBehaviorAdmin` is `0` (see [Allow `admin` exec without a prompt](#allow-admin-exec-without-a-prompt)); otherwise a prompt that the command waits for. |
| A program started in the guest that asks for elevation | Guest | As configured in the guest; answer it from the host with `vm_observe` and `vm_key` (or `vm_click` with raw screen pixels). |

Host-side screenshots and input work on the guest's secure desktop, so a guest UAC prompt can always be answered through HyperHand. The agent cannot see or answer it.

## Using HyperHand

Ask the AI in plain words what to do in a VM. It gets the description of every tool from HyperHand itself; the tools and their exact behavior are listed in [Features](features.md). Scripts that have no MCP connection of their own use the [Python client](../client/README.md).

### Clean up after every turn: hooks

Temporary checkpoints and the AI's hold on a VM are released when the AI calls `vm_end_turn`. To have that happen after every turn, add it as a hook in your MCP client.

The examples in [docs/hooks/](hooks/) call `vm_end_turn` without arguments, so without `vm` they end the whole task; they apply only when the hook uses the same persistent MCP session and default task as the work. `claude-code-settings.json` provides `Stop`; `codex-plugin.json` provides `Stop` and `Interrupt`. There is no unscoped `SubagentStop` hook: it could clean up the parent task when agents share a session. For explicit tasks or hooks that reconnect, the caller must pass the matching `task_id`; a hook adapter must obtain it from the task that actually ended, rather than guessing from a VM name or cleaning every task.

## Updating

### From a release

1. Download the new zip from [Releases](https://github.com/n2ns/hyper-hand/releases) and extract both executables together.
2. Finish active VM tool requests, then run the extracted `hyperhand.exe install` and approve UAC. The installer updates the protected installation and restarts the host components. Existing highest-privilege tray tasks are migrated to the ordinary logon task.
3. Reconnect the MCP client after the tray returns.
4. Ask the AI to call `vm_update_agent` for each VM. It sends the installed `hyperhand-agent.exe` to the running agent, which replaces itself and restarts. Until then, tools refuse an agent that speaks an older protocol with `agent_outdated`.

### From source

See [Install a development build](building.md#install-a-development-build).

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

### Typed text is garbled or missing

The host keyboard sends key strokes, and a non-English IME in the guest can swallow or convert them. This affects `vm_install_agent` and `vm_type` without the agent. Switch the guest IME to English mode, or set English as the default input method (see above). [Installing the agent manually](#install-the-guest-agent-manually) avoids the problem for the install. With the agent installed, `vm_type` injects Unicode key events in the user's session and is not affected.

### The screenshot is black or shows the lock screen

- The VM must be running. A stopped or saved VM has no video output.
- VMConnect must be in basic session mode. In an enhanced session, the host-side screenshot and input go to the console session, which is locked, while the agent sees the user's desktop; actions then refuse with `session_unusable` (`vm_doctor`: `guest.session`, `host.enhanced_session`).
- If the guest screen is locked, run `vm_unlock` with an unlock password stored in the tray (see [Get a usable desktop after `vm_start`](#get-a-usable-desktop-after-vm_start)), or unlock it in VMConnect. If the display is off, wake it with `vm_click` or `vm_key` without `observation_id`.

### Port 8770 is already in use

The settings window shows the error as the server status, the tray icon's tooltip says that the MCP server is not running, and the log records it. Choose another port as described in [Using a different port](#using-a-different-port), then re-add the server in your MCP client with the new URL.

### `admin` exec hangs or times out

The elevation is waiting for a UAC consent prompt in the guest; if nobody answers it in time, the call fails with `elevation_timeout` and the command does not run. Set `ConsentPromptBehaviorAdmin` to `0` as described in [Allow `admin` exec without a prompt](#allow-admin-exec-without-a-prompt). Commands still pending at the timeout are terminated.

### The host service is unavailable

Check `HyperHandService` in Windows Services. Normal tray startup does not repair or elevate the service. Rerun `hyperhand.exe install` with administrator approval to repair the installation. Use the configured owner's account for the tray; another account is not automatically granted broker access. Do not add the human user to Hyper-V Administrators as a workaround.

### Log file

The host tray writes its log to `%LOCALAPPDATA%\HyperHand\hyperhand.log` for the user it runs as. It records the listening address and startup, connection and install errors.

For service startup or broker failures, inspect `%ProgramData%\HyperHand\service-data\broker.log` with administrator access. The service limits it to approximately 1 MiB. Errors may also appear in Event Viewer under **Windows Logs > Application**, with source `HyperHandService`; use the file log if that source is unavailable.
