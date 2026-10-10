# 9. Host Tray

Part of [HyperHand features](../features.md). Section numbers such as 8.6 refer to the chapters listed there.

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
