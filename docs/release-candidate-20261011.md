# Release candidate package: verification (2026-10-11)

Phase F of the TODO execution plan: the `[Unreleased]` changes packaged and installed locally the way a release is. No tag was pushed and nothing was published.

## Package

- Built from the working tree of the phase F commit (source of `7404586` plus the migration notes) with the build step of `.github/workflows/release.yml`: `go build -trimpath -ldflags "-s -w -H windowsgui -X hyperhand/internal/proto.Version=0.3.0"` for both executables, `README.md` and `CHANGELOG.md` copied, `Compress-Archive` to `hyperhand-0.3.0-windows-amd64.zip`. `0.3.0` is a candidate number chosen for the build (the next minor version after 0.2.0, since `[Unreleased]` is breaking); the released version is the maintainer's decision.
- Zip SHA-256 `77185ab3c86d…2dcdb0c`, 6,344,052 bytes. Contents: `hyperhand.exe` `3f95de4a799a…8edf7986`, `hyperhand-agent.exe` `56a6449d32cc…aa93bf76`, `README.md`, `CHANGELOG.md`. Unpacked separately, every file has the same hash as the file packed.
- Evidence: ignored `build/release-0.3.0-candidate/` (`dist/`, the zip, `unpacked/`, `verify.py`, `verify.json`, `test.txt`).

## Installed

- The packaged executables were installed with `go run ./cmd/hyperhand dev-install --no-build` from `build\dev-install` (no UAC; the installer is the same `install --quiet`). The installed `%ProgramFiles%\HyperHand\hyperhand.exe` and `hyperhand-agent.exe` have the package's hashes; the service runs and one tray process is up.
- The running MCP server reports `serverInfo` `{"name": "hyperhand", "version": "0.3.0"}`. `vm_update_agent` on `Win10` returned agent version `0.3.0` with the current protocol, and the running `%LOCALAPPDATA%\HyperHand\hyperhand-agent.exe` has the package's agent hash. `vm_doctor` reported every host and guest check `ok`.

## Tests

The workflow's `go test -race ./...` passes for every package except three desktop hit-test tests of `internal/agent` (`TestWindowAt`, `TestControlHintNativeIdentityAndFallback`, and `TestFocusedHelper` in an earlier run), which failed because this host's desktop was locked (their hits landed on `LockApp.exe`); they need an unlocked interactive desktop, as a CI runner has.

## Left for publishing (maintainer)

- Turn `## [Unreleased]` into the version's section (`## [x.y.z] - <date>`), commit, and push the `vX.Y.Z` tag (see [Releasing](building.md#releasing)). The workflow takes the release notes from that section; without it they are only `HyperHand x.y.z`.
- `Win10-Example` was not touched in this run; it needs `vm_update_agent` after the release is installed.
