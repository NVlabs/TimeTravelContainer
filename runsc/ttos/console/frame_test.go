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
	"time"
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

func TestTimestampedFrameRoundTrip(t *testing.T) {
	timestamp := time.Date(2026, 7, 17, 1, 2, 3, 456789012, time.UTC)
	var encoded bytes.Buffer
	if err := WriteTimestampedFrame(&encoded, StreamStdout, timestamp, []byte("hello")); err != nil {
		t.Fatalf("WriteTimestampedFrame: %v", err)
	}
	frame, consumed, complete, err := DecodeFrame(encoded.Bytes())
	if err != nil || !complete || consumed != encoded.Len() {
		t.Fatalf("DecodeFrame: frame=%+v consumed=%d complete=%t err=%v", frame, consumed, complete, err)
	}
	if frame.Stream != StreamStdout || string(frame.Data) != "hello" || frame.EOF || !frame.Timestamp.Equal(timestamp) {
		t.Errorf("frame=%+v, want timestamped stdout payload", frame)
	}
	var eof bytes.Buffer
	if err := WriteTimestampedFrame(&eof, StreamStderr, timestamp, nil); err != nil {
		t.Fatalf("WriteTimestampedFrame EOF: %v", err)
	}
	frame, _, complete, err = DecodeFrame(eof.Bytes())
	if err != nil || !complete || !frame.EOF || !frame.Timestamp.Equal(timestamp) {
		t.Errorf("timestamped EOF frame=%+v complete=%t err=%v", frame, complete, err)
	}
	if err := WriteTimestampedFrame(&bytes.Buffer{}, StreamStdout, time.Time{}, nil); err == nil {
		t.Fatal("WriteTimestampedFrame accepted a zero timestamp")
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
	reserved[1] = 2
	if _, _, _, err := DecodeFrame(reserved); err == nil {
		t.Fatal("DecodeFrame accepted reserved bytes")
	}
}

func TestCopyFinalFramesPreservesTimestampedRecords(t *testing.T) {
	timestamp := time.Date(2026, 7, 17, 1, 2, 3, 4, time.UTC)
	var input bytes.Buffer
	if err := WriteTimestampedFrame(&input, StreamStdout, timestamp, []byte("output")); err != nil {
		t.Fatalf("WriteTimestampedFrame data: %v", err)
	}
	if err := WriteTimestampedFrame(&input, StreamStdout, timestamp, nil); err != nil {
		t.Fatalf("WriteTimestampedFrame stdout EOF: %v", err)
	}
	if err := WriteTimestampedFrame(&input, StreamStderr, timestamp, nil); err != nil {
		t.Fatalf("WriteTimestampedFrame stderr EOF: %v", err)
	}
	want := append([]byte(nil), input.Bytes()...)
	var got bytes.Buffer
	if err := CopyFinalFrames(&got, &input, false, false); err != nil {
		t.Fatalf("CopyFinalFrames: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("CopyFinalFrames wrote %x, want %x", got.Bytes(), want)
	}
}

func TestCopyFinalFramesSynthesizesMissingEOFs(t *testing.T) {
	var input bytes.Buffer
	if err := WriteFrame(&input, StreamStdout, []byte("last output")); err != nil {
		t.Fatalf("WriteFrame(stdout data): %v", err)
	}
	if err := WriteFrame(&input, StreamStdout, nil); err != nil {
		t.Fatalf("WriteFrame(stdout EOF): %v", err)
	}

	want := append([]byte(nil), input.Bytes()...)
	var stderrEOF bytes.Buffer
	if err := WriteFrame(&stderrEOF, StreamStderr, nil); err != nil {
		t.Fatalf("WriteFrame(stderr EOF): %v", err)
	}
	want = append(want, stderrEOF.Bytes()...)

	var got bytes.Buffer
	if err := CopyFinalFrames(&got, &input, false, false); err != nil {
		t.Fatalf("CopyFinalFrames: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("CopyFinalFrames wrote %x, want %x", got.Bytes(), want)
	}
}

func TestCopyFinalFramesSynthesizesBothEOFs(t *testing.T) {
	var got bytes.Buffer
	if err := CopyFinalFrames(&got, bytes.NewReader(nil), false, false); err != nil {
		t.Fatalf("CopyFinalFrames: %v", err)
	}

	var want bytes.Buffer
	for _, stream := range []byte{StreamStdout, StreamStderr} {
		if err := WriteFrame(&want, stream, nil); err != nil {
			t.Fatalf("WriteFrame(%d EOF): %v", stream, err)
		}
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("CopyFinalFrames wrote %x, want %x", got.Bytes(), want.Bytes())
	}
}

func TestCopyFinalFramesSkipsCoordinatorDuplicateEOF(t *testing.T) {
	var input bytes.Buffer
	if err := WriteFrame(&input, StreamStdout, nil); err != nil {
		t.Fatalf("WriteFrame(stdout EOF): %v", err)
	}
	if err := WriteFrame(&input, StreamStderr, []byte("last error")); err != nil {
		t.Fatalf("WriteFrame(stderr data): %v", err)
	}
	if err := WriteFrame(&input, StreamStderr, nil); err != nil {
		t.Fatalf("WriteFrame(stderr EOF): %v", err)
	}

	var want bytes.Buffer
	if err := WriteFrame(&want, StreamStderr, []byte("last error")); err != nil {
		t.Fatalf("WriteFrame(want stderr data): %v", err)
	}
	if err := WriteFrame(&want, StreamStderr, nil); err != nil {
		t.Fatalf("WriteFrame(want stderr EOF): %v", err)
	}
	var got bytes.Buffer
	if err := CopyFinalFrames(&got, &input, true, false); err != nil {
		t.Fatalf("CopyFinalFrames: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("CopyFinalFrames wrote %x, want %x", got.Bytes(), want.Bytes())
	}
}

func TestCopyFinalFramesDrainsCoordinatorAfterBothClientEOFs(t *testing.T) {
	var input bytes.Buffer
	if err := WriteFrame(&input, StreamStdout, nil); err != nil {
		t.Fatalf("WriteFrame stdout: %v", err)
	}
	if err := WriteFrame(&input, StreamStderr, nil); err != nil {
		t.Fatalf("WriteFrame stderr: %v", err)
	}
	var got bytes.Buffer
	if err := CopyFinalFrames(&got, &input, true, true); err != nil {
		t.Fatalf("CopyFinalFrames: %v", err)
	}
	if input.Len() != 0 {
		t.Fatalf("CopyFinalFrames left %d coordinator bytes unread", input.Len())
	}
	if got.Len() != 0 {
		t.Fatalf("CopyFinalFrames duplicated client EOFs: %x", got.Bytes())
	}
}

func TestCopyFinalFramesRejectsTruncatedFrame(t *testing.T) {
	for _, input := range [][]byte{
		{StreamStdout},
		{StreamStdout, 0, 0, 0, 0, 0, 0, 2, 'x'},
	} {
		var got bytes.Buffer
		if err := CopyFinalFrames(&got, bytes.NewReader(input), false, false); err == nil {
			t.Fatalf("CopyFinalFrames(%x) succeeded, want error", input)
		}
	}
}
