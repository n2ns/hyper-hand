# HyperHand TODO

Updated: 2026-10-15.

This document tracks remaining delivery work, unimplemented capabilities and acceptance gaps. An unchecked acceptance item is not a confirmed defect. Priorities reflect the current workflow: AI-driven Windows VM automation and AutoCAD testing.

## 1. Unimplemented capabilities

These are development candidates, not authorization to implement all of them together. Each change should include its tool contract, focused tests and relevant runtime acceptance.

| Priority | Capability | Minimum delivery and acceptance |
| --- | --- | --- |
| P2 | VMConnect viewer reconnection | Recover the host viewer after VM lifecycle operations leave it disconnected. Verify viewer recovery separately from guest command and screenshot connectivity; the latter can remain healthy while the viewer is disconnected. |

References: [tool behavior](docs/features.md), [Win10 acceptance](docs/acceptance-20261010.md), and [semantic control acceptance](docs/semantic-acceptance-20261010.md).

## 2. Remaining acceptance coverage

Record the exact source/build, environment, expected behavior and independent evidence for each result. Preserve the distinction between unit coverage and actual installed behavior.

- [ ] **Host tray Restart menu:** click the actual menu and verify replacement of the old tray, MCP recovery and absence of duplicate instances. Installer-driven replacement is not equivalent coverage.
- [ ] **Host self-uninstall:** start uninstall from the installed executable; verify self-removal/deferred cleanup, service/task/socket registration cleanup and successful reinstallation.
- [ ] **Display configurations:** test non-default DPI, multiple monitors and negative coordinate origins. Verify screenshot coordinates and actual input targets.
- [ ] **Session transitions:** the refusal in an enhanced session and a manual `tscon` back to the console passed on Win10 (see the [enhanced session record](docs/enhanced-session-acceptance-20261015.md)). Open: a disconnected session (enhanced window closed), network RDP, a session locked while on RDP, and a guest whose UAC prompts for consent.
- [ ] **AutoCAD ribbon (WPF) through UI Automation:** the ribbon tabs were not reached because reads of the main window run into the 10 s helper limit (control-search performance below). See the [acceptance record](docs/acceptance-followups-20261011.md#ui-automation-coverage).
- [ ] **Control-search performance:** cause of the full-tree timeouts found on 2026-10-11: AutoCAD 2015's main window has about 1,400 control-view nodes and HyperHand's node-by-node walk with current properties reads about 120 per second; in .NET, a walk with a CacheRequest took 7.9 s and one cached `FindAll` 4.9 s for the same tree (11.5 s uncached). Decide whether to switch the agent to cached subtree reads (an earlier element-cache experiment was rejected after a VM performance regression). A prototype is in `d1d1c1f`, off by default (`uiaCacheTree`): on the 2,234-node fixture the cached read matched the walk node for node, list 14.3-23.6 s -> 5.1-6.5 s and find 5.0 s -> 2.2-2.5 s (selector properties only); not yet measured on AutoCAD. After the decision, repeat the 2,234-node fixture benchmark and the AutoCAD ribbon. See the [performance record](docs/control-search-performance-20261010.md) and the [acceptance record](docs/acceptance-followups-20261011.md#ui-automation-coverage).
- [ ] **Save at scale:** `vm_save` of a VM with much more memory (the 5-minute job wait); needs the VM's memory raised (a VM setting).
- [ ] **Unlock with a mismatched stored password:** needs the stored unlock password in the user's Credential Manager changed (user decision).
- [ ] **Host restart while a guest job runs:** a `vm_exec background: true` job survives a restart of the installed host and `vm_job` still reports its state and output. Not run on the installed host (see the [jobs acceptance record](docs/tasks-jobs-client-acceptance-20261010.md)).
- [ ] **Agent desktop tests on a locked host:** `TestWindowAt`, `TestFocusedHelper` and `TestControlHintNativeIdentityAndFallback` skip with a reason while the host session is locked; the skip itself was not exercised on a locked host.

Evidence and boundaries: [v0.2.0 acceptance](docs/acceptance-v0.2.0.md#remaining-coverage), [2026-10-10 limits](docs/acceptance-20261010.md#evidence-and-limits), and [semantic acceptance](docs/semantic-acceptance-20261010.md).

## 3. Product scope decisions

- [ ] Decide whether to control a session while it stays on enhanced/RDP (guest-side capture and input, or HyperHand as the RDP client). Returning the session to the console (`vm_take_console`) disconnects the viewer, so a person and HyperHand cannot share the session; see the [enhanced session record](docs/enhanced-session-acceptance-20261015.md).
- [ ] Decide whether unattended first sign-in belongs in scope. Current unlock support requires an already signed-in, locked session with the agent running.
- [ ] Decide whether UWP/MSIX discovery and launch support is needed beyond current Win32 Start Menu/App Paths discovery.
- [ ] Decide whether the test VMs' unlock passwords should have at least 4 characters. `vm_evidence` does not redact secrets shorter than 4 characters (they would garble the record), and `Win10`'s stored password has 1 character, so it appears in exported evidence (counted in `skipped_short_secrets`).
