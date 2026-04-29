package network

import (
	"fmt"
	"net"
	"time"

	"classsend/internal/protocol"
)

// ProbeListener runs on student machines — waits for teacher CLASS_HERE probes
type ProbeListener struct {
	listener     net.Listener
	GetHandshake func() protocol.HandshakePayload // called each time to get current nickname etc.
	OnProbe      func(serverAddr string)          // called when teacher is found
}

func NewProbeListener(getHS func() protocol.HandshakePayload, onProbe func(string)) *ProbeListener {
	return &ProbeListener{
		GetHandshake: getHS,
		OnProbe:      onProbe,
	}
}

func (p *ProbeListener) Start() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", ProbePort))
	if err != nil {
		return fmt.Errorf("probe listener on :%d: %w", ProbePort, err)
	}
	p.listener = ln
	go p.acceptLoop()
	return nil
}

func (p *ProbeListener) acceptLoop() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(conn)
	}
}

func (p *ProbeListener) handle(raw net.Conn) {
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(ProbeTimeout))

	c := protocol.NewConn(raw)

	msg, err := c.Recv()
	if err != nil || msg.Type != protocol.TypeProbe {
		return
	}

	probe, err := protocol.Decode[protocol.ProbePayload](msg)
	if err != nil || probe.ServerAddr == "" {
		return
	}

	// Reply with our identity so teacher knows who we are before we connect back
	hs := p.GetHandshake()
	reply, err := protocol.Encode(protocol.TypeHandshake, hs)
	if err != nil {
		return
	}
	c.Send(reply)

	// Notify core — it will connect back to the teacher's server
	if p.OnProbe != nil {
		p.OnProbe(probe.ServerAddr)
	}
}

func (p *ProbeListener) Stop() {
	if p.listener != nil {
		p.listener.Close()
	}
}
