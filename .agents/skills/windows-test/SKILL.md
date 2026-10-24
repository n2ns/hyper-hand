---
name: windows-test
description: Runs HyperHand's automated tests on the Windows host from WSL — go vet and the race-enabled tests through scripts/test-windows.sh, focused runs, parallel runs from several worktrees without overwriting each other, and reproducing a defect on the unfixed code before fixing it. Use after changing Go code or tests, before committing, when a reviewer reports a defect, and in every subagent that writes code.
argument-hint: [test name pattern]
---

# Running the automated tests

What the script does and why it runs on the host: [Building](../../../docs/building.md#test). All commands run from the repository (or worktree) root in WSL. The tests never touch the installed service or a VM; acceptance on a real VM is the `vm-acceptance` skill.

## 1. Pick a test directory

The script compiles into `%TEMP%\<name>` on the host and deletes that directory first. Several runs at once (subagents in their own worktrees, a reviewer and an implementer) need different names, or they delete each other's executables:

```sh
export HH_TEST_NAME=hyperhand-tests-<worktree or task>   # letters, digits, '.', '_' or '-'; not . or ..
```

A single run on the main checkout may keep the default `hyperhand-tests`. Delete a directory you named when your work ends, in the shell where `HH_TEST_NAME` is set; `${HH_TEST_NAME:?}` stops the command when it is not, which would otherwise delete all of `%TEMP%`:

```sh
rm -rf "$(wslpath "$(/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe -NoProfile -Command '[IO.Path]::GetTempPath()' | tr -d '\r')")${HH_TEST_NAME:?}"
```

## 2. Run

While working on one area, run the tests that cover it; extra arguments go to every test executable:

```sh
scripts/test-windows.sh -test.run 'TestCheckUsable|TestClickAutoActivation'
```

Before committing, run everything once:

```sh
scripts/test-windows.sh
python3 -m unittest discover -s client     # when client/ changed
```

Exit code 0 means vet and every package passed. A `FAIL` line names the package and its log `%TEMP%\<name>\<package>.log`; read the `--- FAIL` and following lines there. `gofmt -l cmd internal` must print nothing.

## 3. Reproduce a defect before fixing it

A reported defect is fixed only after a test fails on the unfixed code ([AGENTS.md](../../../AGENTS.md)):

1. Write the test that shows the failure and run it: it must fail for the reported reason (check the message in the log, not only the exit code).
2. Make the fix, run the test again: it passes.
3. When the test was written after the fix, prove it still catches the defect: `git stash push <fixed files>`, run the test (fails), `git stash pop`, run it again (passes). Stash only the fix, never the new test.

## 4. Known non-product failures

- Three agent tests that need the host desktop (`TestWindowAt`, `TestFocusedHelper`, `TestControlHintNativeIdentityAndFallback`) skip while the host session is locked; `-test.v` lists skips.
- Windows Go cannot build a `\\wsl.localhost` checkout; always go through the script, never `go test` from Windows directly.
