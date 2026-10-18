# HyperHand TODO

Updated: 2026-10-19.

This document tracks remaining delivery work, unimplemented capabilities and acceptance gaps. An unchecked acceptance item is not a confirmed defect. Priorities reflect the current workflow: AI-driven Windows VM automation and AutoCAD testing.

References: [tool behavior](docs/features.md), [how to run acceptance checks](docs/manual-verification.md) and the [research records](docs/research/).

## 1. Remaining acceptance coverage

Run these as described in [Manual verification](docs/manual-verification.md): check against independent state and keep unit coverage apart from installed behavior. Remove an item, or its passed part, once it passes.

- [ ] **Host tray Restart menu:** click the actual menu and verify replacement of the old tray, MCP recovery and absence of duplicate instances. Installer-driven replacement is not equivalent coverage.
- [ ] **Host self-uninstall:** start uninstall from the installed executable; verify self-removal/deferred cleanup, service/task/socket registration cleanup and successful reinstallation.
- [ ] **Display configurations:** test non-default DPI, multiple monitors and negative coordinate origins. Verify screenshot coordinates and actual input targets.
- [ ] **Session transitions:** the refusal in an enhanced session and a manual `tscon` back to the console passed on Win10 ([Hyper-V facts 7 to 10](docs/research/hyper-v.md#enhanced-session-and-the-console)). Open: a disconnected session (enhanced window closed), network RDP, a session locked while on RDP, and a guest whose UAC prompts for consent.
- [ ] **Save at scale:** `vm_save` of a VM with much more memory (the 5-minute job wait); needs the VM's memory raised (a VM setting).
- [ ] **Unlock with a mismatched stored password:** needs the stored unlock password in the user's Credential Manager changed (user decision).
- [ ] **Host restart while a guest job runs:** a `vm_exec background: true` job survives a restart of the installed host and `vm_job` still reports its state and output. Not run on the installed host; reading a job from new sessions and tasks passed.
- [ ] **Agent desktop tests on a locked host:** `TestWindowAt`, `TestFocusedHelper` and `TestControlHintNativeIdentityAndFallback` skip with a reason while the host session is locked; the skip itself was not exercised on a locked host.
- [ ] **VMConnect viewer recovery edge cases:** fresh replacement, healthy reuse and raising a covered viewer passed on Win10-Example ([Hyper-V facts 11 to 14](docs/research/hyper-v.md#vmconnect)). Open: the activation helper's 3-second kill path with a deliberately hung GUI, unknown VMConnect dialogs, and VMConnect in UI languages other than English and Chinese.

## 2. Product scope decisions

- [ ] Decide whether to control a session while it stays on enhanced/RDP (guest-side capture and input, or HyperHand as the RDP client). Currently `vm_take_console` returns the session to the console and recovers an existing disconnected viewer in basic mode; concurrent control while retaining enhanced/RDP mode remains unsupported. See [Hyper-V facts 7 to 14](docs/research/hyper-v.md#enhanced-session-and-the-console).
- [ ] Decide whether unattended first sign-in belongs in scope. Current unlock support requires an already signed-in, locked session with the agent running.
- [ ] Decide whether UWP/MSIX discovery and launch support is needed beyond current Win32 Start Menu/App Paths discovery.
- [ ] Decide whether test VM unlock passwords should have at least 4 characters. `vm_evidence` does not redact secrets shorter than 4 characters (they would garble the record); such secrets can appear in exported evidence and are counted in `skipped_short_secrets`.
