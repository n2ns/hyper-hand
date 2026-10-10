# Acceptance evidence export (`vm_evidence`): acceptance (2026-10-11)

## Build

- Source `82cf8a8` plus the evidence change committed with this record, installed with `go run ./cmd/hyperhand dev-install` (no UAC) as `dev-20261011-041429-82cf8a8-dirty`; installed `hyperhand.exe` `5621dbf4fe64…`, `hyperhand-agent.exe` `c99529aeefc1…`. Win10 was updated with `vm_update_agent`; the export itself recorded the running agent's hash as equal to the host's.
- `go vet ./...` passes; `go test -race` passes for `host`, `hyperv`, `broker`, `mirror`, `tray` and `cmd`. `internal/agent` is unchanged; its desktop hit-test tests need an unlocked host desktop (see the [save/pause record](save-pause-acceptance-20261011.md#build)).
- Review: one round (correctness and the requirement). Three medium findings were fixed and covered by tests: a secret sent JSON-escaped (`\uXXXX`) escaped redaction (now every decoded string of the record is redacted in one pass, including file facts, agent facts and the task ID); a result over 256 KiB was cut into invalid JSON, losing its assertions and file hashes (long strings are now shortened so the structure stays); some text fields were not redacted (covered by the same pass). One low finding was fixed: screenshots of calls dropped beyond 2000 calls were not counted.

## Win10 run

Evidence: ignored `build/phaseE-20261011/` (`accept_e.py`, `export/` with the zips and the unzipped final export).

One task recorded:

- a `vm_batch` of five steps: launch Notepad, type into `${0.handle}`, select all and copy, `vm_clipboard_get` asserting the text, `vm_observe` of the window. It carried three assertions.
- `vm_file_info` of `notepad.exe`.
- a `vm_exec` that printed the VM's stored unlock password and a license-key-like string.
- `vm_exec taskkill` of Notepad.

`vm_evidence` was then called with `redact` set to that string and `files` naming `notepad.exe` and the installed agent. The result was `calls: 9`, `screenshots: 1`, `assertions: {passed: 3, failed: 0, not_evaluated: 0}`, `files: 3`, `redactions: 2`, `skipped_short_secrets: 1`, and a 23 KB zip with its SHA-256.

- The zip holds `evidence.json`, `report.md` and `screenshots/0006-vm_observe-1.png`.
- `evidence.json` has:
  - the host version, protocol and Windows build, with both executables' SHA-256;
  - the VM name, ID, state, checkpoint setting and current checkpoint, and the agent's version, protocol, hostname and user;
  - nine steps in order, the five batch steps with `parent: 1`;
  - the three assertions as passed with their conditions;
  - the three file entries (one from the `vm_file_info` call, two from `files`), whose agent hash equals the installed host's agent.
- `report.md` renders the same as tables, with the screenshot.
- The license-key string appears nowhere in the zip. Secrets shorter than 4 characters are excluded from redaction to avoid garbling the record and are counted in `skipped_short_secrets`. A second task's calls were not part of the export (unit test).

## Not covered

- Journals at the limits (2000 calls, 128 MiB of screenshots) on the installed host; covered by unit tests only.
