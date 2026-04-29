package network

import (
	"fmt"
	"net"
	"sync"
	"time"

	"classsend/internal/protocol"
)

// Client runs on the student machine — maintains the persistent connection to the teacher server
type Client struct {
	mu        sync.Mutex
	conn      *protocol.Conn
	StudentID string
	connected bool

	OnMessage    func(protocol.Message)
	OnDisconnect func()
}

func NewClient() *Client {
	return &Client{}
}

// Connect dials the teacher server, performs handshake, starts the read loop
func (c *Client) Connect(serverAddr string, hs protocol.HandshakePayload) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return fmt.Errorf("already connected")
	}

	raw, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", serverAddr, err)
	}

	conn := protocol.NewConn(raw)

	msg, err := protocol.Encode(protocol.TypeHandshake, hs)
	if err != nil {
		raw.Close()
		return err
	}
	if err := conn.Send(msg); err != nil {
		raw.Close()
		return fmt.Errorf("send handshake: %w", err)
	}

	raw.SetDeadline(time.Now().Add(5 * time.Second))
	reply, err := conn.Recv()
	if err != nil || reply.Type != protocol.TypeAck {
		raw.Close()
		return fmt.Errorf("no ack from server")
	}
	raw.SetDeadline(time.Time{})

	ack, err := protocol.Decode[protocol.AckPayload](reply)
	if err != nil || !ack.Accepted {
		raw.Close()
		return fmt.Errorf("connection rejected: %s", ack.Reason)
	}

	c.conn = conn
	c.StudentID = ack.StudentID
	c.connected = true

	go c.readLoop()
	return nil
}

func (c *Client) readLoop() {
	for {
		msg, err := c.conn.Recv()
		if err != nil {
			c.mu.Lock()
			c.connected = false
			c.mu.Unlock()

			if c.OnDisconnect != nil {
				c.OnDisconnect()
			}
			return
		}
		if c.OnMessage != nil {
			c.OnMessage(msg)
		}
	}
}

// Send sends a message to the teacher
func (c *Client) Send(msg protocol.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.conn == nil {
		return fmt.Errorf("not connected")
	}
	return c.conn.Send(msg)
}

func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *Client) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.connected = false
	}
}
