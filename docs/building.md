# Building HyperHand

This guide is for building HyperHand from source, testing it, installing development builds and publishing releases. To install a released version, see the [user guide](user-guide.md).

## Requirements

- A Windows amd64 host with Hyper-V, to run and test what you build, and WSL on it for development (interop enabled).
- In WSL: Go 1.27 or later, `mingw-w64` (`x86_64-w64-mingw32-gcc`, for race-enabled Windows tests) and Python 3.

Windows Go cannot build or test a checkout inside WSL (it cannot lock files on `\\wsl.localhost`), so the executables and tests are cross-compiled in WSL and run on the host through WSL interop.

## Build

From the repository root in WSL:

```
GOOS=windows go build -ldflags "-H windowsgui" -o build/hyperhand.exe ./cmd/hyperhand
GOOS=windows go build -ldflags "-H windowsgui" -o build/hyperhand-agent.exe ./cmd/hyperhand-agent
```

`-H windowsgui` builds both as GUI programs, so no console window appears. A source build reports its version as `dev`.

Keep `hyperhand-agent.exe` in the same directory as `hyperhand.exe` when installing or updating. Guest installation and update use the installed agent executable.

## Test

```
scripts/test-windows.sh
python3 -m unittest discover -s client
```

`scripts/test-windows.sh` runs `go vet ./cmd/... ./internal/...` for Windows, cross-compiles the race-enabled test executable of every package into `%TEMP%\hyperhand-tests` on the host and runs each there (a test executable started from the WSL path gets Linux paths for itself and its working directory, and some tests fail). It prints `ok` or `FAIL` per package, keeps each package's output in a `.log` beside the executables, and exits nonzero on any failure. Extra arguments go to every test executable, for example `-test.run=TestStageBuild` or `-test.v`. `HH_TEST_NAME` names the directory under `%TEMP%` instead (letters, digits, `.`, `_`, `-`; not `.` or `..`); the script deletes it first, so runs from several worktrees at once need different names. The `windows-test` skill is the procedure.

The tracked `build/go.mod` makes the otherwise ignored `build/` directory a separate module, so the old Go experiments and evidence kept there stay out of package discovery. Keep that file. Three agent tests that hit-test or read UI Automation focus on the host desktop (`TestWindowAt`, `TestFocusedHelper`, `TestControlHintNativeIdentityAndFallback`) are skipped while the host session is locked: Windows lets no program unlock it. Run them on an unlocked desktop (`-test.v` lists skips).

## Install a development build

`build/hyperhand.exe install` installs a build like a release, with one UAC prompt. To install builds repeatedly without UAC:

1. Once per machine and developer, register the development install task from an elevated PowerShell (the only UAC prompt):

   ```
   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev-install-setup.ps1
   ```

   It registers `HyperHand Dev Install`: an on-demand task without triggers that runs `%LOCALAPPDATA%\HyperHandDev\hyperhand.exe install --quiet` with your elevated token. Anyone who can replace that file and start the task runs code as an administrator; remove the task with `-Mode Remove` when you no longer develop HyperHand on this machine. An existing task of that name that runs something else is never replaced: delete it first with `schtasks /Delete /TN "HyperHand Dev Install" /F` from the elevated PowerShell.

2. Build and install without UAC, from the repository root in WSL:

   ```
   scripts/dev-install.sh
   ```

   It builds both executables into `build/dev-install` (version `dev-<yyyyMMdd-HHmmss>-<commit>`, with `-dirty` when `cmd`, `internal`, `go.mod` or `go.sum` have uncommitted changes), then runs `build/dev-install/hyperhand.exe dev-install --no-build` on the host. That copies both executables to `%LOCALAPPDATA%\HyperHandDev`, starts the task, waits for it (`--timeout`, default 180 seconds; extra arguments of the script are passed on), and checks that the installed files equal the build, the service runs and one installed tray process is up. It prints progress and a final result line, writes errors to stderr and exits with a nonzero code on failure. On failure it prints the installer's last log lines; if the service is left stopped, running `build/dev-install/hyperhand.exe dev-install --no-build` again restores it. Installing interrupts active MCP connections. It does not terminate processes by executable name. On a Windows checkout, `go run ./cmd/hyperhand dev-install` builds with Windows Go and does the same.

3. Ask the AI to call `vm_update_agent`. The host sends the `hyperhand-agent.exe` next to `hyperhand.exe` to the running agent, which replaces itself, restarts, and is pinged until it answers (up to 30 seconds). Repeat for each VM.

`vm_update_agent` needs a running agent. If the agent does not answer, use `vm_install_agent` instead.

## Releasing

For maintainers. Add a section for the version to `CHANGELOG.md`, commit, and push a tag:

```
git tag v0.1.0
git push origin v0.1.0
```

Pushing a `vX.Y.Z` tag runs a GitHub Actions workflow that runs the tests, builds `hyperhand-agent.exe` and then `hyperhand.exe` for Windows amd64 with the version stamped in, and publishes a GitHub Release with `hyperhand-X.Y.Z-windows-amd64.zip` (both exes, `README.md` and `CHANGELOG.md`) and `hyperhand-X.Y.Z-windows-amd64.exe`. The agent is copied into `cmd/hyperhand/embedded/` before `hyperhand.exe` is built, which embeds it, so that single executable installs both (`install` prefers an agent beside it, as in the zip and development installs). The release notes are the version's section of `CHANGELOG.md`.
