#!/usr/bin/env bash
# Build an .rpm for the ClassSend 2 STUDENT bundle. Run on Fedora / RHEL /
# openSUSE — needs rpmbuild (Fedora: `sudo dnf install rpm-build`). For
# Debian/Mint use setup/package-deb.sh instead.
#
# Usage:
#   ./setup/package-rpm.sh           # VERSION from build.bat
#   VERSION=0.2.2 ./setup/package-rpm.sh
#
# Inputs (produced by ./build-linux.sh):
#   dist/linux/classsend-agent-linux-amd64
#   dist/linux/student-linux-amd64
#   dist/linux/about.md
#
# Output:
#   dist/linux/classsend2-<VERSION>-1.x86_64.rpm

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

if ! command -v rpmbuild >/dev/null 2>&1; then
    echo "ERROR: rpmbuild not found. Install it (Fedora: sudo dnf install rpm-build)," >&2
    echo "       or use setup/package-deb.sh on Debian/Mint." >&2
    exit 1
fi

VERSION="${VERSION:-$(grep -m1 '^set VERSION=' build.bat | cut -d= -f2 | tr -d '\r')}"
if [[ -z "$VERSION" ]]; then
    echo "ERROR: could not determine VERSION (set VERSION=… or pass via env)" >&2
    exit 1
fi
# RPM versions can't contain '-'; map any pre-release suffix into the release.
RPM_VERSION="${VERSION//-/_}"

DIST="$ROOT/dist/linux"
AGENT_BIN="$DIST/classsend-agent-linux-amd64"
TUI_BIN="$DIST/student-linux-amd64"
ABOUT="$DIST/about.md"
for f in "$AGENT_BIN" "$TUI_BIN" "$ABOUT"; do
    [[ -f "$f" ]] || { echo "ERROR: missing $f — run ./build-linux.sh first" >&2; exit 1; }
done

# ── Build a self-contained rpm tree (no ~/rpmbuild pollution) ─────────────────
TOP=$(mktemp -d)
trap 'rm -rf "$TOP"' EXIT
mkdir -p "$TOP"/{BUILD,RPMS,SOURCES,SPECS,SRPMS,BUILDROOT}

SPEC="$TOP/SPECS/classsend2.spec"
cat > "$SPEC" <<EOF
Name:           classsend2
Version:        $RPM_VERSION
Release:        1%{?dist}
Summary:        ClassSend 2 — student-side classroom agent
License:        Proprietary
URL:            https://github.com/
BuildArch:      x86_64

# Fedora package names (apt equivalents in parentheses):
#   mpv            cast viewer
#   libnotify      notify-send banner            (libnotify-bin)
#   glib2          gdbus to close notifications  (libglib2.0-bin)
#   wmctrl         focus / close windows
#   xdg-utils      xdg-open for push-open / file auto-open
#   systemd        loginctl for screen lock
Requires:       mpv, libnotify, glib2, wmctrl, xdg-utils, systemd
Recommends:     gnome-screenshot
Recommends:     pulseaudio-utils

%description
Background agent and chat TUI for the ClassSend 2 classroom-management system.
The agent connects to a teacher PC over the local network and executes class
commands (lock, mute, screenshot, screen cast viewer, monitoring notification).
The TUI provides chat with the teacher.

# Binaries are prebuilt and stripped-free; skip the default post-processing that
# expects debug info / a build step.
%global debug_package %{nil}
%global __os_install_post %{nil}

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/share/classsend2
mkdir -p %{buildroot}/usr/share/applications
mkdir -p %{buildroot}/etc/xdg/autostart

install -m 0755 $AGENT_BIN %{buildroot}/usr/bin/classsend-agent
install -m 0755 $TUI_BIN   %{buildroot}/usr/bin/classsend
install -m 0644 $ABOUT     %{buildroot}/usr/share/classsend2/about.md

cat > %{buildroot}/usr/share/applications/classsend.desktop <<'DESK'
[Desktop Entry]
Type=Application
Name=ClassSend
Comment=Chat with your teacher
Exec=x-terminal-emulator -e classsend
Icon=utilities-terminal
Categories=Education;
Terminal=false
DESK

cat > %{buildroot}/etc/xdg/autostart/classsend-agent.desktop <<'AUTO'
[Desktop Entry]
Type=Application
Name=ClassSend Agent
Comment=ClassSend 2 student-side background agent
Exec=/usr/bin/classsend-agent
Hidden=false
NoDisplay=true
X-GNOME-Autostart-enabled=true
Terminal=false
AUTO

%preun
# On full removal (not upgrade), stop a running agent.
if [ \$1 -eq 0 ]; then
    pkill -x classsend-agent 2>/dev/null || true
fi

%files
/usr/bin/classsend-agent
/usr/bin/classsend
/usr/share/classsend2/about.md
/usr/share/applications/classsend.desktop
/etc/xdg/autostart/classsend-agent.desktop
EOF

rpmbuild --define "_topdir $TOP" -bb "$SPEC"

mkdir -p "$DIST"
RPM_OUT=$(find "$TOP/RPMS" -name '*.rpm' | head -1)
cp -f "$RPM_OUT" "$DIST/"
FINAL="$DIST/$(basename "$RPM_OUT")"

echo
echo "Built: $FINAL"
echo "Install with:  sudo dnf install ./$(basename "$FINAL")"
echo "Inspect with:  rpm -qlp $FINAL"
