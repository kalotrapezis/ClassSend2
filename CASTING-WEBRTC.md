# Casting v0.0.5-a — replace JPEG-over-TCP with WebRTC

**Status:** design doc, not started.
**Target version:** v0.0.5-a.
**Why:** the v0.0.4 cast pipeline is JPEG-over-TCP at ~30 fps Q85 — no codec compression, no congestion control, no frame dropping under back-pressure, and the same bad-frame pathology that caused the monitoring black-flash. WebRTC is the right tool: hardware-accelerated codecs, adaptive bitrate, native `<video>` decode, "real-time video" semantics (skip rather than queue under load).

## Current pipeline (to be replaced)

| Component | File | Role |
|---|---|---|
| Capture loop | [internal/core/state.go](internal/core/state.go) `StartCasting` | GDI native-resolution capture → JPEG encode in-process |
| Cast server | [internal/network/cast.go](internal/network/cast.go) | `:47821` TCP, latest-frame-only fanout, TCP_NODELAY, 4 MB buffers |
| Wire frame | castserver client write | 4-byte big-endian length + JPEG payload, repeated |
| Cast viewer | [cmd/castviewer/main.go](cmd/castviewer/main.go) | WebView2 host. JS sets `<img src="data:image/jpeg;base64,...">` per frame |
| Signal: start | `CmdStartCast` with `Param = "host:port"` ([state.go:1583](internal/core/state.go:1583)) | Teacher → student, TCP control channel |
| Signal: stop | `CmdStopCast` ([state.go:~1615](internal/core/state.go)) | Teacher → student |

`castviewer.exe` is launched per cast by the agent and `Process.Kill`'d on stop. Wire format and process model unchanged in v0.0.4-b after the WebView2 rewrite.

## Target architecture

```
                      Existing chat TCP (47820)            Existing chat TCP (47820)
                  ───────────────────────────────       ───────────────────────────────
                                 ▲                                     ▲
                                 │ SDP / ICE                           │ SDP / ICE
                                 │ (new TypeCastOffer/                 │
                                 │  TypeCastAnswer/                    │
                                 │  TypeCastICE)                       │
   ┌────────────────────────┐    │                                     │   ┌────────────────────────┐
   │ teacher.exe            │────┘                                     └───│ classsend-agent.exe    │
   │   pion/webrtc          │                                              │   spawns castviewer     │
   │   pion/mediadevices    │                                              │                        │
   │   GDI screen capture ──┼──── VP8 / H.264 video track ───── UDP ───────┼──→ castviewer.exe     │
   │                        │     (RTCPeerConnection)                      │   <video src=          │
   └────────────────────────┘                                              │    MediaStream>        │
                                                                          └────────────────────────┘
```

Key choices:

- **Library:** [`github.com/pion/webrtc/v4`](https://github.com/pion/webrtc) — pure Go, no CGO, MIT-licensed, mature. Already used by major projects.
- **Capture + encode:** [`github.com/pion/mediadevices`](https://github.com/pion/mediadevices) provides `screen.NewScreenSource` for Windows + a software VP8 encoder. Software VP8 at 1080p≈30fps consumes ~25% of one core on a typical laptop — acceptable for teacher PC. For lower CPU, the next iteration can add Windows Media Foundation H.264 via mediadevices' MFT backend.
- **Signaling:** new `MsgType` values `TypeCastOffer`, `TypeCastAnswer`, `TypeCastICE` carrying SDP / ICE-candidate JSON over the existing chat TCP. **No new ports.** The existing TCP server already does newline-delimited JSON; no protocol surgery required.
- **Per-student peer connection:** one `RTCPeerConnection` per connected student. Teacher iterates `app.Server.Students()` on `StartCasting`; for each student creates a PC, adds the video track, sends the offer. Students that join mid-cast also get a fresh offer.
- **NAT/STUN:** LAN-only. Skip STUN/TURN initially — direct host-candidate ICE between teacher and student on the same broadcast domain works without it. If multi-NIC ([MULTI-NIC-BUG.md](MULTI-NIC-BUG.md)) ever needs cross-subnet casting, add a public STUN URL.
- **Cast viewer:** rewrite [cmd/castviewer/main.go](cmd/castviewer/main.go) to load an HTML page with a `<video>` element. The page creates a `RTCPeerConnection`, exchanges SDP/ICE via Go-side `Bind()` callbacks (same pattern as monitoring's `setTopmost`), and calls `video.srcObject = stream` when the track arrives.

## Implementation plan

### Phase 1 — protocol scaffolding (no behavior change)

1. Add to [internal/protocol/protocol.go](internal/protocol/protocol.go):
   ```go
   const (
       TypeCastOffer  MsgType = "CAST_OFFER"   // teacher → student: SDP offer
       TypeCastAnswer MsgType = "CAST_ANSWER"  // student → teacher: SDP answer
       TypeCastICE    MsgType = "CAST_ICE"     // bidirectional: ICE candidate
   )
   type CastSDPPayload struct{ SDP string `json:"sdp"` }
   type CastICEPayload struct{ Candidate string `json:"candidate"`; SDPMid string `json:"sdpMid"`; SDPMLineIndex uint16 `json:"sdpMLineIndex"` }
   ```
2. Add wire-level send/receive helpers in [internal/network/server.go](internal/network/server.go) (`SendCastOffer(studentID, sdp)`, etc.). The existing `Send`/`Conn` plumbing handles JSON framing already.
3. Wire `OnMessage` in core to dispatch the three new types to handlers (initially no-op stubs).

### Phase 2 — teacher side

1. Add `internal/casting/webrtc_teacher.go` (new package):
   - `type Session struct{ pcs map[studentID]*webrtc.PeerConnection; track *webrtc.TrackLocalStaticSample; capture mediadevices.MediaStream }`.
   - `func StartSession(students []StudentInfo, sendOffer SendOfferFn, sendICE SendICEFn) (*Session, error)` — opens screen capture, creates the track once, then per-student creates a PC, adds the track, generates an offer, calls `sendOffer`.
   - `func (s *Session) HandleAnswer(studentID, sdp)` — set remote description.
   - `func (s *Session) HandleICE(studentID, cand)` — add ICE candidate.
   - `func (s *Session) AddStudent(studentID)` — for late joiners.
   - `func (s *Session) Stop()` — close all PCs, stop capture.
2. Replace [internal/core/state.go](internal/core/state.go) `StartCasting`/`StopCasting` to drive `casting.Session` instead of `network.CastServer`. Keep the same TUI/IPC entry points (`^S`, `--t cast`, `CmdStartCast` to spawn castviewer) so the user-visible commands don't move.
3. **Delete** [internal/network/cast.go](internal/network/cast.go) — no more port 47821.

### Phase 3 — student side

1. Rewrite [cmd/castviewer/main.go](cmd/castviewer/main.go):
   - Args change: `-teacher host:port` (the chat server, not a cast-only port). Or better: castviewer reads the agent's IPC for offers, eliminating its own TCP. Either works; the `-teacher` arg is simpler for the v1.
   - The page has `<video id="vid" autoplay muted></video>` (muted is required by browsers for autoplay).
   - JS creates `pc = new RTCPeerConnection({ iceServers: [] })` (LAN, no STUN needed for v1).
   - `pc.ontrack = ev => { vid.srcObject = ev.streams[0]; }`.
   - JS calls `Bind`-exposed Go funcs `sendAnswer(sdp)` and `sendIceCandidate(json)`; Go forwards them as TypeCastAnswer / TypeCastICE on the chat TCP.
   - Conversely, Go has `Bind("waitOffer", ...)` or just `Eval`s `applyOffer(sdp)` when the agent receives a TypeCastOffer.
2. Update [cmd/classsend-agent/syscommands_windows.go](cmd/classsend-agent/syscommands_windows.go) `CmdStartCast` handler: spawn castviewer with `-teacher` instead of `-addr`.

### Phase 4 — cleanup & docs

- Remove the v0.0.4 black-frame guards in castviewer (still present? after this rewrite they're irrelevant — the file is gone).
- Bump version to `0.0.5-a` in [build.bat](build.bat) line 7.
- Update [CHANGELOG.md](CHANGELOG.md) with the v0.0.5-a entry.
- Update [about.md](about.md) and the project memory.
- Delete `cmd/casttest/` (the old JPEG-stream load tester) or rewrite it to drive a synthetic WebRTC session.

## Test plan

1. **Unit:** mock `SendOfferFn` / `SendICEFn` and assert `Session.StartSession` produces a valid offer and accepts a synthetic answer.
2. **Integration without browser:** pion includes a `webrtc.PeerConnection` on both ends — write `cmd/casttest/` v2 that spins up N pion-based "students" against a real teacher session, asserts each receives at least 30 fps for 10 s. No castviewer.exe needed; tests the Go pipeline alone.
3. **End-to-end with browser:** real castviewer.exe on 3 PCs, run a 60-s class, verify smooth playback, verify graceful drop when one PC is unplugged mid-cast. Watch the teacher CPU usage with Task Manager — VP8 software encode at 1080p should sit under 30%.
4. **Adaptive bandwidth:** simulate a slow link with `clumsy.exe` (Windows packet shaper) and verify pion's bandwidth estimator scales the bitrate down rather than freezing.

## Effort estimate

| Phase | Work | Notes |
|---|---|---|
| 1 — protocol | 2 hrs | Pure mechanical: 3 new MsgTypes + payloads + dispatch |
| 2 — teacher | 4-6 hrs | New `internal/casting` package, replace existing StartCasting |
| 3 — student | 4 hrs | New castviewer, IPC plumbing through agent |
| 4 — cleanup | 1 hr | Delete cast.go, update CHANGELOG, build.bat, etc. |
| Testing | 4 hrs | unit + pion-based integration + 3-PC manual |
| **Total** | **~2 days** | |

## Open questions for the implementer

- **Software vs hardware encode.** Pion's `vpx` encoder is software-only. For 1080p × 30 fps × 3 students, software encode is fine (a single track is shared across all PCs — encode once). If we ever broadcast at 4K or to many students, switch to mediadevices' `mft` backend (Windows Media Foundation, hardware H.264).
- **Echo cancellation.** No audio in cast; not relevant.
- **Win7 students.** WebView2 ≠ Win7. The current install matrix already says cast is Win10+. No change.
- **Teacher firewall.** WebRTC ICE host candidates use ephemeral UDP ports. The teacher's Windows Firewall will prompt on first run. Add a `[Run]` entry to the installer that opens the relevant port range, or document the prompt and let the user click "Allow."
- **Resolution.** Captured at native; the encoder downscales if needed. Pion supports `width`/`height` constraints on the screen source — pin to 1280×720 for the v1 to keep CPU predictable; revisit for the v0.6 tier.
- **One peer connection per student vs simulcast.** v1 = per-student. Simulcast (one encode, multiple SVC layers) is a v0.6 optimization; not in scope here.

## Files touched (rough)

- **New:** `internal/casting/webrtc_teacher.go`, `internal/casting/types.go`, `cmd/castviewer/main.go` (rewrite).
- **Modified:** `internal/protocol/protocol.go` (3 types), `internal/network/server.go` (Send helpers + dispatch hooks), `internal/core/state.go` (StartCasting/StopCasting bodies), `cmd/classsend-agent/syscommands_windows.go` (spawn args), `build.bat` (version), `setup/classsend2.iss` (firewall hint?), `CHANGELOG.md`.
- **Deleted:** `internal/network/cast.go`, `cmd/casttest/main.go` (or rewritten).
