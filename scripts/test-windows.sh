#!/bin/sh
# Runs go vet and the race-enabled tests of ./cmd/... and ./internal/... on the Windows host from WSL. Windows Go cannot
# work on a \\wsl.localhost checkout, so the tests are cross-compiled here (mingw-w64 for -race) into
# %TEMP%\hyperhand-tests on the host and run from there, so that os.Args[0] and the working directory are Windows
# paths (no test reads files from its package directory). Extra arguments (such as -test.run=TestStageBuild) go to
# every test executable. Exit code 0 means vet and every package passed.
set -e
cd "$(dirname "$0")/.."
ps=/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe
export GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc
go vet ./cmd/... ./internal/...
wintemp=$("$ps" -NoProfile -Command '[IO.Path]::GetTempPath()' | tr -d '\r')
dir="$(wslpath "$wintemp")hyperhand-tests"
rm -rf "$dir"
mkdir -p "$dir"
# The test executable, its directory and the extra arguments (one per line) reach PowerShell as environment variables,
# so that characters such as | ( ) ; $ in an argument stay data instead of PowerShell syntax.
run='Set-Location -LiteralPath $env:HH_TEST_DIR; $a = @($env:HH_TEST_ARGS -split "`n" | Where-Object { $_ -ne "" }); & $env:HH_TEST_EXE @a; exit $LASTEXITCODE'
export HH_TEST_DIR HH_TEST_EXE HH_TEST_ARGS WSLENV="${WSLENV:+$WSLENV:}HH_TEST_DIR:HH_TEST_EXE:HH_TEST_ARGS"
HH_TEST_DIR=$(wslpath -w "$dir")
HH_TEST_ARGS=$(printf '%s\n' "$@")
failed=0
for pkg in $(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./cmd/... ./internal/...); do
	name=$(echo "$pkg" | tr / _)
	if ! go test -race -c -o "$dir/$name.test.exe" "$pkg" >"$dir/$name.log" 2>&1; then
		echo "FAIL $pkg (build; log: $dir/$name.log)"
		failed=1
		continue
	fi
	HH_TEST_EXE=$(wslpath -w "$dir/$name.test.exe")
	if "$ps" -NoProfile -Command "$run" >"$dir/$name.log" 2>&1; then
		echo "ok   $pkg"
	else
		echo "FAIL $pkg (log: $dir/$name.log)"
		grep -E '^(--- FAIL|panic|FAIL)' "$dir/$name.log" | head -20 || true
		failed=1
	fi
done
exit $failed
