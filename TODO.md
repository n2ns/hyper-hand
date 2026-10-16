# HyperHand TODO

Updated: 2026-10-16.

This document tracks remaining delivery work, unimplemented capabilities and acceptance gaps. An unchecked acceptance item is not a confirmed defect. Priorities reflect the current workflow: AI-driven Windows VM automation and AutoCAD testing.

References: [tool behavior](docs/features.md), [Win10 acceptance](docs/acceptance-20261010.md), [semantic control acceptance](docs/semantic-acceptance-20261010.md), and [AutoCAD and VMConnect acceptance](docs/p1p2-acceptance-20261016.md).

## 1. Remaining acceptance coverage

Record the exact source/build, environment, expected behavior and independent evidence for each result. Preserve the distinction between unit coverage and actual installed behavior.

- [ ] **Host tray Restart menu:** click the actual menu and verify replacement of the old tray, MCP recovery and absence of duplicate instances. Installer-driven replacement is not equivalent coverage.
- [ ] **Host self-uninstall:** start uninstall from the installed executable; verify self-removal/deferred cleanup, service/task/socket registration cleanup and successful reinstallation.
- [ ] **Display configurations:** test non-default DPI, multiple monitors and negative coordinate origins. Verify screenshot coordinates and actual input targets.
- [ ] **Session transitions:** the refusal in an enhanced session and a manual `tscon` back to the console passed on Win10 (see the [enhanced session record](docs/enhanced-session-acceptance-20261015.md)). Open: a disconnected session (enhanced window closed), network RDP, a session locked while on RDP, and a guest whose UAC prompts for consent.
- [ ] **Save at scale:** `vm_save` of a VM with much more memory (the 5-minute job wait); needs the VM's memory raised (a VM setting).
- [ ] **Unlock with a mismatched stored password:** needs the stored unlock password in the user's Credential Manager changed (user decision).
- [ ] **Host restart while a guest job runs:** a `vm_exec background: true` job survives a restart of the installed host and `vm_job` still reports its state and output. Not run on the installed host (see the [jobs acceptance record](docs/tasks-jobs-client-acceptance-20261010.md)).
- [ ] **Agent desktop tests on a locked host:** `TestWindowAt`, `TestFocusedHelper` and `TestControlHintNativeIdentityAndFallback` skip with a reason while the host session is locked; the skip itself was not exercised on a locked host.

Evidence and boundaries: [v0.2.0 acceptance](docs/acceptance-v0.2.0.md#remaining-coverage), [2026-10-10 limits](docs/acceptance-20261010.md#evidence-and-limits), and [semantic acceptance](docs/semantic-acceptance-20261010.md).

## 2. Product scope decisions

- [ ] Decide whether to control a session while it stays on enhanced/RDP (guest-side capture and input, or HyperHand as the RDP client). Currently `vm_take_console` returns the session to the console and recovers an existing disconnected viewer in basic mode; concurrent control while retaining enhanced/RDP mode remains unsupported. See the [enhanced session record](docs/enhanced-session-acceptance-20261015.md) and [viewer recovery acceptance](docs/p1p2-acceptance-20261016.md#p2-vmconnect-recovery).
- [ ] Decide whether unattended first sign-in belongs in scope. Current unlock support requires an already signed-in, locked session with the agent running.
- [ ] Decide whether UWP/MSIX discovery and launch support is needed beyond current Win32 Start Menu/App Paths discovery.
- [ ] Decide whether test VM unlock passwords should have at least 4 characters. `vm_evidence` does not redact secrets shorter than 4 characters (they would garble the record); such secrets can appear in exported evidence and are counted in `skipped_short_secrets`.
