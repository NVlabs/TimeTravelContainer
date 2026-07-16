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
	"errors"
	"fmt"
	"os"
)

// RotatedSuffix is appended to the immediately preceding log generation.
const RotatedSuffix = ".1"

// RotatingWriter retains the current log and one previous generation. WriteFrame
// submits a whole encoded frame in one Write, so rotation never splits a frame.
type RotatingWriter struct {
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

// NewRotatingWriter opens path for append. maxBytes <= 0 disables rotation.
// A single frame larger than maxBytes is retained whole and may exceed the cap.
func NewRotatingWriter(path string, maxBytes int64) (*RotatingWriter, error) {
	if path == "" {
		return nil, errors.New("rotating console log path must not be empty")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening rotating console log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stating rotating console log: %w", err)
	}
	return &RotatingWriter{path: path, maxBytes: maxBytes, file: file, size: info.Size()}, nil
}

// Write implements io.Writer.
func (w *RotatingWriter) Write(data []byte) (int, error) {
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(data)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(data)
	w.size += int64(n)
	return n, err
}

// Close closes the active generation.
func (w *RotatingWriter) Close() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *RotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		w.file = nil
		return fmt.Errorf("closing console log before rotation: %w", err)
	}
	w.file = nil
	rotated := w.path + RotatedSuffix
	if err := os.Remove(rotated); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing old console log generation: %w", err)
	}
	if err := os.Rename(w.path, rotated); err != nil {
		return fmt.Errorf("rotating console log: %w", err)
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("opening new console log generation: %w", err)
	}
	w.file = file
	w.size = 0
	return nil
}
