package network

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const CastPort = 47821

// CastFrameKind classifies a payload coming through SendFrame.
//
// The cast pipeline produces fragmented MP4 (fMP4) bytes from an H.264 encoder.
// The first chunk is the init segment (ftyp + moov boxes); subsequent chunks
// are media fragments (moof + mdat). Each media fragment is either anchored on
// a keyframe (an IDR-bearing GOP boundary that a fresh decoder can start from)
// or a delta fragment that depends on the preceding fragments back to the most
// recent keyframe.
//
// CastServer routes by kind: init is cached and replayed to every new client;
// keyframe fragments serve as join points for clients that haven't synced yet;
// delta fragments are skipped for not-yet-synced clients (they would just
// produce decode errors).
type CastFrameKind int

const (
	FrameInit CastFrameKind = iota
	FrameKeyframe
	FrameDelta
)

// CastServer streams fMP4 fragments to connected student clients over TCP.
//
// One producer goroutine on the teacher hands fragments to SendFrame; each
// connected student has its own send goroutine pulling from a bounded queue.
// A slow student that fills its queue is closed (rather than dropping bytes
// out of an H.264 stream, which would corrupt the decoder until the next IDR).
// The producer never blocks waiting for a student.
type CastServer struct {
	ln           net.Listener
	mu           sync.RWMutex
	clients      map[*castConn]struct{}
	closed       chan struct{}
	initSegment  atomic.Value // []byte — last seen init (ftyp+moov), replayed to new clients
}

type castConn struct {
	conn   net.Conn
	queue  chan []byte // bounded; full → close conn
	done   chan struct{}
	once   sync.Once
	inSync atomic.Bool // set on first keyframe forwarded; deltas are skipped until then
	sent   atomic.Int64
	drops  atomic.Int64
}

const (
	// castQueueDepth bounds per-client buffering. At 30 fps with one fragment
	// per frame, 60 ≈ 2 seconds of headroom — enough to ride out a brief
	// network stall, short enough that a genuinely-slow client gets dropped
	// quickly rather than building latency.
	castQueueDepth = 60

	// castMaxFrame is a safety cap on a single payload. fMP4 fragments at
	// 1080p30 ultrafast typically run 5-200 KB; 20 MB leaves wide margin.
	castMaxFrame = 20 * 1024 * 1024
)

func NewCastServer() (*CastServer, error) {
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", CastPort))
	if err != nil {
		return nil, fmt.Errorf("cast server: %w", err)
	}
	s := &CastServer{
		ln:      ln,
		clients: make(map[*castConn]struct{}),
		closed:  make(chan struct{}),
	}
	go s.acceptLoop()
	return s, nil
}

// LocalAddr returns the address(es) students should dial to receive the cast.
//
// On a single-NIC teacher this is "LAN_IP:port". On a multi-NIC teacher, all
// NIC IPs are returned comma-separated: "192.168.1.50:47821,10.0.0.50:47821".
// Each student tries them in order; only the one on its own subnet succeeds.
// The CmdStartCast.Param wire field is a free-form string, so this fits
// without a protocol bump — but castviewer.exe must know to split on commas.
func (s *CastServer) LocalAddr() string {
	nics := GetLocalNICs()
	if len(nics) == 0 {
		return s.ln.Addr().String()
	}
	_, port, _ := net.SplitHostPort(s.ln.Addr().String())
	addrs := make([]string, 0, len(nics))
	for _, nic := range nics {
		addrs = append(addrs, net.JoinHostPort(nic.IP.String(), port))
	}
	return strings.Join(addrs, ",")
}

// ClientCount returns the number of currently connected students.
func (s *CastServer) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// SendFrame routes a payload to all eligible clients.
//
// kind == FrameInit: the bytes are stored as the init segment and replayed to
// every future client on connect; existing clients have already seen one and
// don't need it resent (init segments don't change mid-stream in our pipeline).
//
// kind == FrameKeyframe: queued for every client. Out-of-sync clients flip to
// in-sync on this fragment.
//
// kind == FrameDelta: queued only for already-in-sync clients. Out-of-sync
// clients silently skip.
//
// Returns counts only for media fragments (init returns 0, 0).
func (s *CastServer) SendFrame(data []byte, kind CastFrameKind) (delivered, dropped int) {
	if kind == FrameInit {
		// Copy because the caller may reuse the buffer.
		buf := make([]byte, len(data))
		copy(buf, data)
		s.initSegment.Store(buf)
		return 0, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for cc := range s.clients {
		if !cc.inSync.Load() {
			if kind != FrameKeyframe {
				continue
			}
			cc.inSync.Store(true)
		}
		select {
		case cc.queue <- data:
			delivered++
		default:
			// Queue saturated → this client is too slow. Closing the conn
			// is preferable to dropping bytes mid-GOP (which would corrupt
			// the decoder until the next IDR anyway).
			cc.kill()
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
	select {
	case <-s.closed:
		return // already closed
	default:
	}
	close(s.closed)
	s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for cc := range s.clients {
		cc.kill()
	}
	s.clients = make(map[*castConn]struct{})
}

func (s *CastServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}

		// Hold the conn until the encoder has produced an init segment.
		// Without it the client cannot start decoding. We give up after a
		// reasonable wait — the producer should be fast on cast start.
		init := s.waitForInit(5 * time.Second)
		if init == nil {
			conn.Close()
			continue
		}

		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
			tc.SetWriteBuffer(4 * 1024 * 1024)
		}
		cc := &castConn{
			conn:  conn,
			queue: make(chan []byte, castQueueDepth),
			done:  make(chan struct{}),
		}
		// Pre-load init segment so it's the first thing on the wire.
		cc.queue <- init

		s.mu.Lock()
		s.clients[cc] = struct{}{}
		s.mu.Unlock()
		go s.serveClient(cc)
	}
}

// waitForInit polls the cached init segment until set or timeout. We don't
// use a sync.Cond because init lands at most once per cast session and the
// poll cost is negligible.
func (s *CastServer) waitForInit(timeout time.Duration) []byte {
	deadline := time.Now().Add(timeout)
	for {
		if v := s.initSegment.Load(); v != nil {
			return v.([]byte)
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-s.closed:
			return nil
		case <-time.After(50 * time.Millisecond):
		}
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
		case chunk, ok := <-cc.queue:
			if !ok {
				return
			}
			cc.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if err := castWriteFrame(w, chunk); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}
			cc.sent.Add(1)
		}
	}
}

// kill marks a client for shutdown. Idempotent — safe to call from both the
// producer side (queue full) and Close (server shutdown).
func (cc *castConn) kill() {
	cc.once.Do(func() {
		close(cc.done)
		cc.conn.Close()
	})
}

// ── Wire format: [4-byte big-endian uint32 size][size bytes payload] ──────────
//
// Same framing as v0.0.5 (which carried JPEG). The payload semantics changed
// to fMP4 chunks in v0.0.6 — old viewers will read sizes correctly but feed
// JPEG-expecting code a moov box, which breaks visibly. The version bump is
// intentional.

func castWriteFrame(w *bufio.Writer, data []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// CastReadFrame reads one length-prefixed payload from r. Exported for
// castviewer (which uses raw net.Dial) and casttest.
func CastReadFrame(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(hdr[:])
	if size == 0 {
		return nil, fmt.Errorf("cast: zero-length frame")
	}
	if size > castMaxFrame {
		return nil, fmt.Errorf("cast: frame too large (%d bytes)", size)
	}
	data := make([]byte, size)
	_, err := io.ReadFull(r, data)
	return data, err
}
