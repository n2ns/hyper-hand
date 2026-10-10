# Session-ended tasks, background jobs and the Python client: acceptance (2026-10-10)

## Builds

- Commits: `d8bbe6f` (default task ends with its MCP session; owner facts), `68424b1` (background jobs, protocol 3), `18629a3` (Python client).
- Installed with `scripts\restart-tray.ps1` from the `68424b1` build: installed and build `hyperhand.exe` SHA-256 `3D376996EFB5…`, `hyperhand-agent.exe` `E206EE445356…CDC7EF80`. Win10 was updated with `vm_update_agent`; it reports protocol 3, and the running `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe` has the same hash.
- `Win10-Example` was not updated. It still runs the protocol 2 agent and will be refused with `agent_outdated` until it is updated with `vm_update_agent`. So will any state restored from its keep baseline checkpoint.
- `go vet ./cmd/... ./internal/...` and `go test -race ./cmd/... ./internal/...` pass. `TestWindowAt` and `TestControlHintNativeIdentityAndFallback` failed only while the host desktop was locked (their hit tests landed on `LockScreenBackstopFrame`) and passed after unlocking. `python -m unittest discover -s client` passes.
- Reviews:

  | Change | Review rounds | Result |
  |---|---|---|
  | Session-ended tasks | Two | First round: one medium documentation finding (only an HTTP `DELETE` ends a session, no idle timeout) and one low race (`all_temp` against an abandoning task), both fixed. Second round: clean. |
  | Background jobs | Three | Medium and low findings on output chunking, the 16 MiB cap, OEM text and `vm_end_turn` cancelling waits were fixed in each round. The third round left two low findings, also fixed. |
  | Python client | One | High and medium findings fixed: timeouts and protocol errors now exit 2, the session is reopened after a host restart, `--find` values are strings, an explicit task file beats `HYPERHAND_TASK_ID`, stdin is UTF-8, and tricky control lines parse correctly. |

## Default task ends with its session (Win10)

The calls went through Example's former `Invoke-HyperHand.ps1`, which opens and deletes one MCP session per call. Evidence: Example's ignored `temp/hyperhand/orphan-20261010/`.

- Before, on the previously installed host (`282FA788…`): the first `vm_exec` without `task_id` succeeded. Every later write was `vm_busy`, owned by the first call's unreachable default task, until the host was replaced. This reproduces the reported failure.
- After, on the new host:
  - Two consecutive `vm_exec` calls without `task_id` both succeeded, and `vm_status` showed `owner: null`.
  - An explicit `task_id` wrote, then a call without a task got `vm_busy` with `owner_task_id`, `owner_idle_ms: 2342` and `owner_in_flight: 0`.
  - `vm_status` showed `owner {task_id, idle_ms: 2673, in_flight: 0, this_task: false}`. From the owner's own task it showed `this_task: true`, with `in_flight: 1` counting that call.
  - The explicit task wrote again from a new session. After its `vm_end_turn`, a call without a task wrote and `owner` was `null` again.

## Background jobs (Win10)

The calls went through `client/hyperhand_client.py`. Evidence: `temp/hyperhand/jobs-20261010/`.

- **Run to exit.** A job wrote 10 lines with Chinese text, one every 2 s, then `warn` to stderr, and exited with 3. It was read by 11 `vm_job` calls with `wait_ms: 15000`, each from a new MCP session and a new task, passing `stdout_next`/`stderr_next` as offsets. The collected output was complete: 10 lines with intact Chinese text, and stderr `warn`. The final state was `exited`, `exit_code` 3, `complete: true`, after 20.3 s.
- **Other calls are not blocked.** While a job ran, a synchronous `vm_exec hostname` returned in 0.21 s.
- **Cancel.** A job started a separate `ping.exe` with `Start-Process` and waited for it. Cancelling from another task was refused with `vm_busy`, since cancel needs write ownership. The owner's cancel returned `cancelled`, and `Get-Process -Id <ping pid>` then reported `False`: the whole tree was terminated.
- **Timeout, listing and refusals.**
  - A job with `timeout_ms: 3000` ended as `timed_out` after 3.0 s.
  - `vm_job` without `job_id` listed all three jobs: `exited`, `cancelled` and `timed_out`.
  - An unknown ID was refused with `no_job`.
  - `background` together with `admin` was refused with `invalid_argument`.
- **Not exercised on the installed host:** jobs surviving a host restart. That needs another UAC prompt. Jobs live in the guest agent, independent of the host process, and the tests cover reading jobs from new sessions and tasks.

## Python client

- Every call above used the client from the command line or as a module: the stable task, saved images, error objects as exceptions, and JSON output.
- Read-only checks against the installed host: `vm_list`, `vm_status`, and `vm_observe` with `--find control_type=Edit` on AutoCAD, which returned the command-line and search edits with their centers and actions. A bad handle returned the `no_window` object with exit code 1.
