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

package console

import (
	"bytes"
	"testing"
)

func TestFrameRoundTripAndFragmentation(t *testing.T) {
	var encoded bytes.Buffer
	if err := WriteFrame(&encoded, StreamStdout, []byte("hello")); err != nil {
		t.Fatalf("WriteFrame(stdout): %v", err)
	}
	if err := WriteFrame(&encoded, StreamStderr, []byte("problem")); err != nil {
		t.Fatalf("WriteFrame(stderr): %v", err)
	}
	if err := WriteFrame(&encoded, StreamStdout, nil); err != nil {
		t.Fatalf("WriteFrame(EOF): %v", err)
	}

	data := encoded.Bytes()
	for size := 0; size < HeaderSize+5; size++ {
		if _, consumed, complete, err := DecodeFrame(data[:size]); err != nil || complete || consumed != 0 {
			t.Fatalf("DecodeFrame(%d bytes) = consumed %d complete %t err %v", size, consumed, complete, err)
		}
	}
	first, consumed, complete, err := DecodeFrame(data)
	if err != nil || !complete || first.Stream != StreamStdout || string(first.Data) != "hello" || first.EOF {
		t.Fatalf("first frame = %+v, consumed %d, complete %t, err %v", first, consumed, complete, err)
	}
	second, n, complete, err := DecodeFrame(data[consumed:])
	if err != nil || !complete || second.Stream != StreamStderr || string(second.Data) != "problem" || second.EOF {
		t.Fatalf("second frame = %+v, consumed %d, complete %t, err %v", second, n, complete, err)
	}
	consumed += n
	last, n, complete, err := DecodeFrame(data[consumed:])
	if err != nil || !complete || last.Stream != StreamStdout || !last.EOF || len(last.Data) != 0 {
		t.Fatalf("EOF frame = %+v, consumed %d, complete %t, err %v", last, n, complete, err)
	}
}

func TestFrameRejectsInvalidData(t *testing.T) {
	if err := WriteFrame(&bytes.Buffer{}, 9, []byte("bad")); err == nil {
		t.Fatal("WriteFrame accepted invalid stream")
	}
	invalidStream := make([]byte, HeaderSize)
	invalidStream[0] = 9
	if _, _, _, err := DecodeFrame(invalidStream); err == nil {
		t.Fatal("DecodeFrame accepted invalid stream")
	}
	reserved := make([]byte, HeaderSize)
	reserved[0] = StreamStdout
	reserved[1] = 1
	if _, _, _, err := DecodeFrame(reserved); err == nil {
		t.Fatal("DecodeFrame accepted reserved bytes")
	}
}
