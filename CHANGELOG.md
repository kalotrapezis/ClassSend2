# Changelog

All notable changes to ClassSend2 are documented here.  
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).  
ClassSend2 adheres to [Semantic Versioning](https://semver.org/).

---

## [Unreleased]

### Planned
- Custom-built minimal ffmpeg (libx264 + mp4 mux + rawvideo demux only). The currently-bundled BtbN GPL build is statically-linked but full-featured — ~200 MB on disk. A purpose-built minimal would be 10-15 MB; the installer would shrink from 60 MB back toward ~30 MB.
- Optional hardware encoder selection (`h264_nvenc` / `h264_qsv` / `h264_amf`) with auto-detection. v0.0.7 still uses libx264 universally — works everywhere, ~10-15% CPU at 1080p30 on a modest i5.
- Persistent scheduled jobs. v0.2.0 keeps the scheduler entirely in teacher-process memory; jobs are lost on restart. The `Scheduler.List()` / `Add()` API is the surface a future implementation would back with `sched.json` in `DataDir`.
- External watchdog (Task Scheduler entry "at logon + every 5 min, start agent if not running") as a belt-and-braces complement to the in-process load monitor.

---

## [0.2.2] — 2026-05-13

A small UX polish release on top of 0.2.1, plus a rebuild of the Win7 agent so legacy student PCs finally pick up the v0.2.0 system-load safe-mode protection.

### Added

- **Keyboard-shortcut column in the `^T` tools overlay** ([internal/tui/model.go](internal/tui/model.go) `overlayTools`). Each tool entry now shows its global `Ctrl`-shortcut right-aligned in `colTextDim`: `Κλείδωμα οθονών   ^L`, `Σίγαση   ^Z`, `Παρακολούθηση   ^W`, etc. Tools without a global binding (block chat, close apps, shutdown) leave the column blank — the empty slot itself is a signal that those still need the menu. Same `tools` slice now carries the `shortcut` field, so the source of truth stays in one place. Mouse/touch users who lean on the menu gradually internalise the bindings without being told to, in the same spirit as VS Code or Sublime menus.

### Fixed

- **Win7 agent shipped stale.** The `dist/classsend-agent-win7-x86.exe` in the v0.2.0 / 0.2.1 installers was the May-7 build — built **before** [cmd/classsend-agent/load_windows.go](cmd/classsend-agent/load_windows.go) landed (May 13). On Win7 students the load-monitor / safe-mode protection was therefore absent. Resolved by re-running `build-win7.bat`; the new binary is ~51 KB larger and includes the safe-mode code path. `GetSystemTimes` and `GlobalMemoryStatusEx` are both Win7-compatible so the protection works identically on legacy hosts.

### Notes

- No protocol or wire-format change. Existing teacher ↔ student pairings keep working in mixed-version setups; the only user-visible effect is the extra column inside the `^T` panel.

---

## [0.2.1] — 2026-05-13

Polish on top of 0.2.0. The monitoring window now has a one-line keyboard hint bar at the bottom of the grid so the new shortcuts are discoverable without opening help.

### Added

- **Bottom hint strip in `monitoring.exe`** ([cmd/monitoring/main.go](cmd/monitoring/main.go) `paintHintBar`). Renders `^F πλήρης οθόνη   ^T πάντα μπροστά   ^W διακοπή   Esc έξοδος   κλικ: εστίαση` centered in a 22 px strip at the bottom of the window. Visible in both grid and focus modes. Cells (and `hitTestCell`) automatically claim only `winH - hintBarH` so a click on the hint row doesn't accidentally focus a cell. On a window smaller than 44 px tall the bar self-suppresses — pathological case, but worth handling.

### Notes

- Single-file change; no protocol impact. Bumped purely to ship the hint bar with a clean version identity.

---

## [0.2.0] — 2026-05-13

The "lessons can plan ahead" release. Three threads landed together in one classroom-driven session: a scheduling system for tool commands, a system-load safe mode for the agent so an overloaded student PC stops dragging the rest of the grid down, and a handful of UX fixes the user spotted during a real lesson on 11 freshly-installed potato PCs.

### Added

#### Scheduling — `--t … | HH:MM` / `… | :NN`

- **Time-clause on tool commands.** Any `--t <action>` can take a `| <when>` suffix to schedule. Two syntaxes:
  - `| HH:MM` — absolute 24h clock time today (e.g. `--t shutdown >* | 13:15`).
  - `| :NN` — NN minutes from now (e.g. `--t sd | :30`).
- **Lock-with-duration.** `--t lock >* | :15` locks the class **immediately** and schedules the paired unlock at +15 min. The pipe-after-lock semantics differ deliberately from other actions (where `:NN` means "fire at +NN") because "lock the class for the next 15 minutes" is the actual classroom verb. `--t lock >* | 09:30` retains the absolute meaning — lock fires at 09:30 and stays locked.
- **Tab default after bare `|`.** `--t shutdown >* |<Tab>` completes to `… | :3`; `--t lock >* |<Tab>` completes to `… | :15`. One-keystroke "lock the class for 15 minutes."
- **Past-time confirmation flow.** Typing `--t lock >* | 09:00` after 09:00 doesn't silently roll to tomorrow — the TUI surfaces `⚠ Η ώρα 09:00 έχει περάσει σήμερα. Προγραμματισμός για αύριο Wed 09:00; (--y / --n)` and parks the command in `pendingSchedule`. `--y` schedules for tomorrow, `--n` cancels. Any other input also cancels (and runs as-typed — natural "I changed my mind, do this instead" flow).
- **`--sched` family.** `--sched` (or `--schedule` / `--sch`) lists pending jobs with stable IDs (`S1`, `S2`, …). `--sched cancel S1` removes one. The scheduler's `Fire` callback runs `app.SendCommand(…)` exactly as if the teacher had typed it at that moment, then pushes a `⏰ Εκτελέστηκε [S1] …` line to the chat scroll so the firing is visible without opening `--sched`.
- **`^X` overlay** for the same list, styled like `^N` and `^T`: a centred panel with cursor, `↑↓` to navigate, `d` to cancel the highlighted job, `Esc` to close. The typed `--sched` form still works (handy when you only want a quick glance and don't want to lose your input draft); the overlay is for when you've accumulated more pending jobs than you can hold in your head. Greek mnemonic: **χ**ρόνος / μετα**χ**ρονισμένες εντολές.
- **Schedulable actions** are the ones where time-shifting makes sense: `lock`, `unlock`, `mute`, `unmute`, `shutdown`, `close`, `block`, `unblock`, `launch`, `tvon`, `tvoff`. Deliberately excluded: `focus` (a scheduled focus is meaningless), `shot` (one-frame snapshot — no value delayed), and the cast family (binds to the teacher's screen, fragile to schedule).
- **Persistence: none.** Jobs live in `Scheduler.jobs` in the teacher process and die with it. The user explicitly framed this as the foundation for a later schedule manager — see the Unreleased section above for the follow-up.

#### System-load safe mode for the agent

- **Background load monitor** ([cmd/classsend-agent/load_windows.go](cmd/classsend-agent/load_windows.go)). Ticks every 2 s sampling `GetSystemTimes` (system-wide CPU%) and `GlobalMemoryStatusEx` (available physical MB). Maintains an atomic `loadIsCritical` boolean with hysteresis: enters CRITICAL when CPU stays at ≥98% for 6 s **or** available memory drops to ≤150 MB for 4 s; exits when CPU is ≤85% for 6 s **and** memory is ≥300 MB for 4 s. Thresholds are deliberately permissive — the deployment target is 8-year-old classroom PCs that idle at 50-70% CPU under normal load; anything less aggressive would mark them CRITICAL permanently.
- **Capture short-circuit.** When CRITICAL, `CmdRequestShot` in the agent skips the entire `GetDC` / `CreateDIBSection` / `BitBlt` path and sends a `ScreenshotPayload{Status:"load"}` frame with empty `Data` instead. No GPU contention, no JPEG encode, no fighting with whatever's already saturating the box. `captureScreenSized` itself returns `errAgentUnderLoad` as a defense-in-depth shortcut for any future caller.
- **Teacher keeps last good thumbnail.** Receiving a `Status:"load"` frame updates `lastSeen` on the monitoring cell (so it isn't marked offline at the 30 s timer) but **doesn't repaint** — the cell keeps showing whatever the kid's screen looked like before the load spike. Goal per the user's wording: "at the beginning when no other program will run it will send a pic. So we have one, just don't replace it, until the next one." When the PC recovers, the next normal screenshot overwrites the stale frame.
- **Transitions logged, steady state silent.** The agent devlog records `load-monitor: ENTER critical reason=cpu cpu=99% avail=820MB` and `EXIT critical …` per transition, plus `capture: skipped (system under load)` on each shot the agent declined to take. Reviewing a `--bug` zip after a tough lesson will show exactly when each PC went hot and when it cooled.

#### Monitoring window shortcuts

- **`^F` fullscreen**, **`^T` always-on-top**, **`^W` stop monitoring + close**, **`Esc` close** (or leave focus mode if focused) inside the monitoring window ([cmd/monitoring/main.go](cmd/monitoring/main.go) `wmKeydown`). Restores behaviour from the older ClassSend that had been missing in v0.0.4+. Shortcuts are window-level (the monitoring window must have focus) — not global hotkeys, since that would conflict with the teacher TUI's existing `^F` / `^T` / `^W` bindings on the terminal side.
- **`^W` cleans up the teacher session too.** New `MsgStopRequest` back-channel message (msg type 6) — monitoring.exe sends it before posting `WM_CLOSE`. Teacher's `readPipeBackChannel` handles it by closing `stopCh`, which triggers the response router's normal shutdown path (sends `MsgStop`, kills monitoring.exe, fires `onEnded`, clears session state). Previously closing the monitoring window left the teacher polling students for shots that nobody was rendering; now `^W` is a true one-keystroke "I'm done monitoring."

#### Path Notes — pin shortcut

- **`s` in the `^N` overlay pins / unpins the highlighted entry** ([internal/core/favorites.go](internal/core/favorites.go), [internal/tui/model.go](internal/tui/model.go) `favoritesTogglePinSelected`). Pinned entries get a `★` marker, sort above the non-pinned recency block, and are exempt from the 50-entry cap (i.e. pinned classroom apps survive even as the teacher accumulates 50+ recent push-opens). New `FavoriteEntry.Pinned` field is JSON-`omitempty` so on-disk format is unchanged for unpinned entries; legacy `favorites.json` loads without migration.

### Changed

- **`ScreenshotPayload` gains a `Status` field** ([internal/protocol/protocol.go](internal/protocol/protocol.go)). `""` / `"ok"` = a normal screenshot is in `Data` (the existing semantics); `"load"` = the student PC is overloaded and `Data` is empty. The field is JSON-`omitempty` so wire frames from normal screenshots are byte-identical to v0.1.0. Old teachers receiving a `"load"` frame from a new agent would see an empty `Data` byte slice and (depending on path) either skip the cell or paint a black thumbnail — recommend upgrading both sides together for clean behaviour.
- **`OnScreenshot` callback signature** gains a `status string` parameter so the load state can flow through to the monitoring session. Internal API, no protocol implications.
- **Favorites sort algorithm** ([internal/core/favorites.go](internal/core/favorites.go) `sortFavorites` / `trimFavorites`). Was a single `AddedAt`-descending sort with a hard cap on tail; now sorts pinned-first then recency-within-group, and the cap trim drops non-pinned tail before touching pinned. If pinned alone exceed the cap, all pinned are kept (cap is soft w.r.t. pins).

### Fixed

- **Path Notes inserted quoted URL that Windows couldn't open.** Selecting `google.gr` from the `^N` overlay produced `--op "google.gr" >`, which `parsePushOpenCmd` was splitting with `strings.Fields` — leaving the literal `"` characters on the target. The agent then asked Windows to open a protocol named `"google.gr"` and got nothing. Two-part fix: `splitQuoted` parser handles `"…"` as a single token across whitespace boundaries (so paths-with-spaces still survive), and the overlay's `favoritesPlaceSelected` only wraps in `"` when the value actually contains whitespace. Regression test in [internal/tui/push_open_test.go](internal/tui/push_open_test.go) pins 8 cases including the exact `"google.gr"` scenario.

### Notes

- **Protocol compatibility.** v0.2.0 is wire-compatible with v0.1.0 for normal screenshot frames. New `"load"` status frames are interpreted only by v0.2.0 teachers; older teachers fall through to whatever the existing "empty data" path does (typically "skip", which is benign). Mixed-version classrooms work — recommend upgrading the agent + teacher together to get the load-safe-mode behaviour end-to-end.
- **Scheduler is teacher-side only.** Students see scheduled commands arrive as normal command frames at fire time; nothing on the wire indicates "this was scheduled." Same payload, same execution path.
- **Load monitor thresholds live as constants** at the top of `startLoadMonitor` in [cmd/classsend-agent/load_windows.go](cmd/classsend-agent/load_windows.go). If a particular deployment finds the defaults wrong, change them and rebuild — there's no settings.json knob in v0.2.0. The numbers are calibrated for the 11 classroom PCs the user installed this build on in 15 minutes.

---

## [0.1.0] — 2026-05-09

The "command surface cleanup" release. After several months of patch-level fixes (0.0.4 through 0.0.9) the command vocabulary had grown faster than the help could keep up, and the syntax-highlighter was being permissive enough that typos like `--t locc` still rendered blue. This release tightens the input UX, gives every parameterised command a worked example in the help, and bumps to 0.1.0 because the user-visible command surface changed in a coherent way (no protocol changes, no breakage of existing typed commands).

### Added

- **Linux-style short forms for `--t` actions** ([internal/tui/model.go](internal/tui/model.go) `actions` map). Every tool action now accepts a 2-3 letter short alongside its full form: `lk/lock`, `ulk/unlock`, `mu/mute`, `umu/unmute`, `sh/shot`, `cl/close`, `sd/shutdown`, `bl/block`, `ubl/unblock`, `fc/focus`, `ln/launch`. The pre-existing `tvon/start-monitoring`, `tvoff/stop-monitoring`, `cast|caston/start-casting`, `castoff/stop-casting` pairings are unchanged.
- **`--rm` as Linux-convention alias for delete** alongside the pre-existing `--del / --rem / --delete`. Matches user mental model — anyone coming from a shell types `rm` first.
- **Worked examples in `^H` help, per parameterised command.** Every command that takes an argument now shows 1-3 concrete `π.χ.` lines underneath the syntax row, covering the common cases. Most useful at 8 a.m. when a teacher is looking up `--blk @BN` form for the first time in a week.
- **Page-down with Space / PgDn in `^H` help and `^V` about overlays** ([internal/tui/model.go](internal/tui/model.go) `handleKey`). The help is now ~150 lines for the teacher; ↑/↓ still scroll line-by-line, but Space advances 20 lines (one page with 2 lines of overlap). PgUp pages back. Hint added to the ΓΕΝΙΚΕΣ ΣΥΝΤΟΜΕΥΣΕΙΣ section so teachers see it immediately.
- **Role-aware help** ([internal/tui/model.go](internal/tui/model.go) `helpLines` → `helpLinesTeacher` / `helpLinesStudent`). Students no longer see teacher-only sections (tools, push-open, content filtering, path notes). Their help is half the size and shows only commands they can actually use.

### Changed

- **Input syntax highlighting is now grammar-aware** ([internal/tui/model.go](internal/tui/model.go) `looksLikeCommand` + new `validateCmdHead` / `validateToolArgs` / `validateSetArgs`). Previously any line containing a token starting with `--` rendered the whole input blue, including obvious typos like `--t locc` or `--xx`. Now the validator walks the tokens left-to-right: the first `--xxx` must be a known command (or a strict prefix of one), and subsequent tokens must fit the command's grammar where one is enumerable (`--t <action>`, `--set <key>`). Other commands stay permissive (no point validating `@X.Y` references at colour time). Concretely: `--t loc` stays blue (prefix of `lock`), `--t locc` turns plain; `--set ni` stays blue, `--set xx` plain; `--he` blue, `--xx` plain. Free text before the first `--xxx` token is unaffected (so `Διαβάστε σελ.4 --pin` still highlights correctly).
- **Help structured by category** rather than by role-of-action. Sections are now ΕΝΤΟΛΕΣ ΕΡΓΑΛΕΙΩΝ / ΔΙΑΧΕΙΡΙΣΗ ΚΑΙ ΑΠΟΣΤΟΛΗ ΜΗΝΥΜΑΤΩΝ / ΦΙΛΤΡΑΡΙΣΜΑ ΠΕΡΙΕΧΟΜΕΝΟΥ / ΡΥΘΜΙΣΕΙΣ rather than the old "EVERYONE / STUDENT / TEACHER" split. Easier to find a command when you remember what it does but not who can run it.
- **Help short-form column shows the canonical short**, not every Tab-aid prefix. So `--cp / --copy`, not `--c / --cp / --copy` — the row was getting too wide and the single-letter column was misleading anyway (`--c` doesn't execute, only Tab-expands). Short forms documented are exactly the ones that work when typed and Enter'd. `--h` is the only universal single-letter short.
- **Tab cycling on `--t <Tab>`** now lists one canonical name per action ([toolNames](internal/tui/model.go)) rather than every alias. Long forms only (`lock`, `unlock`, `mute`, …) plus the actions where the "short" IS the canonical name (`tvon`, `tvoff`, `cast`, `castoff`). Adding the alphabetic shorts (`lk`, `ulk`, …) to the cycle would have doubled the list without any discoverability gain — anyone who knows `lk` types it directly.

### Fixed

- **`--t start<Tab>` now cycles between `start-monitoring` and `start-casting`.** Was a long-standing oversight: only `start-casting` was in `toolNames`, so `start-monitoring` was reachable only by typing the `tvon` alias. Both are present now.

### Notes

- **No protocol changes.** v0.1.0 teacher and v0.0.9 students (or vice-versa) inter-operate normally. Every typed command that worked before still works — this release only added aliases, never removed any.
- **No installer-side changes.** `setup/classsend2.iss` is untouched apart from the version bump. The teacher / student / dev role matrix is identical to v0.0.9.

---

## [0.0.9] — 2026-05-09

The "actually fix the black thumbnails" release. After a long session of testing on a real Win10 hybrid-graphics laptop (i7-7500U + Intel HD 620 + AMD Radeon 530) we found and fixed three independent bugs that had been compounding to make the monitoring grid look broken on certain PCs. Plus an architectural rewrite of the monitoring poll loop that decouples "asking" from "receiving" — slow PCs no longer drop their frames just because we'd already moved on to the next one.

### Fixed

- **Capture path: switched from `BitBlt` + `GetDIBits` to `BitBlt` + `CreateDIBSection`** ([cmd/classsend-agent/syscommands_windows.go](cmd/classsend-agent/syscommands_windows.go)). On certain Win10 + Intel iGPU drivers, `GetDIBits` returns 0 (failure) when the source bitmap is still selected into a memory DC — leaving our pixel buffer untouched at its initial all-zero state. The agent then JPEG-encoded that zero buffer, producing a valid 4 KB image of pure black that round-tripped faithfully through the rest of the pipeline. The new code allocates the bitmap with `CreateDIBSection`, which maps the pixel memory directly into the process address space; `BitBlt` writes straight into our buffer with no extraction step. No more silent failure, no driver-quirk dependency, no `GetDIBits` call at all. This was the actual root cause of the long-running "Win10 thumbnails are black" symptom — `CAPTUREBLT` in v0.0.8 helped some drivers but couldn't fix this one.
- **Monitoring grid: re-INIT preserves cells by name, not slot index** ([cmd/monitoring/main.go](cmd/monitoring/main.go) `handleInit`). When the student list reordered (any join, leave, or sort change), the GDI monitoring matched cells by position. So if a new student showed up at the front of the list, every subsequent cell's pixels got wiped — the cell would paint blank until that student's next round-robin shot. Symptom: only the most-recently-updated cell ever showed real content; everyone else flashed black after each list change. (Or as it was put memorably during testing: "the RGB lights were turning each other off — I wanted them all to stay on.") Fixed by building a `name → cellState` map and looking each name up, so a student's last good thumbnail follows them wherever they end up in the new layout.
- **Late frames are no longer discarded** ([internal/monitoring/session_windows.go](internal/monitoring/session_windows.go)). The old polling loop synchronously waited for student N's response before moving on; if N's frame arrived after the timeout, it was filtered out as "wrong student" and silently dropped. On slow Win10 PCs this meant every frame was thrown away. Replaced with two decoupled goroutines:
  - **Request pacer** sends one `CmdRequestShot` every 2 s in round-robin order. Sequential, polite to the WiFi, doesn't wait for responses.
  - **Response router** drains the shot channel forever, looking each shot up by hostname and routing it to its cell. Doesn't matter when the frame arrived or whose "turn" it was — every shot lands in the right cell.
- **`SelectObject` cleanup ordering** fixed in the (now-fallback) BitBlt path: original DC selection is restored before deleting the bitmap or DC. Doesn't fix the GetDIBits failure (the deeper issue) but eliminates a separate latent handle leak.

### Added

- **"TECHNICAL DIFFICULTIES" placeholder image.** When the agent's own luma sample shows the BitBlt produced a zero buffer (`luma < 2`), it substitutes a clearly-marked amber-on-black hazard-stripe JPEG with the words "TECHNICAL DIFFICULTIES" across the middle, instead of sending a black thumbnail indistinguishable from "the user has a black wallpaper". Cached after first generation. Includes a tiny embedded 5×7 bitmap font (just the letters needed for that one phrase) so we don't pull in a font dependency.
- **Per-student adaptive backoff in the request pacer.** A student who doesn't respond to two consecutive requests is parked for **10 seconds** before the next one; if they still haven't answered, they're parked for **20 minutes**. The number is calibrated to the reality that classroom Atom/Pentium laptops can take 30 s to open Edge and 60 s to open Compress — there's no point hammering them while they're busy. Any incoming response (even a placeholder) clears the backoff. Round-robin walks past parked students, so 8 active PCs and 1 dead one still get a request every 2 s (cycling through 8).
- **Teacher-side black-frame filter with retry-or-keep-last logic.** When the response router decodes a shot and finds it mostly black, it doesn't paint it AS LONG AS the cell already has a good thumbnail to keep showing. It instead fires a fresh `CmdRequestShot` to that student up to 3 times in a row. After 3 blacks the cell keeps its last good image; the next pacer round will try again naturally. **First-frame black is still painted** — better to show the placeholder cell than a forever-stuck "Αναμονή...".
- **Agent stress guard.** Bounded concurrent command handlers (cap 4) with `recover()` per handler so a single panic can't take the agent down. When the cap is hit, new commands get a `BUSY` ack back to the teacher (showing as a red command failure in chat) instead of silently piling up in unbounded goroutines until a slow PC runs out of resources.
- **Health beacon.** Every 30 s the agent's devlog gets a snapshot: goroutine count, heap size, in-flight commands, BUSY drops, and per-process GDI / USER handle counts. The handle counts are particularly useful for spotting GDI leaks (Win10 caps each at 10 000 per process; we've seen `gdi=13 user=6` in the field, well below the cap, but the beacon would surface a leak immediately if one developed).
- **Per-capture detail logging in the agent** (`capture: w=... h=... luma=... blit=...ms ...`). Records the bitmap dimensions, what the agent's own pixel buffer looked like before encode, time spent in each GDI step, and the encoded JPEG size. The `luma` field alone proved decisive in this session — it ruled out network/decode/pipe issues immediately and pointed straight at the broken `GetDIBits` call.
- **Monitoring "you are being watched" banner now auto-fades** ([cmd/classsend-agent/syscommands_windows.go](cmd/classsend-agent/syscommands_windows.go)). The amber notification on the student PC stays opaque for 4 s, then fades out smoothly over 600 ms (12 alpha steps) and closes itself. Window is now `WS_EX_LAYERED` so the alpha animation actually composites cleanly. Student gets the alert without being permanently nagged for the rest of the lesson.

### Changed

- **Reverted `cmd/monitoring/main.go` to the v0.0.3-era native Win32 GDI implementation** (with the `handleInit` name-lookup fix). The WebView2 monitoring window from v0.0.4 was solving a flicker bug that turned out to be a frame-discard issue at the protocol level — once that was fixed properly via the new async request/response architecture, WebView2's complexity wasn't carrying its weight. The GDI version is ~1100 lines of well-tested Win32, has no extra runtime dependency, and renders the grid identically. WebView2 dependency stays in the codebase for the cast viewer.
- **Monitoring poll cadence is now patient by default.** One request every 2 s per student, full round every ~18 s for 9 students. The grid is a "glance every few seconds" view, not a live feed; if the teacher needs a fresh look at one student they double-click into focus mode, which polls that one student at ~1 fps with hi-res capture.
- **Focus-mode stale-shot drain.** When entering focus or after a focus timeout, any leftover shots from the grid's polling round are flushed from the channel before the first focus request goes out. Without this drain, the first frame the teacher saw in focus could be a low-res grid shot from before the click, and post-timeout could be a 3-s-stale frame served as if fresh.

### Notes

- **Hybrid-graphics workaround:** if a Win10 PC continues to show the "TECHNICAL DIFFICULTIES" placeholder even after this build, that PC has dual GPUs (typical: Intel iGPU + discrete AMD/NVIDIA) and our agent process is being routed to the discrete GPU while DWM composes on the integrated one. Cure: Settings → Display → Graphics → Browse → pick `classsend-agent.exe` → "Power saving" (forces iGPU). Or disable the discrete GPU in Device Manager. Driver updates often help too.

---

## [0.0.8] — 2026-05-07

Classroom-UX release. Driven entirely by feedback from a real lesson: monitoring lost late-joining students, Win10 PCs showed black thumbnails, and `--op` was a two-step typing dance. Three bug fixes plus a set of keyboard-first conveniences (Path Notes, attach→push in two keystrokes, hostname-aware sidebar sort, every overlay's title now spells out its shortcut letter).

### Fixed

- **Monitoring grid no longer freezes on the initial student set** ([internal/monitoring/session_windows.go](internal/monitoring/session_windows.go)). `StartSession` now also returns a `nudge func()`; the polling loop's inter-round and focus-mode sleeps `select` on a `wakeCh` so a join can short-circuit them. `cmd/classsend/main.go` chains a wrapper around `app.OnStudentJoin` that fires the nudge whenever a student joins during an active session — the next round picks up the new student via `studentsChanged`, sends a fresh `MsgInit`, and the WebView grid re-renders preserving existing thumbnails by name. Previously you had to close and re-open monitoring to see anyone who connected after `tvon`.
- **Win10 BitBlt black thumbnails** ([cmd/classsend-agent/syscommands_windows.go:295](cmd/classsend-agent/syscommands_windows.go:295)). Added `CAPTUREBLT` (0x40000000) to the BitBlt raster op (now `srcCopy | captureBlt`). Without it, BitBlt of the desktop DC returns black for DWM-composited / hardware-accelerated content on Win10 even though Win11 typically returns the correct frame. Visible symptom in production: every Win10 cell black, the single Win11 cell showing real content.

### Added

#### Path Notes — saved push-open targets

- New persistent list of recent URLs and attached file paths ([internal/core/favorites.go](internal/core/favorites.go)). Stored in `%APPDATA%\ClassSend\favorites.json`, capped at 50 entries (newest-first), duplicates move-to-front. Survives across sessions.
- Auto-populated by every `PushOpenURL` call and every teacher-side `SendFile` (the absolute path of the attached file gets recorded). No manual "add" step needed.
- **^N** opens the Path Notes overlay. Enter places the highlighted entry into the input as `--op "<value>" >` (incomplete on purpose — the teacher appends `*` or a student number; explicit beats accidental broadcast). `d` / Delete removes the entry without closing the panel.
- Smart truncation when an entry is too wide for the overlay: URLs (`http://`, `https://`, `www.`, `ftp://`) keep their **head** so the protocol + domain stay readable; everything else is treated as a file path and keeps the **tail** so the filename / app name stays visible.
- New `--path` command family for manual control:
  - `--pa` / `--path` / `--path open` — open the overlay
  - `--path save <url-or-path>` — save manually; floats to top because `AddedAt = now`
  - `--path delete <exact-value>` — remove
  - `--path remove <value>` — alias for delete

#### Attach + push-open in one action

- **^A → ^O** flow ([internal/tui/model.go:511](internal/tui/model.go:511)): pick a file via the file picker, then ^O sends it to all students with `AutoOpen=true` in the file header — no typing. Equivalent to typing `--op this > *` after attaching.
- New `--op this > *` / `--op this > N` / `--op > *` / `--op > N` syntax recognised when a file is staged. Sends in one wire transfer with `AutoOpen=true` rather than the old two-step (SendFile + PushOpenFile) which uploaded the file twice.
- `App.SendFile` gains an `autoOpen bool` parameter; chunks are flagged at the header so receiving students open immediately on receipt.

#### Hostname-aware sidebar sort

- The student list now sorts by trailing-digit semantics: `Lab1, Lab2, Lab10` (not the lexicographic `Lab1, Lab10, Lab2`). Same prefix groups numerically; cross-prefix groups alphabetically. So `>1` always points at physically-first PC once you've named hostnames `Lab1..Lab12` or `PC1..PC12`.
- `hostnameLess` + `splitHostNum` helpers in [internal/tui/model.go](internal/tui/model.go), regression-tested in [internal/tui/sort_test.go](internal/tui/sort_test.go). Selection follows the sort by ID, not by index — your highlight stays on the same PC across re-orderings.

#### New keyboard shortcuts (teacher only)

- **^W** — toggle classroom monitoring (tvon/tvoff). Repurposed from the old "focus input" binding.
- **^L** — toggle screen-lock on every student.
- **^Z** — toggle mute on every student.
- **^F** — primes a focus-window command (`--t focus ` ready to type the title). The blacklist/whitelist overlay (was ^L) now lives on **^G** — "Content (G)ate".
- All four toggles read `m.state` so they stay in sync with the authoritative class state, even if you change it via `--t lock` from the command line.

#### `--about` is now a window

- Was: dump 20+ lines into the chat area, push everything older off-screen.
- Is: a centered, bordered, scrollable overlay matching the `--help` style. Built from `buildinfo` (live build string + role + log path) plus `about.md` read at runtime — `about.md` can still be edited post-install without rebuild.
- [about.md](about.md) rewritten with three contact lines at the top (kalotrapezis@gmail.com, https://github.com/kalotrapezis/ClassSend2, https://blogs.sch.gr/goodtable/), the rest trimmed to fit the overlay.

### Changed

- **Every overlay title spells out its shortcut letter** in parens. `(H)elp / Βοήθεια`, `(T)ools — Εργαλεία`, `File (A)ttachment`, `Content (G)ate — Λίστες`, `Path (N)otes`. Discoverable without reading help.
- Help text reorganised: ^W / ^L / ^Z / ^G / ^N / ^F all listed under teacher shortcuts; `--path` family documented in its own block under PUSH-OPEN.
- `monitoring.StartSession` signature changed: returns `(stop, nudge func(), err error)` instead of `(stop, err)`. Single caller updated; non-Windows stub matches.
- `App.SendFile` signature changed: gains a final `autoOpen bool` parameter.

### Compatibility

- **No wire-format change.** v0.0.7 ↔ v0.0.8 mixed deployments still talk to each other — chat, monitoring requests, push-open, lock/mute, casting all keep working across the version skew.
- **The BitBlt fix is student-side, not teacher-side.** `captureScreen` runs inside `classsend-agent.exe` on each student PC, so a Win10 student showing black thumbnails will keep showing black thumbnails until that student's agent is updated to v0.0.8. Updating only the teacher PC will not fix the symptom — re-run `ClassSend2-Setup-v0.0.8.exe` on every Win10 student to roll out the fix.
- All other changes (Path Notes, attach→push shortcuts, hostname sort, overlay rebindings, --about window, monitoring nudge) are teacher-side state machine and TUI; teacher-only upgrade is enough for those.
- Cast pipeline unchanged from v0.0.7 (still bundled ffmpeg → fMP4/H.264).

---

## [0.0.7] — 2026-05-06

Cast is now self-contained: a fresh teacher install can broadcast immediately, no separate ffmpeg installation required. Cost is installer size — 18 MB → 60 MB — but the teacher PC already has plenty of disk to spare and IT admins no longer need a second `winget install` step.

### Added

- **Bundled ffmpeg.exe** in [third_party/ffmpeg/](third_party/ffmpeg/) (BtbN's static GPL build, ~200 MB on disk, ~42 MB inside the installer thanks to LZMA2 ultra64). Powers the H.264 cast pipeline added in v0.0.6. Not committed to git (well above GitHub's 100 MB per-file limit) — [`fetch-ffmpeg.bat`](fetch-ffmpeg.bat) at the repo root pulls the latest BtbN release on demand. The Inno Setup compile step fails loudly if it's missing.
- **"Teacher Screen Casting" component** on the installer's role page. Checked by default for Teacher / Dev installs, automatically disabled (greyed + unchecked) when Student is selected — students decode the H.264 stream natively in WebView2 and don't need ffmpeg. Skipping the component drops the installed footprint by ~200 MB and the installer download by ~42 MB. The checkbox sits below the role selection so the installer page now reads as: pick role → opt out of casting if you don't need it.
- **`fetch-ffmpeg.bat`** at the repo root. Idempotent download + extract of BtbN's GPL build into `third_party/ffmpeg/`. Safe to run on a clean clone; skips silently if the binary is already present. Documented in [third_party/ffmpeg/README.md](third_party/ffmpeg/README.md) including the GPL note.

### Changed

- **`build.bat` warns** if `third_party\ffmpeg\ffmpeg.exe` is missing, pointing the developer at `fetch-ffmpeg.bat`. The Inno Setup step still fails hard on a missing file — the warning is just an earlier signal.
- Installer compresses ~80 s longer because of ffmpeg.exe (LZMA2 ultra64 on a 200 MB binary). Still inside the build budget.

### Compatibility

- **No wire-format change.** v0.0.7 teachers and v0.0.6 students (or the reverse) work identically — the only difference is whether ffmpeg is bundled or pulled in separately.
- Old (≤ 0.0.5) installers / binaries still incompatible at the cast layer — that break landed in v0.0.6.

---

## [0.0.6] — 2026-05-06

The cast pipeline is rewritten from JPEG-per-frame to fragmented MP4 / H.264. Bandwidth drops ~30× at the same perceptual quality (≈ 70 KB/s per viewer at 720p30 vs ≈ 2 MB/s before); end-to-end latency is one to two frames; and Chromium's hardware H.264 decoder inside WebView2 does the work on the student side, so we ship zero new decoder code.

The wire envelope is unchanged (`[4-byte BE size][payload]`) but the payload semantics moved from "one JPEG per frame" to "one fMP4 chunk per call". This is a wire-incompatible bump for cast — old viewers (≤ 0.0.5) will read sizes correctly but feed JPEG-expecting code an `ftyp` box and render nothing useful. Installer ships matched binaries on both sides, so this only matters for mixed-version installs.

### Changed

#### Cast wire layer ([internal/network/cast.go](internal/network/cast.go))
- New `SendFrame(data []byte, kind CastFrameKind)` API. `kind` is one of `FrameInit` (cached, replayed to every new client), `FrameKeyframe` (sync point — every viewer must receive at least one before it can decode), `FrameDelta` (P-frame, only forwarded to in-sync clients).
- Per-client bounded queue (`castQueueDepth=60` ≈ 2 s of headroom). On overflow the slow client is closed rather than dropping bytes mid-GOP — silent corruption is worse than a brief reconnect.
- New clients are held in `acceptLoop` until an init segment is published, then receive the cached init first, then skip delta fragments until the next keyframe. With `-g 30` that wait is ≤ 1 s.
- `CastClient` / `DialCast` removed (dead code since v0.0.4-b).

#### fMP4 box parser ([internal/network/fmp4_box.go](internal/network/fmp4_box.go))
- New file. `readBox` parses one MP4 box (32-bit and 64-bit sizes); `FMP4Splitter` drives a stream of boxes into `(init, false)` then `(media, false)` pairs, each emit being one logical chunk. Skips `free`/`sidx` and other ancillary boxes ffmpeg can emit.
- Eleven unit tests in [fmp4_box_test.go](internal/network/fmp4_box_test.go) and [cast_test.go](internal/network/cast_test.go) cover the happy path, truncated/oversized boxes, init-replay-to-new-client, skip-deltas-until-keyframe, and slow-client kill.

#### Teacher producer ([cmd/classsend/syscommands_teacher_windows.go](cmd/classsend/syscommands_teacher_windows.go))
- Replaced the in-process `image/jpeg` encoder with an `ffmpeg.exe` sidecar. Pipeline: BitBlt → pre-allocated BGRA buffer → ffmpeg stdin → libx264 ultrafast/zerolatency → fMP4 stdout → splitter → `srv.SendFrame`.
- Capture buffer reused across frames — at 1080p that's 8 MB / frame, so 30 fps × allocate-per-frame would have been ~240 MB/s of GC pressure on the old path.
- Three goroutines: capture loop (main), drain stderr (logs ffmpeg diagnostics), parse stdout. Shutdown sequence closes stdin → drains parser → waits 2 s → kills if needed.
- Encoder flags: `-c:v libx264 -preset ultrafast -tune zerolatency -bf 0 -g 30 -keyint_min 30 -profile:v baseline -level 3.1 -pix_fmt yuv420p`. baseline + level 3.1 means MSE codec string `avc1.42E01F`, which Chromium accepts universally.
- Mux flags: `-f mp4 -movflags +empty_moov+default_base_moof+frag_every_frame`. One fragment per frame so the producer can tag keyframes by index (`mediaIdx % castGOP == 0`) rather than parsing `tfhd` sample flags.
- `findFFmpegExe()` falls through `CLASSSEND_FFMPEG` env var → beside the running exe → `PATH`. If nothing matches, cast logs a clear "install ffmpeg" message and waits for stop.

#### Student viewer ([cmd/castviewer/main.go](cmd/castviewer/main.go))
- `<img src="data:image/jpeg;base64,...">` swap replaced by `<video>` + Media Source Extensions. Init segment goes to `applyInit`; media fragments go to `applyFragment`; both decode base64 to `Uint8Array` and append via `sourceBuffer.appendBuffer`.
- Append queue with `PENDING_MAX=120` cap so a stalled `updateend` can't run the page out of memory. `sourceBuffer.mode = 'sequence'` accepts contiguous fragments without timestamp validation.
- Multi-NIC dial logic (v0.0.5-b) unchanged.

#### casttest harness ([cmd/casttest/main.go](cmd/casttest/main.go))
- Rewritten to drive the real ffmpeg pipeline with `-f lavfi -i testsrc=...` instead of generating synthetic JPEG. Verifies the exact code path the teacher uses (encoder, splitter, server, viewer fan-out) without needing a desktop session.
- Verified clean: 2 viewers, 20 s, 30 fps, **0 dropped frames** across 600 fragments / 1160 deliveries / 22 MB total.

### Added

- `CLASSSEND_FFMPEG` environment variable for development — points at any ffmpeg.exe so you don't have to copy one beside `teacher.exe` for every test build.

### Known limitations / runtime requirements

- **`ffmpeg.exe` must be available** on the teacher PC. The v0.0.6 installer does not bundle it (planned for a follow-up release). Quick install: `winget install Gyan.FFmpeg`. Cast is the only feature that needs it; everything else continues to work.
- **Late-joining viewers** (a student reopening the cast viewer mid-stream) wait up to ~1 s for the next keyframe before video appears. This is GOP-bound; halving GOP would halve the wait at the cost of bandwidth.
- **Resolution changes mid-cast** (e.g. plugging in an external monitor while broadcasting) require a cast restart — the encoder is configured with fixed `-s WxH` at startup.

---
- `^1`–`^0` tool shortcut keys
- Persistent teacher daemon (session survives TUI close)
- System tray icon for student agent
- Subnet scan with 30-day network history prioritization
- Restore custom icon for 32-bit builds (generate `resource_386.syso` via `rsrc` or `goversioninfo`)
- Win7 chat TUI — would require downgrading bubbletea/lipgloss to v0.25-era and rewriting `internal/tui/model.go` against the older API, or splitting the TUI into its own go.work module

---

## [0.0.5-b] — 2026-05-06

Two pre-existing classroom bugs: a teacher with two NICs on different subnets could only serve students on `nics[0]`, and the student TUI sometimes stayed on "searching" even though the agent had already connected. Both fixed and covered by regression tests.

### Fixed

#### Multi-NIC teacher
- **`Scanner` advertises the right IP per subnet.** [internal/network/scanner.go](internal/network/scanner.go) used to bake `nics[0].IP` into a single `serverAddr` string at construction. It now holds `serverPort` and computes a per-NIC dial-back address: `scanAll` uses each NIC's IP for that NIC's subnet sweep; `fastPath`/retry use a new pure helper `pickAdvertiseAddr(studentIP, port, nics)` that subnet-matches against `GetLocalNICs()`. Students on the second subnet now receive an IP they can actually route to.
- **`CastServer.LocalAddr()` returns all NIC addresses comma-separated** ([internal/network/cast.go](internal/network/cast.go)). [cmd/castviewer/main.go](cmd/castviewer/main.go) parses the list and dials each in order with a 3 s per-candidate timeout — first success wins. `CmdStartCast.Param` wire format unchanged (free-form string), so no protocol bump. Old castviewer binaries (≤ 0.0.4) on a multi-NIC teacher will fail to dial; the new installer ships the new viewer.
- **`MAC cache` already persists** to `mac_cache.json` and survives reboots, so the new per-IP advertise lookup makes dual-mode networks work across nights for free — `fastPath` probes each cached IP with the right teacher-side NIC IP automatically.
- **Single-NIC cost is essentially zero**: `fastPath`/retry snapshot `GetLocalNICs()` once per call instead of per cached IP, and `LocalAddr()` returns one `"ip:port"` string with no comma.

#### "Agent connected but TUI stuck on searching" race
Diagnosed as two distinct races, both fixed:
- **Agent-side IPC write interleaving.** [cmd/classsend-agent/main.go](cmd/classsend-agent/main.go): `replayHistoryToTUI` and the event hooks (`OnConnected`, `OnDisconnected`, `OnRawMessage`) both wrote JSON+newline frames to the same loopback conn with no serialisation. Concurrent writes could tear each other's frames; the TUI's `bufio.Scanner` then read a half-frame, failed to parse, and silently `continue`'d. Fixed with `tuiWriteMu` — every write to `tuiConn` now goes through `writeToTUI()` which holds that mutex briefly.
- **TUI bootstrap race** (the actual bug witnessed). `app.ConnectViaAgent(conn)` was called *before* `tui.New(app)` set `OnConnected`. The agent's first `TypeConnected` frame from the history replay landed at `ConnectViaAgent`'s switch with `OnConnected==nil` and was silently dropped. Fixed by mirroring the agent's connection state in `App.agentConnected atomic.Bool` (set inside the IPC handler regardless of hook wiring) and exposing it via `IsConnectedToTeacher()`. `tui.New` queries it after wiring hooks and synthesises an `evConnected` if needed — so the TUI never misses the initial transition.
- **Bonus**: `replayHistoryToTUI` re-checks `IsConnected()` at the end and re-emits `TypeConnected` if the agent finished its TCP connect during the replay itself. Closes the TOCTOU between the initial check and the replay actually finishing.

### Added

- **`internal/network/scanner_test.go`** — `TestPickAdvertiseAddr_*` covers the canonical 192.168.1.x / 10.20.2.x scenario, the off-subnet/no-NIC fallbacks, and 127.0.0.1 (dev mode + loopback probes).
- **`cmd/classsend-agent/main_test.go`** — `TestWriteToTUI_NoInterleaving` runs 8 writers × 200 frames concurrently and asserts every line on the receiving side parses as a clean `ipc.Frame`.
- **`internal/core/state_test.go`** — `TestIsConnectedToTeacher_BootstrapRace` verifies that a `TypeConnected` frame arriving before any hook is wired still updates `IsConnectedToTeacher()`. `TestIsConnectedToTeacher_LateOnConnected` verifies the late-hook synthesis path that `tui.New` relies on.

### Build / repo

- Version bumped 0.0.4-b → 0.0.5-b. `set VERSION=` in [build.bat](build.bat) and `MyAppVersion` in [setup/classsend2.iss](setup/classsend2.iss) updated together.
- [MULTI-NIC-BUG.md](MULTI-NIC-BUG.md) updated: the bug analysis is preserved, the file now leads with a "FIXED in v0.0.5-b" header pointing at the implementation.

---

## [0.0.4-b] — 2026-05-05

Cast viewer rewritten on WebView2 to match the monitoring rewrite from 0.0.4-a. Same architectural reasoning: stop fighting GDI's `StretchDIBits` quirks, hand the pixel pipeline to a real browser engine.

### Changed

#### Cast viewer rewritten on WebView2
- **New `castviewer.exe`** ([cmd/castviewer/](cmd/castviewer/)) — standalone WebView2 process. Takes `-addr host:port`, dials the teacher's `CastServer` directly, base64-encodes each JPEG frame and pushes it to the page via `Eval("applyFrame('...')")`. CSS `object-fit: contain` handles letterbox; `<img src=>` swap is flicker-free.
- **Agent now spawns instead of renders.** [cmd/classsend-agent/syscommands_windows.go](cmd/classsend-agent/syscommands_windows.go) lost ~250 lines of Win32 cast code (wndproc, `drawCastFrame`, `runCastViewWindow`, `updateCastFrame`, `castFramePix`/`castFrameW`/`castFrameH` state). `CmdStartCast` now `exec.Command("castviewer.exe", "-addr", ...)`; `CmdStopCast` `Process.Kill`s it. A reaper goroutine clears `castProc` if the viewer exits on its own (TCP closed, X clicked) so the next `StopCast` doesn't try to kill a dead PID.
- **The TCP wire format and `CastServer` are unchanged.** `internal/network/cast.go` and `internal/core/state.go` did not move. An old agent talking to a new teacher (or the reverse) still works for everything except the viewer's UI tech.
- Imports `image/draw` and `classsend/internal/network` removed from the agent (no longer needed). `vkCharF`/`vkCharT` constants and the `castViewProcCB` callback also gone.

### Added

- **`cmd/casttest/`** — drives the cast pipeline end-to-end without a teacher or students. Spins up the project's real `network.NewCastServer`, generates synthetic JPEG frames at a configurable rate, spawns N `castviewer.exe` processes, and watches for early viewer crashes. Verified clean: **3 viewers, 20 fps, 15-second run, 0 dropped frames across 1200 deliveries.**
- **`castviewer.exe` shipped by the installer** for `UseModern64` and `UseModern32` student installs (and Dev). Win7 students don't get a castviewer — WebView2 isn't supported there. The agent's `findCastViewerExe` returns empty on Win7, so `CmdStartCast` is logged and ignored gracefully.

### Build / repo

- `build.bat` now builds `castviewer.exe` (modern x64) and `dist\castviewer-win10-x86.exe` (modern x86).

### Known limitations

- Cast viewer requires WebView2 runtime on the **student** PC, not just the teacher. Win7 students cannot run a cast viewer; they still receive every other teacher command.
- `network.CastClient` / `network.DialCast` in [internal/network/cast.go](internal/network/cast.go) are now unreferenced public API — kept for any future caller that wants the wire format helpers, but no production code uses them.

---

## [0.0.4-a] — 2026-05-04

First release of the 0.0.4 line. Two themes: monitoring is rewritten on top of WebView2, and the install matrix grows from two tiers to three.

### Changed

#### Monitoring rewritten on WebView2
- **`monitoring.exe` is now a thin WebView2 host.** The previous ~1100-line hand-rolled Win32 GDI grid (cached back-buffers, CPU-side BGRA resize, intermittent `StretchDIBits` retries) is gone. `cmd/monitoring/main.go` is now ~500 lines, half of which is the embedded HTML/CSS/JS page. Layout is CSS Grid with `aspect-ratio: 16/9` cells; image elements are reused so each shot is a single `<img src=>` swap that the browser decodes flicker-free.
- **The teacher↔monitoring named-pipe protocol (`MsgInit` / `MsgShot` / `MsgOffline` / `MsgStop` / `MsgFocus`) is byte-for-byte identical** to 0.0.3. `teacher.exe` and `internal/monitoring/session_windows.go` did not change. An old `teacher.exe` will still drive the new `monitoring.exe` and vice versa.
- **Click-to-focus and Esc-to-exit** still work; the click round-trip goes JS → `Bind("onCellClick")` → `MsgFocus` on the same back-channel pipe.
- **Runtime requirement:** Microsoft Edge WebView2. Ships with Windows 11; auto-installed via Edge on Windows 10. Teacher-side only — student PCs are unaffected.
- Adds dependency `github.com/jchv/go-webview2`.

#### Three-tier install matrix
The installer now picks one of three binary tiers based on the detected OS at install time:

| System | Student TUI | Agent | Toolchain |
|---|---|---|---|
| Win10/11 x64 | `student.exe` (64-bit) | `classsend-agent.exe` (64-bit) | Go 1.24 |
| Win10/11 x86 *(new)* | `student-win10-x86.exe` (32-bit, full TUI) | `classsend-agent-win10-x86.exe` | Go 1.24 |
| Win7 / Win8 any | *(none — agent only)* | `classsend-agent-win7-x86.exe` | Go 1.20 |

The `[Code]` block in `setup/classsend2.iss` exposes three predicates — `UseModern64`, `UseModern32`, `UseLegacy32` — and each `[Files]` / `[Icons]` / `[Run]` entry is gated on the right one. Win7 PCs no longer get a broken desktop shortcut to a non-running `classsend.exe`.

### Fixed

- **Student list jitter in the monitoring grid.** `Server.Students()` ([`internal/network/server.go`](internal/network/server.go)) returned students in Go map-iteration order — randomised on every call. The monitoring poll loop's `studentsChanged` compares by index, so it considered the list changed every single tick, re-sent `MsgInit` every tick, and the grid re-shuffled forever even when nobody joined or left. Now sorted by ID.
- **Per-cell screenshot loss on roster changes.** Old `monitoring.exe` preserved a cell's screenshot only when the slot index *and* the name both matched on `MsgInit`. With a real reorder, only one slot would survive. The new `applyInit` in the WebView builds a `name → cell` map first and copies the previous `<img src>` across by name, so a join/leave/rename only affects the changed students.
- **No more black flash on transient comm failures.** A new shot is now applied via `<img src=>`; the browser keeps the previous frame visible until the new JPEG decodes. `applyOffline` tints the cell red but **keeps the last good screenshot** rather than clearing it — no more "show a black box because we missed one poll".

### Added

- **`cmd/fakeagent/`** — synthetic student. Connects directly to the teacher's `:47820`, completes the handshake, replies to `CmdRequestShot` with a generated JPEG (gradient + name + frame counter). Each instance gets its own hue. Use multiple in parallel for full-stack monitoring tests without four real PCs.
- **`cmd/monitortest/`** — drives `monitoring.exe` directly through its named pipe. Spawns the binary, sends `MsgInit` with three fake students, streams shots, **reorders the list every 10 s** (regression test for the slot-vs-name preservation), and **marks one cell offline every 15 s**. Verified clean (zero panics, zero pipe errors) on the v0.0.4-a build.
- **`build-win7.bat`** — produces `dist\classsend-agent-win7-x86.exe` using Go 1.20.14 at `C:\Go120` with `GOOS=windows GOARCH=386`. Backs up `go.mod`, downgrades the `go 1.24.2` directive in-place (Go 1.20's parser rejects three-component versions), builds, then restores. Always restores on exit even if the build fails.
- **`build.bat`** now also produces the Win10 x86 pair (`dist\student-win10-x86.exe`, `dist\classsend-agent-win10-x86.exe`) using the modern toolchain with `GOARCH=386`. Warns if `dist\classsend-agent-win7-x86.exe` is missing from a previous `build-win7.bat` run.

### Build / repo

- `cmd/classsend/resource.syso`, `cmd/classsend-agent/resource.syso`, `cmd/monitoring/resource.syso` were x86-64 COFF only; renamed to `resource_amd64.syso` so Go's filename-based architecture filtering links them only on amd64. 32-bit builds compile cleanly but ship without an embedded icon — see Planned for the follow-up.

### Known limitations

- Win7 students do not get the chat TUI. The agent runs and handles every teacher-side command (lock, mute, monitoring, push-open, autostart, etc.); only the student-side chat window is missing.
- 32-bit builds (`student-win10-x86.exe`, both 32-bit agents) ship without a custom executable icon.
- Monitoring requires the Microsoft Edge WebView2 runtime on the teacher PC (preinstalled on Win11; auto-installed via Edge on Win10).

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

[Unreleased]: https://github.com/kalotrapezis/ClassSend2/compare/v0.0.5-b...HEAD
[0.0.5-b]: https://github.com/kalotrapezis/ClassSend2/compare/v0.0.4-b...v0.0.5-b
[0.0.4-b]: https://github.com/kalotrapezis/ClassSend2/compare/v0.0.4-a...v0.0.4-b
[0.0.4-a]: https://github.com/kalotrapezis/ClassSend2/compare/v0.0.2...v0.0.4-a
[0.0.2]: https://github.com/kalotrapezis/ClassSend2/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/kalotrapezis/ClassSend2/releases/tag/v0.0.1
