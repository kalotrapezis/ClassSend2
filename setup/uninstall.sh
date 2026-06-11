#!/usr/bin/env bash
# Reverse install.sh and install-teacher.sh. Does NOT touch a .deb/.rpm-installed
# copy — use `apt remove classsend2` / `dnf remove classsend2` for those.

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "This script must be run as root (sudo)." >&2
    exit 1
fi

pkill -x classsend-agent 2>/dev/null || true
pkill -x classsend 2>/dev/null || true
pkill -x classsend-teacher 2>/dev/null || true

rm -f /usr/local/bin/classsend-agent
rm -f /usr/local/bin/classsend
rm -f /usr/local/bin/classsend-teacher
rm -rf /usr/local/share/classsend2
rm -f /etc/xdg/autostart/classsend-agent.desktop
rm -f /usr/share/applications/classsend.desktop
rm -f /usr/share/applications/classsend-teacher.desktop

echo "Uninstalled."
echo "Per-user data in ~/.config/classsend2/ and ~/.config/autostart/classsend-agent.desktop is left in place."
