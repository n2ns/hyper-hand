---
name: dev-install
description: Installs the current HyperHand checkout on the Windows host without UAC and brings the guest agents up to date — scripts/dev-install.sh, the recovery steps for its failures, vm_update_agent and the version and hash checks. Use whenever a changed build must run for real (before a VM check, an acceptance run or after a fix), and when a tool returns agent_outdated.
argument-hint: [vm ...]
---

# Installing a development build

The running service and tray are the installed copies under `%ProgramFiles%\HyperHand`, and each guest runs its own agent; a build in `build/` changes nothing until it is installed. Rules: [AGENTS.md](../../../AGENTS.md#build-and-verify); details: [Building](../../../docs/building.md#install-a-development-build).

## 1. Install on the host

From the repository root in WSL:

```sh
scripts/dev-install.sh             # extra arguments go to dev-install, e.g. --timeout 300
```

It prints `built <version>`; remember the version for step 3. Exit code 0 means the installed files equal the build, the service runs and one tray is up. Installing interrupts MCP connections, so finish running tool calls first. On failure:

| Message | What to do |
|---|---|
| the task is not registered | Stop and ask the user to run `scripts\dev-install-setup.ps1` once from an elevated PowerShell. Never install with a UAC prompt instead. |
| the task runs `<other path>`, not `...\HyperHandDev\hyperhand.exe` | Stop and ask the user to run, elevated, `schtasks /Delete /TN "HyperHand Dev Install" /F` and then `scripts\dev-install-setup.ps1`. |
| a development install is queued or running | Wait and run the script again; never stop the task. |
| the installer failed, with log lines | Read the lines. If the service is left stopped, `build/dev-install/hyperhand.exe dev-install --no-build` reinstalls the same build and restores it. |
| the install task did not finish within N s | Inspect it in the log (`%LOCALAPPDATA%\HyperHand\hyperhand.log`) before retrying; it was not stopped. |

Never kill HyperHand processes by name; the installer replaces the service and the tray.

## 2. Update the guest agents

For each VM the work will use (the argument, or the VMs named in [AGENTS.md](../../../AGENTS.md#test-vms-are-disposable)) that is running:

```sh
python3 client/hyperhand_client.py --vm <vm> vm_update_agent
```

An agent that does not answer needs `vm_install_agent` instead (guest keyboard in English mode). A VM that is off keeps its old agent; tools refuse it with `agent_outdated` after it starts, and this step is repeated then.

## 3. Check

- `python3 client/hyperhand_client.py --vm <vm> vm_status`: `agent.version` is the version printed in step 1.
- `python3 client/hyperhand_client.py --vm <vm> vm_doctor`: every check `ok`.
- When the installer changed: `/mnt/c/Windows/System32/sc.exe qfailure HyperHandService` shows `RESET_PERIOD` 86400 and restarts after 5000, 30000 and 60000 ms, and `sc.exe qfailureflag HyperHandService` shows `TRUE` ([Host tray](../../../docs/features/host-tray.md)). Ending the service process to watch the restart needs an elevated prompt; ask the user (`taskkill /PID <pid> /F`, then `sc query HyperHandService` after 5 s).

For an acceptance run, also compare the hashes as described in the `vm-acceptance` skill.
