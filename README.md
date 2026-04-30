# ClassSend2

A classroom management application for teachers and students, built with Go. Provides a terminal UI for real-time chat, student monitoring, screen lock, and classroom command dispatch over a local network.

> Successor to the original ClassSend, deployed in a real Greek classroom.

---

## Features

### Teacher
- Real-time chat broadcast to all connected students
- Student roster with online/offline status, IP, hand-up indicator, and mute state
- **Commands** (`--t`): lock/unlock screen, mute/unmute audio, close apps, launch/focus app, capture screenshots, shutdown, start/stop monitoring, start/stop screen casting
- **Monitoring grid** (`tvon`): live screenshot thumbnails from all student PCs in a Win32 grid window
- **Screen casting** (`casting` / `cas`, stop: `casoff`, toggle: `^S`): broadcasts the teacher's screen as JPEG frames to all students in real time
- Pin/unpin messages (`--pin` / `--upin`), delete messages (`--del`), report students (`--rem`)
- Blacklist + whitelist overlay (`^L`) with fuzzy enforcement and import/export
- File transfer (chunked, 32 KB frames) with zip-all download
- Push-open: send a URL or file to open on all student machines (`push_open`)
- Media library pin for shared resources
- Tab completion, command history (↑/↓), input syntax highlighting

### Student
- Chat with teacher and class
- `classsend-agent.exe` runs silently in the background (auto-started at login)
- Student UI (`student.exe`) connects to the agent via local IPC; can be closed and reopened without dropping the session
- Late-join message history replay
- Amber monitoring notification banner (appears when teacher starts monitoring)
- **Cast viewer**: resizable window (960×600) showing teacher's screen; `T` = toggle always-on-top, `F` = maximize, `X` = hide; reopen with `--cast`
- **Blacklist enforcement**: messages containing blacklisted words (with fuzzy matching) are blocked before sending

### Easter Eggs
- `--coffee` — ☕ break reminder
- `--matrix` — full-screen katakana rain (~4 s, 60 ms ticks)

---

## Architecture

```
cmd/classsend/              Teacher TUI + Student TUI (role baked in at build time)
cmd/classsend-agent/        Student background agent (hidden Win32 process)
cmd/monitoring/             Teacher-side screenshot grid (Win32 GUI)
internal/protocol/          Wire format, message types, command constants
internal/core/              App state, business logic, persistence
internal/ipc/               Agent ↔ TUI IPC over TCP loopback 127.0.0.1:14789
internal/tui/               Bubbletea model, Lipgloss styles, custom events
internal/network/           TCP server, client, scanner, NIC detection, probe
internal/monitoring/        Session orchestrator, named-pipe protocol
setup/classsend2.iss        Inno Setup 6 installer script
build.bat                   Builds all four executables + installer
```

### Communication layers

```
Teacher TUI ──TCP──► Teacher server (internal/network/server.go)
                           │
                      broadcast/unicast
                           │
               ┌───────────▼───────────┐
               │  classsend-agent.exe  │  (per student PC)
               │  · TCP client         │
               │  · System commands    │
               │  · IPC server :14789  │
               └───────────┬───────────┘
                           │ local IPC
                    student.exe (TUI)
```

---

## Build

**Requirements:** Go 1.24+ (main build), Go 1.20.14 at `C:\Go120\` (Win7 agent build), Windows, Inno Setup 6.

```bat
build.bat
```

Output files:

| File | Role | Notes |
|---|---|---|
| `teacher.exe` | Teacher PC | TUI; place `monitoring.exe` beside it |
| `student.exe` | Student PCs | Chat TUI only |
| `classsend-agent.exe` | Student PCs | Hidden background process |
| `monitoring.exe` | Teacher PC | Screenshot grid |
| `dist/ClassSend2-Setup-v0.0.2.exe` | All PCs | Inno Setup installer (Win7 SP1 – Win11) |

Role is baked in at build time via `-ldflags="-X main.defaultRole=teacher"` (or `student`).  
`classsend-agent.exe` is built with `-H windowsgui` — no console window.

The installer bundles two agent binaries and selects at install time:
- **Win10+ x64** → native 64-bit agent (Go 1.24+)
- **Win7/8 or 32-bit** → 32-bit agent built with Go 1.20.14

---

## Installation

Run `dist/ClassSend2-Setup-v0.0.2.exe` and choose a role:

| Role | What gets installed | Auto-start |
|---|---|---|
| **Teacher** | `teacher.exe` + `monitoring.exe` | None |
| **Student** | `student.exe` + `classsend-agent.exe` | Agent at login (HKCU Run) |
| **Developer / Testing** | All four executables | Agent with `--dev` at login |

The Developer option is designed for single-machine testing: start Agent → start Teacher → start Student.

**Minimum OS:** Windows 7 SP1 (32-bit or 64-bit).

---

## Data files

Stored in `%APPDATA%\ClassSend\`:

| File | Contents |
|---|---|
| `messages.json` | Chat history (teacher-side) |
| `lists.json` | Blacklist + whitelist |
| `settings.json` | Nickname (per role) |

Default lists ship with 54 blacklist entries and 43 whitelist entries seeded from real classroom data.

---

## Key commands

| Command | Effect |
|---|---|
| `--pin <msg>` | Pin a message (broadcast) |
| `--upin` | Unpin current message |
| `--del @X.Y` | Delete a message |
| `--rem @student` | Report/remove a student |
| `--pass <text>` | Broadcast a password/note |
| `--black <word>` | Add word to blacklist |
| `--blk @BN` | Remove blacklist entry by index |
| `--wh <word>` | Add word to whitelist |
| `--wh @WN` | Remove whitelist entry by index |
| `--clr` / `--clr @s` | Clear chat / clear system messages only |
| `--dl` | Download file; `--dl all` zips all |
| `--a` | Open file picker attachment |
| `--cp` | Copy pinned content |
| `--op` | Open pinned file/URL |
| `--t <cmd>` | Send system command (lock/unlock/mute/shot/tvon/tvoff/casting/casoff/…) |
| `--set nickname <name>` | Set display name (persisted, synced to agent) |
| `--set autostart on/off` | Enable/disable agent autostart |
| `--set list import <file>` | Import blacklist/whitelist |
| `--set list export [file]` | Export blacklist/whitelist |
| `--cast` *(student)* | Reopen cast viewer window if closed |

**Keyboard shortcuts:**

| Key | Action |
|---|---|
| `^S` | Toggle screen casting (teacher) |
| `^T` | Tools overlay |
| `^A` | File picker |
| `^H` | Help overlay |
| `^L` | Blacklist/Whitelist overlay (teacher) |
| `↑` / `↓` | Command history |
| `Tab` | Autocomplete command |

**Cast viewer shortcuts (student window):**

| Key | Action |
|---|---|
| `T` | Toggle always-on-top |
| `F` | Toggle maximize / restore |
| `X` | Hide window (cast continues) |

---

## Dependencies

- [Bubbletea](https://github.com/charmbracelet/bubbletea) — TUI framework
- [Lipgloss](https://github.com/charmbracelet/lipgloss) — terminal styling
- [Bubbles](https://github.com/charmbracelet/bubbles) — TUI components
- Windows `user32` / `kernel32` / `gdi32` (via `golang.org/x/sys/windows`) — system commands and GUI

---

## License

ClassSend2 is free software, released under the **GNU General Public License v3.0**.  
See [LICENSE](LICENSE) for the full terms.

Copyright (C) 2025 Teo Kalotrapezis
