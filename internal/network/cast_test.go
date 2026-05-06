package network

import (
	"bufio"
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// dialAndDrain dials the cast server and reads frames into the provided slice
// under mu until the connection closes. Returns the goroutine's "done" channel
// so the test can wait for clean shutdown.
func dialAndDrain(t *testing.T, addr string, mu *sync.Mutex, frames *[][]byte) chan struct{} {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		r := bufio.NewReaderSize(conn, 4*1024*1024)
		for {
			frame, err := CastReadFrame(r)
			if err != nil {
				return
			}
			mu.Lock()
			*frames = append(*frames, frame)
			mu.Unlock()
		}
	}()
	return done
}

// drained spins until len(*frames) == n or until timeout. Returns the count
// observed at exit.
func drained(mu *sync.Mutex, frames *[][]byte, n int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(*frames)
		mu.Unlock()
		if got >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return len(*frames)
}

// TestCastServer_NewClientReceivesInitFirst is the core invariant: a client
// connecting after the encoder has emitted its init segment must receive that
// init segment as the first thing on the wire, before any media fragments.
// Without this MSE on the viewer side has no codec config and rejects the
// fragments that follow.
func TestCastServer_NewClientReceivesInitFirst(t *testing.T) {
	srv, err := NewCastServer()
	if err != nil {
		t.Fatalf("NewCastServer: %v", err)
	}
	defer srv.Close()

	initSegment := []byte("INIT-SEGMENT-ftyp+moov")
	srv.SendFrame(initSegment, FrameInit)

	addr := srv.ln.Addr().String()
	var (
		mu     sync.Mutex
		frames [][]byte
	)
	dialAndDrain(t, addr, &mu, &frames)

	// First frame should be init, even though we didn't send any media yet.
	if got := drained(&mu, &frames, 1, 2*time.Second); got < 1 {
		t.Fatalf("expected at least 1 frame (init), got %d", got)
	}
	mu.Lock()
	if !bytes.Equal(frames[0], initSegment) {
		t.Fatalf("first frame on wire was not init segment: got %q", frames[0])
	}
	mu.Unlock()
}

// TestCastServer_DeltaSkippedUntilKeyframe verifies that a client that
// connects mid-stream does NOT receive delta fragments — only the next
// keyframe and everything after. A delta fragment delivered to a fresh
// decoder produces visible corruption.
func TestCastServer_DeltaSkippedUntilKeyframe(t *testing.T) {
	srv, err := NewCastServer()
	if err != nil {
		t.Fatalf("NewCastServer: %v", err)
	}
	defer srv.Close()

	srv.SendFrame([]byte("INIT"), FrameInit)

	addr := srv.ln.Addr().String()
	var (
		mu     sync.Mutex
		frames [][]byte
	)
	dialAndDrain(t, addr, &mu, &frames)

	// Wait until the client has received init.
	if got := drained(&mu, &frames, 1, 2*time.Second); got < 1 {
		t.Fatalf("init not received, got %d frames", got)
	}

	// Now push deltas — these should NOT appear on the wire (client is not
	// in sync yet because no keyframe has been seen since accept).
	srv.SendFrame([]byte("delta-A"), FrameDelta)
	srv.SendFrame([]byte("delta-B"), FrameDelta)

	// Push a keyframe — this should appear, and flip the client to in-sync.
	srv.SendFrame([]byte("KEY-1"), FrameKeyframe)

	// Now subsequent deltas should flow.
	srv.SendFrame([]byte("delta-C"), FrameDelta)
	srv.SendFrame([]byte("delta-D"), FrameDelta)

	// Expect 4 frames total: INIT, KEY-1, delta-C, delta-D.
	if got := drained(&mu, &frames, 4, 2*time.Second); got != 4 {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("expected 4 frames, got %d: %v", got, framesAsStrings(frames))
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"INIT", "KEY-1", "delta-C", "delta-D"}
	got := framesAsStrings(frames)
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("frame %d: got %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

// TestCastServer_SlowClientGetsDropped confirms that overflow on a per-client
// queue closes the client. We achieve overflow by pausing the reader (never
// calling Read) and pumping more than castQueueDepth fragments through.
func TestCastServer_SlowClientGetsDropped(t *testing.T) {
	srv, err := NewCastServer()
	if err != nil {
		t.Fatalf("NewCastServer: %v", err)
	}
	defer srv.Close()

	srv.SendFrame([]byte("INIT"), FrameInit)

	addr := srv.ln.Addr().String()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Set a small read buffer on the OS side so the kernel can't absorb our
	// floods invisibly.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}

	// Wait until server registered the client.
	for i := 0; i < 100 && srv.ClientCount() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ClientCount() != 1 {
		t.Fatalf("expected 1 client, got %d", srv.ClientCount())
	}

	// First flood with a keyframe so the client is in-sync, then floods of
	// deltas should fill the queue.
	srv.SendFrame([]byte("KEY"), FrameKeyframe)
	// Push way more than queue depth; one of these will trigger kill.
	bigPayload := bytes.Repeat([]byte("x"), 1024*1024) // 1 MB each
	for i := 0; i < castQueueDepth*4; i++ {
		srv.SendFrame(bigPayload, FrameDelta)
	}

	// The slow client should have been killed; ClientCount returns to 0
	// after serveClient's deferred cleanup runs.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ClientCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("slow client was not dropped: ClientCount still %d", srv.ClientCount())
}

func framesAsStrings(fs [][]byte) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = string(f)
	}
	return out
}
