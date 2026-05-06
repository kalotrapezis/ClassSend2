package network

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// makeBox builds a minimal MP4 box with the given 4-char type and body.
// Used to hand-craft synthetic fMP4 streams for the splitter tests so we
// don't have to depend on ffmpeg being available.
func makeBox(boxType string, body []byte) []byte {
	if len(boxType) != 4 {
		panic("box type must be 4 ASCII chars")
	}
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(body)))
	copy(out[4:8], boxType)
	copy(out[8:], body)
	return out
}

func TestReadBox_Standard(t *testing.T) {
	body := []byte("hello")
	stream := makeBox("ftyp", body)
	r := bufio.NewReader(bytes.NewReader(stream))

	bt, full, err := readBox(r)
	if err != nil {
		t.Fatalf("readBox: %v", err)
	}
	if bt != "ftyp" {
		t.Fatalf("type = %q, want ftyp", bt)
	}
	if !bytes.Equal(full, stream) {
		t.Fatalf("full bytes mismatch: got %x, want %x", full, stream)
	}
}

func TestReadBox_TruncatedHeader(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte{0x00, 0x00, 0x00})) // only 3 bytes
	_, _, err := readBox(r)
	if err == nil {
		t.Fatal("expected error on truncated header")
	}
}

func TestReadBox_TruncatedBody(t *testing.T) {
	// Header claims 16 bytes but we only supply 12 (header + 4 body bytes).
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr[:4], 16)
	copy(hdr[4:8], "moov")
	stream := append(hdr, []byte{1, 2, 3, 4}...)

	r := bufio.NewReader(bytes.NewReader(stream))
	_, _, err := readBox(r)
	if err == nil {
		t.Fatal("expected error on truncated body")
	}
}

func TestReadBox_ZeroSizeRejected(t *testing.T) {
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr[:4], 0)
	copy(hdr[4:8], "mdat")
	r := bufio.NewReader(bytes.NewReader(hdr))
	_, _, err := readBox(r)
	if err == nil {
		t.Fatal("expected error on size==0 (unbounded box)")
	}
}

func TestReadBox_OversizedRejected(t *testing.T) {
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr[:4], boxMaxSize+1)
	copy(hdr[4:8], "mdat")
	r := bufio.NewReader(bytes.NewReader(hdr))
	_, _, err := readBox(r)
	if err == nil {
		t.Fatal("expected error on oversized box")
	}
}

// TestFMP4Splitter_HappyPath drives the splitter through a synthetic stream
// matching what ffmpeg emits with +empty_moov+default_base_moof+frag_every_frame:
//
//	ftyp moov | moof mdat | moof mdat | moof mdat | EOF
//
// Expects: one init chunk (ftyp+moov), then three media chunks (moof+mdat).
func TestFMP4Splitter_HappyPath(t *testing.T) {
	ftyp := makeBox("ftyp", []byte("isom"))
	moov := makeBox("moov", []byte("MOOV-payload"))
	moof1 := makeBox("moof", []byte("moof-1"))
	mdat1 := makeBox("mdat", []byte("frame-1-pixels"))
	moof2 := makeBox("moof", []byte("moof-2"))
	mdat2 := makeBox("mdat", []byte("frame-2-pixels"))
	moof3 := makeBox("moof", []byte("moof-3"))
	mdat3 := makeBox("mdat", []byte("frame-3-pixels"))

	var stream bytes.Buffer
	for _, b := range [][]byte{ftyp, moov, moof1, mdat1, moof2, mdat2, moof3, mdat3} {
		stream.Write(b)
	}

	sp := NewFMP4Splitter(bufio.NewReader(&stream))

	// First chunk: init = ftyp + moov concatenated.
	c1, err := sp.NextChunk()
	if err != nil {
		t.Fatalf("chunk 1: %v", err)
	}
	if !c1.Init {
		t.Fatalf("chunk 1: expected Init=true")
	}
	wantInit := append(append([]byte{}, ftyp...), moov...)
	if !bytes.Equal(c1.Data, wantInit) {
		t.Fatalf("chunk 1 init bytes mismatch")
	}

	// Three media chunks: each is moof+mdat concatenated.
	wants := [][]byte{
		append(append([]byte{}, moof1...), mdat1...),
		append(append([]byte{}, moof2...), mdat2...),
		append(append([]byte{}, moof3...), mdat3...),
	}
	for i, want := range wants {
		c, err := sp.NextChunk()
		if err != nil {
			t.Fatalf("chunk %d: %v", i+2, err)
		}
		if c.Init {
			t.Fatalf("chunk %d: expected Init=false", i+2)
		}
		if !bytes.Equal(c.Data, want) {
			t.Fatalf("chunk %d bytes mismatch", i+2)
		}
	}

	// Stream is now drained.
	if _, err := sp.NextChunk(); err != io.EOF {
		t.Fatalf("expected EOF after stream drained, got %v", err)
	}
}

// TestFMP4Splitter_SkipsFreeBoxes verifies that boxes other than ftyp/moov/
// moof/mdat are silently passed over. ffmpeg can emit free/sidx in some
// configurations and they shouldn't break the stream.
func TestFMP4Splitter_SkipsFreeBoxes(t *testing.T) {
	ftyp := makeBox("ftyp", []byte("isom"))
	free := makeBox("free", []byte("padding"))
	moov := makeBox("moov", []byte("MOOV"))
	sidx := makeBox("sidx", []byte("SIDX"))
	moof := makeBox("moof", []byte("MOOF"))
	mdat := makeBox("mdat", []byte("MDAT"))

	var stream bytes.Buffer
	for _, b := range [][]byte{ftyp, free, moov, sidx, moof, mdat} {
		stream.Write(b)
	}

	sp := NewFMP4Splitter(bufio.NewReader(&stream))

	c1, err := sp.NextChunk()
	if err != nil {
		t.Fatalf("chunk 1: %v", err)
	}
	if !c1.Init {
		t.Fatal("chunk 1 should be init")
	}
	// init contains ftyp + moov but free is dropped.
	wantInit := append(append([]byte{}, ftyp...), moov...)
	if !bytes.Equal(c1.Data, wantInit) {
		t.Fatalf("init mismatch: got %x", c1.Data)
	}

	c2, err := sp.NextChunk()
	if err != nil {
		t.Fatalf("chunk 2: %v", err)
	}
	if c2.Init {
		t.Fatal("chunk 2 should be media")
	}
	wantMedia := append(append([]byte{}, moof...), mdat...)
	if !bytes.Equal(c2.Data, wantMedia) {
		t.Fatalf("media mismatch")
	}
}

// TestFMP4Splitter_MoofWithoutInit triggers the error path where a fragment
// arrives before any ftyp/moov was seen. Real ffmpeg never emits this, but a
// corrupt stream (truncated, byte-shifted) might.
func TestFMP4Splitter_MoofWithoutInit(t *testing.T) {
	moof := makeBox("moof", []byte("MOOF"))
	sp := NewFMP4Splitter(bufio.NewReader(bytes.NewReader(moof)))
	_, err := sp.NextChunk()
	if err == nil {
		t.Fatal("expected error when moof arrives before init")
	}
}
