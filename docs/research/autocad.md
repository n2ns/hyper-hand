# AutoCAD 2015: research record

Facts about AutoCAD 2015 (English, trial) in a Windows 10 guest that HyperHand's behavior and its AutoCAD workflows rest on, each with the date and how it was obtained. Generic Windows facts are in [Windows and UI Automation](windows.md), timings in [UI Automation performance](uia-performance.md). **Re-verify the affected facts for another AutoCAD version or a third-party plug-in.**

## Facts

### Windows and input

1. **Dynamic-input tooltip takes the foreground.** With the mouse cursor over the drawing area, typing a command name shows the dynamic-input tooltip `CAcDynInputWndControl` at the cursor, and it becomes the active and foreground window, with keyboard focus and caret in its child `Edit`; it forwards the keys to the command line. Style `0x94000000` (`WS_POPUP | WS_VISIBLE | WS_CLIPSIBLINGS`), extended style `0`, owner and parent the main frame `AfxMDIFrame110u`, same process and UI thread. Its autocomplete list is a `ListBox` (extended style `0x8C`, child of the desktop, no owner) that never takes the foreground. With the cursor elsewhere no tooltip appears. Runtime (Win32 readout, `GetGUIThreadInfo`, screenshots), 2026-10-10. Used by the input-popup rule of [4.9](../features/observation-input.md#49-text-vm_type).
2. **Floating palettes are not input popups.** The floating Properties palette has style `0x960C3500` (`WS_THICKFRAME | WS_SYSMENU`, no `WS_CAPTION`), extended style `0x180`, owned by the main frame on the same thread. The Layer Properties Manager opened docked (a child window). Runtime, 2026-10-10.
3. **Text typed before a modal dialog appears is lost.** `_qnew` followed by Enter and more text: the `Select template` dialog appeared after the following characters had been sent; neither its `File name` box nor the command line received them. Runtime, 2026-10-10.
4. **Command-line Unicode input.** Unicode keys typed into the floating command line set `USERS1` to `管道验收ABC123` exactly (read back through AutoCAD's COM API). After **Alt**, the ribbon shows KeyTips, and **Esc** returns the input to the command line. Runtime, 2026-10-07.

### UI Automation

5. **Main window size.** The main window has about 1,342 to 1,400 control-view nodes. Runtime, 2026-10-11 and 2026-10-16.
6. **Drawing area.** The focused drawing-area `Pane` exposes no UIA patterns (`supported: []`); only clicks and keyboard input reach it. Runtime, 2026-10-11.
7. **Ribbon tabs are buttons.** The ribbon's `Home` and `Insert` tabs are exposed as `Button` with Invoke, AutomationIDs `ACAD.ID_TabHome` and `ACAD.ID_TabInsert`. `TabItem` controls belong to the drawing and layout tabs. `Home` showed 72 visible ribbon commands and `Insert` 29. Runtime on `Win10-Example`, 2026-10-16.
8. **Standard dialogs.** `Select template` and `Select File` are `#32770` dialogs; `File name:` supports ValuePattern (Chinese text read back exactly), `Cancel` supports Invoke. In `Options`, the tab control is a `Tab` with AutomationId `1796` and 11 tab items; `Cancel` has AutomationId `2`. The floating command line's edit does not expose ValuePattern. Runtime, 2026-10-07 to 2026-10-11.

### Startup and busy states

9. **Trial reminder.** At startup the trial reminder blocks the main window for a while; a control-tree read then reports the window as not responding. Runtime, 2026-10-10.
10. **Elevated start.** Started elevated, AutoCAD shows the Qt licensing dialog of `LMU.exe`; all its windows run at `high` integrity (see [Windows fact 1](windows.md#input-and-focus)). Runtime, 2026-10-11.
11. **Busy UI thread.** An AutoLISP loop creating 1.5 million circles hangs the main window (`IsHungAppWindow` `True`) until it finishes; keys sent meanwhile are queued and processed afterwards (63 s later in that run), so a successful send does not mean AutoCAD handled them. A following `REGEN` did not hang it. Runtime, 2026-10-11.
