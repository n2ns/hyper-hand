---
name: vm-acceptance
description: Runs an acceptance check of a HyperHand change on the installed host and a real guest VM, following docs/manual-verification.md — preflight, temporary checkpoint, a runner script that checks independent state, evidence under build/, cleanup, and what to update afterwards (TODO, research, README). Use when a change must be verified in a real VM, or to work off an acceptance item in TODO.md.
argument-hint: [item]
---

# Acceptance on a real VM

The criteria are in [Manual verification](../../../docs/manual-verification.md): test VM baseline, independent checks, evidence and cleanup, plus setups for UAC, cold boot, enhanced session and others. This skill is the order of work. All commands run from the repository root in WSL; `hh` below stands for `python3 client/hyperhand_client.py --vm <vm> --task-file build/<item>-<yyyymmdd>/task`.

## 1. Preflight

1. The automated tests pass (the `windows-test` skill).
2. Install with the `dev-install` skill and update the VM's agent.
3. Hashes: `sha256sum "/mnt/c/Program Files/HyperHand/hyperhand.exe" "/mnt/c/Program Files/HyperHand/hyperhand-agent.exe"` equal the files in `build/dev-install/`; `hh vm_file_info paths='["%LOCALAPPDATA%\\HyperHand\\hyperhand-agent.exe"]'` gives the agent's hash in the guest.
4. `hh vm_doctor`: every check `ok`. Note what exists before the run: `hh vm_checkpoints`, `hh vm_windows`. A VM without automatic sign-in that was off waits at the sign-in screen after `vm_start` (`agent_required`); ask the user to sign in. Read the screen size from an original-size screenshot (`Run.screen_size()`) instead of trusting the local notes.

## 2. Run

1. Write the runner as a script in `build/<item>-<yyyymmdd>/` (ignored by git), so the run can be repeated. Use [acceptance.py](acceptance.py): `Run(vm, item)` creates that directory, its task file and the client; `run.checkpoint()` takes the temporary checkpoint and keeps its ID in the evidence directory (one file per VM), so a runner restarted after a crash restores the same one; `run.check(name, condition, evidence)` records each check and `run.save()` writes the results. Put the checks in `try:` and `run.cleanup()` and `run.save()` in `finally:`. Give each check its own function and run them selectively (for example through an `ONLY` environment variable), so a failed check can be repeated alone.
2. Every check uses independent state, not the tool's own report: `png_size` of the saved screenshot, `run.guest_cursor()` after a click, `run.process_name(pid)`, `run.foreground()`, a fixture's state file, file hashes or a fresh observation. Fixtures: `run.list_fixture(counts)` (Win32 lists with every item exposed), `run.open_start_flyout()`, and the setups in [Manual verification](../../../docs/manual-verification.md#setups-for-specific-checks). Stop the processes you started (`run.stop_process`).
3. Change guest settings only inside the checkpoint. Host settings (enhanced session mode, a VM's memory or checkpoint type) need the user or `vm_set_checkpoint_type`; set them back afterwards.
4. On a failure, first decide whether the product, the fixture or the runner is wrong; a product defect is reproduced, fixed in its own commit and the run repeated on the new build.

## 3. Clean up

1. `run.cleanup()` restores the checkpoint and calls `vm_end_turn`; when the host was reinstalled during the run (a new run ID no longer owns the temp checkpoint) it removes it with `all_temp: true`. It checks that `errors` and `skipped` are empty, that no checkpoint the run added is left and that a new task can write (ownership was released, with a probe that does not keep ownership itself). A failing call becomes a failed `cleanup` check instead of an exception. Without a runner, `Run(vm, item).cleanup()` restores the checkpoint of a crashed run on that VM; on a later day pass the run's date (`Run(vm, item, day="yyyymmdd")`), since the evidence directory is dated.
2. `hh vm_doctor` is all `ok` and the agent's hash is unchanged; a restored checkpoint can hold an older agent, so update it again (`dev-install` skill, step 2) when it does.

## 4. Record the outcome

Results are not written up as records ([document map](../../../docs/doc-map.md#rules)):

- Passed: remove the item, or its passed part, from [TODO](../../../TODO.md). A conclusion users rely on becomes a sentence in README's Known limitations.
- Not passed or not run: keep or narrow the TODO item with what is still open.
- Facts learned about Hyper-V, Windows, UI Automation or an application go to [research](../../../docs/research/) with date, environment and how they were observed; changed behavior goes to its Features chapter.
- Report to the user: build version, what passed, what failed or was not run, and where the evidence is.
