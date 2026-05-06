package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"classsend/internal/ipc"
)

// TestWriteToTUI_NoInterleaving exercises the agent→TUI write path under
// concurrent producers — replayHistoryToTUI plus event-hook callbacks
// (OnConnected/OnRawMessage/OnDisconnected) — and verifies that the reader
// side observes a stream of well-formed newline-delimited JSON frames.
//
// Pre-fix this test reliably reported scanner errors because TCP's Write was
// not atomic across goroutines: bytes from different frames interleaved on
// the wire. With tuiWriteMu in writeToTUI the stream stays clean.
func TestWriteToTUI_NoInterleaving(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	tuiMu.Lock()
	tuiConn = server
	tuiMu.Unlock()
	defer func() {
		tuiMu.Lock()
		tuiConn = nil
		tuiMu.Unlock()
	}()

	const writers = 8
	const framesPerWriter = 200
	const totalFrames = writers * framesPerWriter

	// Reader side: parse every line as a Frame. Any unmarshal error indicates
	// interleaved bytes.
	parsed := make(chan int, 1)
	parseErr := make(chan error, 1)
	go func() {
		count := 0
		// 64-bit RawMessage payloads + envelope ≈ <200 B per line; a 1 MB
		// scanner buffer is plenty.
		scanner := bufio.NewScanner(client)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for scanner.Scan() {
			var f ipc.Frame
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				parseErr <- err
				return
			}
			count++
			if count == totalFrames {
				parsed <- count
				return
			}
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			parseErr <- err
		} else {
			parsed <- count
		}
	}()

	// Writers: half mimic replayHistoryToTUI (direct writeToTUI on the local
	// conn ref), half mimic event hooks (sendToTUI through the global).
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]int{"writer": i})
			for j := 0; j < framesPerWriter; j++ {
				f := ipc.Frame{Type: ipc.TypeForward, Data: payload}
				if i%2 == 0 {
					writeToTUI(server, f)
				} else {
					sendToTUI(f)
				}
			}
		}()
	}
	wg.Wait()

	select {
	case n := <-parsed:
		if n != totalFrames {
			t.Fatalf("got %d frames, want %d", n, totalFrames)
		}
	case err := <-parseErr:
		t.Fatalf("frame parse error (interleaved bytes): %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for frames")
	}
}
