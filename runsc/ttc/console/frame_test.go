// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
