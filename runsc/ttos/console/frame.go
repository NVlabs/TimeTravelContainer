// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package console defines the TimeTravelOS detached-console stream format.
package console

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// HeaderSize matches Docker's stdcopy multiplexing header size.
	HeaderSize = 8
	// MaxPayload is the largest payload accepted in one frame.
	MaxPayload = 1 << 20

	// StreamStdout identifies standard output.
	StreamStdout byte = 1
	// StreamStderr identifies standard error.
	StreamStderr byte = 2
)

// Frame is one stdout/stderr record. A zero-length frame marks EOF for its
// stream; producers never emit empty data frames.
type Frame struct {
	Stream byte
	Data   []byte
	EOF    bool
}

// WriteFrame writes one Docker-compatible multiplexing frame. Empty data emits
// an EOF marker for stream.
func WriteFrame(w io.Writer, stream byte, data []byte) error {
	if err := validateStream(stream); err != nil {
		return err
	}
	if len(data) > MaxPayload {
		return fmt.Errorf("console frame payload %d exceeds %d", len(data), MaxPayload)
	}
	encoded := make([]byte, HeaderSize+len(data))
	encoded[0] = stream
	binary.BigEndian.PutUint32(encoded[4:HeaderSize], uint32(len(data)))
	copy(encoded[HeaderSize:], data)
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
	if data[1] != 0 || data[2] != 0 || data[3] != 0 {
		return Frame{}, 0, false, fmt.Errorf("invalid console frame reserved bytes")
	}
	size := int(binary.BigEndian.Uint32(data[4:8]))
	if size > MaxPayload {
		return Frame{}, 0, false, fmt.Errorf("console frame payload %d exceeds %d", size, MaxPayload)
	}
	if len(data) < HeaderSize+size {
		return Frame{}, 0, false, nil
	}
	payload := append([]byte(nil), data[HeaderSize:HeaderSize+size]...)
	return Frame{Stream: data[0], Data: payload, EOF: size == 0}, HeaderSize + size, true, nil
}

func validateStream(stream byte) error {
	if stream != StreamStdout && stream != StreamStderr {
		return fmt.Errorf("invalid console stream %d", stream)
	}
	return nil
}
