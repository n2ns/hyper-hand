# Windows and UI Automation: research record

Facts about the Windows guest, its input, focus, UAC and UI Automation behavior that HyperHand's behavior rests on, each with the date, environment and how it was obtained. Application-specific facts are in [AutoCAD](autocad.md), UI Automation timings in [UI Automation performance](uia-performance.md). **Re-verify the affected facts after a guest Windows update.**

Environment unless stated otherwise: guest `Win10` (Windows 10, Chinese UI, 1920 x 1080, basic console session) with the HyperHand agent of that date.

## Facts

### Input and focus

1. **UIPI filters targeted input into higher-integrity windows** ([Microsoft: SendInput](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-sendinput)). Input through the Hyper-V keyboard and mouse is not filtered: a raw click closed an elevated (`high` integrity) Qt dialog of `LMU.exe` that the agent could not type into. Runtime, 2026-10-11. Used by `integrity_mismatch` ([4.4](../features/observation-input.md#44-actions-targets-check-chain-activation-and-observe_after)).
2. **A foreground lock defeats `SetForegroundWindow`.** After a window called `LockSetForegroundWindow(LSFW_LOCK)`, `SetForegroundWindow` from another process returned `False` and the foreground stayed. Attaching to the foreground thread, a zero mouse move and a retry activated the target. Runtime with AutoCAD 2015, 2026-10-11.
3. **Activating a hung window blocks.** For a window whose UI thread does not pump messages, `IsHungAppWindow` is `True` and Windows shows a `Ghost` window titled `<title> (未响应)` in the foreground. Restoring or raising the hung window waits on its thread: before the agent checked for hung windows, a `vm_set_value` did not return within 120 s and every other call to the VM queued behind it. Runtime with a WinForms fixture, 2026-10-11. Used by `target_not_responding`.
4. **The Run dialog drops long commands.** A command typed into Win+R runs at 259 characters or fewer after environment variables are expanded; at 260 or more the dialog closes without running it or showing a message. A 248-character command containing `%LOCALAPPDATA%` (14 characters, 26 expanded) did not run for that reason. Typing itself was intact. Runtime through the Hyper-V keyboard, 2026-10-11. `vm_install_agent` types about 90 characters.

### UAC

5. **Consent prompt.** With `ConsentPromptBehaviorAdmin` 2 the prompt appears on the secure desktop; on this Chinese guest its buttons read `是` and `否` with no access keys shown and the focus on `否`. Raw `alt+y`, `left` then `enter`, or a raw click on `是` approve it; `esc` declines it (`Start-Process` reports "The operation was canceled by the user"). Unanswered, the prompt times out. Runtime, 2026-10-11, inside a temporary checkpoint. Used by [5.5](../features/commands.md).

### UI Automation

6. **SelectionItem availability property.** `UIA_IsSelectionItemPatternAvailablePropertyId` is 30036 (30037 is a different property). Checked against the Windows SDK, 2026-10-10; a native LISTBOX test fails with 30037.
7. **WPF exposes text children with the button's name.** A WPF button's child TextBlock appears in the control view with the same name, so a name-only query matches both; add `control_type`. Runtime with a WPF fixture, 2026-10-10.
8. **Pattern support is provider-specific.** A standard `#32770` dialog's edit supports ValuePattern; a WPF ListItem supports SelectionItem; AutoCAD's custom-drawn areas expose no patterns ([AutoCAD](autocad.md)). Invoke has no generic success postcondition, so the result of an Invoke cannot be read back in general. Runtime, 2026-10-07 to 2026-10-11.

### Packaged apps

9. **`Get-StartApps` lists packaged apps by AUMID.** It returns the Applications folder's entries with `Name` and `AppID`; for packaged apps the AppID is the Application User Model ID `PackageFamilyName!ApplicationId`, for desktop programs a path or a registered ID without `!`. On the development Windows host (not the guest) it returned 30 packaged entries, run from a `-NoProfile` PowerShell child; the whole test, including that child, took 0.64 s. Runtime, 2026-10-24. Used by [7.5](../features/clipboard-launch-wait.md#75-vm_apps). Not yet observed on the `Win10` guest.
10. **`ActivateApplication` fails fast for an AUMID that is not installed.** With `AO_NOERRORUI` it returned HRESULT `0x80073CF1` ("Package was not found") without showing anything. Runtime on the development Windows host, 2026-10-24. Used by [7.2](../features/clipboard-launch-wait.md#72-vm_launch).
11. **A UWP app's top-level window belongs to `ApplicationFrameHost.exe`.** The visible frame has class `ApplicationFrameWindow`; the app's content is a child window of class `Windows.UI.Core.CoreWindow` owned by the app's process, and is detached from the frame while the app is minimized or suspended. Packaged desktop programs own their windows. Widely reported Windows 10 behavior, not in Microsoft's API documentation; not yet observed with HyperHand on the `Win10` guest (2026-10-24). Used by `hosted_pid` ([4.2](../features/observation-input.md#42-vm_windows)).
