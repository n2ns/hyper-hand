#!/bin/sh
# Builds both executables for Windows into build/dev-install with a dev-<yyyyMMdd-HHmmss>-<commit>[-dirty] version,
# then runs that hyperhand.exe dev-install --no-build on the Windows host (WSL interop): it copies the build to
# %LOCALAPPDATA%\HyperHandDev, runs the preauthorized "HyperHand Dev Install" task without UAC and checks the result.
# Run from WSL; extra arguments (such as --timeout 300) go to dev-install. Exit code 0 means installed and checked.
set -e
cd "$(dirname "$0")/.."
commit=$(git rev-parse --short HEAD)
version="dev-$(date +%Y%m%d-%H%M%S)-$commit"
[ -z "$(git status --porcelain -- cmd internal go.mod go.sum)" ] || version="$version-dirty"
for name in hyperhand hyperhand-agent; do
	GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -X hyperhand/internal/proto.Version=$version" \
		-o "build/dev-install/$name.exe" "./cmd/$name"
done
echo "built $version"
exec build/dev-install/hyperhand.exe dev-install --no-build "$@"
