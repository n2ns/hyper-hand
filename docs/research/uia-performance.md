# UI Automation performance measurements

Own measurements of control-tree observations and control searches in a guest, behind the limits and defaults of [4. Observation and Input](../features/observation-input.md). They are end-to-end MCP wall-clock times (including the runner's evidence writing) unless stated otherwise, not isolated UIA traversal timings, and not a performance gate. Large windows stay close to the agent's 10-second UIA helper deadline, so callers should keep observations bounded and searches scoped.

## AutoCAD 2015 main window (2026-10-11)

About 1,400 control-view nodes. The agent's walk of that date read about 120 nodes/s; a cached .NET `FindAll` read 1,382 nodes in 4.9 s. `vm_observe` with `max_depth: 10, max_nodes: 1000` and `vm_find_controls` with the default and with `max_visited: 1500` ran into the 10-second deadline.

## Per-element property caches (2026-10-16)

Same-process A/B benchmark inside the Windows 10 AutoCAD test guest, three alternating rounds, 5,000-node / 64-depth observation limit; both modes returned identical results (1,342 nodes, same focus, selected text and search results). Times in ms:

| Operation | Without caches | With caches | Median |
| --- | --- | --- | --- |
| Full observation of the AutoCAD main window | 14497.363, 14252.897, 14304.376 | 9296.293, 9600.045, 9899.549 | 14304.376 → 9600.045 (32.9% less) |
| Exact AutomationId search (`ACAD.ID_TabInsert`) | 7166.383, 5564.100, 5713.498 | 6046.454, 5998.039, 7890.190 | 5713.498 → 6046.454 (5.8% more) |

On a 2,234-node WPF fixture the observation median went from 12775.714 to 8667.562 ms and the exact search median from 4680.755 to 4624.770 ms. The caches speed up observations; they do not speed up searches. Exact-ID searches for the ribbon tabs through MCP took a median of 6.5 s (`Home`) and 7.4 s (`Insert`).

## Per-parent child limit (2026-10-24)

AutoCAD 2015 main window with a new drawing, `vm_observe` with `max_depth: 10, max_nodes: 1000`, two samples through MCP after the per-parent child limit (4.3): 612 nodes in 7792 and 9439 ms, close to the 10-second deadline, `controls_truncated: true`. The ribbon's root pane had 103 direct children: the 15 visible ones (the active tab's panel bar, the tab buttons, the tab control) were read and the 88 offscreen panel bars of the other tabs were left out (`omitted_children=88`). The same call ran into the 10-second deadline on 2026-10-11, before the property caches; there is no measurement with the caches but without the limit, so the share of each change is unknown. Same guest as above.

## Large WPF fixture (2026-10-10)

A topmost WPF window with 1,100 filler buttons before a target group (2,234 control-view nodes). Three samples each through MCP, before the rectangle hint:

| Operation | Samples (ms) | Median (ms) |
| --- | --- | --- |
| Full exact AutomationId search | 6947.619, 5053.863, 4727.648 | 5053.863 |
| Late-control identity value assertion | 4751.096, 4750.802, 4657.140 | 4750.802 |
| Late-control identity SetValue | 3876.720, 3944.368, 4145.936 | 3944.368 |
| Target-group subtree observation | 4888.913, 4439.507, 4429.637 | 4439.507 |

Locating a subtree root by runtime ID still scans the owning window's control view. The rectangle hint for previously observed controls ([4.3](../features/observation-input.md)) has no comparable timings.

A four-property UIA cache tried on 2026-10-10 was slower in the VM and removed: median search, wait, SetValue and subtree times went from 4417.621 / 4764.840 / 3958.399 / 5046.766 ms to 5173.030 / 5101.811 / 5109.613 / 5100.415 ms. A local native benchmark had shown it faster; local benchmarks do not predict VM performance.
