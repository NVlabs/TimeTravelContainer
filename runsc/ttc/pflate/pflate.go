// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package pflate compresses on every processor into one DEFLATE stream, or
// one gzip member. The input is cut into segments, each compressed on its own
// by a goroutine and written in order. A segment's compressor starts afresh
// and ends the segment with a sync flush, which aligns it to a byte without
// ending the stream; the last segment ends it. Any flate or gzip reader
// decompresses what was written. The stream is a little larger than a single
// compressor makes, as a segment does not refer to the data before it.
package pflate

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"runtime"
	"sync"
)

// segmentBytes is the size of the input segments compressed apart.
const segmentBytes = 1 << 20

// probeBytes is how much of a segment is compressed first, at best speed, to
// find whether it compresses: flate spends most of its time looking for
// matches in data that does not. A segment whose probe does not shrink by a
// thirty-second is stored, as flate stores data that does not compress at
// all, six times as fast as best speed compresses it.
const probeBytes = 64 << 10

// segment is the compression of one segment of the input.
type segment struct {
	input  []byte
	output *bytes.Buffer
	// final is set on the segment that ends the stream.
	final bool
	err   error
	done  chan struct{}
}

// Writer compresses what is written to it into one DEFLATE stream.
type Writer struct {
	w     io.Writer
	level int
	// current is the segment being filled.
	current []byte
	// pending are the segments being compressed, or compressed and not yet
	// written, in order: at most one for each processor.
	pending []*segment
	limit   int
	// compressors, inputs and outputs are kept for reuse: a compressor's
	// state is hundreds of kilobytes. Only Write and Close use inputs and
	// outputs. probeCompressors are at best speed, for probes, and
	// storeCompressors store the segments that do not compress.
	compressors      sync.Pool
	probeCompressors sync.Pool
	storeCompressors sync.Pool
	inputs           [][]byte
	outputs          []*bytes.Buffer
	// segments are the compressed lengths of the segments written.
	segments []int64
	err      error
	closed   bool
}

// NewWriter returns a Writer that compresses onto w at level, a flate
// compression level.
func NewWriter(w io.Writer, level int) (*Writer, error) {
	compressor, err := flate.NewWriter(io.Discard, level)
	if err != nil {
		return nil, err
	}
	z := &Writer{w: w, level: level, limit: runtime.GOMAXPROCS(0)}
	z.compressors.Put(compressor)
	return z, nil
}

// Write implements io.Writer.
func (z *Writer) Write(p []byte) (int, error) {
	if z.closed {
		return 0, errors.New("pflate: write to a closed Writer")
	}
	written := 0
	for len(p) != 0 {
		if z.err != nil {
			return written, z.err
		}
		if z.current == nil {
			z.current = z.input()
		}
		n := copy(z.current[len(z.current):cap(z.current)], p)
		z.current = z.current[:len(z.current)+n]
		p = p[n:]
		written += n
		if len(z.current) == cap(z.current) {
			z.compress(false)
		}
	}
	return written, z.err
}

// input returns a segment's input buffer, empty.
func (z *Writer) input() []byte {
	if n := len(z.inputs); n != 0 {
		input := z.inputs[n-1]
		z.inputs = z.inputs[:n-1]
		return input[:0]
	}
	return make([]byte, 0, segmentBytes)
}

// output returns a segment's output buffer, empty.
func (z *Writer) output() *bytes.Buffer {
	if n := len(z.outputs); n != 0 {
		output := z.outputs[n-1]
		z.outputs = z.outputs[:n-1]
		output.Reset()
		return output
	}
	return new(bytes.Buffer)
}

// compress starts the compression of the current segment, first writing the
// oldest pending one while as many as there are processors are pending.
func (z *Writer) compress(final bool) {
	s := &segment{input: z.current, output: z.output(), final: final, done: make(chan struct{})}
	z.current = nil
	for len(z.pending) >= z.limit && z.err == nil {
		z.writeOldest()
	}
	z.pending = append(z.pending, s)
	go z.run(s)
}

// run compresses s.
func (z *Writer) run(s *segment) {
	defer close(s.done)
	pool, level := &z.compressors, z.level
	if z.level != flate.NoCompression && z.level != flate.HuffmanOnly && !z.compresses(s.input[:min(len(s.input), probeBytes)]) {
		pool, level = &z.storeCompressors, flate.NoCompression
	}
	compressor, _ := pool.Get().(*flate.Writer)
	if compressor == nil {
		// The level was checked by NewWriter.
		compressor, _ = flate.NewWriter(s.output, level)
	} else {
		compressor.Reset(s.output)
	}
	s.output.Grow(len(s.input) + len(s.input)/64 + 64)
	if _, err := compressor.Write(s.input); err != nil {
		s.err = err
		return
	}
	if s.final {
		s.err = compressor.Close()
	} else {
		s.err = compressor.Flush()
	}
	pool.Put(compressor)
}

// compresses reports whether flate at best speed makes probe smaller.
func (z *Writer) compresses(probe []byte) bool {
	compressor, _ := z.probeCompressors.Get().(*flate.Writer)
	var size countingWriter
	if compressor == nil {
		compressor, _ = flate.NewWriter(&size, flate.BestSpeed)
	} else {
		compressor.Reset(&size)
	}
	defer z.probeCompressors.Put(compressor)
	if _, err := compressor.Write(probe); err != nil || compressor.Flush() != nil {
		return true
	}
	return size.n < len(probe)-len(probe)/32
}

// countingWriter counts what is written to it.
type countingWriter struct{ n int }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}

// writeOldest waits for the oldest pending segment and writes it.
func (z *Writer) writeOldest() {
	s := z.pending[0]
	copy(z.pending, z.pending[1:])
	z.pending = z.pending[:len(z.pending)-1]
	<-s.done
	defer func() {
		if s.input != nil {
			z.inputs = append(z.inputs, s.input)
		}
		z.outputs = append(z.outputs, s.output)
	}()
	if z.err != nil {
		return
	}
	if s.err != nil {
		z.err = s.err
		return
	}
	if _, err := z.w.Write(s.output.Bytes()); err != nil {
		z.err = err
		return
	}
	z.segments = append(z.segments, int64(s.output.Len()))
}

// Segments returns the compressed lengths of the segments written, in order:
// once the Writer is closed, those of the whole stream, by which a Reader
// decompresses them at once.
func (z *Writer) Segments() []int64 {
	return z.segments
}

// Close ends the stream and writes what remains of it. It does not close the
// underlying writer.
func (z *Writer) Close() error {
	if z.closed {
		return z.err
	}
	z.closed = true
	if z.err == nil {
		// The last segment ends the stream, empty or not.
		z.compress(true)
	}
	for len(z.pending) != 0 {
		z.writeOldest()
	}
	return z.err
}

// GzipWriter compresses what is written to it into one gzip member, as
// Writer does.
type GzipWriter struct {
	w      io.Writer
	flate  *Writer
	crc    uint32
	size   uint32
	closed bool
}

// NewGzipWriter returns a GzipWriter that compresses onto w at level, a flate
// compression level. It writes the member's header at once.
func NewGzipWriter(w io.Writer, level int) (*GzipWriter, error) {
	compressor, err := NewWriter(w, level)
	if err != nil {
		return nil, err
	}
	// No name, comment, extra field or modification time, and an unknown
	// operating system, as compress/gzip writes it.
	header := [10]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 255}
	switch level {
	case flate.BestCompression:
		header[8] = 2
	case flate.BestSpeed:
		header[8] = 4
	}
	if _, err := w.Write(header[:]); err != nil {
		return nil, err
	}
	return &GzipWriter{w: w, flate: compressor}, nil
}

// Write implements io.Writer.
func (z *GzipWriter) Write(p []byte) (int, error) {
	n, err := z.flate.Write(p)
	z.crc = crc32.Update(z.crc, crc32.IEEETable, p[:n])
	z.size += uint32(n)
	return n, err
}

// Close ends the member and writes what remains of it. It does not close the
// underlying writer.
func (z *GzipWriter) Close() error {
	if z.closed {
		return z.flate.err
	}
	z.closed = true
	if err := z.flate.Close(); err != nil {
		return err
	}
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[:4], z.crc)
	binary.LittleEndian.PutUint32(trailer[4:], z.size)
	_, err := z.w.Write(trailer[:])
	return err
}
