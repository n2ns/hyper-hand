# HyperHand Features

This document describes the behaviour of HyperHand as implemented: the host tray program, the MCP tools it exposes, the guest agent and the protocol between them.

The exact behavior is split by chapter, one file each. Read the chapter of the tool or area you work on; section numbers are global (8.6 is in chapter 8).

- [1. Architecture](features/architecture.md): 1.1 Host side, 1.2 Hyper-V socket, 1.3 Connections and request ordering, 1.4 Cancellation, 1.5 Agent service loop, 1.6 Task ownership
- [2. Results, Errors and VM Selection](features/results-errors.md): 2.1 Result format, 2.2 Error object and codes, 2.3 VM selection
- [3. VM and Checkpoint Tools](features/vm-checkpoints.md): 3.1 vm_list, 3.2 vm_start, vm_shutdown and vm_turn_off, 3.3 Checkpoints, 3.4 PowerShell-based operations, 3.5 Session readiness and unlock: vm_start, vm_status, vm_unlock
- [4. Observation and Input](features/observation-input.md): 4.1 Observations, 4.2 vm_windows, 4.3 vm_observe, 4.4 Actions: targets, check chain, activation and observe_after, 4.5 Input serialisation, 4.6 Mouse: vm_click, vm_drag, vm_scroll, 4.7 Control actions: vm_set_value, vm_invoke, 4.8 Keyboard: vm_key, 4.9 Text: vm_type
- [5. Commands](features/commands.md): 5.1 vm_exec, 5.2 Shells, 5.3 Working directory, 5.4 Timeout, 5.5 Elevated execution (admin), 5.6 Output decoding, 5.7 Background jobs: vm_exec background, vm_job
- [6. Files](features/files.md): 6.1 vm_push, 6.2 Unchanged-file skipping, 6.3 vm_pull, 6.4 Temporary .hhpart files, 6.5 Directory mirror, 6.6 vm_file_info
- [7. Clipboard, Launching, Waiting and Turn End](features/clipboard-launch-wait.md): 7.1 vm_clipboard_get and vm_clipboard_set, 7.2 vm_launch, 7.3 vm_wait, 7.4 vm_end_turn, 7.5 vm_apps
- [8. Guest Agent Installation, Update and Diagnostics](features/agent.md): 8.1 vm_install_agent, 8.2 Agent install command, 8.3 Single instance and startup, 8.4 Agent readiness check, 8.5 vm_update_agent, 8.6 Protocol version, 8.7 vm_doctor
- [9. Host Tray](features/host-tray.md): 9.1 Running, 9.2 install, 9.3 Log
- [10. Wire Protocol](features/wire-protocol.md): 10.1 Frame format, 10.2 Operations, 10.3 Error behaviour, 10.4 Broker checkpoint operations
