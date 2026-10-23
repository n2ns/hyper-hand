# HyperHand Project Guide

## Project and callers

- HyperHand is an MCP server that lets AI agents see and operate Windows virtual machines on Hyper-V.
- The callers and the readers of every result are AI agents, not people. Design tool descriptions, return text and error messages for AI usability: as few calls per task as possible, machine-readable results that carry the facts the next call needs (actual handles, coordinates and so on), errors that state the cause and the next action. Make these trade-offs yourself; do not ask the user about them as if they were human UI preferences.

## Read the documents relevant to the task

Read the relevant sections before changing behavior; a task does not require reading every document. Before adding, moving or editing a document, read the [document map](docs/doc-map.md): which document owns which fact, and the documentation rules (TODO.md holds only open work; acceptance results are not kept as records).

| Task | Read |
|---|---|
| A tool's arguments, results, errors or limits | Its chapter in [Features](docs/features.md) (read only that chapter; section numbers are global). |
| Results, error codes, VM selection or task ownership | [Results, errors and VM selection](docs/features/results-errors.md), [Architecture](docs/features/architecture.md). |
| Host and agent exchange | [Wire protocol](docs/features/wire-protocol.md) and `internal/proto`. |
| Host installer, service, tray or VMConnect viewer | [Host tray](docs/features/host-tray.md); [Hyper-V research](docs/research/hyper-v.md#vmconnect). |
| Behavior that depends on Hyper-V, Windows, UI Automation or AutoCAD | The facts in [research](docs/research/); re-verify the affected ones after upgrades. |
| Building, tests, development installs or releasing | [Building](docs/building.md). |
| Installing a build on the host and updating guest agents | The `dev-install` skill ([SKILL.md](.agents/skills/dev-install/SKILL.md)). |
| Accepting a change on a real VM | [Manual verification](docs/manual-verification.md) and the `vm-acceptance` skill ([SKILL.md](.agents/skills/vm-acceptance/SKILL.md)); [TODO](TODO.md) for pending checks. |
| Releasing a version | The `release` skill ([SKILL.md](.agents/skills/release/SKILL.md)); the tag push needs explicit authorization. |
| Installation, setup or troubleshooting as users see it | [User guide](docs/user-guide.md). |

## Test VMs are disposable

- Use the designated general-test VM for HyperHand development and the designated AutoCAD VM only for AutoCAD checks. Resolve their actual names from private/local environment notes (`build/local-test-environment.md` on this checkout) and confirm with `vm_list`; do not select another project's VM by inference.
- The Hyper-V VMs this project is developed and tested against can be thrown away and rebuilt at any time.
- Do not worry about, warn about or ask about security inside the VMs: automatic logon, plain-text passwords, disabling UAC prompts, lowering security policy, passing passwords through `vm_exec` and the like. Whatever gets the automated tests running, do it the simplest way.
- This applies to the guest VMs only. Accounts, permissions and security settings on the host (the physical machine) are still handled with care: do not change the user's own account or its group memberships. The installer's changes to the dedicated service account are part of the product and are not affected.

## Build and verify

Development happens in WSL; the product runs on the Windows host. Run from the repository root in WSL:

```sh
GOOS=windows go build -ldflags "-H windowsgui" -o build/hyperhand.exe ./cmd/hyperhand
GOOS=windows go build -ldflags "-H windowsgui" -o build/hyperhand-agent.exe ./cmd/hyperhand-agent
scripts/test-windows.sh            # go vet plus go test -race of ./cmd/... and ./internal/..., run on the Windows host
python3 -m unittest discover -s client
scripts/dev-install.sh             # builds and installs on the host WITHOUT UAC, then checks the result
```

- Windows Go cannot build or test a `\\wsl.localhost` checkout; build with `GOOS=windows` and test with `scripts/test-windows.sh` ([Building](docs/building.md#test)). Windows programs need their full `/mnt/c/...` path (the Windows PATH is not appended in WSL). The MCP endpoint `http://127.0.0.1:8770/mcp` is reachable from WSL, so `client/hyperhand_client.py` works there.
- The tracked `build/go.mod` makes the otherwise ignored `build/` directory a separate module, so its old Go experiments stay out of package discovery. Do not delete it.
- `build/` holds ignored local artifacts. Delete generated test payloads (filler files such as `big.bin`) when the run ends; they can be regenerated. Keep one copy of large third-party test media (installer images): the archive or the extracted tree, not both. Release directories (`build/release-*`) are deleted after the GitHub Release is published. Keep `build/go.mod`, `build/dev-install` and evidence referenced from `docs/`.
- The running service and tray use the installed copies under `%ProgramFiles%\HyperHand`; the files in `build/` are not the running version. Install a build only with `scripts/dev-install.sh` (exit code 0 means installed and checked): it copies the build to `%LOCALAPPDATA%\HyperHandDev` and runs the preauthorized task `HyperHand Dev Install`, never prompting for UAC. Then update each guest agent with `vm_update_agent`.
- Never run `hyperhand.exe install` without `--quiet` during development: it prompts for UAC. If `dev-install` reports that the task is not registered, ask the user to run `scripts\dev-install-setup.ps1` once from an elevated PowerShell (if it reports an existing task that runs something else, the user first removes it with `schtasks /Delete /TN "HyperHand Dev Install" /F`, elevated); do not fall back to a UAC install.
- Never kill processes by name (`taskkill /IM`, `Stop-Process -Name`); the installer owns service and tray replacement.
- Do not add PowerShell or batch to the installer (`cmd/hyperhand/setup_*.go`).
- When a change to `internal/proto` alters what the host and agent exchange, increment `proto.Protocol`; the host refuses older agents with `agent_outdated` and there is no compatibility path.
- A behavior change updates the chapter in `docs/features/` that holds the exact behavior, `docs/user-guide.md` where usage changes, and `[Unreleased]` in `CHANGELOG.md` (Keep a Changelog format), in the same commit ([document map](docs/doc-map.md)). Releasing: see the Releasing section of `docs/building.md`.
