# Control search and subtree acceptance - 2026-10-10

The installed host and Win10 guest passed the control-search acceptance run `0d581125a491`. All eight stages passed, including a real AutoCAD 2015 Options dialog and restoration of the initial VM state. This validates an installed development build, not a tagged release or every AutoCAD workflow.

This record covers the original `control-search-20261010` build. The later rectangle-hint build has not completed installed performance and AutoCAD regression acceptance; see the separate [performance record](control-search-performance-20261010.md).

## Installed binaries

Both endpoints reported `control-search-20261010`, protocol 2. The guest was updated with `vm_update_agent`. Its actual running process path and SHA-256 were checked before the test checkpoint and again after restoring it.

| Component | Installed path | SHA-256 |
| --- | --- | --- |
| Host | `C:\Program Files\HyperHand\hyperhand.exe` | `BA941A039936DEC2A9EE1D2DBA90DD9D0BA4BA26503E10B374249C94A0FAEF8D` |
| Guest agent | `C:\Users\ExampleUser\AppData\Local\HyperHand\hyperhand-agent.exe` | `1FF2B07771E8EC4164C9F2CA9B3DE3AA506DD9BC4929612B8B03A64AAAC66567` |

Host installation verification also checked that the service was running, verified the tray path and staged agent hash, and confirmed preservation of the disabled Start with Windows preference. The maintained packages passed `go test -race ./cmd/... ./internal/... -count=1 -timeout=240s` and `go vet ./cmd/... ./internal/...`. The existing ignored Go experiments under `build/` remain outside that maintained-package check.

## Large-tree fixture

An isolated WPF window contained 1,100 filler buttons before the target group. The complete UI Automation control view contained 2,234 nodes because the provider also exposed child text and other controls. A separate JSON state file confirmed value changes and the button's business effect.

| Case | Installed result |
| --- | --- |
| Existing whole-window observation | `max_nodes: 1000` truncated the tree before `LateTarget` |
| Search beyond the old observation limit | Exact AutomationId search found `LateTarget`, returned `unique`, and reported 2,234 visited nodes |
| Reuse search identity | `vm_wait` matched the initial value; `vm_set_value` verified the new value, also confirmed in the independent state file |
| Scan budget | `max_visited: 50` returned `incomplete`, `truncated: true`, and `max_visited`; it did not claim absence |
| Duplicate controls | Name plus `control_type: Button` returned both matching buttons with status `multiple` |
| Match budget | The same duplicate query with `max_matches: 1` returned `incomplete`, not `unique` |
| Complete absence | An unmatched AutomationId returned `not_found` only after completing the search |
| Subtree observation | The group returned 12 nodes, with the selected root at index 0 and no filler or outside-group controls |
| Scoped search and AND filters | The repeated button name became unique within the group; `Edit` returned two controls; an AutomationId combined with the wrong type returned `not_found` |
| Subtree actions and waits | A child index supported SetValue and a value assertion; Invoke increased the independently recorded click count to 1; the outside text remained unchanged |
| Dynamic lifecycle | A newly added button was found in the scoped search; after removal, its cached identity satisfied `control_gone`, subtree observation returned `stale_element`, and a fresh scoped search returned `not_found` |

The initial exploratory run `07804389b023` incorrectly expected name-only WPF queries to return only buttons. The provider also exposed identically named child TextBlocks. The harness was corrected to include `control_type: Button`, its baseline was restored, and the full final run passed. No product change was needed for that harness correction.

Large-tree searches and late-control identity lookups took several seconds, approximately 4-7 seconds in the observed calls. A subtree reduces returned content and subsequent scoped enumeration, but locating its runtime ID still scans the owning window's control view. These results do not demonstrate constant-time identity lookup or guarantee latency for larger/slower providers.

## Actual AutoCAD dialog

AutoCAD 2015 English was discovered through `vm_apps` and launched into its own unsaved `Drawing1.dwg`. The real `Options` dialog belonged to `acad.exe`, PID 9340, HWND 3015858, with the main window as its owner.

The tested chain was:

1. Search the Options window for type `Tab`: unique result, AutomationId `1796`, with 64 visited nodes.
2. Observe that result as a subtree: the tab root plus 11 tab items, totaling 12 nodes.
3. Assert the subtree's `Display` item had `selected: false`.
4. Invoke `Select` on that child index: `verified: true`.
5. Assert the same runtime identity had `selected: true`; the captured Options screenshot also showed the Display page.
6. Find Cancel using AutomationId `2`, exact name `Cancel`, and type `Button`; invoke it and assert `window_gone`.

The two selected-state checks completed in 142 ms and 229 ms; the closed-window assertion completed in 23 ms. No option values were saved.

At startup, AutoCAD's existing trial reminder temporarily blocked its main window. An observation during this state returned `stale_risk: target not responding: control tree not read`, with a screenshot. The reminder showed 24 trial days remaining and disappeared before a proposed Try click; that stale coordinate request was correctly refused with `stale_observation`. The subsequent Options chain succeeded. This startup-provider limitation is distinct from the successful dialog acceptance; no activation, license purchase, or account change was performed.

## Cleanup and evidence

The final run restored its own Standard checkpoint and `vm_end_turn` deleted that temporary checkpoint without errors or skipped items. The original manual checkpoint `76221ce2-ee75-48bc-b135-829277608b43` remained. Final window identities exactly matched the initial shell-only desktop: HWNDs 65686 and 65806, both belonging to explorer PID 5488. The new guest agent remained installed with the expected path/hash. A new task successfully executed a probe and released ownership. `vm_doctor` reported all nine checks as `ok`.

Local evidence is retained under ignored `build/control-search-20261010/`:

- `0d581125a491/results.json`: all eight final stages and their assertions.
- `run.json`: initial and restored identities, checkpoint, and guest hashes.
- `evidence/0d581125a491-*.json`: raw MCP results for the final run.
- `evidence/0d581125a491-054-vm_observe.png` and `-058-vm_observe.png`: actual Options dialog before and after selecting Display.
- `host-before.json`, `host-final.json`, and `verification.txt`: host installation and maintained-package checks.
- `fixture.ps1`, `acceptance.py`, and `autocad-acceptance.py`: the local fixture and runner.

Not exercised here: every AutoCAD dialog, third-party CAD plug-in providers, maximum 20,000-node searches, provider hangs during search, or performance under simultaneous independent UI workloads. Unit/MCP tests cover additional contract and stale-identity cases; they do not replace those real-provider scenarios.
