# Bug: Multi-NIC teacher only advertises one subnet

**Status:** open as of v0.0.4 (2026-05-06)
**Severity:** any student on a subnet whose teacher NIC is *not* `nics[0]` cannot connect.
**Scope:** chat / monitoring / casting all affected — different fixes needed for each.

## Symptom

Teacher PC has two ethernet adapters on different subnets (e.g. NIC A: 192.168.1.50/24, NIC B: 10.0.0.50/24). Students from one subnet connect fine; students from the other subnet are discovered by the scanner but cannot complete the handshake — they receive an IP they can't route to and dial-back times out.

## Root cause

Most of the network stack already does the right thing:

| Component | File | Behaviour |
|---|---|---|
| Teacher TCP server | [internal/network/server.go:49](internal/network/server.go:49) | `net.Listen("tcp", ":47820")` — all interfaces ✓ |
| Cast server | [internal/network/cast.go:39](internal/network/cast.go:39) | `net.Listen("tcp4", ":47821")` — all interfaces ✓ |
| Student probe listener | [internal/network/probe.go:26](internal/network/probe.go:26) | `:47821` — all interfaces ✓ |
| Subnet scanner | [internal/network/scanner.go:172](internal/network/scanner.go:172) | iterates `GetLocalNICs()`, scans each NIC's subnet in parallel ✓ |

The bug is in **what the teacher advertises** as its server address. Two locations:

### Bug 1 — chat / monitoring serverAddr

[internal/core/state.go:239-243](internal/core/state.go:239):
```go
nics := network.GetLocalNICs()
if len(nics) == 0 {
    return fmt.Errorf("no active network interfaces found")
}
serverAddr := fmt.Sprintf("%s:%d", nics[0].IP.String(), network.ServerPort)
// ...
scanner := network.NewScanner(serverAddr, a.devMode, onFound)
```

`serverAddr` is the IP the **probe payload** tells the student to dial back ([scanner.go:218](internal/network/scanner.go:218): `protocol.Encode(protocol.TypeProbe, protocol.ProbePayload{ServerAddr: s.serverAddr, ...})`). Only `nics[0]`'s IP is encoded. A student on the second subnet receives that IP and can't route to it.

### Bug 2 — cast LocalAddr

[internal/network/cast.go:52-59](internal/network/cast.go:52):
```go
func (s *CastServer) LocalAddr() string {
    nics := GetLocalNICs()
    if len(nics) == 0 {
        return s.ln.Addr().String()
    }
    _, port, _ := net.SplitHostPort(s.ln.Addr().String())
    return net.JoinHostPort(nics[0].IP.String(), port)
}
```

Called from [internal/core/state.go:1584](internal/core/state.go:1584) `StartCasting`. The result is broadcast to all students in `CmdStartCast.Param` ([state.go:1595](internal/core/state.go:1595)). Single address, so even if (1) is fixed, students on the second subnet still get the wrong cast IP.

## Fix

### Fix for chat / monitoring (the easy one)

The scanner already iterates NICs at [internal/network/scanner.go:156-185](internal/network/scanner.go:156). Make `Scanner.serverAddr` a function-of-NIC instead of a constant string.

Concretely:
1. Change [internal/network/scanner.go:25](internal/network/scanner.go:25) `serverAddr string` → drop it (or keep as a fallback).
2. Change `NewScanner` signature to take `serverPort int` instead of `serverAddr string`.
3. In [scanner.go:172-183](internal/network/scanner.go:172) (the `for _, nic := range nics` loop in `scanAll`), build the per-NIC `serverAddr` as `fmt.Sprintf("%s:%d", nic.IP.String(), serverPort)`. Pass it into `probe()` so each student receives the IP of the NIC it's being probed from.
4. Update `Scanner.probe()` ([scanner.go:207](internal/network/scanner.go:207)) to take `serverAddr` as a parameter (or stash it on a per-call struct) instead of reading `s.serverAddr`.
5. Update [internal/core/state.go:243](internal/core/state.go:243) caller — pass `network.ServerPort` and remove the `nics[0]` line.
6. **Fast-path** at [scanner.go:135-153](internal/network/scanner.go:135) (`fastPath`): the cache stores `IPHistory` per MAC. To send the right `serverAddr` here, look up which NIC's subnet contains the student IP and use that NIC's IP. Helper:

   ```go
   func (s *Scanner) advertiseAddrFor(studentIP string) string {
       ip := net.ParseIP(studentIP)
       for _, nic := range GetLocalNICs() {
           ipnet := &net.IPNet{IP: nic.IP.Mask(nic.Mask), Mask: nic.Mask}
           if ipnet.Contains(ip) {
               return net.JoinHostPort(nic.IP.String(), strconv.Itoa(s.serverPort))
           }
       }
       // Fallback: first NIC (existing behaviour)
       nics := GetLocalNICs()
       if len(nics) > 0 {
           return net.JoinHostPort(nics[0].IP.String(), strconv.Itoa(s.serverPort))
       }
       return ""
   }
   ```

### Fix for cast (harder)

`CmdStartCast.Param` is a single string carrying the cast server address. For multi-NIC two options:

**Option A — per-student CmdStartCast.** Don't broadcast; iterate connected students and send each one a `CmdStartCast` with the cast address on the NIC matching that student's subnet.

- Where: [internal/core/state.go:1583-1601](internal/core/state.go:1583) `StartCasting()`. Replace the `Broadcast(msg)` call with a loop over `app.Server.Students()`, computing the right `serverAddr` per student via the same `advertiseAddrFor(student.IP)` helper.
- The `CastServer` itself doesn't change; `LocalAddr()` becomes obsolete (delete it or keep as a default for single-NIC).
- Pro: no wire-format change. Old students still work.
- Con: a student that joins mid-cast has to be told the right address — the existing "send current cast state on join" path needs the same per-student logic. Look for `CmdStartCast` in [internal/core/state.go](internal/core/state.go) and the `OnJoin` handler.

**Option B — list of addresses.** Change `CmdStartCast.Param` to a comma-separated list (`"192.168.1.50:47821,10.0.0.50:47821"`); have the cast viewer try each in order.

- Where: [cmd/castviewer/main.go](cmd/castviewer/main.go) `streamLoop()` — wrap the `net.DialTimeout` in a loop over the parsed list. First successful dial wins.
- The teacher emits all NIC IPs, doesn't need to know which subnet each student is on.
- Pro: simpler teacher-side; resilient to NIC changes during a session.
- Con: needs castviewer.exe rebuild. Old castviewer.exes (v0.0.4) would parse the first IP only — if it's the unreachable one, they fail. Acceptable for a v0.0.5 bump.

**Recommendation:** Option B. It's the simpler fix and the cost is just rebuilding castviewer.exe; a fresh installer ships it anyway.

## Test plan

A test harness for multi-NIC needs two real subnets or virtual ones (Hyper-V Internal switches, two npcap loopback adapters, etc.). For a quick check without that:

1. **Static unit test:** add a test that asserts `Scanner.advertiseAddrFor(ip)` picks the right NIC for a synthetic NIC list. No network needed.
2. **End-to-end:** add a `--fake-nic 10.0.0.0/24` flag to [cmd/fakeagent/main.go](cmd/fakeagent/main.go) that makes the agent send a synthetic IP in the `IP` field, and assert the teacher's `serverAddr` advertised back uses the matching NIC. Could be folded into the existing fakeagent — connect, complete handshake, capture the cast address received via `CmdStartCast`.
3. **Manual:** real two-NIC setup with one student per subnet. Verify both join the chat and both see the cast.

## Files touched (rough estimate)

- [internal/network/scanner.go](internal/network/scanner.go) — ~30 lines (per-NIC serverAddr, helper, signature change)
- [internal/core/state.go](internal/core/state.go) — ~10 lines at the `NewScanner` callsite + ~15 in `StartCasting` if Option A; ~5 lines for Option B
- [internal/network/cast.go](internal/network/cast.go) — Option A: deprecate `LocalAddr`; Option B: no change
- [cmd/castviewer/main.go](cmd/castviewer/main.go) — Option B only: ~20 lines for the comma-separated dial loop
- [internal/protocol/protocol.go](internal/protocol/protocol.go) — no change (Param is already a free-form string)

Estimated effort: half a day for Option A on chat/monitoring; another half-day for Option B on cast + multi-NIC test plan + manual verification.

## Notes for the implementer

- `network.PrimaryMAC()` uses the first NIC's MAC for the student identity. That's **correct** to keep — the MAC is just an identity token; it doesn't have to come from the NIC the student is currently using.
- `GetLocalNICs()` already filters loopback and IPv6, so the per-NIC iteration is safe.
- The fast-path scanner ([scanner.go:135](internal/network/scanner.go:135)) probes cached IPs out of subnet order — that's why the per-NIC parameter doesn't work there and the IP-→-NIC lookup is needed.
- Don't forget `--dev` mode at [scanner.go:160-170](internal/network/scanner.go:160) which probes own IPs and `127.0.0.1`. Loopback isn't on any "real" NIC; advertise `127.0.0.1` as the serverAddr for those probes.
