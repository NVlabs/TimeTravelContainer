// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package console defines the TimeTravelContainer detached-console stream format.
package console

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

const (
	// HeaderSize matches Docker's stdcopy multiplexing header size.
	HeaderSize = 8
	// MaxPayload is the largest payload accepted in one frame.
	MaxPayload = 1 << 20

	timestampFlag = byte(1)
	timestampSize = 8

	// StreamStdout identifies standard output.
	StreamStdout byte = 1
	// StreamStderr identifies standard error.
	StreamStderr byte = 2
)

// Frame is one stdout/stderr record. A zero-length frame marks EOF for its
// stream; producers never emit empty data frames.
type Frame struct {
	Stream    byte
	Data      []byte
	EOF       bool
	Timestamp time.Time
}

// WriteFrame writes one legacy Docker-compatible multiplexing frame. Empty
// data emits an EOF marker for stream.
func WriteFrame(w io.Writer, stream byte, data []byte) error {
	return writeFrame(w, stream, data, time.Time{})
}

// WriteTimestampedFrame writes an internal timestamped multiplexing frame. The
// timestamp metadata is removed by DecodeFrame and never reaches guest output.
func WriteTimestampedFrame(w io.Writer, stream byte, timestamp time.Time, data []byte) error {
	if timestamp.IsZero() {
		return fmt.Errorf("timestamped console frame has a zero timestamp")
	}
	return writeFrame(w, stream, data, timestamp)
}

func writeFrame(w io.Writer, stream byte, data []byte, timestamp time.Time) error {
	if err := validateStream(stream); err != nil {
		return err
	}
	if len(data) > MaxPayload {
		return fmt.Errorf("console frame payload %d exceeds %d", len(data), MaxPayload)
	}
	prefix := 0
	if !timestamp.IsZero() {
		prefix = timestampSize
	}
	encoded := make([]byte, HeaderSize+prefix+len(data))
	encoded[0] = stream
	if prefix != 0 {
		encoded[1] = timestampFlag
		binary.BigEndian.PutUint64(encoded[HeaderSize:HeaderSize+timestampSize], uint64(timestamp.UnixNano()))
	}
	binary.BigEndian.PutUint32(encoded[4:HeaderSize], uint32(prefix+len(data)))
	copy(encoded[HeaderSize+prefix:], data)
	// One logical Write keeps a frame wholly within one rotating generation.
	return writeFull(w, encoded)
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// DecodeFrame decodes the first complete frame in data. Complete is false when
// more bytes are required; consumed is then zero.
func DecodeFrame(data []byte) (frame Frame, consumed int, complete bool, err error) {
	if len(data) < HeaderSize {
		return Frame{}, 0, false, nil
	}
	if err := validateStream(data[0]); err != nil {
		return Frame{}, 0, false, err
	}
	if data[1]&^timestampFlag != 0 || data[2] != 0 || data[3] != 0 {
		return Frame{}, 0, false, fmt.Errorf("invalid console frame reserved bytes")
	}
	timestamped := data[1]&timestampFlag != 0
	size := int(binary.BigEndian.Uint32(data[4:8]))
	maxSize := MaxPayload
	if timestamped {
		maxSize += timestampSize
		if size < timestampSize {
			return Frame{}, 0, false, fmt.Errorf("timestamped console frame payload %d is too short", size)
		}
	}
	if size > maxSize {
		return Frame{}, 0, false, fmt.Errorf("console frame payload %d exceeds %d", size, maxSize)
	}
	if len(data) < HeaderSize+size {
		return Frame{}, 0, false, nil
	}
	payload := data[HeaderSize : HeaderSize+size]
	var timestamp time.Time
	if timestamped {
		timestamp = time.Unix(0, int64(binary.BigEndian.Uint64(payload[:timestampSize]))).UTC()
		payload = payload[timestampSize:]
	}
	decoded := append([]byte(nil), payload...)
	return Frame{Stream: data[0], Data: decoded, EOF: len(decoded) == 0, Timestamp: timestamp}, HeaderSize + size, true, nil
}

func validateStream(stream byte) error {
	if stream != StreamStdout && stream != StreamStderr {
		return fmt.Errorf("invalid console stream %d", stream)
	}
	return nil
}
