# UI Automation and VMConnect acceptance — 2026-10-16

Source: `451f480` plus the accompanying uncommitted P1/P2 changes. AutoCAD acceptance used **Win10-PipeSifu**, VM GUID `D1715399-5EB8-4363-8DB5-0B4CA0285492`, Windows guest `PIPESIFU-VM`, AutoCAD 2015. General HyperHand fixture tests used Win10 earlier; that VM was subsequently restored to its initial checkpoint at the user's request and left off. AutoCAD tests must use Win10-PipeSifu.

## P1: bounded UIA caches and real Ribbon operations

The production walk now builds caches for each visited element instead of materializing an unbounded subtree. Depth, node, visited and match limits still apply while navigating; password descendants remain excluded. Name and Value are read live. Searches cache only the selector properties they require, with live full-property verification of matches.

The same-process A/B benchmark used the existing AutoCAD main window (PID 3708, HWND 262792), three alternating uncached/cached rounds, a 5,000-node/64-depth observation limit, and exact `ACAD.ID_TabInsert` search. The benchmark executable was built from this checkout. Job `job-bb37e0907f7b` exited 0. It found 1,342 nodes in both modes; the final complete result structures, focus, selected text and search results were equal, with zero differing nodes.

| Operation | Uncached rounds (ms) | Cached rounds (ms) | Median |
| --- | --- | --- | --- |
| Full control observation | 14497.363, 14252.897, 14304.376 | 9296.293, 9600.045, 9899.549 | 14304.376 → 9600.045 (32.9% less time) |
| Exact ID search | 7166.383, 5564.100, 5713.498 | 6046.454, 5998.039, 7890.190 | 5713.498 → 6046.454 (5.8% more time) |

The cache improves observations here; these samples do **not** demonstrate faster searches. Full observations are still close to the 10-second helper deadline. Callers should retain bounded observations and scoped searches for large or slower windows.

Installed acceptance used host and guest `dev-20261016-180311-451f480-dirty`, protocol 5. The actual AutoCAD Ribbon exposes `Home` and `Insert` as **Button / Invoke**, IDs `ACAD.ID_TabHome` and `ACAD.ID_TabInsert`; its TabItem controls belong to drawing/layout tabs. The initial TabItem-only script therefore stopped without performing an action, and the replacement script used the observed IDs.

- Exact ID-only searches and ID+Button searches returned the same runtime identities. Three ID-only samples had median elapsed times 6.4878 s (Home) and 7.3928 s (Insert), including MCP overhead.
- A 20-visited-node search returned an explicit incomplete result; a 20-node observation respected its limit.
- Home → Insert → Home used only the two identified header Invoke actions. `verified: null` was retained as the tool's generic Invoke result; it was not misreported as selected-state verification.
- Independent observations found 72 visible Ribbon commands on Home, 29 on Insert, and the identical original 72-command set after restoring Home. The original/restored and Insert screenshots were inspected. The 1,000-node observations correctly reported truncation; the verification compared the visible Ribbon command band, not a claimed complete main-window tree.
- Existing AutoCAD PID, document title and window remained present. No drawing command or save was issued.

Earlier 2,234-node stable fixture A/B observations had median 12775.714 → 8667.562 ms, with equal results; exact ID search medians were 4680.755 → 4624.770 ms. Fixture acceptance covered limits, duplicate/absent selectors, live subtree actions, dynamic stale identities, moved controls and covered coordinate hints. Native tests cover cached/uncached equality and password pruning.

Local raw evidence (ignored build artifacts): `build/p1p2-pipesifu-20261016/native-result.json`, `ribbon-buttons-20261016T110826Z/summary.json` and its images; earlier fixture evidence is under `build/p1p2-20261016/`.

## P2: VMConnect recovery

The installed console task now accepts a fixed mode and local VM GUID through the installed executable. It checks the system VMConnect image and the exact local VM title, supports localhost and the local computer name, verifies recognized disconnect UI before replacing a viewer, and forces a new viewer through enhanced options to basic mode without editing saved settings. Unknown dialogs are left untouched. VM lifecycle hooks request recovery asynchronously.

On `dev-20261016-180311-451f480-dirty`, the guest began unlocked in an enhanced session (`console:false`) with viewer PID 10364, title `VECTOR 上的 Win10-PipeSifu - 虚拟机连接`. `vm_take_console` returned `moved` and `console:true`; the old viewer was replaced by PID 19744, HWND 2885986, with one matching viewer. However, the initial full-tree readiness probe timed out. Replacement alone was not counted as successful video readiness.

Subsequent candidates replaced that probe with a shallow `StatusBar` search and a lookup confined to its direct children. Static inspection confirmed the WinForms control names and parent relationships, but live diagnosis showed that the ToolStrip items expose text with **empty AutomationIDs and runtime IDs**. `LockIcon` is the internal control name, not its exported UIA AutomationID on this machine. Treating it as an AutomationID produced a false negative even though video was connected. The generic runtime-ID lookup for the status bar can still fall back to a bounded whole-window walk if its coordinate hint is covered; the helper keeps a 3-second timeout.

- `vm_save` then `vm_start` returned `saved` then `running`, `desktop:usable`; AutoCAD PID 3708 was preserved.
- A new Standard temporary checkpoint of the current running session (`793e72be-5467-4dbd-90b2-1873631f82fe`, `holds_memory:true`) was restored immediately. The VM returned running and `vm_start` reported a usable desktop. This was not a restore of an older user baseline.
- Recovery after both lifecycle paths reused viewer PID 19744 with `viewer_count:1`; no duplicate viewer appeared. The guest stayed unlocked on the console and AutoCAD remained usable.

Initial host screen-region captures were occluded by another application and were rejected as viewer-image evidence. After bringing the verified viewer window to the foreground, `build/p1p2-pipesifu-20261016/viewer-foreground.png` visibly showed the VMConnect title and AutoCAD desktop. This independently confirms the host viewer frame, rather than substituting a guest screenshot. The new temporary checkpoint was deleted by its exact ID and merged into the current disk; existing baseline checkpoints were retained.

The earlier candidate **`dev-20261016-182040-451f480-dirty`** uses exact English/Chinese connected-video labels inside the identified status bar. All eight strings were independently checked against the installed Microsoft resource assemblies and the assignments in `VmisStatusStrip.InformConnectedState`. VM-running and input-capture text are not connection evidence; unknown labels remain unknown. Temporary diagnostic logging was removed.

With that candidate installed on the host and guest, `vm_start` returned `desktop:usable`. At 18:20:58 the host independently logged:

```json
{"pid":19744,"previous_pid":0,"state":"viewer_connected","viewer_count":1,"vm":"Win10-PipeSifu","window":2885986}
```

That candidate's installed host SHA-256 matched the staged build: `B0E648D27D85738139BF149D506327ADECEF1DDF19C6F71CE4C49485DD4C5A37`. The 18:20 probe verified only the existing recovered viewer; it did not establish successful fresh replacement on that build.

A fresh-replacement retest on the same `182040` candidate at 22:17 replaced old viewer PID 19744 with PID 38256, HWND 4721074. The old process exited and the guest returned to `console:true`, but at 22:17:23 the helper reported that the basic viewer had not become visible within 20 seconds, without a last-read error. The saved enhanced-session preference remained false and unchanged. This retest failed the complete recovery acceptance criterion.

Native inspection found the new main window visible and enabled, with a visible `OPWindowClass` video surface and no `TscShellContainerClass`. The readiness check incorrectly required only the latter class. At 22:18, a probe of the reused PID 38256 reported `viewer_connected`, independently confirming that the status-bar connection check worked; healthy reuse did not convert the failed replacement into a passing result.

The correction accepts either visible video surface class, `OPWindowClass` or `TscShellContainerClass`, while retaining the enabled main window, basic-mode selection evidence, exact connected-video status label and single-viewer requirements. Focused tests cover both classes, hidden surfaces and unrelated container classes. Candidate **`dev-20261016-222100-451f480-dirty`** was installed on both host and guest with matching versions; the full race tests and vet checks passed.

**Programmatic recovery acceptance passed on `222100`.** Before the 22:40 retest, `vm_status` reported the guest unlocked with `console:false`; the existing viewer was PID 38256, HWND 4721074. `vm_take_console` returned `moved` and `console:true`. At 22:40:39 the host logged successful fresh replacement:

```json
{"pid":26744,"previous_pid":38256,"state":"basic_viewer_visible","viewer_count":1,"vm":"Win10-PipeSifu","window":3409288}
```

A repeated `vm_start` at 22:41 returned `desktop:usable` and retained the same viewer. At 22:41:22 the host logged:

```json
{"pid":26744,"previous_pid":0,"state":"viewer_connected","viewer_count":1,"vm":"Win10-PipeSifu","window":3409288}
```

The installed host SHA-256 matched the staged `222100` build: `B92465BDB1D79CD6DD487735014F5B95571D63F07E8B3ED17536B69D044B84CF`. These results establish fresh replacement followed by healthy reuse without an additional viewer.

The user then reported that the viewer had disappeared. Independent inspection confirmed that PID 26744 still had an enabled, non-minimized, uncloaked window on the current virtual desktop and within the screen, but VS Code and VSCodium completely covered it. Neither covering window was topmost. The `basic_viewer_visible` label was too strong: `IsWindowVisible` and connected video did not establish actual presentation. The guest image `vm_observe-224129-aa5023.png` showed a normal desktop (AutoCAD was not open), but was not accepted as host-viewer image evidence.

An asynchronous `SetWindowPos(HWND_TOP)` request and ordinary foreground activation did not resolve the occlusion, even when the position call returned success. Attaching only the caller and foreground input queues also failed. Candidate `225052` temporarily attached both the caller-to-viewer and caller-to-foreground queues; activation then succeeded with foreground HWND 3409288. The queues were detached after the call, with no injected keyboard/mouse input and no permanent topmost setting.

**The host viewer was visibly restored at 22:51.** The DPI-aware capture `build/p2-redo-20261016/restored-window-225052.png` passed an exact foreground HWND/PID check and was inspected: it shows the Win10-PipeSifu VMConnect title, normal guest desktop and running status. This is actual host-viewer evidence. Earlier captures showing the IDE were rejected. Native diagnostics are retained in `viewer-visibility.json`, `viewer-occlusion.json` and `host-desktops-224855.json` under the same artifact directory.

The final candidate **`dev-20261016-225545-451f480-dirty`** bounds attached-input activation in a separate internal process with a 3-second kill-and-wait deadline. It validates the HWND, PID, system VMConnect executable and exact local VM title before activation; both processes verify foreground readback. Normal completion detaches both input queues. The connection state is now `basic_viewer_connected`, with a separate point-in-time `foreground` boolean. Focused identity/argument tests, independent review, full race tests and vet passed. A deliberately hung GUI and the timeout termination path were not exercised live.

Host and guest were updated to `225545`. The installed host SHA-256 equals the staged build: `F252662CCAF25BB0B9BFE06FEAAB2B2DE9A71486D32D1FB03C9C41154C67D9F5`. At 22:56:17, manual open on the installed candidate retained PID 26744 and reported `viewer_connected`, `viewer_count:1`, `foreground:true`.

**Final-build P2 recovery acceptance passed.** After the user completed enhanced-session login, the final build independently reported `Tok`, unlocked, `console:false`, with viewer PID 26744 / HWND 3409288. `vm_take_console` returned `moved`, unlocked and `console:true`. At 23:40:30 (host log time, UTC+07) the automatic recovery recorded:

```json
{"foreground":true,"pid":30040,"previous_pid":26744,"state":"basic_viewer_connected","viewer_count":1,"vm":"Win10-PipeSifu","window":1901414}
```

The original viewer exited. The replacement was already in the foreground when captured, and the inspected DPI-aware host image `build/p2-redo-20261016/final-fresh-viewer-225545.png` shows the exact Win10-PipeSifu VMConnect title and normal guest desktop. Its foreground HWND/PID check passed. This closes the previously missing complete final-build sequence: enhanced login, real disconnect, old-viewer replacement, connected basic video and visible foreground presentation. AutoCAD was not open during this P2 retest; the P1 AutoCAD evidence above is separate.

A subsequent `vm_start` returned `desktop:usable`; `vm_status` confirmed the final agent version and unlocked console session. The 23:41:03 log retained PID 30040, `viewer_connected` and `viewer_count:1`; `foreground:false` correctly reflected that another application had subsequently taken focus. Healthy reuse does not repeatedly steal focus. Independent native/process checks confirmed old PID 26744 was gone and only one system VMConnect process remained. The saved client configuration SHA-256 stayed `1415BA2D477B0E6E91533BFE50D70A78E9730D07F2BC5CD5F934012FD7E3C2F3`. Final evidence files are `final-complete-native.json`, `final-complete-visibility.json`, `final-complete-process-config.json` and `final-complete-host-log.txt` in `build/p2-redo-20261016/`. Win10 remained off. Unknown dialogs, other localized title/status layouts, and every possible host focus arrangement were not live-tested.

## Checks and remaining boundaries

`go test -race ./cmd/... ./internal/...`, `go vet ./cmd/... ./internal/...` and `git diff --check` passed during this work. Focused console tests were rerun after the readiness changes. Installation used only `go run ./cmd/hyperhand dev-install`, which verified the installed service and tray; guest updates used `vm_update_agent`.

Other TODO items (host uninstall/restart UI, display variants, first login and additional session transitions) are outside this change. This record does not claim that every large UIA window will complete a full observation within 10 seconds.
