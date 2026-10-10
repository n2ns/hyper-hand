# Sequential batches (`vm_batch`): acceptance (2026-10-11)

## Build

- Source: `0bfd2f0` plus the uncommitted `vm_batch` change committed with this record. Installed with `go run ./cmd/hyperhand dev-install` (no UAC) as `dev-20261011-024532-0bfd2f0-dirty`. Build and installed `hyperhand.exe` SHA-256 `30e582a585cc…d70d4feb`, `hyperhand-agent.exe` `bded49643802…242907ef`.
- `Win10` was updated with `vm_update_agent`; the running `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe` has the same hash. The guest protocol did not change.
- `go vet ./...`, `go test -race ./...` and `python -m unittest discover -s client` pass.

## Win10 runs

All calls went through `client/hyperhand_client.py` with one task. Evidence: ignored `build/phaseB-20261011/` (`accept_b.py`, one JSON file per batch, the returned screenshot).

- **Real UI flow.** One `vm_batch` of five steps: `vm_launch notepad.exe` (assert `handle` exists); `vm_type` into `${0.handle}` (assert `applied_chars` equals the text length); `vm_key` `ctrl+a`, `ctrl+c` into `${0.handle}`; `vm_clipboard_get` (assert `text` equals the typed text); `vm_observe` of `${0.handle}`. All five steps were `ok`, `completed: 5`, and the screenshot came back as an image item indexed by `steps[4].images`; it shows Notepad with the typed text selected. A direct select-all, copy and clipboard read afterwards returned the same text.
- **Failed assertion.** `ctrl+end`, type ` more`, copy, `vm_clipboard_get` asserting `text` equals `wrong`, then a fourth step typing `SHOULD NOT APPEAR`. The result was `assertion_failed` with `failed_step: 3`, `last_completed: 2`, `completed: 3`, four step entries and `failed_assertion.actual` `HyperHand batch 20261011 more`. A direct copy afterwards showed the fourth step had not run.
- **Failed step.** `vm_type` into handle 1, then a step typing `SHOULD NOT APPEAR 2`. The result was `step_failed` with `failed_step: 0` and `last_completed: -1`; `steps[0].error` was the step's own `no_window` object with its `next`. The second step had not run.
- **Structural refusal.** A batch whose second step referenced `${5.handle}` was refused with `invalid_argument`, `step: 1`, and its first step had not typed anything.
- **Ownership.** A first attempt ran while an earlier task of the same session still owned `Win10`: step 0 (`vm_launch`) stopped the batch with `step_failed`, its error `vm_busy` naming the owner task, and nothing ran. After that task's `vm_end_turn` the batch ran.
- Cleanup: Notepad was ended with `vm_exec taskkill /PID <pid> /F` (exit code 0) and the task with `vm_end_turn`.

## Not covered

- `vm_end_turn` from another connection while a batch is waiting in a `vm_wait` step; covered by the task tests only through direct `vm_wait` calls.
