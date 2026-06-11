#!/usr/bin/env bash
# Build a .deb for the ClassSend 2 STUDENT bundle. Run on a Debian-family box
# (Linux Mint / Ubuntu 22.04+) — dpkg-deb is required and is not present on
# Fedora. For Fedora/RHEL use setup/package-rpm.sh instead.
#
# Usage:
#   ./setup/package-deb.sh           # VERSION from build.bat
#   VERSION=0.2.2 ./setup/package-deb.sh
#
# Inputs (produced by ./build-linux.sh):
#   dist/linux/classsend-agent-linux-amd64
#   dist/linux/student-linux-amd64
#   dist/linux/about.md
#
# Output:
#   dist/linux/classsend2_<VERSION>_amd64.deb

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

if ! command -v dpkg-deb >/dev/null 2>&1 && ! command -v ar >/dev/null 2>&1; then
    echo "ERROR: need dpkg-deb (Debian/Mint) or ar+tar (binutils) to build a .deb." >&2
    echo "       On Fedora: dnf install binutils, or use setup/package-rpm.sh." >&2
    exit 1
fi

VERSION="${VERSION:-$(grep -m1 '^set VERSION=' build.bat | cut -d= -f2 | tr -d '\r')}"
if [[ -z "$VERSION" ]]; then
    echo "ERROR: could not determine VERSION (set VERSION=… or pass via env)" >&2
    exit 1
fi

DIST="$ROOT/dist/linux"
AGENT_BIN="$DIST/classsend-agent-linux-amd64"
TUI_BIN="$DIST/student-linux-amd64"
ABOUT="$DIST/about.md"
for f in "$AGENT_BIN" "$TUI_BIN" "$ABOUT"; do
    [[ -f "$f" ]] || { echo "ERROR: missing $f — run ./build-linux.sh first" >&2; exit 1; }
done

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

PKG="$STAGE/classsend2_${VERSION}_amd64"
mkdir -p "$PKG/DEBIAN" "$PKG/usr/bin" "$PKG/usr/share/classsend2" \
         "$PKG/usr/share/applications" "$PKG/etc/xdg/autostart"

install -m 0755 "$AGENT_BIN" "$PKG/usr/bin/classsend-agent"
install -m 0755 "$TUI_BIN"   "$PKG/usr/bin/classsend"
install -m 0644 "$ABOUT"     "$PKG/usr/share/classsend2/about.md"

SIZE_KB=$(du -sk "$PKG/usr" | cut -f1)

cat > "$PKG/DEBIAN/control" <<EOF
Package: classsend2
Version: $VERSION
Section: education
Priority: optional
Architecture: amd64
Installed-Size: $SIZE_KB
Maintainer: Theologos Kalotrapezis <kalotrapezis@gmail.com>
Depends: mpv, libnotify-bin, libglib2.0-bin, wmctrl, xdg-utils, systemd
Recommends: gnome-screenshot | scrot | imagemagick, pulseaudio-utils | pipewire-pulse
Description: ClassSend 2 — student-side classroom agent
 Background agent and chat TUI for the ClassSend 2 classroom-management
 system. The agent connects to a teacher PC over the local network and
 executes class commands (lock, mute, screenshot, screen cast viewer,
 monitoring notification). The TUI provides chat with the teacher.
EOF

cat > "$PKG/usr/share/applications/classsend.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=ClassSend
Comment=Chat with your teacher
Exec=x-terminal-emulator -e classsend
Icon=utilities-terminal
Categories=Education;
Terminal=false
EOF
chmod 0644 "$PKG/usr/share/applications/classsend.desktop"

cat > "$PKG/etc/xdg/autostart/classsend-agent.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=ClassSend Agent
Comment=ClassSend 2 student-side background agent
Exec=/usr/bin/classsend-agent
Hidden=false
NoDisplay=true
X-GNOME-Autostart-enabled=true
Terminal=false
EOF
chmod 0644 "$PKG/etc/xdg/autostart/classsend-agent.desktop"

cat > "$PKG/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
exit 0
EOF
chmod 0755 "$PKG/DEBIAN/postinst"

cat > "$PKG/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e
pkill -x classsend-agent 2>/dev/null || true
exit 0
EOF
chmod 0755 "$PKG/DEBIAN/prerm"

OUT="$DIST/classsend2_${VERSION}_amd64.deb"
if command -v dpkg-deb >/dev/null 2>&1; then
    dpkg-deb --build --root-owner-group "$PKG" "$OUT"
else
    # Fallback for non-Debian hosts (e.g. Fedora): a .deb is just an `ar`
    # archive of debian-binary + control.tar.gz + data.tar.gz, in that order.
    # dpkg/apt accept a plain GNU-ar archive, so binutils is all we need.
    echo "dpkg-deb not found — assembling .deb with ar + tar"
    echo "2.0" > "$STAGE/debian-binary"
    tar --owner=root --group=root -czf "$STAGE/control.tar.gz" -C "$PKG/DEBIAN" .
    tar --owner=root --group=root -czf "$STAGE/data.tar.gz" -C "$PKG" --exclude=./DEBIAN .
    rm -f "$OUT"
    ( cd "$STAGE" && ar rc "$OUT" debian-binary control.tar.gz data.tar.gz )
fi

echo
echo "Built: $OUT"
echo "Install with:  sudo apt install ./$(basename "$OUT")"
echo "Inspect with:  dpkg-deb --contents $OUT"
