# Changelog

All notable changes to ClassSend2 are documented here.  
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).  
ClassSend2 adheres to [Semantic Versioning](https://semver.org/).

---

## [Unreleased]

### Planned
- `^1`–`^0` tool shortcut keys
- Persistent teacher daemon (session survives TUI close)
- System tray icon for student agent
- Subnet scan with 30-day network history prioritization

---

## [0.0.2] — 2026-04-30

### Added

#### Screen casting
- **Screen casting** (`--t casting` / `--t cas`, stop with `--t casoff` / `^S` toggle): high-quality, low-latency screen broadcast over a dedicated TCP stream (port 47821), separate from the main chat connection
  - **30 FPS** GDI native-resolution capture (`GetDIBits` → BGRA → JPEG) with adaptive quality control (Q50–Q90, starts at Q85, adjusts every 60 frames based on frame-drop rate)
  - **Latest-frame-only fanout** — each student connection holds only the newest frame (`atomic.Value` + buffered-1 notify channel); slow students drop frames instead of queuing, teacher never blocks
  - **TCP_NODELAY** and 4 MB socket buffers on the cast server for minimal latency
  - `CmdStartCast` carries the server address (`LAN_IP:47821`) as its parameter; students dial directly, bypassing the main TCP connection entirely
  - `^S` keyboard shortcut in TUI toggles casting on/off; `"Casting●"` indicator in bottom bar when active
  - `Casting` field in `ClassState` / `StatePayload` so late-joining students receive correct state

#### Cast viewer — windowed
- Cast viewer on student machines converted from a fullscreen popup (`wsPopup|wsExTopmost`) to a **resizable overlapped window** (`WS_OVERLAPPEDWINDOW`) centered at 960×600
- **`X` button** hides the window instead of destroying it — window is reused when the next cast starts
- **`T` key** toggles always-on-top while the cast window is focused
- **`F` key** toggles maximize / restore
- **`--cast`** student command to reopen the cast viewer if they closed it mid-session

#### Blacklist enforcement
- **Client-side blocking**: student's `trySend()` checks the locally-synced blacklist before sending — message is rejected immediately with a warning, never reaches the server
- **Server-side blocking**: teacher server also rejects blacklisted messages as a safety net; blocked messages stored with `Blocked: true`, shown to teacher only with a `[🚫]` prefix, not broadcast to other students
- **Fuzzy matching** via Levenshtein distance, scaled to word length:
  - ≤ 4 chars → exact match only
  - 5–7 chars → 1 edit allowed
  - 8–10 chars → 2 edits allowed
  - 11+ chars → 3 edits allowed
  - Words shorter than 3 characters are skipped; whitelist entries override blacklist matches
- `StatePayload` now carries `Blacklist` and `Whitelist` slices — students receive the lists on join and on every add/remove mutation, so enforcement is always in sync

#### `--set` UX fixes
- `--set` is now highlighted blue+bold in the input field like all other `--` commands
- **Tab completion** cycles through `--set nickname`, `--set autostart`, `--set list`
- Partial input (e.g. `--se`, `--s`) tab-completes to `--set`

#### Nickname sync fix
- `--set nickname <name>` now propagates to the background agent process via a new `TypeSetNickname` IPC frame — chat messages from the student now show the correct nickname instead of the machine hostname

#### Windows 7 compatibility
- Added a **32-bit agent build** (`classsend-agent-win7-x86.exe`) compiled with **Go 1.20.14** — the last Go release to support Windows 7
- Installer auto-detects the OS at install time: Win10+ x64 gets the native 64-bit agent; Win7/8, Win7 x64, and Win10 x86 get the 32-bit legacy agent
- Single unified installer supports **Windows 7 SP1 through Windows 11**

### Fixed
- Blacklist / whitelist list mutation methods (`AddToBlacklist`, `RemoveBlacklistEntry`, `AddToWhitelist`, `RemoveWhitelistEntry`) now broadcast updated state to all connected students immediately after each change
- Mutex deadlock in list mutation methods — explicit `Unlock()` before `broadcastState()` call instead of `defer`
- `TypeShowCast` and `TypeSetNickname` IPC frames now handled in agent's `serveTUI()` loop

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

[Unreleased]: https://github.com/kalotrapezis/ClassSend2/compare/v0.0.2...HEAD
[0.0.2]: https://github.com/kalotrapezis/ClassSend2/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/kalotrapezis/ClassSend2/releases/tag/v0.0.1
