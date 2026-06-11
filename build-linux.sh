#!/usr/bin/env bash
# Native Linux build of the ClassSend 2 bundle — agent, student TUI, teacher TUI.
# Run this on the Linux box itself (Fedora, Mint, Ubuntu, …); the Go toolchain
# cross-targets nothing here, it's a straight host build. CGO is disabled so the
# binaries are fully static and run on any distro regardless of glibc version.
#
#     ./build-linux.sh
#
# Output → dist/linux/:
#   classsend-agent-linux-amd64   student background agent
#   student-linux-amd64           student chat TUI   (role baked in)
#   teacher-linux-amd64           teacher TUI        (role baked in)
#   about.md                      bundled so --about works
#
# Next: setup/install.sh (student PC) · setup/install-teacher.sh (teacher PC) ·
#       setup/package-deb.sh / setup/package-rpm.sh (mass student deployment).

set -euo pipefail
cd "$(dirname "$0")"

# Single source of truth for the version lives in build.bat (shared with the
# Windows release) — parse it out so every platform reports the same string.
VERSION="${VERSION_OVERRIDE:-$(grep -m1 '^set VERSION=' build.bat | cut -d= -f2 | tr -d '\r')}"
if [[ -z "$VERSION" ]]; then
    echo "ERROR: could not read VERSION from build.bat" >&2
    exit 1
fi
BUILDTIME="$(date +%Y-%m-%dT%H:%M:%S)"
VFLAGS="-X classsend/internal/buildinfo.Version=$VERSION -X classsend/internal/buildinfo.BuildTime=$BUILDTIME"

export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
OUT="dist/linux"
mkdir -p "$OUT"

echo "Building ClassSend 2 v$VERSION (built $BUILDTIME) for linux/amd64…"

echo "  • classsend-agent-linux-amd64"
go build -ldflags="$VFLAGS" -o "$OUT/classsend-agent-linux-amd64" ./cmd/classsend-agent

echo "  • student-linux-amd64"
go build -ldflags="-X main.defaultRole=student $VFLAGS" -o "$OUT/student-linux-amd64" ./cmd/classsend

echo "  • teacher-linux-amd64"
go build -ldflags="-X main.defaultRole=teacher $VFLAGS" -o "$OUT/teacher-linux-amd64" ./cmd/classsend

cp -f about.md "$OUT/about.md"

echo
echo "Linux bundle ready in $OUT/:"
ls -1 "$OUT"
echo
echo "Install:"
echo "  student PCs : sudo ./setup/install.sh         (or build a .deb/.rpm)"
echo "  teacher PC  : sudo ./setup/install-teacher.sh  (or just run the binary)"
