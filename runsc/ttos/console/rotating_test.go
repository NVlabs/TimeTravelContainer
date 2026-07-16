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
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRotatingWriterKeepsWholeFrames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	w, err := NewRotatingWriter(path, 28)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"first", "second", "third"} {
		if err := WriteFrame(w, StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := readFrameStrings(t, path+RotatedSuffix), []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rotated frames=%q, want %q", got, want)
	}
	if got, want := readFrameStrings(t, path), []string{"third"}; !reflect.DeepEqual(got, want) {
		t.Errorf("current frames=%q, want %q", got, want)
	}
}

func TestRotatingWriterReopenAndOversizedFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	w, err := NewRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	if err := WriteFrame(w, StreamStderr, []byte("larger-than-cap")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	w, err = NewRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := WriteFrame(w, StreamStderr, []byte("next")); err != nil {
		t.Fatalf("WriteFrame(next): %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got, want := readFrameStrings(t, path+RotatedSuffix), []string{"larger-than-cap"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rotated frames=%q, want %q", got, want)
	}
	if got, want := readFrameStrings(t, path), []string{"next"}; !reflect.DeepEqual(got, want) {
		t.Errorf("current frames=%q, want %q", got, want)
	}
}

func TestRotatingWriterConfigurableGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	w, err := NewRotatingWriterWithFiles(path, 10, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriterWithFiles: %v", err)
	}
	for _, value := range []string{"one", "two", "three", "four"} {
		if err := WriteFrame(w, StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for generation, want := range map[int][]string{
		2: {"two"},
		1: {"three"},
		0: {"four"},
	} {
		if got := readFrameStrings(t, GenerationPath(path, generation)); !reflect.DeepEqual(got, want) {
			t.Errorf("generation %d frames=%q, want %q", generation, got, want)
		}
	}
	if _, err := os.Stat(GenerationPath(path, 3)); !os.IsNotExist(err) {
		t.Errorf("generation 3 exists or stat failed unexpectedly: %v", err)
	}
}

func TestRotatingWriterSingleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	w, err := NewRotatingWriterWithFiles(path, 10, 1)
	if err != nil {
		t.Fatalf("NewRotatingWriterWithFiles: %v", err)
	}
	for _, value := range []string{"old", "new"} {
		if err := WriteFrame(w, StreamStderr, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := readFrameStrings(t, path), []string{"new"}; !reflect.DeepEqual(got, want) {
		t.Errorf("current frames=%q, want %q", got, want)
	}
}

func readFrameStrings(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	var values []string
	for len(data) != 0 {
		frame, consumed, complete, err := DecodeFrame(data)
		if err != nil || !complete {
			t.Fatalf("DecodeFrame(%q): complete=%t err=%v", path, complete, err)
		}
		values = append(values, string(frame.Data))
		data = data[consumed:]
	}
	return values
}
