// Package ipc handles communication between the student agent process and the TUI.
// The agent runs hidden in the background; the TUI connects to it via local TCP loopback.
package ipc

import (
	"bufio"
	"encoding/json"
	"net"
)

// AgentAddr is the loopback address the agent listens on.
const AgentAddr = "127.0.0.1:14789"

// Frame is the message unit on the IPC connection (newline-delimited JSON).
type Frame struct {
	Type string          `json:"t"`
	Data json.RawMessage `json:"d,omitempty"`
}

const (
	TypeConnected    = "connected"    // agent → TUI: connected to teacher
	TypeDisconnected = "disconnected" // agent → TUI: lost connection to teacher
	TypeForward      = "fwd"          // agent → TUI: raw protocol.Message forwarded from teacher
	TypeSend         = "send"         // TUI → agent: send a chat message
)

// SendPayload is the data field for TypeSend frames.
type SendPayload struct {
	Text string `json:"text"`
}

// Listen creates the agent's TCP listener.
func Listen() (net.Listener, error) {
	return net.Listen("tcp", AgentAddr)
}

// Dial connects the TUI to the agent.
func Dial() (net.Conn, error) {
	return net.DialTimeout("tcp", AgentAddr, 0) // fast fail if agent not running
}

// WriteFrame writes a frame to a connection (newline-terminated JSON).
func WriteFrame(conn net.Conn, f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = conn.Write(b)
	return err
}

// ReadFrames reads frames from a connection and sends them to the returned channel.
// The channel is closed when the connection drops.
func ReadFrames(conn net.Conn) <-chan Frame {
	ch := make(chan Frame, 64)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 4<<20), 4<<20) // 4 MB — covers file chunks
		for scanner.Scan() {
			var f Frame
			if json.Unmarshal(scanner.Bytes(), &f) == nil {
				ch <- f
			}
		}
	}()
	return ch
}
