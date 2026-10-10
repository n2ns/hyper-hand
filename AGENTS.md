# HyperHand Project Guide

## Project and callers

- HyperHand is an MCP server that lets AI agents see and operate Windows virtual machines on Hyper-V.
- The callers and the readers of every result are AI agents, not people. Design tool descriptions, return text and error messages for AI usability: as few calls per task as possible, machine-readable results that carry the facts the next call needs (actual handles, coordinates and so on), errors that state the cause and the next action. Make these trade-offs yourself; do not ask the user about them as if they were human UI preferences.
- What HyperHand does is in `README.md` (written for human readers), the tool list and usage in `docs/user-guide.md`, the exact behavior in `docs/features/<chapter>.md` (index `docs/features.md`: read only the chapter of the tool or area you work on), building, testing, development installs and releasing in `docs/building.md`.

## Test VMs are disposable

- Develop and accept HyperHand on the `Win10` VM. `Win10-Example` belongs to Example testing; do not use it for HyperHand work.
- The Hyper-V VMs this project is developed and tested against can be thrown away and rebuilt at any time.
- Do not worry about, warn about or ask about security inside the VMs: automatic logon, plain-text passwords, disabling UAC prompts, lowering security policy, passing passwords through `vm_exec` and the like. Whatever gets the automated tests running, do it the simplest way.
- This applies to the guest VMs only. Accounts, permissions and security settings on the host (the physical machine) are still handled with care: do not change the user's own account or its group memberships. The installer's changes to the dedicated service account are part of the product and are not affected.

## Build and verify

Run from the repository root:

```powershell
go build -ldflags "-H windowsgui" -o build\hyperhand.exe .\cmd\hyperhand
go build -ldflags "-H windowsgui" -o build\hyperhand-agent.exe .\cmd\hyperhand-agent
go vet ./cmd/... ./internal/...
go test -race ./cmd/... ./internal/...
go run ./cmd/hyperhand dev-install    # builds this checkout and installs it on the host WITHOUT UAC, then checks the result
```

- `./...` works as well: the tracked `build\go.mod` makes the otherwise ignored `build\` directory a separate module, so its old Go experiments stay out of package discovery. Do not delete it.
- The running service and tray use the installed copies under `%ProgramFiles%\HyperHand`; the files in `build\` are not the running version. Install a build only with `go run ./cmd/hyperhand dev-install` from the repository root (`--no-build` installs the files already in `build\dev-install`; exit code 0 means installed and checked): it goes through the preauthorized task `HyperHand Dev Install` and never prompts for UAC. Then update each guest agent with `vm_update_agent`.
- Never run `hyperhand.exe install` without `--quiet` during development: it prompts for UAC. If `dev-install` reports that the task is not registered, ask the user to run `scripts\dev-install-setup.ps1` once from an elevated PowerShell; do not fall back to a UAC install.
- Never kill processes by name (`taskkill /IM`, `Stop-Process -Name`); the installer owns service and tray replacement.
- Do not add PowerShell or batch to the installer (`cmd/hyperhand/setup_*.go`).
- When a change to `internal/proto` alters what the host and agent exchange, increment `proto.Protocol`; the host refuses older agents with `agent_outdated` and there is no compatibility path.
- A behavior change updates the chapter in `docs/features/` that holds the exact behavior, `docs/user-guide.md` where usage changes, and `[Unreleased]` in `CHANGELOG.md` (Keep a Changelog format), in the same commit. Releasing: see the Releasing section of `docs/building.md`.
