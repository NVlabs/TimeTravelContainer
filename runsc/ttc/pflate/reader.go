// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pflate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/klauspost/compress/flate"
)

// finalBlock is an empty final stored block. A segment that does not end the
// stream ends with a sync flush, aligned to a byte; followed by finalBlock, it
// is a DEFLATE stream of its own.
var finalBlock = []byte{0x01, 0x00, 0x00, 0xff, 0xff}

// maxSegmentLength bounds the compressed length of a segment, which flate
// makes at most a little longer than the segment.
const maxSegmentLength = segmentBytes + segmentBytes/64 + 4096

// Reader decompresses a stream a Writer wrote, given its segments' compressed
// lengths, on every processor: each segment is decompressed on its own, by a
// goroutine, and read in order.
type Reader struct {
	r io.ReaderAt
	// next is where the next segment to start begins in r, and lengths are
	// the compressed lengths of the segments not yet started.
	next    int64
	lengths []int64
	// pending are the segments being decompressed, or decompressed and not
	// yet read, in order: at most one for each processor.
	pending []*readSegment
	limit   int
	// current is the segment being read, of which out is what is left.
	current *readSegment
	out     []byte
	// buffers are kept for reuse.
	buffers [][]byte
	// startErr is why the next segment could not be started, which Read
	// returns once it has returned the segments before it.
	startErr error
	err      error
}

// readSegment is the decompression of one segment.
type readSegment struct {
	// compressed is the segment, followed by finalBlock, and output what it
	// decompresses to.
	compressed []byte
	output     []byte
	err        error
	done       chan struct{}
}

// NewReader returns a Reader of the stream at offset in r whose segments have
// the compressed lengths Writer.Segments returned.
func NewReader(r io.ReaderAt, offset int64, lengths []int64) *Reader {
	return &Reader{r: r, next: offset, lengths: lengths, limit: runtime.GOMAXPROCS(0)}
}

// Read implements io.Reader.
func (z *Reader) Read(p []byte) (int, error) {
	for len(z.out) == 0 {
		if z.err != nil {
			return 0, z.err
		}
		for len(z.pending) < z.limit && len(z.lengths) != 0 && z.startErr == nil {
			z.startErr = z.start()
		}
		if len(z.pending) == 0 {
			z.err = z.startErr
			if z.err == nil {
				z.err = io.EOF
			}
			continue
		}
		s := z.pending[0]
		z.pending = z.pending[1:]
		<-s.done
		if z.current != nil {
			z.buffers = append(z.buffers, z.current.compressed, z.current.output)
		}
		z.current = s
		if s.err != nil {
			z.err = s.err
			continue
		}
		z.out = s.output
	}
	n := copy(p, z.out)
	z.out = z.out[n:]
	return n, nil
}

// start reads the next segment and starts its decompression.
func (z *Reader) start() error {
	length := z.lengths[0]
	if length <= 0 || length > maxSegmentLength {
		return fmt.Errorf("pflate: a segment of %d compressed bytes", length)
	}
	s := &readSegment{compressed: z.buffer(int(length) + len(finalBlock)), output: z.buffer(segmentBytes), done: make(chan struct{})}
	// A segment that ends the stream may be read with io.EOF; one cut short
	// is an error.
	if n, err := z.r.ReadAt(s.compressed[:length], z.next); n != int(length) {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	copy(s.compressed[length:], finalBlock)
	z.lengths = z.lengths[1:]
	z.next += length
	z.pending = append(z.pending, s)
	go s.decompress()
	return nil
}

// buffer returns a buffer of size bytes.
func (z *Reader) buffer(size int) []byte {
	for index := len(z.buffers) - 1; index >= 0; index-- {
		if buffer := z.buffers[index]; cap(buffer) >= size {
			z.buffers = append(z.buffers[:index], z.buffers[index+1:]...)
			return buffer[:size]
		}
	}
	return make([]byte, size, max(size, maxSegmentLength+len(finalBlock)))
}

// decompress decompresses s, which ends within a segment's length.
func (s *readSegment) decompress() {
	defer close(s.done)
	decompressor := flate.NewReader(bytes.NewReader(s.compressed))
	n := 0
	var err error
	for n < len(s.output) && err == nil {
		var read int
		read, err = decompressor.Read(s.output[n:])
		n += read
	}
	switch {
	case errors.Is(err, io.EOF):
		err = nil
	case err == nil:
		var extra [1]byte
		if _, err = io.ReadFull(decompressor, extra[:]); err == nil {
			err = errors.New("pflate: a segment decompresses to more than a segment")
		} else if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	s.output, s.err = s.output[:n], errors.Join(err, decompressor.Close())
}
