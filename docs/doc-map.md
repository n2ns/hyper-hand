# Document map

Which document owns which kind of information, and the rules for keeping them. Read this before adding, moving or editing a document. Each fact has one owner; other documents link to it instead of repeating it.

### For users

| Document | Owns (not) |
|---|---|
| [README](../README.md) | Product intro for human readers, requirements, quick start, short feature summary, architecture overview, Known limitations including the environments HyperHand was tested in, privacy summary, uninstall summary (not exact tool behavior, build steps or test results). |
| [User guide](user-guide.md) | Step-by-step installation, client setup, guest agent installation, optional guest setup, UAC, hooks, updating, uninstalling and troubleshooting (not exact tool behavior, which it links in Features). |
| [Privacy](privacy.md) | What HyperHand sends over the network, handles and stores, and who can reach the MCP endpoint. |
| [Python client](../client/README.md) | Usage of `client/hyperhand_client.py` from the command line and as a module (not tool behavior). |
| [Hook examples](hooks/) | Client hook configurations that call `vm_end_turn`; their use is explained in the user guide. |
| [CHANGELOG](../CHANGELOG.md) | Release history in Keep a Changelog format, with migration notes (not current plans, test records or agent instructions). |

### For maintainers

| Document | Owns (not) |
|---|---|
| [Features](features.md) and its chapters in [features/](features/) | The exact implemented behavior, which is the contract for AI callers: architecture, result and error format, every tool's arguments, results, errors and limits, the guest agent, the host tray and the wire protocol (not upstream evidence, test procedures, test results or plans). The index lists the chapters; section numbers are global. |
| [Building](building.md) | Requirements, building, automated tests, development installs and releasing (not acceptance procedures). |
| [Manual verification](manual-verification.md) | How to accept a change on the installed host and a real guest: test VM baseline, preparation, checking against independent state, evidence location, cleanup, setups for specific checks, release package check (not results; pending checks go in TODO). |
| [TODO](../TODO.md) | Open work, missing acceptance coverage and product decisions not yet made (not verified behavior, test results or release history). |

### Evidence

| Document | Owns (not) |
|---|---|
| Research: [Hyper-V and VMConnect](research/hyper-v.md), [Windows and UI Automation](research/windows.md), [AutoCAD 2015](research/autocad.md), [UI Automation performance](research/uia-performance.md) | Dated upstream evidence the behavior rests on: facts about Hyper-V, VMConnect, Windows, UI Automation and AutoCAD, each with its date, environment and how it was obtained (runtime check, source or resource inspection, documentation), and own performance measurements (not HyperHand's behavior, decisions or acceptance results). |

### For agents

| Document | Owns (not) |
|---|---|
| [AGENTS.md](../AGENTS.md) / [CLAUDE.md](../CLAUDE.md) | Agent rules, safety boundaries for the host and the test VMs, build and verify commands, task routing (not a full reference); CLAUDE.md imports AGENTS.md and adds only Claude Code-specific rules. |
| [Skills](../.agents/skills/) | Step-by-step agent procedures: running the automated tests, installing a development build, running an acceptance check on a real VM (with its helper module `acceptance.py`), preparing a release (not the rules, which stay in AGENTS.md, nor acceptance criteria, which stay in Manual verification). `.claude/skills/` links to them for Claude Code. |
| [Document map](doc-map.md) | These tables and the documentation rules below. |

## Rules

- Update the document that owns a fact and link to it from others. A behavior change updates its Features chapter, the user guide where usage changes, and `[Unreleased]` in CHANGELOG, in the same commit.
- When documents and code disagree, establish the intended behavior from the task, the code and the evidence, then decide which needs updating; do not change one merely to match the other.
- Acceptance results are not kept as records. When a check passes, remove its TODO item (or the passed part of it); a conclusion users rely on, such as the environments HyperHand was tested in, becomes a sentence in README's Known limitations. Evidence files stay in the ignored `build/` directory.
- Upstream facts found while developing or testing go to `docs/research/` with their date, environment and source, and say whether they were observed at runtime or read from documentation or sources. Features states the behavior and may link the fact it rests on; re-check affected facts after Windows, Hyper-V or AutoCAD updates.
- TODO holds only unfinished work. Remove an item when it is done or decided; move what still holds to its owner (behavior to Features, facts to research).
- A documentation reorganization does not by itself verify an old claim or TODO item.
- Keep consumer-project deployment and integration records in that project's private documentation. Keep actual machine identities and VM mappings in private/local notes; public examples and test fixtures use synthetic identities. Preserve dates, methods and technical limits when anonymizing research.
- Keep AGENTS.md short: add a rule only after an agent repeated a mistake or for a new safety boundary, and check it for outdated or conflicting rules when changing it.
- All repository documents are written in English.
- A new document gets a row in the tables above; a row linking a directory covers every file in it.
