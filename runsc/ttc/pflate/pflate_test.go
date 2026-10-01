// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pflate

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"
)

// testInput returns size bytes: random ones, or text that compresses.
func testInput(size int, random bool) []byte {
	data := make([]byte, size)
	if random {
		rand.New(rand.NewSource(int64(size))).Read(data)
		return data
	}
	words := []string{"time ", "travel ", "container ", "branch ", "fork ", "\n"}
	generator := rand.New(rand.NewSource(int64(size)))
	for i := 0; i < size; {
		i += copy(data[i:], words[generator.Intn(len(words))])
	}
	return data
}

var testSizes = []int{0, 1, 1000, segmentBytes - 1, segmentBytes, segmentBytes + 1, 3*segmentBytes + segmentBytes/2}

// What a Writer compresses, a flate reader decompresses, at any size and
// level, and however it was written.
func TestWriterRoundTrips(t *testing.T) {
	for _, size := range testSizes {
		for _, random := range []bool{false, true} {
			for _, level := range []int{flate.BestSpeed, flate.DefaultCompression, flate.HuffmanOnly} {
				input := testInput(size, random)
				var compressed bytes.Buffer
				z, err := NewWriter(&compressed, level)
				if err != nil {
					t.Fatal(err)
				}
				// Writes of odd sizes cross segments.
				for rest := input; len(rest) != 0; {
					n := min(len(rest), 777777)
					if _, err := z.Write(rest[:n]); err != nil {
						t.Fatal(err)
					}
					rest = rest[n:]
				}
				if err := z.Close(); err != nil {
					t.Fatal(err)
				}
				output, err := io.ReadAll(flate.NewReader(&compressed))
				if err != nil {
					t.Fatalf("size %d random %t level %d: %v", size, random, level, err)
				}
				if !bytes.Equal(output, input) {
					t.Fatalf("size %d random %t level %d: decompressed %d bytes differ from the %d written", size, random, level, len(output), len(input))
				}
			}
		}
	}
}

// A GzipWriter writes one member, which a gzip reader decompresses and checks.
func TestGzipWriterRoundTrips(t *testing.T) {
	for _, size := range testSizes {
		input := testInput(size, size%2 == 0)
		var compressed bytes.Buffer
		z, err := NewGzipWriter(&compressed, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range input[:min(len(input), 3)] {
			if _, err := z.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
		if len(input) > 3 {
			if _, err := z.Write(input[3:]); err != nil {
				t.Fatal(err)
			}
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		if err := z.Close(); err != nil {
			t.Fatalf("closing twice: %v", err)
		}
		stream := bytes.NewReader(compressed.Bytes())
		reader, err := gzip.NewReader(stream)
		if err != nil {
			t.Fatal(err)
		}
		reader.Multistream(false)
		output, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(output, input) {
			t.Fatalf("size %d: decompressed %d bytes differ from the %d written", size, len(output), len(input))
		}
		if stream.Len() != 0 {
			t.Fatalf("size %d: %d bytes follow the member", size, stream.Len())
		}
	}
}

type failingWriter struct{ after int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("the disk is full")
	}
	w.after--
	return len(p), nil
}

// A failure to write the stream is returned, by a later Write or by Close.
func TestWriterReturnsWriteFailure(t *testing.T) {
	z, err := NewWriter(&failingWriter{after: 1}, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	input := testInput(8*segmentBytes, true)
	_, writeErr := z.Write(input)
	closeErr := z.Close()
	if writeErr == nil && closeErr == nil {
		t.Fatal("a failed write of the stream was not returned")
	}
	if _, err := z.Write([]byte{1}); err == nil {
		t.Fatal("a closed Writer accepted a write")
	}
}

func BenchmarkGzipWriter(b *testing.B) {
	for _, random := range []bool{false, true} {
		input := testInput(64*segmentBytes, random)
		for _, level := range []int{flate.BestSpeed, flate.DefaultCompression} {
			name := fmt.Sprintf("text/%d", level)
			if random {
				name = fmt.Sprintf("random/%d", level)
			}
			b.Run(name, func(b *testing.B) {
				benchmarkGzipWriter(b, input, level)
			})
		}
	}
}

func benchmarkGzipWriter(b *testing.B, input []byte, level int) {
	b.SetBytes(int64(len(input)))
	var compressed int
	for i := 0; i < b.N; i++ {
		counter := &countingWriter{}
		z, err := NewGzipWriter(counter, level)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := z.Write(input); err != nil {
			b.Fatal(err)
		}
		if err := z.Close(); err != nil {
			b.Fatal(err)
		}
		compressed = counter.n
	}
	b.ReportMetric(float64(compressed)/float64(len(input)), "ratio")
}
