// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// RotatedSuffix is appended to the immediately preceding log generation.
const RotatedSuffix = ".1"

// GenerationPath returns the path for a log generation. Generation zero is the
// active log; positive generations are older, with larger values being older.
func GenerationPath(path string, generation int) string {
	if generation <= 0 {
		return path
	}
	return path + "." + strconv.Itoa(generation)
}

// RetentionStats records cumulative encoded console-log data evicted by retention.
type RetentionStats struct {
	EvictedBytes       int64     `json:"evicted_bytes"`
	EvictedGenerations uint64    `json:"evicted_generations"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
}

// RetentionStatsPath returns the durable accounting path for logPath.
func RetentionStatsPath(logPath string) string { return logPath + ".retention.json" }

// ReadRetentionStats loads durable retention accounting.
func ReadRetentionStats(path string) (RetentionStats, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return RetentionStats{}, nil
	}
	if err != nil {
		return RetentionStats{}, fmt.Errorf("reading console retention stats: %w", err)
	}
	var stats RetentionStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return RetentionStats{}, fmt.Errorf("decoding console retention stats: %w", err)
	}
	if stats.EvictedBytes < 0 {
		return RetentionStats{}, errors.New("console retention stats contain negative evicted bytes")
	}
	return stats, nil
}

// RotatingWriter retains a configurable number of generations. WriteFrame
// submits a whole encoded frame in one Write, so rotation never splits a frame.
type RotatingWriter struct {
	path      string
	maxBytes  int64
	maxFiles  int
	file      *os.File
	size      int64
	statsPath string
	stats     RetentionStats
}

// NewRotatingWriter opens path for append. maxBytes <= 0 disables rotation.
// A single frame larger than maxBytes is retained whole and may exceed the cap.
func NewRotatingWriter(path string, maxBytes int64) (*RotatingWriter, error) {
	return NewRotatingWriterWithFiles(path, maxBytes, 2)
}

// NewRotatingWriterWithFiles is equivalent to NewRotatingWriter, retaining
// maxFiles generations including the active log.
func NewRotatingWriterWithFiles(path string, maxBytes int64, maxFiles int) (*RotatingWriter, error) {
	return NewRotatingWriterWithStats(path, maxBytes, maxFiles, "")
}

// NewRotatingWriterWithStats also persists cumulative generation-eviction stats.
func NewRotatingWriterWithStats(path string, maxBytes int64, maxFiles int, statsPath string) (*RotatingWriter, error) {
	if path == "" {
		return nil, errors.New("rotating console log path must not be empty")
	}
	if maxFiles < 1 {
		return nil, fmt.Errorf("rotating console log max files must be at least 1, got %d", maxFiles)
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
	w := &RotatingWriter{path: path, maxBytes: maxBytes, maxFiles: maxFiles, file: file, size: info.Size(), statsPath: statsPath}
	if statsPath != "" {
		w.stats, err = ReadRetentionStats(statsPath)
		if err != nil {
			file.Close()
			return nil, err
		}
	}
	for generation := maxFiles; ; generation++ {
		generationPath := GenerationPath(path, generation)
		info, statErr := os.Stat(generationPath)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			file.Close()
			return nil, fmt.Errorf("stating excess console log generation %d: %w", generation, statErr)
		}
		if err := os.Remove(generationPath); err != nil {
			file.Close()
			return nil, fmt.Errorf("removing excess console log generation %d: %w", generation, err)
		}
		if err := w.recordEviction(info.Size()); err != nil {
			file.Close()
			return nil, err
		}
	}
	return w, nil
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

func (w *RotatingWriter) recordEviction(bytes int64) error {
	if bytes < 0 {
		return errors.New("cannot record a negative console eviction")
	}
	if w.statsPath == "" {
		return nil
	}
	w.stats.EvictedBytes += bytes
	w.stats.EvictedGenerations++
	w.stats.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(&w.stats)
	if err != nil {
		return fmt.Errorf("encoding console retention stats: %w", err)
	}
	dir := filepath.Dir(w.statsPath)
	temp, err := os.CreateTemp(dir, ".console-retention-*")
	if err != nil {
		return fmt.Errorf("creating console retention stats: %w", err)
	}
	tempName := temp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return fmt.Errorf("setting console retention stats permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("writing console retention stats: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("syncing console retention stats: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("closing console retention stats: %w", err)
	}
	if err := os.Rename(tempName, w.statsPath); err != nil {
		return fmt.Errorf("publishing console retention stats: %w", err)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening console retention stats directory: %w", err)
	}
	if err := dirFile.Sync(); err != nil {
		dirFile.Close()
		return fmt.Errorf("syncing console retention stats directory: %w", err)
	}
	if err := dirFile.Close(); err != nil {
		return fmt.Errorf("closing console retention stats directory: %w", err)
	}
	ok = true
	return nil
}

func (w *RotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		w.file = nil
		return fmt.Errorf("closing console log before rotation: %w", err)
	}
	w.file = nil
	if w.maxFiles == 1 {
		info, statErr := os.Stat(w.path)
		if statErr != nil && !os.IsNotExist(statErr) {
			return fmt.Errorf("stating active console log during rotation: %w", statErr)
		}
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing active console log during rotation: %w", err)
		}
		if statErr == nil {
			if err := w.recordEviction(info.Size()); err != nil {
				return err
			}
		}
	} else {
		for generation := w.maxFiles - 1; generation >= 1; generation-- {
			destination := GenerationPath(w.path, generation)
			var evictedSize int64
			evicted := false
			if generation == w.maxFiles-1 {
				if info, err := os.Stat(destination); err == nil {
					evictedSize = info.Size()
					evicted = true
				} else if !os.IsNotExist(err) {
					return fmt.Errorf("stating oldest console log generation: %w", err)
				}
			}
			if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing console log generation %d: %w", generation, err)
			}
			if evicted {
				if err := w.recordEviction(evictedSize); err != nil {
					return err
				}
			}
			source := GenerationPath(w.path, generation-1)
			if err := os.Rename(source, destination); err != nil {
				if os.IsNotExist(err) && generation > 1 {
					continue
				}
				return fmt.Errorf("rotating console log generation %d: %w", generation, err)
			}
		}
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("opening new console log generation: %w", err)
	}
	w.file = file
	w.size = 0
	return nil
}
