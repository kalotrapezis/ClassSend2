package core

import (
	"net"
	"testing"
	"time"

	"classsend/internal/ipc"
)

// TestIsConnectedToTeacher_BootstrapRace verifies that if a TypeConnected frame
// arrives via the agent IPC pipe BEFORE the TUI has wired OnConnected, the App
// still reports IsConnectedToTeacher()=true. Pre-fix the frame would land in
// ConnectViaAgent's switch with OnConnected=nil and the event was silently
// dropped — the TUI then stayed on "searching" until the user restarted it.
//
// This is a regression test for the witnessed-once "agent connected but TUI
// not updated" bug.
func TestIsConnectedToTeacher_BootstrapRace(t *testing.T) {
	app := &App{Role: RoleStudent}

	// Pipe representing the agent ↔ TUI IPC link. We write from the "agent"
	// side; ConnectViaAgent reads on the "tui" side.
	agentSide, tuiSide := net.Pipe()
	defer agentSide.Close()
	defer tuiSide.Close()

	// Note: OnConnected is NOT set yet — this is the racy state.
	app.ConnectViaAgent(tuiSide)

	// Agent writes TypeConnected to mimic the replay-on-dial flow.
	if err := ipc.WriteFrame(agentSide, ipc.Frame{Type: ipc.TypeConnected}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The reader goroutine processes the frame and updates agentConnected.
	// Poll briefly — net.Pipe is synchronous so the write returns only after
	// the reader has consumed it, but the dispatch happens in the goroutine.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if app.IsConnectedToTeacher() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !app.IsConnectedToTeacher() {
		t.Fatal("IsConnectedToTeacher() = false after TypeConnected; bootstrap state lost")
	}

	// Disconnect should flip the flag back.
	if err := ipc.WriteFrame(agentSide, ipc.Frame{Type: ipc.TypeDisconnected}); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !app.IsConnectedToTeacher() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if app.IsConnectedToTeacher() {
		t.Fatal("IsConnectedToTeacher() = true after TypeDisconnected")
	}
}

// TestIsConnectedToTeacher_LateOnConnected verifies the same property under
// a delayed-hook pattern: hook is registered AFTER the frame arrives, and the
// caller is expected to consult IsConnectedToTeacher() to bootstrap. This is
// what tui.New does in the production code path.
func TestIsConnectedToTeacher_LateOnConnected(t *testing.T) {
	app := &App{Role: RoleStudent}
	agentSide, tuiSide := net.Pipe()
	defer agentSide.Close()
	defer tuiSide.Close()

	app.ConnectViaAgent(tuiSide)
	_ = ipc.WriteFrame(agentSide, ipc.Frame{Type: ipc.TypeConnected})

	// Wait for the dispatch goroutine to land the state.
	time.Sleep(50 * time.Millisecond)

	// Now wire the hook. In the original bug, the TypeConnected event was
	// already lost — this hook would only fire on a future reconnect.
	fired := make(chan struct{}, 1)
	app.OnConnected = func() { fired <- struct{}{} }

	// The TUI's contract: after wiring hooks, query state and synthesise the
	// event if needed. That's what tui.New does. Simulate it here.
	if app.IsConnectedToTeacher() {
		app.OnConnected()
	}

	select {
	case <-fired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OnConnected never fired despite IsConnectedToTeacher()==true")
	}
}
