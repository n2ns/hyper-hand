---
name: vm-acceptance
description: Runs an acceptance check of a HyperHand change on the installed host and a real guest VM, following docs/manual-verification.md — preflight, temporary checkpoint, a runner script that checks independent state, evidence under build/, cleanup, and what to update afterwards (TODO, research, README). Use when a change must be verified in a real VM, or to work off an acceptance item in TODO.md.
argument-hint: [item]
---

# Acceptance on a real VM

The criteria are in [Manual verification](../../../docs/manual-verification.md): test VM baseline, independent checks, evidence and cleanup, plus setups for UAC, cold boot, enhanced session and others. This skill is the order of work. All commands run from the repository root in WSL; `hh` below stands for `python3 client/hyperhand_client.py --vm <vm> --task-file build/<item>-<yyyymmdd>/task`.

## 1. Preflight

1. `scripts/test-windows.sh` passes (and `python3 -m unittest discover -s client` when the client changed).
2. Install with the `dev-install` skill and update the VM's agent.
3. Hashes: `sha256sum "/mnt/c/Program Files/HyperHand/hyperhand.exe" "/mnt/c/Program Files/HyperHand/hyperhand-agent.exe"` equal the files in `build/dev-install/`; `hh vm_file_info paths='["%LOCALAPPDATA%\\HyperHand\\hyperhand-agent.exe"]'` gives the agent's hash in the guest.
4. `hh vm_doctor`: every check `ok`. Note what exists before the run: `hh vm_checkpoints`, `hh vm_windows`.

## 2. Run

1. Create the evidence directory `build/<item>-<yyyymmdd>/` (ignored by git) and the task file in it, then `hh vm_checkpoint label=<item>`; keep the returned `id`.
2. Write the runner as a script in that directory (Python with `client/hyperhand_client.py` as a module, or `hh` calls), so the run can be repeated. Every step checks independent state (a fixture's state file, process identity, file hashes, a fresh observation), not the tool's own report; save JSON results and screenshots next to it.
3. Change guest settings only inside the checkpoint. Host settings (enhanced session mode, a VM's memory or checkpoint type) need the user or `vm_set_checkpoint_type`; set them back afterwards.
4. On a failure, first decide whether the product, the fixture or the runner is wrong; a product defect is reproduced, fixed in its own commit and the run repeated on the new build.

## 3. Clean up

1. `hh vm_restore id=<checkpoint id>`, then `hh vm_end_turn`: `errors` and `skipped` are empty.
2. `hh vm_checkpoints` lists only what existed before; `hh vm_doctor` is all `ok`; the agent's hash is unchanged; a call with a new task (`python3 client/hyperhand_client.py --vm <vm> vm_exec command=hostname`) succeeds, so ownership was released.

## 4. Record the outcome

Results are not written up as records ([document map](../../../docs/doc-map.md#rules)):

- Passed: remove the item, or its passed part, from [TODO](../../../TODO.md). A conclusion users rely on becomes a sentence in README's Known limitations.
- Not passed or not run: keep or narrow the TODO item with what is still open.
- Facts learned about Hyper-V, Windows, UI Automation or AutoCAD go to [research](../../../docs/research/) with date, environment and how they were observed; changed behavior goes to its Features chapter.
- Report to the user: build version, what passed, what failed or was not run, and where the evidence is.
