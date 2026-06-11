#!/usr/bin/env bash
# Install the ClassSend 2 TEACHER TUI on the teacher's own Linux machine.
# Unlike the student bundle there is NO autostart and NO background agent — the
# teacher launches the TUI by hand from a terminal (or the desktop entry).
#
# Note: on Linux the teacher can chat, send files, schedule commands and view
# the class; OS-level student commands (lock/mute/screenshot/window control)
# run in each student's agent, so they work cross-platform.
#
# Screen casting (broadcasting the teacher's screen) works on Linux too, but
# only from an X11 session — it shells out to ffmpeg's x11grab. Wayland blocks
# unattended screen capture, so on Fedora KDE/GNOME log in with the X11/Xorg
# session if you intend to cast. Needs ffmpeg with libx264 OR libopenh264.
#
# Run from the project root, after build-linux.sh:
#     sudo ./setup/install-teacher.sh

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "This script must be run as root (sudo)." >&2
    exit 1
fi

cd "$(dirname "$0")/.."
ROOT="$(pwd)"
DIST="$ROOT/dist/linux"

for f in teacher-linux-amd64 about.md; do
    if [[ ! -f "$DIST/$f" ]]; then
        echo "ERROR: missing $DIST/$f — run ./build-linux.sh first" >&2
        exit 1
    fi
done

# Casting dependency probe — non-fatal. Casting needs ffmpeg with an H.264
# encoder; everything else in the teacher TUI works without it.
if ! command -v ffmpeg >/dev/null 2>&1; then
    echo "NOTE: ffmpeg not found — screen casting will be disabled (chat/files/etc. still work)."
    if command -v dnf >/dev/null 2>&1; then
        echo "  Fedora: sudo dnf install ffmpeg   (full, has libx264; or ffmpeg-free for libopenh264)"
    elif command -v apt >/dev/null 2>&1; then
        echo "  Debian/Mint: sudo apt install ffmpeg"
    fi
elif ! ffmpeg -hide_banner -encoders 2>/dev/null | grep -qE "libx264|libopenh264"; then
    echo "NOTE: ffmpeg has no libx264/libopenh264 encoder — casting will be disabled."
    command -v dnf >/dev/null 2>&1 && echo "  Fedora: sudo dnf install openh264 ffmpeg  (RPM Fusion gives libx264)"
fi

install -m 0755 "$DIST/teacher-linux-amd64" /usr/local/bin/classsend-teacher
install -d /usr/local/share/classsend2
install -m 0644 "$DIST/about.md" /usr/local/share/classsend2/about.md

install -d /usr/share/applications
cat > /usr/share/applications/classsend-teacher.desktop <<'EOF'
[Desktop Entry]
Type=Application
Name=ClassSend Teacher
Comment=Run the ClassSend 2 classroom console
Exec=x-terminal-emulator -e classsend-teacher
Icon=utilities-terminal
Categories=Education;
Terminal=false
EOF
chmod 0644 /usr/share/applications/classsend-teacher.desktop

echo
echo "Installed (teacher):"
echo "  /usr/local/bin/classsend-teacher"
echo "  /usr/share/applications/classsend-teacher.desktop"
echo
echo "Start it with:  classsend-teacher"
echo "To uninstall:   sudo ./setup/uninstall.sh"
