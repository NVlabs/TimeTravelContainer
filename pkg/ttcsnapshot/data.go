// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ttcsnapshot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// LocalDataStore stores authoritative standalone whole-file objects. Reflink
// is opportunistic; correctness always falls back to a complete copy.
type LocalDataStore struct {
	root string
}

// OpenLocalDataStore opens a standalone bulk-object directory.
func OpenLocalDataStore(root string) (*LocalDataStore, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("TTC bulk-object directory must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &LocalDataStore{root: root}, nil
}

func objectKindName(kind ObjectKind) (string, error) {
	switch kind {
	case ObjectKindPage:
		return "pages", nil
	case ObjectKindFile:
		return "files", nil
	default:
		return "", fmt.Errorf("object kind %d has no standalone bulk data", kind)
	}
}

func (store *LocalDataStore) objectPath(id ObjectID) (string, error) {
	name, err := objectKindName(id.Kind)
	if err != nil {
		return "", err
	}
	if err := id.validate(); err != nil {
		return "", err
	}
	return filepath.Join(store.root, name, snapshotKey(id.Snapshot), fmt.Sprintf("%02x", byte(id.Ordinal)), id.key()+".data"), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func installTemporary(temporary *os.File, temporaryPath, destination string, length int64) error {
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, destination); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, statErr := os.Stat(destination)
		if statErr != nil || info.Size() != length {
			return fmt.Errorf("existing immutable object %q has unexpected length", destination)
		}
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	return nil
}

// Install writes exactly length bytes and atomically publishes id.
func (store *LocalDataStore) Install(id ObjectID, source io.Reader, length int64) error {
	if length < 0 {
		return errors.New("negative TTC bulk-object length")
	}
	destination, err := store.objectPath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".object-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, err := io.CopyN(temporary, source, length)
	if err != nil {
		temporary.Close()
		return fmt.Errorf("writing TTC bulk object after %d bytes: %w", written, err)
	}
	return installTemporary(temporary, temporaryPath, destination, length)
}

// Clone installs destination from a complete source object. It first attempts
// FICLONE and transparently uses a whole-file copy when unavailable.
func (store *LocalDataStore) Clone(sourceID, destinationID ObjectID) error {
	sourcePath, err := store.objectPath(sourceID)
	if err != nil {
		return err
	}
	destination, err := store.objectPath(destinationID)
	if err != nil {
		return err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".clone-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := unix.IoctlFileClone(int(temporary.Fd()), int(source.Fd())); err != nil {
		if err := temporary.Truncate(0); err != nil {
			temporary.Close()
			return err
		}
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			temporary.Close()
			return err
		}
		if _, err := io.Copy(temporary, source); err != nil {
			temporary.Close()
			return err
		}
	}
	return installTemporary(temporary, temporaryPath, destination, info.Size())
}

// Open returns a read-only complete object after verifying its exact length.
func (store *LocalDataStore) Open(id ObjectID, length int64) (*os.File, error) {
	path, err := store.objectPath(id)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != length || !info.Mode().IsRegular() {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("TTC bulk object %s has length %d, want %d", id.key(), info.Size(), length)
	}
	return file, nil
}

// Remove deletes an object whose authoritative graph record was reclaimed.
func (store *LocalDataStore) Remove(id ObjectID) error {
	path, err := store.objectPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ReclaimUnreferenced removes complete bulk objects absent from graph and
// quarantines unknown names. Callers serialize it with graph/data commits.
func (store *LocalDataStore) ReclaimUnreferenced(graph *Store) (removed, quarantined int, err error) {
	quarantine := filepath.Join(store.root, "quarantine")
	for _, kind := range []string{"pages", "files"} {
		base := filepath.Join(store.root, kind)
		walkErr := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, fs.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				return nil
			}
			name := strings.TrimSuffix(entry.Name(), ".data")
			id, parseErr := ParseObjectID(name)
			if parseErr != nil || name == entry.Name() {
				if err := os.MkdirAll(quarantine, 0700); err != nil {
					return err
				}
				destination := filepath.Join(quarantine, fmt.Sprintf("%d-%s", quarantined, entry.Name()))
				if err := os.Rename(path, destination); err != nil {
					return err
				}
				quarantined++
				return nil
			}
			if _, ok := graph.Object(id); ok {
				return nil
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			removed++
			return nil
		})
		if walkErr != nil {
			return removed, quarantined, walkErr
		}
	}
	return removed, quarantined, nil
}
