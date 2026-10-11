# HyperHand TODO

Updated: 2026-10-11.

This document tracks remaining delivery work, unimplemented capabilities and acceptance gaps. An unchecked acceptance item is not a confirmed defect. Priorities reflect the current workflow: AI-driven Windows VM automation and AutoCAD testing.

## 0. Execution plan (autonomous phases)

A checklist an AI can run phase after phase without the user. Run completed on 2026-10-11. Work the phases in order; each one ends with a commit and push, so an interrupted run resumes at the first unchecked phase.

Every phase follows the same steps:

1. Implement, with focused tests; `go vet ./cmd/... ./internal/...` and `go test -race ./cmd/... ./internal/...` pass.
2. Install on the host with `go run ./cmd/hyperhand dev-install` (no UAC), then `vm_update_agent` on `Win10`.
3. Accept on `Win10` only (`Win10-PipeSifu` belongs to PipeSifu testing); keep evidence under ignored `build/<phase>-<date>/` and write an acceptance record in `docs/` when the phase adds or proves behavior.
4. Update the affected chapter in `docs/features/`, `docs/user-guide.md` if usage changes, `CHANGELOG.md` [Unreleased] and this TODO (check the phase, move finished items to section 6).
5. Review by risk as the global rules say (correctness and stated requirements only; a re-review checks only the fixes), then commit and push.

Stop conditions: a step that needs a user decision, a UAC prompt or work on the host desktop, or a failure that cannot be reproduced, is recorded under the phase as "Blocked: ..." and the run continues with the next phase. Never push a release tag.

- [x] **A. Housekeeping.** Correct stale statuses in this TODO (for example, `Win10-PipeSifu` agents have been updated). Keep the historical Go experiments under ignored `build/` out of package discovery so that `go test -race ./...` passes cleanly (section 4); preserve those artifacts.
- [x] **B. Sequential batches with assertions** (section 2, P2). One tool call runs ordered tool steps with per-step results and optional assertions, stops at the first failure or failed assertion, reports the last completed step and never repeats a side-effecting step automatically. Contract, tests, Win10 acceptance with a real multi-step UI flow.
- [x] **C. Automatable acceptance gaps** (sections 1 and 3), on `Win10` only: mirror expired plans, old-agent upgrade errors, cancellation and mid-transfer disconnects with staging cleanup; guest uninstall and reinstall (independent recovery path); blocked graceful shutdown with an unsaved disposable document; long checkpoint merges at the timeout boundary; Production-only checkpoints; hung UI threads. Fix what fails (reproduce first).
  Done: see the [acceptance record](docs/acceptance-gaps-20261011.md); a hung target no longer blocks every call to its VM (fixed).
  Done later the same day: Production-only checkpoints, after adding `vm_set_checkpoint_type` (user decision); see the [acceptance record](docs/production-checkpoint-acceptance-20261011.md). It found that `kind` did not identify production checkpoints, since fixed: `kind` is Hyper-V's snapshot type and `holds_memory` tells whether a checkpoint saved memory.
  Resolved 2026-10-11 (user decision): the 15-minute merge boundary is covered by unit tests instead of a real merge (host: `TestDeleteMergeTimeoutIsNotCompletion`; the job wait itself: the `hyperv` deadline test).
  Resolved 2026-10-11 (user decision): the pending sign-out after a blocked shutdown is Windows behavior; `vm_shutdown` now reports it and names the way out, and the acceptance criterion is no power-off and an accurate report.
- [x] **D. VM save and pause controls** (section 2, P2). Explicit Save and Pause operations and the resume readiness of the agent and desktop; Win10 acceptance of save, pause and resume.
- [x] **E. Acceptance evidence export** (section 2, P2). Package versions, environment, steps, assertions, screenshots and file hashes into one reviewable artifact without credentials or unrelated data; produce one for a real Win10 run.
- [x] **F. Release preparation** (section 4). Migration notes in `CHANGELOG.md` for removed tools/parameters and guest upgrades; build the release package locally and verify packaged and installed versions and hashes. Stop before tagging: publishing is the user's decision.
  Done: migration notes in `CHANGELOG.md`; a `0.3.0` candidate package built with the workflow's build step, installed without UAC and verified on the host and `Win10` (see the [record](docs/release-candidate-20261011.md)). Publishing (version section, tag) is left to the user.

Not in this plan (need the user or the host desktop): the product decisions in section 5; the host tray Restart menu, host self-uninstall (UAC) and interactive host UAC (section 3); display configurations that change VM settings; VMConnect viewer reconnection (host window); publishing the release.

## 1. Directory mirror delivery

Directory mirroring was implemented in `67ba68d` and pushed to `main`. Ordinary `vm_push` copy behavior is preserved. The new `mode: mirror` uses a read-only plan followed by a single-use apply, including empty directories, drift checks, verified copying and deletion of extra target entries. See the [mirror contract](docs/features/files.md#65-directory-mirror).

Completed verification: maintained-package race tests and vet, ten consecutive mirror-engine race runs, host and guest builds, independent review, and installed Win10 mirror acceptance. Windows junction rejection, locked-file failure and directory-to-file replacement during deletion were also covered by focused tests. See the [joint acceptance record](docs/ui-wait-mirror-acceptance-20261010.md) for exact runtime coverage and limits.

- [x] Install the mirror-capable host and guest builds, then verify running versions, executable paths and SHA-256 hashes. Mirror acceptance used `ui-wait-mirror-20261010`; later installed builds are recorded in the corresponding acceptance documents. No release has been published.
- [x] Real Win10: planning makes no changes and does not reserve VM writes; new/changed/unchanged files, empty directories, missing roots, extra-entry removal and empty-source cleanup; source/target drift; consumed/foreign-task plans; and ordinary copy preserving extras.
- [x] Real Win10: a locked destination reports `mirror_partial` with confirmed and pending operations. Independent hashes prove an earlier copy completed while the locked original and later extra file were preserved.
- [x] Extend installed-runtime acceptance to expired plans, old-agent upgrade errors, cancellation and mid-transfer disconnects. Require a fresh plan after partial/unknown outcomes and verify temporary staging/task-plan cleanup after interruption. Existing host/engine tests do not establish these real transport-failure cases. Passed on Win10: see the [acceptance record](docs/acceptance-gaps-20261011.md#directory-mirror-interruption-and-expiry).
- [x] Run the symbolic-link cases in an environment that permits creating them. The race-enabled mirror test binary passed inside Win10 with elevation: 45 PASS records including subtests, zero skips. This includes symbolic-link/ancestor-link rejection, junction rejection and cancellation after a copy; it does not simulate a broken host/guest transport.

Local evidence is under ignored `build/mirror-20261010/` (initial verification and the earlier blocked preflight) and `build/ui-wait-20261010/` (installed joint acceptance). It is not included in Git.

## 2. Unimplemented capabilities

These are development candidates, not authorization to implement all of them together. Each change should include its tool contract, focused tests and relevant runtime acceptance.

| Priority | Capability | Minimum delivery and acceptance |
| --- | --- | --- |
| P2 | VMConnect viewer reconnection | Recover the host viewer after VM lifecycle operations leave it disconnected. Verify viewer recovery separately from guest command and screenshot connectivity; the latter can remain healthy while the viewer is disconnected. |

References: [tool behavior](docs/features.md), [Win10 acceptance](docs/acceptance-20261010.md), and [semantic control acceptance](docs/semantic-acceptance-20261010.md).

## 3. Remaining acceptance coverage

Record the exact source/build, environment, expected behavior and independent evidence for each result. Preserve the distinction between unit coverage and actual installed behavior.

- [ ] **Host tray Restart menu:** click the actual menu and verify replacement of the old tray, MCP recovery and absence of duplicate instances. Installer-driven replacement is not equivalent coverage.
- [ ] **Cold boot without a signed-in user:** verify that the VM remains running, desktop unavailability is reported clearly and subsequent status queries work. Automatic sign-in or unlocking an existing session does not cover this case.
- [ ] **Original AutoCAD focus-fallback branch:** reproduce failure of `SetForegroundWindow`, establish that the fallback actually runs, then verify the foreground target and input destination. The tested KeyTips recovery sequence does not cover this branch.
- [ ] **Host self-uninstall:** start uninstall from the installed executable; verify self-removal/deferred cleanup, service/task/socket registration cleanup and successful reinstallation. Uninstall/reinstall from an external release executable has already passed.
- [ ] **Display configurations:** test non-default DPI, multiple monitors and negative coordinate origins. Verify screenshot coordinates and actual input targets. The second VM is covered: screenshot coordinates, VM-bound observations, concurrent calls and scaled-pixel input on both VMs passed (see the [second VM acceptance](docs/second-vm-acceptance-20261010.md)).
- [ ] **Session transitions:** verify clear refusal and recovery when switching between console and enhanced/RDP sessions. Enhanced/RDP desktop control is not currently supported; adding it is a separate scope decision below.
- [ ] **Additional CAD/UIA providers:** test required dialogs, custom-drawn controls and elevated CAD targets. Check tree truncation, supported actions, state readback and explicit unsupported results. Reading a control tree does not prove all its controls can be operated.
- [ ] **Control-search performance:** compare three samples per operation on the same 2,234-node Win10 fixture, then repeat AutoCAD and dynamic-control acceptance. The visible-control rectangle hint with strict runtime identity verification is installed; the maintained-package race suite and vet passed. Installed acceptance remains incomplete after full-tree UIA timeouts before the hint benchmarks; investigate their cause before continuing. The earlier element-cache experiment was rejected after a VM performance regression. See the [performance record](docs/control-search-performance-20261010.md).
- [ ] **Unresponsive applications:** hung UI threads passed after a fix (see the [acceptance record](docs/acceptance-gaps-20261011.md#hung-ui-thread)); AutoCAD regeneration remains. UIA helper/host timeouts already exist (10/12 seconds); only add further window-message timeout handling if an actual unbounded path is identified.
- [ ] **Hyper-V keyboard into the Run dialog:** with the agent gone, a 248-character check command with parentheses and `&` typed by `vm_type` did not run, while the same checks as three shorter commands did (see the [acceptance record](docs/acceptance-gaps-20261011.md#not-verified)). Isolate the cause (length, a character, or timing) before relying on long raw-keyboard commands.
- [ ] **Save at scale:** `vm_save` of a VM with much more memory (the 5-minute job wait) and a session that locks while saved or right after resume; `Win10` saved in 3.6 s and stayed unlocked.
- [ ] **Evidence journal limits on the installed host:** 2000 calls and 128 MiB of screenshots, with the dropped counts in `omitted`; covered by unit tests only.
- [ ] **Interactive UAC and unlock failures:** test consent approval/cancellation and mismatched stored credentials. Existing elevation acceptance used a no-consent administrator policy.

Evidence and boundaries: [v0.2.0 acceptance](docs/acceptance-v0.2.0.md#remaining-coverage), [2026-10-10 limits](docs/acceptance-20261010.md#evidence-and-limits), and [semantic acceptance](docs/semantic-acceptance-20261010.md).

## 4. Release and verification workflow


## 5. Product scope decisions

- [ ] Decide whether to support enhanced/RDP desktops. Current screenshots and input target the VM console, and targeted actions refuse an unusable session.
- [ ] Decide whether unattended first sign-in belongs in scope. Current unlock support requires an already signed-in, locked session with the agent running.
- [ ] Decide whether UWP/MSIX discovery and launch support is needed beyond current Win32 Start Menu/App Paths discovery.
- [ ] Decide whether the test VMs' unlock passwords should have at least 4 characters. `vm_evidence` does not redact secrets shorter than 4 characters (they would garble the record), and `Win10`'s stored password has 1 character, so it appears in exported evidence (counted in `skipped_short_secrets`).

Known behavior kept on purpose: an action marks itself as mutating before it asks the agent to activate the target, so even a refusal before anything changed (such as `target_not_responding` for a hung window) makes older observations stale and the caller observes again. Activation that fails part-way can change the foreground, so the conservative invalidation stays.

## 6. Completed capabilities to keep out of the backlog

- [x] Window groups, target integrity checks, structured tool results/errors, observation IDs and freshness checks.
- [x] Typing into AutoCAD with its dynamic-input tooltip: the agent accepts a bare input popup of the target (no caption, sizing border or system menu; same process and UI thread; owner chain to the target) as the foreground while the target stays usable, so `vm_type "_qnew\n"` with the cursor in the drawing area runs the command; modal dialogs and floating palettes still stop input. Installed Win10 acceptance: see the [acceptance record](docs/dyninput-acceptance-20261010.md).
- [x] Ownership of session-per-call clients: deleting an MCP session ends its default task once no call of it is in flight, releasing the VM (temp checkpoints kept); explicit task IDs keep ownership across sessions. `vm_busy` reports `owner_idle_ms` and `owner_in_flight` and names `vm_end_turn {task_id, vm}` for an abandoned explicit owner; `vm_status` reports `owner`. There is no idle takeover and no session timeout: a client that dies without deleting its session keeps its default task. Installed Win10 acceptance: see the [acceptance record](docs/tasks-jobs-client-acceptance-20261010.md).
- [x] Asynchronous guest command jobs: `vm_exec background: true` and `vm_job` (state, incremental output by offsets, `wait_ms`, process-tree cancel, listing); jobs live in the agent and survive MCP reconnects, task ends and host restarts; retention 32 jobs / 24 hours / 16 MiB per stream. Installed Win10 acceptance: see the [acceptance record](docs/tasks-jobs-client-acceptance-20261010.md). Not run on the installed host: a host restart while a job runs.
- [x] Minimal official client: `client/hyperhand_client.py` (module and command; stable task ID, `key=value` arguments, UTF-8 JSON, errors as failures, saved images, control search). See [client/README.md](client/README.md).
- [x] Required `vm` on every tool except `vm_list` (no default VM; `all_temp` needs `vm`), so a call meant for a VM that is off never reaches another one. Installed-host acceptance with `Win10` and `Win10-PipeSifu`: see the [acceptance record](docs/vm-required-acceptance-20261010.md).
- [x] UIA semantic actions, state readback and four-direction semantic scrolling; custom-provider coverage remains bounded by the acceptance records.
- [x] UI condition waits and assertions: six window/control kinds, exact enabled/value/state matching, one-shot checks, timeout/cancellation, unknown-state protection and concurrent actions. See the [wait contract](docs/features/clipboard-launch-wait.md#73-vm_wait).
- [x] Control search and subtree observation: bounded exact property search, explicit unique/multiple/not-found/incomplete results, subtree diff isolation and direct use of returned identities by actions and waits. Installed Win10 acceptance covered a 2234-node fixture and the real AutoCAD Options tab/Cancel workflow. See the [search contract](docs/features/observation-input.md#control-search-and-subtree-observation) and [acceptance record](docs/control-search-acceptance-20261010.md).
- [x] Checkpoint trees and stable IDs, keep/delete/subtree operations, `save_current` and temporary-checkpoint cleanup.
- [x] Duplicate-name ambiguity for checkpoint restore, keep and delete; this was already verified in the 2026-10-10 acceptance.
- [x] Desktop application discovery and `vm_doctor` diagnostics.
- [x] Historical Go experiments under ignored `build/` stay out of package discovery: the tracked `build/go.mod` makes that directory a separate module, so `go vet ./...` and `go test -race ./...` pass in this workspace (2026-10-11); the artifacts were kept.
- [x] Sequential batches with assertions: `vm_batch` runs up to 64 tool steps through the tools' own handlers, passes earlier results by `${<step>.<path>}`, checks results with `equals`/`contains`/`exists`, stops at the first `step_failed` or `assertion_failed` with `failed_step` and `last_completed`, and never repeats a step. See the [batch contract](docs/features/clipboard-launch-wait.md#76-vm_batch) and the [acceptance record](docs/batch-acceptance-20261011.md).
- [x] Guest uninstall and reinstall through paths independent of the agent (Run dialog uninstall, Hyper-V keyboard checks, `vm_install_agent`): registration, files and process removed, then path, hash and communication restored. See the [acceptance record](docs/acceptance-gaps-20261011.md#guest-uninstall-and-reinstall).
- [x] Hung target windows: actions return `target_not_responding` at once instead of blocking every call to the VM; search and UI waits report the helper timeout as `target_not_responding`. See the [acceptance record](docs/acceptance-gaps-20261011.md#hung-ui-thread).
- [x] VM save and pause: `vm_save` and `vm_pause`, resumed by `vm_start` with the same readiness wait and `previous_state`; saved and paused VMs (`EnabledState` 6 and 9) are reported by name. See the [contract](docs/features/vm-checkpoints.md#32-vm_start-vm_shutdown-vm_turn_off-vm_save-and-vm_pause) and the [acceptance record](docs/save-pause-acceptance-20261011.md).
- [x] Acceptance evidence export: every task journals its calls, and `vm_evidence` writes one reviewable zip (versions and hashes, environment, steps, assertions, screenshots, file hashes; only this task and VM; stored unlock password and given strings redacted, secrets under 4 characters skipped and counted). See the [contract](docs/features/clipboard-launch-wait.md#77-vm_evidence) and the [acceptance record](docs/evidence-acceptance-20261011.md).
- [x] Long checkpoint merges: a merge outlasting the 15-minute wait is reported as `failed` with `elapsed_ms` and `next` saying it may still be running, nothing claimed deleted, the delete not repeated; covered by unit tests by decision (2026-10-11), no real 15-minute merge.
- [x] Blocked graceful shutdown: no power-off (Win10 acceptance); the guest may stay in a pending sign-out, which `vm_shutdown` now reports with the way out. Document recovery is not promised: it is Windows behavior (decision 2026-10-11). See the [acceptance record](docs/acceptance-gaps-20261011.md#blocked-graceful-shutdown).
- [x] Production-only checkpoints: `vm_set_checkpoint_type` switches the VM setting; on Win10 a ProductionOnly checkpoint saved no memory, `vm_restore start: false` left the VM off, `vm_start` reconnected after the cold start, the disk was restored, default restore started the VM and `vm_end_turn` cleaned up. See the [acceptance record](docs/production-checkpoint-acceptance-20261011.md).
- [x] Checkpoint `kind` and `holds_memory`: Hyper-V gives standard and production checkpoints the same snapshot type, so `kind` reports it as is and `holds_memory` (state `running` or `saved`) tells whether restoring brings the programs back. See the [contract](docs/features/vm-checkpoints.md#33-checkpoints).
- [x] Release 0.3.0 published 2026-10-11 (tag `v0.3.0` at `82e18fe`, GitHub Release Latest); installed on the host from the release zip (hashes equal, server reports 0.3.0) and the agents of `Win10` and `Win10-PipeSifu` updated to 0.3.0.
- [x] Agent desktop tests on a locked host: `TestWindowAt`, `TestFocusedHelper` and `TestControlHintNativeIdentityAndFallback` skip with a reason while the session is locked (`WTSSessionInfoEx`; Windows cannot be unlocked by a program, decision 2026-10-11) and run as before otherwise. The skip itself was not exercised on a locked host.
- [x] Guest agent UI in English (decision 2026-10-11): tray menu `Host connected` / `Waiting for the host` / `Exit` and the uninstall message box, checked on the Win10 screen.
- [x] Directory mirror implementation, host-side verification and installed Win10 acceptance; interruption, expiry and old-agent coverage passed (section 1); release remains in section 4.

The old title selectors remain removed. UI waits use HWND/PID, exact control properties or observation-bound runtime identity; they do not restore the legacy title-based interface.
