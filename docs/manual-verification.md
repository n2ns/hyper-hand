# Manual Verification

How to accept a HyperHand change on the installed host and a real guest. Results are not kept as records: when a check passes, its TODO item (or the passed part) is removed, and a conclusion users rely on becomes a sentence in the README's Known limitations; pending checks are in [TODO](../TODO.md). Upstream facts found while testing go to [research](research/).

## Test VMs

- Use the designated VMs as [AGENTS.md](../AGENTS.md#test-vms-are-disposable) requires. Before a run, confirm the guest identity, Windows version and UI language, console session, resolution, user privileges, automatic sign-in, UAC policy and AutoCAD version where relevant. Keep the actual VM mapping and observed baseline in private/local environment notes; do not infer current settings from historical research records.
- Guest security settings may be changed freely for a check; change them inside a temporary checkpoint and restore it afterwards, so the VM returns to this baseline.

## Before a run

1. `scripts/test-windows.sh` passes, and `python3 -m unittest discover -s client` when the client changed. Three agent desktop tests fail or skip while the host session is locked ([Building](building.md#test)); that is not a product failure.
2. Install the build with `scripts/dev-install.sh` and update the guest with `vm_update_agent`. Never install through a UAC prompt ([Building](building.md#install-a-development-build)).
3. Check that the installed `%ProgramFiles%\HyperHand\hyperhand.exe` and the running guest agent (`%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe`, the path of the running process) have the build's SHA-256, and that `vm_doctor` reports every check `ok`.
4. Create a temporary checkpoint of the running VM before changing guest state.

## During a run

- Drive the tools with [`client/hyperhand_client.py`](../client/README.md) and one task (`--task-file`), from a runner script kept with the evidence; the `vm-acceptance` skill's `acceptance.py` provides the task, checkpoint, cleanup, independent checks and generic fixtures.
- Check every result against independent state, not against the tool's own report: a fixture that publishes its state to a separate file (written to a temporary file in the same directory and renamed, so the runner never reads half a file), process identity, file hashes, checkpoint listings or a fresh observation. `applied_chars` counts characters sent, not processed.
- A fixture must not fail on its own: an unhandled exception dialog in a WinForms fixture once took the focus and swallowed typed characters, which looked like lost input.
- Record which build ran each step. Coverage by unit tests is stated as such and is not counted as installed behavior.
- Host-side viewer evidence (VMConnect) needs a DPI-aware capture taken while the viewer is the verified foreground window (HWND and PID checked); a guest screenshot does not show what the host viewer displays.
- Keep scripts, JSON results and screenshots under the ignored `build/<item>-<yyyymmdd>/`.

## After a run

- Restore the temporary checkpoint and call `vm_end_turn`; its `errors` and `skipped` are empty and the checkpoint listing holds only the checkpoints that existed before. A host reinstall during the run gives the task a new run ID that does not own the run's temp checkpoint; remove it with `vm_end_turn` `all_temp: true` for that VM.
- A restored checkpoint can hold an older guest agent; update it again when its version differs from the build.
- The running guest agent still has the expected hash, `vm_doctor` reports every check `ok`, the guest clipboard is restored, and a new task can write (ownership was released).
- Host settings the run changed (enhanced session mode, a VM's checkpoint type or memory) are set back.

## Setups for specific checks

| Check | Setup |
| --- | --- |
| Interactive UAC | Set `ConsentPromptBehaviorAdmin` to 2 with `vm_exec admin` inside the temporary checkpoint. Start the elevated program from a `background: true` job with `Start-Process -Verb RunAs`, so the agent stays free; watch the prompt with `vm_observe` and answer with raw `vm_key alt+y` or `esc` ([Windows fact 5](research/windows.md#uac)). A waiting `vm_exec admin` blocks other agent calls, including screenshots. |
| No signed-in user | Set `AutoAdminLogon` to `0` under `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon` inside the temporary checkpoint, then `vm_shutdown` and `vm_start`. |
| Enhanced session | The user allows enhanced session mode on the host (`Set-VMHost -EnableEnhancedSessionMode $true`) and signs in through VMConnect's enhanced session; set it back afterwards. See [Hyper-V facts 7 to 10](research/hyper-v.md#enhanced-session-and-the-console). |
| Production checkpoints | `vm_set_checkpoint_type ProductionOnly`, then back to `Standard`. Leave a marker file before and after the checkpoint to see what the disk holds. |
| Blocked shutdown | Leave unsaved text in Notepad and call `vm_shutdown` inside the temporary checkpoint; the guest may end up at an unusable sign-in screen ([Hyper-V fact 6](research/hyper-v.md#shutdown)), recovered only with `vm_turn_off` or `vm_restore`. |
| Hung window | A WinForms fixture whose UI thread sleeps (for example 240 s) after its control tree was read. |
| Busy AutoCAD | An AutoLISP loop that creates many entities (1.5 million circles hang the main window for about a minute). |
| Mirror interruption | A large source file (3 GiB) and an extra file in the target, so a partial apply is visible; interrupt during the stream (MCP cancellation, or `dev-install --no-build`) and check for leftover `.hhpart` and `hyperhand-mirror-*` files on host and guest. |
| Old agent | Push the agent of an older release into the guest and start its `install`; tools then return `agent_outdated` until `vm_update_agent`. |
| Long control lists | A WinForms window with Win32 `ListBox`es of, for example, 150 and 1,200 items, which expose every item to UI Automation. File Explorer's item view is not one: it exposes only realized items ([Windows fact 15](research/windows.md#explorer)). |
| Packaged apps | Calculator and Settings are packaged on Windows 10. Calculator is multi-instance: a second activation starts another process ([Windows fact 12](research/windows.md#packaged-apps)); stop every instance you started. |
| Shell flyout | A raw Windows key press opens the Start flyout; its process depends on the Windows build and `vm_windows` does not list it ([Windows fact 13](research/windows.md#shell-flyouts)). |
| Screenshot size | Compare the saved PNG's header with the expected size, and read the screen size from a `max_size: -1` screenshot first. |
| Opening a folder | Start Explorer with `vm_exec` (`Start-Process explorer.exe -ArgumentList <folder>`) and find the window by title; `vm_launch` of `explorer.exe` cannot follow it ([Windows fact 14](research/windows.md#explorer)). |
| Typing into the Run dialog | Keep commands at 259 characters or fewer after variable expansion ([Windows fact 4](research/windows.md#input-and-focus)). |

## Release package

Before tagging, a candidate can be checked the way a release is built: run the build step of `.github/workflows/release.yml` locally (with the candidate version in `-X hyperhand/internal/proto.Version=`), then delete `cmd/hyperhand/embedded/hyperhand-agent.exe` (later builds would embed it), put the packed executables into `build/dev-install` and install them with `build/dev-install/hyperhand.exe dev-install --no-build`, check that the MCP `serverInfo` version and the guest agent's version after `vm_update_agent` are the candidate's, and that `vm_doctor` reports every check `ok`. The single-file installer is checked by installing the candidate `hyperhand.exe` alone (no `hyperhand-agent.exe` beside it) from an elevated prompt with `install --quiet`: the installed agent is the embedded one, and Apps and Features lists HyperHand with the candidate version. Publishing is the [Releasing](building.md#releasing) step.
