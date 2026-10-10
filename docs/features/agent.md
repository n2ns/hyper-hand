# 8. Guest Agent Installation, Update and Diagnostics

Part of [HyperHand features](../features.md). Section numbers such as 8.6 refer to the chapters listed there.

### 8.1 vm_install_agent

`vm_install_agent` installs the agent without a guest password. It needs a user logged on to the guest desktop and the guest IME in English mode.

1. The ordinary tray reads `hyperhand-agent.exe` next to its own executable, normally under `%ProgramFiles%\HyperHand`, and streams it to the service.
2. The service stages the contents in its working directory, enables the VM's Guest Service Interface if it is disabled (then waits 3 s) and copies the file with `Copy-VMFile` to `C:\Users\Public\HyperHand\hyperhand-agent.exe`, creating the path and overwriting. The service does not open a caller-supplied host source path.
3. It presses `win+r`, waits 1.5 s, types `C:\Users\Public\HyperHand\hyperhand-agent.exe install` on the synthetic keyboard and presses `enter`.
4. It pings the agent (see 8.4).

Because step 3 types blindly, a failure there is visible only on screen; the tool description advises checking with `vm_observe`.

### 8.2 Agent install command

`hyperhand-agent.exe install` runs in the user's session and needs no administrator rights. `vm_install_agent` runs it through the Run dialog; it can also be run by hand in the guest. Started without `install`, the agent runs for the current session only, without autostart.

- It force-terminates every other running `hyperhand-agent.exe` and waits 500 ms.
- Unless it already runs from there, it copies itself to `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe`, retrying up to 20 times 250 ms apart while the old image is still locked.
- It sets the `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` value `HyperHandAgent` to the quoted installed path, so the agent starts at logon.
- It starts the installed copy.
- On error it shows a message box and exits with code 1.

`hyperhand-agent.exe uninstall` reverses the install. It runs as the logged-on user without administrator rights. Run it in the guest directly, not through `vm_exec`, because it stops the agent that would be executing it.

- It stops every other running `hyperhand-agent.exe`.
- It deletes the `HyperHandAgent` value under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`.
- It deletes `%LOCALAPPDATA%\HyperHand` and `C:\Users\Public\HyperHand`. The file of a running program cannot be deleted but can be renamed on its volume, so before deleting the folder it is running from it moves its own executable to `%TEMP%\hyperhand-agent-uninstalled-<pid>.exe`, where it remains (or to the parent of that folder if `%TEMP%` is on another volume), and leaves the folder as its working directory. No script or helper process is started.
- It shows a message box listing what was removed.

### 8.3 Single instance and startup

- The agent uses the mutex `HyperHandAgent`; a second instance exits immediately.
- At startup it marks itself DPI-aware and deletes a leftover `hyperhand-agent.exe.old` next to itself, retrying for up to 10 s while the previous version is still exiting.

### 8.4 Agent readiness check

`vm_install_agent` and `vm_update_agent` ping the agent every 2 s for up to 30 s, each ping limited to 5 s.

- Success returns `{"agent": {"version": "0.3.0", "hostname": "WIN10", "user": "WIN10\\tester", "protocol": <n>}}`. An agent that answers with an older protocol is refused with `agent_outdated` (see 8.6).
- Otherwise the call is refused with `agent_required`, reason `the agent did not answer within 30 s: <last error>` and `next` `look at the screen with vm_observe, then call vm_install_agent again`.

### 8.5 vm_update_agent

`vm_update_agent` replaces the running agent with the installed `hyperhand-agent.exe` next to the host executable. The agent must already be running (`agent_required` otherwise).

1. The host sends the whole executable as the `update_agent` payload.
2. The agent writes `<exe>.new`, deletes any `<exe>.old`, renames the running `<exe>` to `<exe>.old` and `<exe>.new` to `<exe>`. If the last rename fails, the old file is renamed back and the update fails. An empty payload fails with `empty payload`.
3. After a successful update the agent sends its response, releases its mutex, starts the new executable with the same arguments and exits. It restarts even if the response could not be sent.
4. The host closes its connection, waits 2 s and pings the new agent (see 8.4).

The update replaces the file the agent is running from; the HKCU Run entry is unchanged.

### 8.6 Protocol version

The guest protocol has a generation number (`proto.Protocol` in `internal/proto/proto.go`), raised whenever the host and the agent exchange something new and reported by the agent's `ping` as `protocol`. There is no compatibility path for older agents:

- Every new connection to an agent is checked once: before the first op other than `ping` or `update_agent`, the host pings the agent and refuses an older `protocol` with `agent_outdated` (fields `agent_protocol`, `host_protocol`; `next` `call vm_update_agent`) for that op and every later one on the connection. `vm_status` (which only pings) and `vm_update_agent` therefore still work on an old agent, so that it can be reported and replaced; `vm_start`, `vm_install_agent` and `vm_update_agent` additionally check the protocol of the agent that answers their readiness ping.
- An agent that answers `unknown op` to a request is reported as `agent_outdated` with the same `next`.
- `vm_status` and `vm_doctor` report the protocol without refusing.

### 8.7 vm_doctor

`vm_doctor` diagnoses the host and the selected VM. It is read-only and changes nothing. Result: `{"checks": [{"check": "host.service", "status": "ok", "detail": "HyperHandService is running"}, ...]}`; `status` is `ok`, `warn` or `fail`, and every `warn` or `fail` has a `suggestion` naming a command or tool call.

| Check | Looks at | fail / warn |
|---|---|---|
| `host.service` | `HyperHandService` in the service control manager | fail: not installed or not running (`run hyperhand.exe install (elevated)`, `Start-Service HyperHandService`); warn: access denied when querying |
| `host.mcp` | this process answered the call; `detail` names the URL it serves | always ok |
| `host.pipe` | dialling `\\.\pipe\HyperHandService` (2 s) | fail: not reachable |
| `host.hvsocket` | the registry registration of the Hyper-V socket service ID (see 1.2) | fail: not registered |
| `host.enhanced_session` | `EnhancedMode` under `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization` | warn: the host allows enhanced session mode |
| `vm.power` | the selected VM | fail: the VM cannot be resolved (see 2.3) or is not running; the guest checks are then skipped |
| `guest.agent` | a 5 s ping: version, protocol, hostname, user | fail: no answer (`call vm_start ...; ... vm_install_agent`), or protocol older than the host's (`call vm_update_agent`) |
| `guest.session` | `session_state` | fail: not available, or not the console session; warn: a UAC prompt is open, or the session is locked |
| `guest.integrity` | the agent's integrity level from `list_windows` | warn: the window list or the level is unavailable |
