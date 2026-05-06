package network

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// fMP4 box layout (ISO/IEC 14496-12, simplified for what ffmpeg emits):
//
//	┌────────────┬────────────┬─────────────────────────┐
//	│  size:4 BE │  type:4 BE │  body (size-8 bytes)    │
//	└────────────┴────────────┴─────────────────────────┘
//
// If size == 1, an extended 64-bit size follows the type field; size == 0
// means "extends to end of file" and we treat that as an error since our
// stream is unbounded.
//
// In our pipeline ffmpeg is invoked with
//
//	-f mp4 -movflags +empty_moov+default_base_moof+frag_every_frame
//
// which produces, in order:
//
//	[ftyp] [moov]                       <- init segment (cached)
//	[moof] [mdat]                       <- one fragment per input frame
//	[moof] [mdat]
//	...
//
// readBox returns one full box (header + body) per call. The caller pairs
// (moof, mdat) into a single media fragment downstream.

const (
	// boxMaxSize caps an individual box payload. fMP4 fragments at 1080p run
	// well under 1 MB; 16 MB keeps a wide safety margin against a single
	// pathological I-frame burst.
	boxMaxSize = 16 * 1024 * 1024
)

// readBox reads one MP4 box from r and returns its 4-char type and the full
// box bytes (header included). Returns io.EOF on a clean end-of-stream.
func readBox(r *bufio.Reader) (boxType string, full []byte, err error) {
	var hdr [8]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return "", nil, err
	}
	size32 := binary.BigEndian.Uint32(hdr[:4])
	boxType = string(hdr[4:8])

	switch {
	case size32 == 0:
		return "", nil, fmt.Errorf("fmp4: box %q with size==0 (extends to EOF) is unsupported on a streaming source", boxType)

	case size32 == 1:
		// 64-bit extended size in the next 8 bytes.
		var ext [8]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return "", nil, err
		}
		size64 := binary.BigEndian.Uint64(ext[:])
		if size64 < 16 {
			return "", nil, fmt.Errorf("fmp4: box %q has invalid extended size %d", boxType, size64)
		}
		if size64 > boxMaxSize {
			return "", nil, fmt.Errorf("fmp4: box %q size %d exceeds %d cap", boxType, size64, boxMaxSize)
		}
		body := make([]byte, size64-16)
		if _, err = io.ReadFull(r, body); err != nil {
			return "", nil, err
		}
		full = make([]byte, 0, size64)
		full = append(full, hdr[:]...)
		full = append(full, ext[:]...)
		full = append(full, body...)
		return boxType, full, nil

	case size32 < 8:
		return "", nil, fmt.Errorf("fmp4: box %q has invalid size %d", boxType, size32)

	default:
		if size32 > boxMaxSize {
			return "", nil, fmt.Errorf("fmp4: box %q size %d exceeds %d cap", boxType, size32, boxMaxSize)
		}
		body := make([]byte, size32-8)
		if _, err = io.ReadFull(r, body); err != nil {
			return "", nil, err
		}
		full = make([]byte, 0, size32)
		full = append(full, hdr[:]...)
		full = append(full, body...)
		return boxType, full, nil
	}
}

// FMP4Splitter consumes a stream of MP4 boxes and emits cast chunks: one init
// segment (ftyp + moov concatenated) followed by media fragments (moof + mdat
// concatenated). It is single-threaded; the caller drives it with NextChunk.
//
// Keyframe classification is the caller's responsibility — fragment-level
// keyframe-ness is not in the moof header in a way that's cheap to extract,
// and our producer already knows the encoder's GOP structure (IDR at every
// frame whose index % keyint == 0). The splitter just hands chunks back in
// stream order; the producer tags them.
type FMP4Splitter struct {
	r        *bufio.Reader
	initBuf  []byte // accumulates ftyp/moov before the first moof
	initSent bool
	pending  []byte // moof bytes awaiting their mdat
}

// NewFMP4Splitter wraps r. The reader's buffer size matters for throughput —
// callers should pass a buffered reader sized to comfortably hold one moof
// plus mdat (a few hundred KB).
func NewFMP4Splitter(r *bufio.Reader) *FMP4Splitter {
	return &FMP4Splitter{r: r}
}

// FMP4Chunk is one callback payload from NextChunk.
type FMP4Chunk struct {
	Data []byte
	Init bool // true exactly once, for the ftyp+moov init segment
}

// NextChunk reads ahead until it can return one chunk. Returns io.EOF on a
// clean end of stream from the underlying reader. Skips boxes that are
// neither part of init nor (moof, mdat) pairs — ffmpeg can emit free/sidx/etc.
// in some configurations.
func (s *FMP4Splitter) NextChunk() (FMP4Chunk, error) {
	for {
		boxType, full, err := readBox(s.r)
		if err != nil {
			return FMP4Chunk{}, err
		}

		if !s.initSent {
			switch boxType {
			case "ftyp", "moov", "styp":
				s.initBuf = append(s.initBuf, full...)
				continue
			case "moof":
				// First moof seals the init segment.
				if len(s.initBuf) == 0 {
					return FMP4Chunk{}, fmt.Errorf("fmp4: first moof arrived with no preceding ftyp/moov")
				}
				init := s.initBuf
				s.initBuf = nil
				s.initSent = true
				s.pending = full
				return FMP4Chunk{Data: init, Init: true}, nil
			default:
				// free, skip, anything else before init — discard.
				continue
			}
		}

		switch boxType {
		case "moof":
			// Two moofs in a row would mean the previous fragment had no
			// mdat — shouldn't happen with our ffmpeg invocation, but if it
			// does we drop the pending and start fresh.
			s.pending = full
		case "mdat":
			if s.pending == nil {
				// mdat without preceding moof — drop, can't decode.
				continue
			}
			chunk := append(s.pending, full...)
			s.pending = nil
			return FMP4Chunk{Data: chunk, Init: false}, nil
		default:
			// styp/sidx/free between fragments — ignore.
		}
	}
}
