#!/usr/bin/env bash
# Build .deb packages for ClassSend 2. Two packages, for the two machine roles:
#
#   classsend2          — student bundle: background agent (autostarts) + chat TUI
#   classsend2-teacher  — teacher console (no agent, no autostart)
#
# Run on a Debian-family box (Mint/Ubuntu) for native dpkg-deb, or anywhere with
# binutils (ar) + tar — the script falls back to assembling the .deb by hand, so
# it works on Fedora too. For RPMs use setup/package-rpm.sh.
#
# Usage:
#   ./setup/package-deb.sh [student|teacher|both]   # default: both
#   VERSION=0.2.3 ./setup/package-deb.sh teacher
#
# Inputs (produced by ./build-linux.sh):
#   dist/linux/classsend-agent-linux-amd64
#   dist/linux/student-linux-amd64
#   dist/linux/teacher-linux-amd64
#   dist/linux/about.md
#
# Output:
#   dist/linux/classsend2_<VERSION>_amd64.deb
#   dist/linux/classsend2-teacher_<VERSION>_amd64.deb

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

ROLE="${1:-both}"
DIST="$ROOT/dist/linux"

require() { [[ -f "$1" ]] || { echo "ERROR: missing $1 — run ./build-linux.sh first" >&2; exit 1; }; }

# assemble_deb <staged-pkg-dir> <output.deb> — uses dpkg-deb if present, else
# hand-rolls the ar archive (debian-binary + control.tar.gz + data.tar.gz).
assemble_deb() {
    local PKG="$1" OUT="$2"
    if command -v dpkg-deb >/dev/null 2>&1; then
        dpkg-deb --build --root-owner-group "$PKG" "$OUT" >/dev/null
    else
        local TMP; TMP=$(mktemp -d)
        echo "2.0" > "$TMP/debian-binary"
        tar --owner=root --group=root -czf "$TMP/control.tar.gz" -C "$PKG/DEBIAN" .
        tar --owner=root --group=root -czf "$TMP/data.tar.gz" -C "$PKG" --exclude=./DEBIAN .
        rm -f "$OUT"
        ( cd "$TMP" && ar rc "$OUT" debian-binary control.tar.gz data.tar.gz )
        rm -rf "$TMP"
    fi
    echo "Built: $OUT"
}

make_student_deb() {
    require "$DIST/classsend-agent-linux-amd64"
    require "$DIST/student-linux-amd64"
    require "$DIST/about.md"

    local STAGE; STAGE=$(mktemp -d); trap 'rm -rf "$STAGE"' RETURN
    local PKG="$STAGE/pkg"
    mkdir -p "$PKG/DEBIAN" "$PKG/usr/bin" "$PKG/usr/share/classsend2" \
             "$PKG/usr/share/applications" "$PKG/etc/xdg/autostart"

    install -m 0755 "$DIST/classsend-agent-linux-amd64" "$PKG/usr/bin/classsend-agent"
    install -m 0755 "$DIST/student-linux-amd64"         "$PKG/usr/bin/classsend"
    install -m 0644 "$DIST/about.md"                    "$PKG/usr/share/classsend2/about.md"

    cat > "$PKG/usr/share/applications/classsend.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=ClassSend
Comment=Chat with your teacher
Exec=classsend
Icon=utilities-terminal
Categories=Education;
Terminal=true
EOF

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

    local SIZE_KB; SIZE_KB=$(du -sk "$PKG/usr" | cut -f1)
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
    printf '#!/bin/sh\nset -e\nexit 0\n' > "$PKG/DEBIAN/postinst"
    printf '#!/bin/sh\nset -e\npkill -x classsend-agent 2>/dev/null || true\nexit 0\n' > "$PKG/DEBIAN/prerm"
    chmod 0755 "$PKG/DEBIAN/postinst" "$PKG/DEBIAN/prerm"

    assemble_deb "$PKG" "$DIST/classsend2_${VERSION}_amd64.deb"
}

make_teacher_deb() {
    require "$DIST/teacher-linux-amd64"
    require "$DIST/about.md"

    local STAGE; STAGE=$(mktemp -d); trap 'rm -rf "$STAGE"' RETURN
    local PKG="$STAGE/pkg"
    mkdir -p "$PKG/DEBIAN" "$PKG/usr/bin" "$PKG/usr/share/classsend2" \
             "$PKG/usr/share/applications"

    install -m 0755 "$DIST/teacher-linux-amd64" "$PKG/usr/bin/classsend-teacher"
    install -m 0644 "$DIST/about.md"            "$PKG/usr/share/classsend2/about.md"

    cat > "$PKG/usr/share/applications/classsend-teacher.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=ClassSend Teacher
Comment=Run the ClassSend 2 classroom console
Exec=classsend-teacher
Icon=utilities-terminal
Categories=Education;
Terminal=true
EOF

    local SIZE_KB; SIZE_KB=$(du -sk "$PKG/usr" | cut -f1)
    cat > "$PKG/DEBIAN/control" <<EOF
Package: classsend2-teacher
Version: $VERSION
Section: education
Priority: optional
Architecture: amd64
Installed-Size: $SIZE_KB
Maintainer: Theologos Kalotrapezis <kalotrapezis@gmail.com>
Recommends: ffmpeg
Description: ClassSend 2 — teacher console
 The teacher-side classroom console for ClassSend 2: chat, file push,
 scheduled commands, the live monitoring grid and screen casting (X11).
 Runs in a terminal; launch with classsend-teacher. No background agent and
 no autostart — install the classsend2 package (not this) on student PCs.
EOF

    assemble_deb "$PKG" "$DIST/classsend2-teacher_${VERSION}_amd64.deb"
}

case "$ROLE" in
    student) make_student_deb ;;
    teacher) make_teacher_deb ;;
    both)    make_student_deb; make_teacher_deb ;;
    *) echo "usage: $0 [student|teacher|both]" >&2; exit 2 ;;
esac

echo
echo "Install with:  sudo apt install ./dist/linux/<file>.deb"
