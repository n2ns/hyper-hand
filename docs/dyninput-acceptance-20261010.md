# AutoCAD dynamic-input typing acceptance (2026-10-10)

## Problem

`vm_type {handle: <AutoCAD main window>, text: "_qnew\n"}` stopped with `partial_input` after the first characters of the command name ("keyboard input target is no longer the visible, enabled foreground window with the requested PID"). Reported by the Example 3D session (TODO, AI-caller feedback).

## Cause (reproduced on Win10 with the installed `d56fa66` build)

- Reproduced only with the mouse cursor over the drawing area: AutoCAD 2015 shows its dynamic-input tooltip at the cursor as soon as a command name is typed. With the cursor elsewhere, all 6 characters arrived and the template dialog opened.
- With the cursor in the drawing area: `applied_chars: 2` of 6. `vm_windows` then showed the foreground window `CAcDynInputWndControl` of the same `acad.exe`.
- The tooltip window (guest Win32 readout): style `0x94000000` (`WS_POPUP | WS_VISIBLE | WS_CLIPSIBLINGS`), extended style `0` (neither `WS_EX_TOOLWINDOW` nor `WS_EX_NOACTIVATE`), owner and parent the main frame `AfxMDIFrame110u`, same process and UI thread. `GetGUIThreadInfo`: it is the active window, and the keyboard focus and caret are in its child `Edit`. The screenshot shows `_Q` typed in it with the autocomplete `_QUIT`: the keys did go into the tooltip, which forwards them to the command line.
- Its autocomplete list is a `ListBox` (extended style `0x8C`, child of the desktop, no owner) that never takes the foreground.
- The agent's `validateInputTarget` required `GetForegroundWindow()` to equal the target handle before every character, so the first character typed after the tooltip appeared was refused. The host side already counts the tooltip as part of the main window's group (owner link), so `vm_key` was not affected.
- For the fix boundary, the floating Properties palette was measured too: style `0x960C3500` (`WS_THICKFRAME | WS_SYSMENU`, no `WS_CAPTION`), extended style `0x180`, owned by the main frame on the same thread. The Layer Properties Manager opened docked (a child window).

## Change

The agent accepts as foreground the target itself, or an input popup of it while the target stays visible, enabled and not minimized: a visible, enabled `WS_POPUP` window without caption, sizing border or system menu, not a dialog (`#32770`), on the same process and UI thread as its owner, whose owner chain reaches the target. A target that is itself such a popup stands for its first non-popup owner. Modal dialogs (they disable their owner and have a caption), floating palettes and other windows still stop the input; the refusal now names the foreground window's handle, class and PID. Contract: [features.md 4.9](features/observation-input.md#49-text-vm_type); unit cases in `internal/agent/input_test.go` (`TestInputForeground`, modelled on the measured styles).

Review: one independent round, no high findings. Two medium findings (floating palettes lacking `WS_CAPTION` would have counted as popups; a palette target would hand later keys to the main frame) are fixed by also excluding `WS_THICKFRAME` and `WS_SYSMENU`. One low finding is accepted: if the tooltip the host chose as the target closes before the agent's first check, the call is refused with nothing typed.

## Installed acceptance

- Build: the working tree of this change (on `d56fa66`); `go vet` and `go test -race ./cmd/... ./internal/...` pass. Installed with `scripts\restart-tray.ps1`; installed and build SHA-256 identical: `hyperhand.exe` `282FA788BBA6…35CF2BBACF`, `hyperhand-agent.exe` `4A73937371AA…BAF688E61BD149`.
- Guest: `vm_update_agent` on `Win10` only; the running agent `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe` has the same SHA-256. `Win10-Example` was not touched and still runs the previous agent (protocol unchanged, so it keeps working; it needs `vm_update_agent` to get this fix).
- AutoCAD 2015 on Win10, cursor clicked into the drawing area and `Esc` before each case:
  - A, the acceptance criterion: one `vm_type {handle: main frame, text: "_qnew\n"}` returned `applied_chars: 6, total_chars: 6`; the `Select template` dialog (`#32770`, owned by the main frame) was then the foreground.
  - B, evidence that the tooltip held the foreground during typing: `vm_type "_qnew"` returned 5/5, after which `vm_windows` showed `CAcDynInputWndControl` as the foreground (the old agent refused at this point); a following `vm_type "\n"` (1/1) opened `Select template`.
  - C, an Enter that opens a modal dialog followed by more text: `"_qnew\nabc"` returned 9/9. The dialog appeared after the agent had already typed `abc`; neither its `File name` box (still `acad.dwt`) nor the command line history received it. This timing gap is independent of the change (the old agent checked at the same points) and is documented in features.md 4.9 and the user guide.

Local evidence (screenshots, `accept-log.json`, window readouts) is under Example's ignored `temp/hyperhand/dyninput-20261010/`.
