# Hyper-V and Virtual Machine Connection: research record

Facts about Hyper-V, its WMI and PowerShell interfaces and Virtual Machine Connection (VMConnect) that HyperHand's behavior rests on, each with the date, environment and how it was obtained. The behavior built on them is in [Features](../features.md). **Re-verify the affected facts after a Windows or Hyper-V update on the host.**

Environment unless stated otherwise: the development host (Windows with Hyper-V, Chinese UI), general-test and AutoCAD-test guests (Windows 10, 1920 x 1080, basic console session at the time of these measurements).

## Facts

### Power states and checkpoints

1. **Paused and saved states.** Hyper-V reports `EnabledState` 9 for a paused VM and 6 for a saved one, not 32768/32769. Runtime, 2026-10-11: `vm_status` read `"9"` and `"6"` before HyperHand mapped them. Used by [3.2](../features/vm-checkpoints.md).
2. **Production checkpoints are not marked as such.** A production checkpoint made by `Checkpoint-VM` has `SnapshotType` `Standard`; `Recovery` is the snapshot type of recovery checkpoints made by backup software. `Msvm_VirtualSystemSettingData` has no snapshot type for production checkpoints. Runtime on `Win10` (2026-10-11, checkpoint setting `ProductionOnly`) and Microsoft's WMI documentation. HyperHand therefore reports `kind` as Hyper-V gives it and states memory in `holds_memory` ([3.3](../features/vm-checkpoints.md)).
3. **Checkpoint state of a running VM.** Under `Standard`, a checkpoint of a running VM lists `state: saved` (it holds memory); under `ProductionOnly` it lists `state: off` (disk only) and restores to Off. Runtime, 2026-10-11.
4. **Restoring a running Standard checkpoint resumes it.** The restored VM is Running even when HyperHand sends no Start (`start: false`): the flag suppresses HyperHand's Start call and does not force a power state. Runtime, 2026-10-07, v0.2.0, with a dedicated data disk: both disks and an in-memory process (same PID, start time and token) came back.
5. **Deleting checkpoints.** A checkpoint must be deleted through Hyper-V so that its differencing disk is merged; AVHDX files are never deleted directly ([Microsoft: checkpoints](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/checkpoints)). After a merge, Hyper-V PowerShell in the same process can still return the cached path of the merged AVHDX; a fresh PowerShell process reads the current chain. Runtime, 2026-10-07.

### Shutdown

6. **A blocked shutdown signs the user out.** With unsaved text in Notepad, the shutdown integration service request (`InitiateShutdown`, `Force` false) left the VM Running after 3 minutes, but Windows had started signing the user out: the agent had exited, the console showed the lock screen and then the sign-in screen, and neither Hyper-V keyboard input (password, Ctrl+Alt+Del) nor clicks reached the password box for more than 8 minutes. Only turning the VM off recovered it. Runtime on `Win10`, 2026-10-11. Used by [3.2](../features/vm-checkpoints.md) (`vm_shutdown`'s reason and `next`).

### Enhanced session and the console

Runtime on `Win10`, 2026-10-15: host with enhanced session mode allowed (`Set-VMHost -EnableEnhancedSessionMode $true`), guest user in Administrators with `ConsentPromptBehaviorAdmin` 0, network RDP off (`fDenyTSConnections` 1), a VMBus RDP listener in `qwinsta`. See also [Microsoft: enhanced session mode](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/enhanced-session-mode).

7. **An enhanced session takes the user's session off the console.** Signing in through VMConnect's enhanced session moved session 1 to the VMBus RDP connection; `console` became a new, empty session. The guest screen became the size of the VMConnect window (1365 x 768), and the host screenshot (still 1920 x 1080) showed the lock screen.
8. **`tscon <session> /dest:console`, run elevated in the guest, moves it back at once**: unlocked, at the console resolution (1920 x 1080), with no other session left. Screenshots, Hyper-V input and agent actions work again without waiting. Right after the move the agent can read the session as locked for a moment. Windows maximized at the RDP resolution keep that size.
9. **The viewer does not fall back to basic mode.** After `tscon`, the enhanced VMConnect window shows a lost-connection message ("由于另一个用户已连接到虚拟机，因此已丢失连接") with **Reconnect** and **Exit**; **Reconnect** takes the session back to RDP. A person and HyperHand cannot share the session.
10. **UAC prompts follow the session.** With a consent prompt policy, the prompt appears in the session that is on RDP and cannot be answered through the Hyper-V console (inferred from 7 and the secure desktop's session; not run with a consent prompt).

### VMConnect

Runtime and resource inspection on the host, 2026-10-16, using the AutoCAD test guest. Used by [9. Host tray](../features/host-tray.md) (viewer recovery).

11. **Video surface classes.** A connected viewer's RDP area is a visible `OPWindowClass` (basic renderer) or `TscShellContainerClass` (shell renderer) window. A freshly started basic viewer had only `OPWindowClass`.
12. **Status bar through UI Automation.** The status bar's ToolStrip items expose their text with empty AutomationIDs and empty runtime IDs. `LockIcon` is the internal WinForms control name, not a UIA AutomationID.
13. **Connected-video labels.** The English and Chinese labels for connected video are the strings in the installed Microsoft resource assemblies assigned in `VmisStatusStrip.InformConnectedState` (all eight checked against the assemblies). VM-running and input-capture texts do not indicate connected video. Other UI languages were not inspected.
14. **A covered viewer cannot be raised by ordinary calls.** A VMConnect window covered by non-topmost windows (VS Code, VSCodium) stayed covered after `SetWindowPos(HWND_TOP)` (which returned success), ordinary foreground activation, and activation with only the caller's and the foreground's input queues attached. Attaching both the caller-to-viewer and caller-to-foreground queues for the activation made it the foreground window. `IsWindowVisible` and connected video do not show that the viewer is actually visible on screen.
