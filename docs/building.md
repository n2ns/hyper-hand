# Building HyperHand

This guide is for building HyperHand from source, testing it, installing development builds and publishing releases. To install a released version, see the [user guide](user-guide.md).

## Requirements

- Windows amd64 with Hyper-V, to run and test what you build.
- Go 1.27 or later.

## Build

From the repository root:

```
go build -ldflags "-H windowsgui" -o build\hyperhand.exe .\cmd\hyperhand
go build -ldflags "-H windowsgui" -o build\hyperhand-agent.exe .\cmd\hyperhand-agent
```

`-H windowsgui` builds both as GUI programs, so no console window appears. A source build reports its version as `dev`.

Keep `hyperhand-agent.exe` in the same directory as `hyperhand.exe` when installing or updating. Guest installation and update use the installed agent executable.

## Test

```
go vet ./cmd/... ./internal/...
go test -race ./cmd/... ./internal/...
python -m unittest discover -s client
```

`go test -race ./...` also works: `build\go.mod` makes the otherwise ignored `build\` directory a separate module, so the old Go experiments and evidence kept there stay out of package discovery. Keep that file.

## Install a development build

`build\hyperhand.exe install` installs a build like a release, with one UAC prompt. To install builds repeatedly without UAC:

1. Once per machine and developer, register the development install task from an elevated PowerShell (the only UAC prompt):

   ```
   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev-install-setup.ps1
   ```

   It registers `HyperHand Dev Install`: an on-demand task without triggers that runs `build\dev-install\hyperhand.exe install --quiet` of this checkout with your elevated token. Anyone who can replace that file and start the task runs code as an administrator; remove the task with `-Mode Remove` when you no longer develop HyperHand on this machine. An existing task of that name that runs something else is never replaced.

2. Build and install without UAC, from the repository root:

   ```
   go run ./cmd/hyperhand dev-install
   ```

   It builds both executables into `build\dev-install` (version `dev-<yyyyMMdd-HHmmss>-<commit>`, with `-dirty` when `cmd`, `internal`, `go.mod` or `go.sum` have uncommitted changes), starts the task, waits for it (`--timeout`, default 180 seconds), and checks that the installed files equal the build, the service runs and one installed tray process is up. It prints progress and a final result line, writes errors to stderr and exits with a nonzero code on failure. `--no-build` installs the files already there. On failure it prints the installer's last log lines; if the service is left stopped, running it again with `--no-build` restores it. Installing interrupts active MCP connections. It does not terminate processes by executable name.

3. Ask the AI to call `vm_update_agent`. The host sends the `hyperhand-agent.exe` next to `hyperhand.exe` to the running agent, which replaces itself, restarts, and is pinged until it answers (up to 30 seconds). Repeat for each VM.

`vm_update_agent` needs a running agent. If the agent does not answer, use `vm_install_agent` instead.

## Releasing

For maintainers. Add a section for the version to `CHANGELOG.md`, commit, and push a tag:

```
git tag v0.1.0
git push origin v0.1.0
```

Pushing a `vX.Y.Z` tag runs a GitHub Actions workflow that runs the tests, builds `hyperhand.exe` and `hyperhand-agent.exe` for Windows amd64 with the version stamped in, and publishes a GitHub Release with `hyperhand-X.Y.Z-windows-amd64.zip` (both exes, `README.md` and `CHANGELOG.md`). The release notes are the version's section of `CHANGELOG.md`.
