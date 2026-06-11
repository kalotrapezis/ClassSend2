#!/usr/bin/env bash
# Build .rpm packages for ClassSend 2. Two packages, for the two machine roles:
#
#   classsend2          — student bundle: background agent (autostarts) + chat TUI
#   classsend2-teacher  — teacher console (no agent, no autostart)
#
# Run on Fedora / RHEL / openSUSE — needs rpmbuild (Fedora: dnf install rpm-build).
# For .deb use setup/package-deb.sh.
#
# Usage:
#   ./setup/package-rpm.sh [student|teacher|both]   # default: both
#   VERSION=0.2.3 ./setup/package-rpm.sh teacher
#
# Inputs (produced by ./build-linux.sh):
#   dist/linux/classsend-agent-linux-amd64
#   dist/linux/student-linux-amd64
#   dist/linux/teacher-linux-amd64
#   dist/linux/about.md
#
# Output:
#   dist/linux/classsend2-<VERSION>-1.<dist>.x86_64.rpm
#   dist/linux/classsend2-teacher-<VERSION>-1.<dist>.x86_64.rpm

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
RPM_VERSION="${VERSION//-/_}" # rpm versions can't contain '-'

ROLE="${1:-both}"
DIST="$ROOT/dist/linux"
AGENT_BIN="$DIST/classsend-agent-linux-amd64"
STUDENT_BIN="$DIST/student-linux-amd64"
TEACHER_BIN="$DIST/teacher-linux-amd64"
ABOUT="$DIST/about.md"

require() { [[ -f "$1" ]] || { echo "ERROR: missing $1 — run ./build-linux.sh first" >&2; exit 1; }; }

# build_rpm <specfile> — builds in an isolated tree and copies the result to DIST.
build_rpm() {
    local SPEC="$1"
    local TOP; TOP=$(mktemp -d)
    mkdir -p "$TOP"/{BUILD,RPMS,SOURCES,SPECS,SRPMS,BUILDROOT}
    rpmbuild --define "_topdir $TOP" -bb "$SPEC" >/dev/null
    local OUT; OUT=$(find "$TOP/RPMS" -name '*.rpm' | head -1)
    cp -f "$OUT" "$DIST/"
    echo "Built: $DIST/$(basename "$OUT")"
    rm -rf "$TOP"
}

make_student_rpm() {
    require "$AGENT_BIN"; require "$STUDENT_BIN"; require "$ABOUT"
    local SPEC; SPEC=$(mktemp --suffix=.spec)
    cat > "$SPEC" <<EOF
Name:           classsend2
Version:        $RPM_VERSION
Release:        1%{?dist}
Summary:        ClassSend 2 — student-side classroom agent
License:        Proprietary
BuildArch:      x86_64
Requires:       mpv, libnotify, glib2, wmctrl, xdg-utils, systemd
Recommends:     gnome-screenshot
Recommends:     pulseaudio-utils

%description
Background agent and chat TUI for the ClassSend 2 classroom-management system.
The agent connects to a teacher PC over the local network and executes class
commands (lock, mute, screenshot, screen cast viewer, monitoring notification).
The TUI provides chat with the teacher.

%global debug_package %{nil}
%global __os_install_post %{nil}

%install
mkdir -p %{buildroot}/usr/bin %{buildroot}/usr/share/classsend2 \\
         %{buildroot}/usr/share/applications %{buildroot}/etc/xdg/autostart
install -m 0755 $AGENT_BIN   %{buildroot}/usr/bin/classsend-agent
install -m 0755 $STUDENT_BIN %{buildroot}/usr/bin/classsend
install -m 0644 $ABOUT       %{buildroot}/usr/share/classsend2/about.md
cat > %{buildroot}/usr/share/applications/classsend.desktop <<'DESK'
[Desktop Entry]
Type=Application
Name=ClassSend
Comment=Chat with your teacher
Exec=classsend
Icon=utilities-terminal
Categories=Education;
Terminal=true
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
if [ \$1 -eq 0 ]; then pkill -x classsend-agent 2>/dev/null || true; fi

%files
/usr/bin/classsend-agent
/usr/bin/classsend
/usr/share/classsend2/about.md
/usr/share/applications/classsend.desktop
/etc/xdg/autostart/classsend-agent.desktop
EOF
    build_rpm "$SPEC"; rm -f "$SPEC"
}

make_teacher_rpm() {
    require "$TEACHER_BIN"; require "$ABOUT"
    local SPEC; SPEC=$(mktemp --suffix=.spec)
    cat > "$SPEC" <<EOF
Name:           classsend2-teacher
Version:        $RPM_VERSION
Release:        1%{?dist}
Summary:        ClassSend 2 — teacher console
License:        Proprietary
BuildArch:      x86_64
Recommends:     ffmpeg

%description
The teacher-side classroom console for ClassSend 2: chat, file push, scheduled
commands, the live monitoring grid and screen casting (X11). Runs in a terminal;
launch with classsend-teacher. No background agent and no autostart — install
the classsend2 package (not this) on student PCs.

%global debug_package %{nil}
%global __os_install_post %{nil}

%install
mkdir -p %{buildroot}/usr/bin %{buildroot}/usr/share/classsend2 \\
         %{buildroot}/usr/share/applications
install -m 0755 $TEACHER_BIN %{buildroot}/usr/bin/classsend-teacher
install -m 0644 $ABOUT        %{buildroot}/usr/share/classsend2/about.md
cat > %{buildroot}/usr/share/applications/classsend-teacher.desktop <<'DESK'
[Desktop Entry]
Type=Application
Name=ClassSend Teacher
Comment=Run the ClassSend 2 classroom console
Exec=classsend-teacher
Icon=utilities-terminal
Categories=Education;
Terminal=true
DESK

%files
/usr/bin/classsend-teacher
/usr/share/classsend2/about.md
/usr/share/applications/classsend-teacher.desktop
EOF
    build_rpm "$SPEC"; rm -f "$SPEC"
}

case "$ROLE" in
    student) make_student_rpm ;;
    teacher) make_teacher_rpm ;;
    both)    make_student_rpm; make_teacher_rpm ;;
    *) echo "usage: $0 [student|teacher|both]" >&2; exit 2 ;;
esac

echo
echo "Install with:  sudo dnf install ./dist/linux/<file>.rpm"
