# Control search performance verification - 2026-10-10

Status: the rectangle-hint implementation is installed on the host and Win10 guest. Local checks passed, but installed performance acceptance is incomplete. The first candidate correctness run encountered UI Automation provider deadlines. There are no valid three-sample timings for this final candidate and no measured speedup is claimed. Further experiments were stopped at the user's request to commit and push.

## Installed candidate

| Component | Version | SHA-256 |
| --- | --- | --- |
| Host | `control-search-hint-20261010` | `F4EBEC2306CB6DBD7344AAA583F40851AEEA997448F685D93B590EBF3538CC20` |
| Guest | `control-search-hint-20261010` | `96EF904D6391EED10AD2A525754E0D86E937F81695EEF3A0308B0B1D2A32826D` |

The running guest executable was verified at `C:\Users\ExampleUser\AppData\Local\HyperHand\hyperhand-agent.exe`. Host installation checks verified the installed binary hashes, running service, installed tray process, and preserved startup preference. Maintained-package race tests and vet passed before installation.

The host supplies an internal observed-rectangle hint for an already observed control. The guest validates the point-selected element's exact runtime identity and scope before using it; a failed hint falls back to the original traversal. Initial full searches still traverse the control view. The public tool contract is unchanged. Performance for visible, moved, covered, and offscreen controls must therefore be distinguished.

## Comparable baseline

The final baseline used the original `control-search-20261010` host and guest, with host SHA-256 `BA941A039936DEC2A9EE1D2DBA90DD9D0BA4BA26503E10B374249C94A0FAEF8D` and guest SHA-256 `1FF2B07771E8EC4164C9F2CA9B3DE3AA506DD9BC4929612B8B03A64AAAC66567`.

The WPF fixture contained 1,100 filler buttons, 2,234 UI Automation nodes, a late target Edit, and a 12-node target group. Its window was topmost, and each sample was preceded by explicit activation and a foreground check outside the timer. The candidate runner uses the same fixture and commands. Different checkpoint runs necessarily use different process IDs and runtime identities.

| Operation | Three baseline samples (ms) | Median (ms) |
| --- | --- | --- |
| Full exact AutomationId search | 6947.619, 5053.863, 4727.648 | 5053.863 |
| Late-control identity value assertion | 4751.096, 4750.802, 4657.140 | 4750.802 |
| Late-control identity SetValue | 3876.720, 3944.368, 4145.936 | 3944.368 |
| Target-group subtree observation | 4888.913, 4439.507, 4429.637 | 4439.507 |

These are end-to-end MCP wall-clock measurements, including response evidence written by the local runner, not isolated UIA traversal timings. Baseline token: `aeca8ee01790`; raw summary: `build/control-search-perf-20261010/baseline-topmost-benchmark.json`. Its checkpoint restoration and task release passed.

## Candidate outcome and remaining validation

In final-candidate run `a5ab8083bae5`, the initial ordinary whole-window observation with a 1,000-node limit reported that the target was not responding. A bounded 50-node search and a complete duplicate search subsequently succeeded, but later full-search and subtree cases reached the UI Automation deadline. The dynamic lifecycle case passed, including add, remove, a cached-identity gone assertion, stale subtree rejection, and a fresh not-found result.

This run did not pass overall. The ordinary observation and initial full searches do not use the new hint path, so the current evidence does not establish that the hint caused the deadlines. No timeout was increased and no failed sample was relabeled as successful. The run restored its checkpoint, verified the running guest hash, preserved the original stable windows, deleted its temporary checkpoint, and released its tasks.

A fresh diagnostic run, `c3f696c7cd22`, reached setup successfully but was stopped before diagnostic calls or timings when the user requested delivery. Cleanup passed: the checkpoint was restored and deleted, both task records were ended, the installed guest hash matched, original stable window identities were preserved, and an independent command probe confirmed ownership was released.

The following installed checks remain incomplete for the final hint build:

- Three comparable candidate samples for all four operations, followed by a measured baseline comparison.
- A fully passing large-tree, limit, duplicate, subtree, and dynamic correctness run.
- Moved-control and overlapping-control regression, including a covering control with the same AutomationId but a different runtime identity.
- A real AutoCAD Options search, subtree, selection, assertion, and Cancel regression. The earlier control-search build passed this workflow; that result does not validate the final hint build.

## Rejected approach and preserved evidence

An earlier four-property UIA cache candidate (`control-search-perf-20261010`, guest SHA-256 `F2851BEFC19310B8B535B3F91E0612B31C954A50E9C7A2A22A33427899E218C2`) was rejected after VM timings regressed. Against its contemporaneous ordinary-window baseline, median search, wait, SetValue, and subtree times changed from 4417.621 / 4764.840 / 3958.399 / 5046.766 ms to 5173.030 / 5101.811 / 5109.613 / 5100.415 ms. That cache optimization was removed. A local single-pair native Win32 benchmark had looked faster, but it did not predict VM performance and is not evidence for the final implementation.

Earlier ownership probes were blocked by an external task. The user later released it; the acceptance runner never interrupted or released that task. Actual VM baselines contained an existing AutoCAD process and drawing, so restoration was checked against captured window identities rather than an empty desktop. One earlier restoration's strict window comparison failed when AutoCAD produced an additional tooltip. Read-only inspection confirmed a direct UIA ToolTip child; all original stable windows remained. The initial failure and reconciliation are both retained. Later comparisons exclude only verified transient tooltip windows and preserve the original CAD main and owned-window identities.

Ignored evidence is under `build/control-search-perf-20261010/`:

- `baseline-topmost-benchmark.json`, `aeca8ee01790/results.json`: comparable baseline and cleanup.
- `candidate1-benchmark.json`, `candidate1-performance-conclusion.json`: rejected cache candidate.
- `a5ab8083bae5/results.json`: final hint candidate correctness failures, successful dynamic case, and cleanup.
- `c3f696c7cd22/results.json`: stopped diagnostic setup and cleanup.
- `evidence/`: raw MCP responses, runtime identities, screenshots, ownership checks, and checkpoint/task cleanup.
- `acceptance.py`, `fixture.ps1`, `benchmark.py`, `hint-regression.py`, `autocad-acceptance.py`: reusable runners for the remaining checks.

This report records an installed implementation with an open runtime acceptance gap, not a completed performance acceptance.
