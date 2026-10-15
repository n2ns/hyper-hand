# UI wait and directory mirror acceptance — 2026-10-10

The installed host and Win10 guest passed the combined UI wait and directory mirror acceptance run. The final complete run was `7c5e5daf1d3a`; all ten stages passed and the runner exited with code 0. This is an installed-development-build acceptance, not a tagged release or an AutoCAD drawing-workflow acceptance.

## Installed binaries

Both endpoints reported `ui-wait-mirror-20261010`, protocol 2. SHA-256 was checked against the actual installed files, including the running guest process's executable path before the test checkpoint and after restoring it.

| Component | Installed path | SHA-256 |
| --- | --- | --- |
| Host | `C:\Program Files\HyperHand\hyperhand.exe` | `C55AC852A3784691F40DAEAF228E111121CBDE91BB5F89F89B2E2E3082F58D35` |
| Guest agent | `C:\Users\ExampleUser\AppData\Local\HyperHand\hyperhand-agent.exe` | `941C70709F4B8D2B43370C492BB927ABADE530990DF06AF0F39FF37A9955B542` |

The staged agent beside the installed host had the same agent hash. `vm_update_agent` completed successfully. `C:\Users\Public\HyperHand` is a staging path; the running agent installs under the guest user's `LOCALAPPDATA`.

The installed host tray path and both installed hashes were rechecked after acceptance. The service remained running with its registered installed executable, and the pre-existing disabled Start with Windows preference was preserved.

Maintained packages passed `go test -race ./cmd/... ./internal/... -count=1 -timeout=240s` and `go vet ./cmd/... ./internal/...`; the independent MCP wait tests also passed three race-enabled repetitions. The unrelated historical Go experiments under ignored `build/` still prevent a clean full `./...` run; since resolved by `build/go.mod`, see [building](building.md).

## UI conditions and assertions

An isolated WPF fixture published its actual state to a file. The runner checked this state independently of the `vm_wait` result. For enabled, value, toggle and selection changes, it first confirmed the condition was false, started a pending wait, then triggered the fixture mutation through another MCP session in the same task.

| Case | Installed result |
| --- | --- |
| Enabled state, Chinese text, empty string, toggle on, selected item | Passed; pending waits observed the requested transitions and matched the independent fixture state |
| Read-only state; exact AutomationId and name together | Passed |
| Observation ID and control index | Passed for an empty value and for disappearance of the observed control |
| Control appears, then disappears | Passed; absence was confirmed from an untruncated tree |
| Window appears after startup delay | Initially absent; the expected WPF title appeared after 4,377 ms, rather than matching a transient PowerShell console |
| Window closes | Passed; delayed fixture closed after 7,902 ms; separately triggered popup closure also passed |
| Window becomes foreground | Pending wait satisfied after targeted activation; independently checked against `vm_windows` |
| Unsatisfied normal timeout | Still-open window returned `satisfied: false` after 700 ms |
| `check_only` with `assert` | Unsatisfied control comparison returned `assertion_failed` with the last actual state |
| Truncated tree | `max_nodes: 1` returned unknown and did not prove a missing control was gone |
| Unavailable value/state; password subtree | Did not satisfy comparisons; the synthetic password was absent from returned data |
| Invalid expected fields, missing comparison, excessive timeout | Returned `invalid_argument` |
| Real action while waiting | Same-task `vm_set_value` completed in 0.299 seconds while the wait was pending; the wait then satisfied and the fixture independently confirmed the value |
| Scoped cancellation | A separate task's `vm_end_turn` reported one cancelled wait; that wait returned `failed` with an explicit `vm_end_turn` cancellation reason |

The password fixture used a separate window. Password subtrees intentionally mark a control tree as truncated, so a property selector in that tree cannot establish uniqueness. This conservative result also affected the initial mixed-fixture run; it was not treated as a successful positive comparison. A deadline expiring during a UIA/provider request remains a provider error with `last.unknown`, rather than evidence that a condition was evaluated as false. The normal-timeout case above therefore uses a window condition.

## Directory mirror

Every mirror destination was inside a unique test directory. Guest contents were independently enumerated and file bytes checked with SHA-256.

| Case | Installed result |
| --- | --- |
| Preview | Returned the plan without changing the destination |
| Added and overwritten files; unchanged file | Exact resulting hashes matched; unchanged `same.txt` was explicitly listed as `skip` in both plan summary/changes and apply completion |
| Empty directories; extra files and directories | Required directories appeared and extra entries were removed |
| Empty source | Removed destination contents while retaining the destination root |
| Missing destination root | Plan did not create it; apply created the expected contents |
| Ordinary copy | Preserved an extra destination file |
| Source drift and guest drift | Apply returned `plan_stale`; independent guest inventories remained unchanged |
| Plan replay and foreign task's plan | Rejected with `plan_stale` |
| Read-only plan ownership | Another task could plan while the existing owner retained the ability to write |
| Locked DLL replacement | Returned `mirror_partial`; the earlier new file was present, the locked original DLL retained its hash, and the extra DLL was preserved because deletion had not begun |

An additional race-enabled `internal/mirror` test executable ran elevated in Win10 with a 90-second test deadline. It exited 0 without timing out: 45 passing test/subtest records, **zero skipped tests**. This included symbolic links and ancestor links, junctions and ancestor junctions, root boundaries, payload corruption/cancellation, concurrent target changes, and cancellation after copying while preserving extras. No guest security setting was changed for these checks.

## Cleanup and evidence

The runner restored its own temporary checkpoint, verified the guest executable hash again, and ended its task with empty `errors` and `skipped` lists. The temporary checkpoint appeared in `deleted_checkpoints`; a fresh checkpoint listing contained only the original manual checkpoint `76221ce2-ee75-48bc-b135-829277608b43`. A new task successfully executed a read-only shell command through `vm_exec`, proving that write ownership had been released. The native test's separate directory was also removed and its task ended.

The final guest was running in an unlocked console session, with the expected agent version and hash. All nine `vm_doctor` checks were `ok`. The final window list contained the shell taskbar and Program Manager only, with the same Explorer PID and window handles as the earliest recorded window inventory (`evidence/f91ef01a2bd2-007-vm_windows.json`, taken after launching the first fixture and before any restore). That initial list likewise contained no AutoCAD, Notepad, or Edge window. This run does not claim acceptance of those applications' workflows.

Local evidence is under the ignored `build/ui-wait-20261010/` directory:

- `acceptance.py`, `fixture.ps1`, and `hh.py`: combined runner, fixture and MCP transport.
- `7c5e5daf1d3a/results.json`: complete successful run; per-call JSON and screenshots use the same prefix under `evidence/`.
- `guest_tests.py`, `guest-mirror-tests.json`, and `guest-mirror-tests.stdout.txt`: elevated native race test, exit status, full output and cleanup.
- `final-check.json`: final status, windows, doctor, checkpoints, running-agent hash and task cleanup.
- `final-independent-summary.json` and `host-final.json`: the parent reviewer's independent runtime, restored-window identity, checkpoint, diagnostic and installed-host verification.

Mid-transfer socket disconnect/cancellation through the installed mirror transport, live ten-minute plan expiry, host restart plan loss, and an old-agent mirror refusal were not exercised in this VM run. Native cancellation tests cover the mirror engine, not those end-to-end transport cases. Multi-monitor/DPI, RDP/enhanced sessions, elevated UI targets and real AutoCAD command/dialog workflows are outside this fixture acceptance.
