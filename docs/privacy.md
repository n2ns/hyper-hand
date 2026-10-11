# Privacy

HyperHand runs entirely on your machine and its Hyper-V VMs.

## Network

- HyperHand collects no telemetry and makes no outbound network connections.
- The host MCP server listens on `127.0.0.1` only (port 8770 by default, set with `-port`). It is not reachable from other machines.
- The ordinary host tray talks to `HyperHandService` through a local named pipe. The service tunnels guest traffic over the fixed HyperHand Hyper-V socket service. It does not pass through the guest's or the host's network adapters.
- Copying the agent into the guest uses the Hyper-V Guest Service Interface (`Copy-VMFile`).

## Data handled

Screenshots, typed text, clipboard contents, command output and file contents pass between the guest, the host service, the host tray and the MCP client that requested them. A stored unlock password passes only from Windows Credential Manager through the tray and the service to the VM's keyboard; it is not returned to MCP clients or written to logs. Host file reads and writes for `vm_push` and `vm_pull` run under the tray user's permissions. The service has its own temporary working directory; see below for local storage.

## Data stored locally

On the host:

- Log file: `%LOCALAPPDATA%\HyperHand\hyperhand.log` (listening address, errors).
- Installed executables: `%ProgramFiles%\HyperHand\hyperhand.exe` and `hyperhand-agent.exe`.
- Windows service `HyperHandService`, running automatically as `NT SERVICE\HyperHandService`; that dedicated account is added to Hyper-V Administrators.
- `%ProgramData%\HyperHand\config.json`, including the installing user's SID used for local broker access control. This configuration is protected from ordinary user changes.
- `%ProgramData%\HyperHand\service-data`, the service's writable working directory, including temporary staging for guest agent installation.
- Service error log: `%ProgramData%\HyperHand\service-data\broker.log`, with an approximately 1 MiB size limit. Service errors may also appear in the Windows Application event log under `HyperHandService`; use the file log if the event source is unavailable.
- Unlock passwords set in the tray's settings window: generic credentials `HyperHand:<VM name>` in Windows Credential Manager, for the current user on this machine (protected by Windows for that user). Remove them with **Clear unlock password** in the tray or in Credential Manager; uninstallation does not remove them.
- Scheduled task `HyperHand`, created by `hyperhand.exe install` to start the ordinary tray for the installing user at logon, with least privilege.
- Scheduled task `HyperHand Console`, created by `hyperhand.exe install` for the installing user: no trigger; when run, it starts `vmconnect.exe localhost "<VM>"` with that user's highest privileges (see Access control).
- `%LOCALAPPDATA%\HyperHand\settings.json`: the VMs whose console opens when `vm_start` starts them.
- Registry key `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\3ce544e1-2645-4383-b332-fedf8a18736b`, which registers the Hyper-V socket service.
- Unique `.hyperhand-*.hhpart` files in the destination directory while `vm_pull` writes a file; renamed to the target on completion, deleted on failure.
- `hyperhand-mirror-*.hhpart` in the user's temporary directory while a mirror apply captures changed file bytes. Mirror plans (paths, sizes and hashes) remain in host memory for at most 10 minutes of usability, bounded to 16 plans; task cleanup discards them.

In the guest:

- The agent at `C:\Users\Public\HyperHand\hyperhand-agent.exe` (copied by `vm_install_agent`) and `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe` (installed copy).
- Registry value `HyperHandAgent` under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`.
- Unique `.hyperhand-*.hhpart` files in the destination directory while `vm_push` writes a file; renamed to the target on completion, deleted on failure.
- `hyperhand-mirror-payload-*` files and `hyperhand-mirror-*` staging directories in the agent user's temporary directory during mirror apply. Host and guest staging is removed on normal completion, handled errors and cancellation; abrupt process termination can leave these artifacts.
- Temporary `hh-admin-*.ps1` or `hh-admin-*.cmd` files under the elevated worker's temp directory for commands with `admin: true`; deleted when the command finishes. Commands and results pass over a single-use local named pipe, not a guest network connection.

Host uninstallation preserves user logs, guest files and nonempty service working data. When service data remains, the owner configuration is also retained for reinstallation. Otherwise the configuration and empty data directory are removed. Installed host and agent executables are deleted only if they still match the hashes recorded for cleanup; directories containing other files are left in place.

## Access control

The MCP server needs no authentication by design: it listens only on `127.0.0.1:8770` for the programs on this computer. Any of them can control the VMs through all HyperHand tools, including running commands in the guest, copying files between host and guest, and unlocking a locked session with a stored unlock password (without being able to read the password).

The service's named pipe permits the configured owner, SYSTEM, administrators and the service account. This restricts direct broker access only; MCP callers reach the service through the tray's HTTP endpoint, which needs no authentication. The service exposes specific Hyper-V operations and the fixed guest socket tunnel, not an arbitrary host command execution endpoint. Guest `vm_exec` still runs in the guest.

The tray runs without elevation. The service uses a dedicated virtual account, not LocalSystem, and installation does not add the human user to Hyper-V Administrators. That service account nevertheless has broad Hyper-V management rights, including VM and checkpoint operations. Host UAC settings are not changed.

The `HyperHand Console` task runs Virtual Machine Connection with the installing user's full administrator token, without a UAC prompt. Any process running as that user can run the task, with any text as the VM name: the tray checks the names it passes, but the task itself does not, and quotes in the text can add further `vmconnect.exe` arguments. The task can only start `vmconnect.exe`; within its window, the Hyper-V features it offers (for example inserting media or changing settings) then run with administrator rights, as they would after approving UAC for Virtual Machine Connection yourself. Uninstallation removes the task.
