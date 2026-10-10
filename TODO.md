# HyperHand TODO

Updated: 2026-10-10.

This document tracks remaining delivery work, unimplemented capabilities and acceptance gaps. An unchecked acceptance item is not a confirmed defect. Priorities reflect the current workflow: AI-driven Windows VM automation and AutoCAD testing.

## 1. Directory mirror delivery

Directory mirroring was implemented in `67ba68d` and pushed to `main`. Ordinary `vm_push` copy behavior is preserved. The new `mode: mirror` uses a read-only plan followed by a single-use apply, including empty directories, drift checks, verified copying and deletion of extra target entries. See the [mirror contract](docs/features.md#65-directory-mirror) and [usage guide](docs/user-guide.md#other-tools).

Completed verification: maintained-package race tests and vet, ten consecutive mirror-engine race runs, host and guest builds, independent review, and installed Win10 mirror acceptance. Windows junction rejection, locked-file failure and directory-to-file replacement during deletion were also covered by focused tests. See the [joint acceptance record](docs/ui-wait-mirror-acceptance-20261010.md) for exact runtime coverage and limits.

- [x] Install the new host and guest builds, then verify running versions, executable paths and SHA-256 hashes. Both now run `ui-wait-mirror-20261010`; no release has been published.
- [x] Real Win10: planning makes no changes and does not reserve VM writes; new/changed/unchanged files, empty directories, missing roots, extra-entry removal and empty-source cleanup; source/target drift; consumed/foreign-task plans; and ordinary copy preserving extras.
- [x] Real Win10: a locked destination reports `mirror_partial` with confirmed and pending operations. Independent hashes prove an earlier copy completed while the locked original and later extra file were preserved.
- [ ] Extend installed-runtime acceptance to expired plans, old-agent upgrade errors, cancellation and mid-transfer disconnects. Require a fresh plan after partial/unknown outcomes and verify temporary staging/task-plan cleanup after interruption. Existing host/engine tests do not establish these real transport-failure cases.
- [x] Run the symbolic-link cases in an environment that permits creating them. The race-enabled mirror test binary passed inside Win10 with elevation: 45 PASS records including subtests, zero skips. This includes symbolic-link/ancestor-link rejection, junction rejection and cancellation after a copy; it does not simulate a broken host/guest transport.

Local evidence is under ignored `build/mirror-20261010/` (initial verification and the earlier blocked preflight) and `build/ui-wait-20261010/` (installed joint acceptance). It is not included in Git.

## 2. Unimplemented capabilities

These are development candidates, not authorization to implement all of them together. Each change should include its tool contract, focused tests and relevant runtime acceptance.

| Priority | Capability | Minimum delivery and acceptance |
| --- | --- | --- |
| P1 | Control search and subtree observation | Find controls by properties such as name or AutomationId, then read a selected subtree; return explicit ambiguity, stale-target and truncation results. Current whole-window trees default to 200 nodes and are capped at 1000; AutoCAD has reached the default limit in real observations. |
| P1 | Asynchronous guest command jobs | Submit a command and receive a job ID; query state and incremental output, cancel its process tree, and retrieve results after reconnecting. Define retention and restart behavior. Current `vm_exec` waits for completion; detached `vm_launch` does not capture command results. |
| P1/P2 | Recovery of abandoned task ownership | Inspect the owning task and in-flight calls, then provide controlled cleanup and release. Preserve active work and avoid unsafe automatic takeover. Current VM write ownership lasts until successful `vm_end_turn`; there is no idle takeover. |
| P2 | VM save and pause controls | Add explicit Save/Pause operations and define resume readiness for the agent and desktop. Saved/Paused states are already recognized, and `vm_start` already requests Running; checkpoint restore is a separate capability. |
| P2 | Sequential batches with assertions | Execute ordered tool steps with per-step results, stop on failure and identify the last completed step. Do not automatically repeat side effects. Existing key sequences and action readback remain available. |
| P2 | Acceptance evidence export | Package versions, environment, steps, assertions, screenshots and file hashes into a reviewable artifact. Exclude credentials and unrelated data. Current evidence primarily consists of local ignored files. |
| P2 | VMConnect viewer reconnection | Recover the host viewer after VM lifecycle operations leave it disconnected. Verify viewer recovery separately from guest command and screenshot connectivity; the latter can remain healthy while the viewer is disconnected. |

References: [tool behavior](docs/features.md), [Win10 acceptance](docs/acceptance-20261010.md), and [semantic control acceptance](docs/semantic-acceptance-20261010.md).

## 3. Remaining acceptance coverage

Record the exact source/build, environment, expected behavior and independent evidence for each result. Preserve the distinction between unit coverage and actual installed behavior.

- [ ] **Host tray Restart menu:** click the actual menu and verify replacement of the old tray, MCP recovery and absence of duplicate instances. Installer-driven replacement is not equivalent coverage.
- [ ] **Cold boot without a signed-in user:** verify that the VM remains running, desktop unavailability is reported clearly and subsequent status queries work. Automatic sign-in or unlocking an existing session does not cover this case.
- [ ] **Original AutoCAD focus-fallback branch:** reproduce failure of `SetForegroundWindow`, establish that the fallback actually runs, then verify the foreground target and input destination. The tested KeyTips recovery sequence does not cover this branch.
- [ ] **Host self-uninstall:** start uninstall from the installed executable; verify self-removal/deferred cleanup, service/task/socket registration cleanup and successful reinstallation. Uninstall/reinstall from an external release executable has already passed.
- [ ] **Guest uninstall and reinstall:** verify process exit, startup registration and file cleanup, then reinstall and check path/version/hash and communication. Use an independent launch/recovery path; the agent must not depend on its own `vm_exec` while uninstalling itself.
- [ ] **Display and VM configurations:** test non-default DPI, multiple monitors, negative coordinate origins and a second VM. Verify screenshot coordinates and actual input targets.
- [ ] **Session transitions:** verify clear refusal and recovery when switching between console and enhanced/RDP sessions. Enhanced/RDP desktop control is not currently supported; adding it is a separate scope decision below.
- [ ] **Additional CAD/UIA providers:** test required dialogs, custom-drawn controls and elevated CAD targets. Check tree truncation, supported actions, state readback and explicit unsupported results. Reading a control tree does not prove all its controls can be operated.
- [ ] **Unresponsive applications:** exercise hung UI threads and AutoCAD regeneration. UIA helper/host timeouts already exist (10/12 seconds); only add further window-message timeout handling if an actual unbounded path is identified.
- [ ] **Blocked graceful shutdown:** use an unsaved disposable document to block shutdown; verify that failure does not silently force power-off and the document remains recoverable.
- [ ] **Production-only checkpoints:** prevent fallback to Standard; verify reported kind, disk restoration, actual power state, `start: false`, default startup, reconnection and cleanup. Standard memory/disk restore has already passed.
- [ ] **Long checkpoint merges:** exercise the timeout boundary and report any merge still running without claiming completion or automatically replaying deletion.
- [ ] **Interactive UAC and unlock failures:** test consent approval/cancellation and mismatched stored credentials. Existing elevation acceptance used a no-consent administrator policy.

Evidence and boundaries: [v0.2.0 acceptance](docs/acceptance-v0.2.0.md#remaining-coverage), [2026-10-10 limits](docs/acceptance-20261010.md#evidence-and-limits), and [semantic acceptance](docs/semantic-acceptance-20261010.md).

## 4. Release and verification workflow

- [ ] Publish a release containing the current AI-oriented tool surface, protocol generation 2, semantic actions, directory mirroring and UI waits/assertions. They remain under [Unreleased](CHANGELOG.md#unreleased). Include migration notes for removed tools/parameters and guest upgrade requirements; verify packaged and installed binary versions/hashes.
- [ ] Keep historical local Go experiments out of the default package-discovery path so that `go test -race ./...` can run cleanly in this workspace. The existing ignored `build/acceptance-20261010-full` and `build/service-poc-20261006/service` contain incompatible old sources. Current maintained packages pass `go test -race ./cmd/... ./internal/...`; that is not a passing full `./...` result. Preserve unrelated artifacts when addressing the test layout.

## 5. Product scope decisions

- [ ] Decide MCP authentication and a pause/disable-control mechanism before wider distribution. The loopback MCP endpoint has no authentication; broker pipe ACLs and task IDs do not authenticate MCP callers.
- [ ] Decide whether to support enhanced/RDP desktops. Current screenshots and input target the VM console, and targeted actions refuse an unusable session.
- [ ] Decide whether unattended first sign-in belongs in scope. Current unlock support requires an already signed-in, locked session with the agent running.
- [ ] Decide whether UWP/MSIX discovery and launch support is needed beyond current Win32 Start Menu/App Paths discovery.
- [ ] Decide whether guest tray text should be standardized in English, then verify the chosen language in the actual UI.

## 6. Completed capabilities to keep out of the backlog

- [x] Window groups, target integrity checks, structured tool results/errors, observation IDs and freshness checks.
- [x] UIA semantic actions, state readback and four-direction semantic scrolling; custom-provider coverage remains bounded by the acceptance records.
- [x] UI condition waits and assertions: six window/control kinds, exact enabled/value/state matching, one-shot checks, timeout/cancellation, unknown-state protection and concurrent actions. See the [wait contract](docs/features.md#73-vm_wait).
- [x] Checkpoint trees and stable IDs, keep/delete/subtree operations, `save_current` and temporary-checkpoint cleanup.
- [x] Duplicate-name ambiguity for checkpoint restore, keep and delete; this was already verified in the 2026-10-10 acceptance.
- [x] Desktop application discovery and `vm_doctor` diagnostics.
- [x] Directory mirror implementation, host-side verification and installed Win10 acceptance; additional interruption/upgrade coverage remains in section 1, and release remains in section 4.

The old title selectors remain removed. UI waits use HWND/PID, exact control properties or observation-bound runtime identity; they do not restore the legacy title-based interface.
