# Changelog

All notable changes to ClassSend2 are documented here.  
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).  
ClassSend2 adheres to [Semantic Versioning](https://semver.org/).

---

## [Unreleased]

### Added
- **Screen casting** (`--t casting` / `--t cas`, stop with `--t casoff` / `^S` toggle): high-quality, low-latency screen broadcast over a dedicated TCP stream (port 47821), separate from the main chat connection.
  - **30 FPS** GDI native-resolution capture (`GetDIBits` → BGRA → JPEG) with adaptive quality control (Q50–Q90, starts at Q85, adjusts every 60 frames based on frame-drop rate)
  - **Latest-frame-only fanout** — each student connection holds only the newest frame (`atomic.Value` + buffered-1 notify channel); slow students drop frames instead of queueing, teacher never blocks
  - **TCP_NODELAY** and 4 MB socket buffers on the cast server for minimal latency
  - **Fullscreen Win32 viewer** on student machines (`wsPopup | wsExTopmost`, covers entire screen); students cannot close it — only `CmdStopCast` from the teacher does
  - `CmdStartCast` carries the server address (`LAN_IP:47821`) as its parameter; students dial directly, bypassing the main TCP connection entirely
  - New commands `CmdStartCast` / `CmdStopCast`; `Casting` field in `ClassState` / `StatePayload` so late-joining students receive correct state
  - `^S` keyboard shortcut in TUI toggles casting on/off; `"Casting●"` indicator in bottom bar when active

### Planned
- `^1`–`^0` tool shortcut keys
- Persistent teacher daemon (session survives TUI close)
- System tray icon for student agent
- Subnet scan with 30-day network history prioritization

---

## [0.0.1] — 2025-04-29

### Added

#### Core protocol & networking
- TCP server/client with newline-delimited JSON wire format
- Full message type set: chat, system, pin, file transfer, commands, ACK, push-open
- Chunked file transfer (32 KB frames) with AutoOpen support
- Student probe → auto-connect flow with NIC detection and subnet scanning
- Late-join full message-history replay and class-state sync

#### Teacher TUI
- Real-time chat with `@X.Y` message numbering (window of 10)
- Student roster sidebar: online/offline dots, IP, hand-up ✋, mute 🔇 indicators
- Command palette: `--pin`, `--upin`, `--del`, `--rem`, `--pass`, `--black`, `--blk`, `--wh`, `--clr`, `--dl`, `--a`, `--cp`, `--op`, `--t`, `--set`
- `--clr @s` clears only system messages (case-insensitive)
- Pinned messages (`@pN`) and pinned files (`@fN`) with consistent bar + inline labels
- File picker overlay (`^A`), tools overlay (`^T`), help overlay (`^H`)
- Blacklist + whitelist overlay (`^L`): indexed entries, add/remove in place
- Tab completion and bash-style `↑`/`↓` command history
- Input syntax highlighting (blue+bold for `--` commands)
- `--set nickname <name>` persisted to `settings.json` (max 32 chars)
- `--set list import <file>` / `export [file]` (auto-detects old `{word,addedAt,source}` and new plain-string JSON)
- Default lists: 54 blacklist + 43 whitelist entries seeded from real classroom data
- Push-open: send URL or file path to all connected students
- Media library pin
- "PC not found" notification silenced after 5 repetitions (shown at counts 1 and 5 only)
- `--coffee` easter egg — ☕ system message
- `--matrix` easter egg — 72-tick full-screen katakana rain at 60 ms/tick

#### Student TUI & agent split
- `student.exe` — chat TUI only; connects to local agent via IPC
- `classsend-agent.exe` — hidden Win32 background process (`-H windowsgui`); survives TUI close
- IPC over `127.0.0.1:14789` (newline-delimited JSON): `connected`, `disconnected`, `fwd`, `send` frame types
- Agent replays full message history + class state on TUI reconnect
- `--dev` flag: agent skips autostart, both processes run on the same machine

#### Student system commands (agent-side, Windows only)
- Lock screen: full-screen Win32 overlay with low-level keyboard hook; 5-second re-assertion loop
- Dev mode auto-unlock after 5 seconds
- Unlock, Shutdown, Close all apps, Mute/Unmute system audio
- Launch app, Focus app
- Screenshot request (`shot`)
- Monitoring notification banner: amber always-on-top strip (`tvon` / `tvoff`), non-activating, no taskbar entry
- Autostart via `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

#### Monitoring (monitoring.exe)
- Win32 grid GUI with one cell per student: name bar + screenshot thumbnail + offline state
- Sequential `CmdRequestShot` loop orchestrated by `internal/monitoring/session_windows.go`
- Named pipe `\\.\pipe\ClassSendMonitor` for teacher ↔ monitoring IPC
- Double-buffered GDI painting with HALFTONE scaling
- R↔B channel swap for BGRA DIBs
- Teacher TUI aliases: `tvon` / `tvoff` / `shot` in `--t` command

#### Installer (Inno Setup 6)
- Three role options: Teacher, Student, Developer/Testing
- Student install: agent placed in Program Files, HKCU Run key set
- Developer install: all four executables + three desktop shortcuts (all with `--dev`)
- Uninstall removes HKCU Run key and all files

#### Persistence (`%APPDATA%\ClassSend\`)
- `messages.json` — chat history (teacher)
- `lists.json` — blacklist + whitelist
- `settings.json` — nickname per role

#### UI / style
- Dark warm Lipgloss theme
- Bubbletea model covers teacher view, student view, and waiting/connecting states
- `viewWaiting` fallback to 80×24 when `WindowSizeMsg` has not yet arrived (fixes blank screen on double-click launch)

---

[Unreleased]: https://github.com/kalotrapezis/ClassSend2/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/kalotrapezis/ClassSend2/releases/tag/v0.0.1
