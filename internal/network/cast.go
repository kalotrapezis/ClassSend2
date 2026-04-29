package network

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const CastPort = 47821

// CastServer streams JPEG frames to connected student clients over dedicated TCP.
//
// Design: one capture goroutine on the teacher produces frames; each student has
// its own send goroutine.  A per-client latest-frame-only slot (atomic.Value +
// buffered-1 notify channel) means slow students automatically drop intermediate
// frames rather than building up a queue.  The teacher never blocks waiting for
// a student.
type CastServer struct {
	ln      net.Listener
	mu      sync.RWMutex
	clients map[*castConn]struct{}
}

type castConn struct {
	conn   net.Conn
	latest atomic.Value  // stores []byte — always the most recent frame
	notify chan struct{}  // cap 1: "new frame ready"
	done   chan struct{}  // closed by CastServer.Close()
	sent   atomic.Int64
	drops  atomic.Int64
}

func NewCastServer() (*CastServer, error) {
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", CastPort))
	if err != nil {
		return nil, fmt.Errorf("cast server: %w", err)
	}
	s := &CastServer{
		ln:      ln,
		clients: make(map[*castConn]struct{}),
	}
	go s.acceptLoop()
	return s, nil
}

// LocalAddr returns "LAN_IP:port" — the address students should connect to.
func (s *CastServer) LocalAddr() string {
	nics := GetLocalNICs()
	if len(nics) == 0 {
		return s.ln.Addr().String()
	}
	_, port, _ := net.SplitHostPort(s.ln.Addr().String())
	return net.JoinHostPort(nics[0].IP.String(), port)
}

// ClientCount returns the number of currently connected students.
func (s *CastServer) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// SendFrame delivers a JPEG frame to all connected students.
// Returns (delivered, dropped) counts for this call.
func (s *CastServer) SendFrame(frame []byte) (delivered, dropped int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for cc := range s.clients {
		cc.latest.Store(frame)
		select {
		case cc.notify <- struct{}{}:
			delivered++
		default:
			// notify slot already occupied — serveClient will re-read latest
			dropped++
			cc.drops.Add(1)
		}
	}
	return
}

// DrainStats returns cumulative (sent, dropped) and resets the counters.
func (s *CastServer) DrainStats() (sent, dropped int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for cc := range s.clients {
		sent += cc.sent.Swap(0)
		dropped += cc.drops.Swap(0)
	}
	return
}

func (s *CastServer) Close() {
	s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for cc := range s.clients {
		close(cc.done)
		cc.conn.Close()
	}
	s.clients = make(map[*castConn]struct{})
}

func (s *CastServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
			tc.SetWriteBuffer(4 * 1024 * 1024)
		}
		cc := &castConn{
			conn:   conn,
			notify: make(chan struct{}, 1),
			done:   make(chan struct{}),
		}
		s.mu.Lock()
		s.clients[cc] = struct{}{}
		s.mu.Unlock()
		go s.serveClient(cc)
	}
}

func (s *CastServer) serveClient(cc *castConn) {
	defer func() {
		s.mu.Lock()
		delete(s.clients, cc)
		s.mu.Unlock()
		cc.conn.Close()
	}()

	w := bufio.NewWriterSize(cc.conn, 4*1024*1024)
	for {
		select {
		case <-cc.done:
			return
		case <-cc.notify:
			v := cc.latest.Load()
			if v == nil {
				continue
			}
			frame := v.([]byte)
			// 2-second deadline per frame — a slow student is skipping frames
			// but we still give TCP time to drain the socket buffer
			cc.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if err := castWriteFrame(w, frame); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}
			cc.sent.Add(1)
		}
	}
}

// ── CastClient ────────────────────────────────────────────────────────────────

// CastClient connects to the teacher's CastServer and delivers frames via OnFrame.
type CastClient struct {
	conn    net.Conn
	OnFrame func([]byte) // called for each received JPEG frame
}

func DialCast(serverAddr string) (*CastClient, error) {
	conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetReadBuffer(4 * 1024 * 1024)
	}
	return &CastClient{conn: conn}, nil
}

// Run blocks, reading frames until the connection closes or Close is called.
func (c *CastClient) Run() {
	defer c.conn.Close()
	r := bufio.NewReaderSize(c.conn, 4*1024*1024)
	for {
		frame, err := castReadFrame(r)
		if err != nil {
			return
		}
		if c.OnFrame != nil {
			c.OnFrame(frame)
		}
	}
}

func (c *CastClient) Close() { c.conn.Close() }

// ── Wire format: [4-byte big-endian uint32 size][size bytes JPEG] ─────────────

func castWriteFrame(w *bufio.Writer, data []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func castReadFrame(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(hdr[:])
	if size > 20*1024*1024 { // 20 MB safety cap
		return nil, fmt.Errorf("cast: frame too large (%d bytes)", size)
	}
	data := make([]byte, size)
	_, err := io.ReadFull(r, data)
	return data, err
}
