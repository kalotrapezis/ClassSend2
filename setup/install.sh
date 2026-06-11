#!/usr/bin/env bash
# No-package install of the ClassSend 2 STUDENT bundle — for distros where the
# .deb/.rpm won't drop in cleanly, or users who just want files copied. Same end
# state as the packages: binaries in /usr/local/bin, autostart .desktop in
# /etc/xdg/autostart, launcher in /usr/share/applications.
#
# For the TEACHER machine use setup/install-teacher.sh instead — you do NOT want
# the student autostart agent running on the teacher PC.
#
# Run from the project root, after build-linux.sh:
#     sudo ./setup/install.sh

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "This script must be run as root (sudo)." >&2
    exit 1
fi

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

DIST="$ROOT/dist/linux"
for f in classsend-agent-linux-amd64 student-linux-amd64 about.md; do
    if [[ ! -f "$DIST/$f" ]]; then
        echo "ERROR: missing $DIST/$f — run ./build-linux.sh first" >&2
        exit 1
    fi
done

# Dependency probe — non-fatal, but warn about missing runtime tools and show
# the right install command for the detected package manager.
missing=""
for cmd in mpv notify-send gdbus wmctrl loginctl pactl xdg-open; do
    command -v "$cmd" >/dev/null 2>&1 || missing="$missing $cmd"
done
# Screenshot backend — Wayland needs spectacle (KDE) or grim (wlroots); the
# X11 tools (scrot/import) don't work under Wayland. Need at least one usable.
shot_ok=0
for cmd in gnome-screenshot spectacle grim scrot import; do
    if command -v "$cmd" >/dev/null 2>&1; then shot_ok=1; break; fi
done
[[ $shot_ok -eq 0 ]] && missing="$missing (a screenshot tool: spectacle/grim on Wayland, scrot/imagemagick on X11)"

if [[ -n "$missing" ]]; then
    echo "WARNING: these runtime tools are missing —$missing"
    if command -v dnf >/dev/null 2>&1; then
        echo "  Fedora (KDE):  sudo dnf install mpv libnotify glib2 wmctrl pulseaudio-utils spectacle xdg-utils"
        echo "  Fedora (GNOME):sudo dnf install mpv libnotify glib2 wmctrl pulseaudio-utils gnome-screenshot xdg-utils"
    elif command -v apt >/dev/null 2>&1; then
        echo "  Debian/Mint:   sudo apt install mpv libnotify-bin libglib2.0-bin wmctrl pulseaudio-utils gnome-screenshot xdg-utils"
    elif command -v pacman >/dev/null 2>&1; then
        echo "  Arch:          sudo pacman -S mpv libnotify glib2 wmctrl libpulse gnome-screenshot xdg-utils"
    fi
    echo "  Continuing anyway."
fi

install -m 0755 "$DIST/classsend-agent-linux-amd64" /usr/local/bin/classsend-agent
install -m 0755 "$DIST/student-linux-amd64"         /usr/local/bin/classsend
install -d /usr/local/share/classsend2
install -m 0644 "$DIST/about.md" /usr/local/share/classsend2/about.md

install -d /etc/xdg/autostart
cat > /etc/xdg/autostart/classsend-agent.desktop <<'EOF'
[Desktop Entry]
Type=Application
Name=ClassSend Agent
Comment=ClassSend 2 student-side background agent
Exec=/usr/local/bin/classsend-agent
Hidden=false
NoDisplay=true
X-GNOME-Autostart-enabled=true
Terminal=false
EOF
chmod 0644 /etc/xdg/autostart/classsend-agent.desktop

install -d /usr/share/applications
cat > /usr/share/applications/classsend.desktop <<'EOF'
[Desktop Entry]
Type=Application
Name=ClassSend
Comment=Chat with your teacher
Exec=x-terminal-emulator -e classsend
Icon=utilities-terminal
Categories=Education;
Terminal=false
EOF
chmod 0644 /usr/share/applications/classsend.desktop

echo
echo "Installed (student bundle):"
echo "  /usr/local/bin/classsend-agent"
echo "  /usr/local/bin/classsend"
echo "  /usr/local/share/classsend2/about.md"
echo "  /etc/xdg/autostart/classsend-agent.desktop"
echo "  /usr/share/applications/classsend.desktop"
echo
echo "The agent starts on next login. To start now:  classsend-agent &"
echo "To uninstall: sudo ./setup/uninstall.sh"
