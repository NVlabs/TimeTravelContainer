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
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// LocalDataStore stores authoritative standalone whole-file objects. Reflink
// is opportunistic; correctness always falls back to a complete copy.
type LocalDataStore struct {
	root   string
	rootFD *os.File
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

// OpenLocalDataStoreFD opens a donated bulk-object directory. All access stays
// descriptor-relative so an isolated Sentry can read and create graph objects
// without receiving a host pathname or one descriptor per object.
func OpenLocalDataStoreFD(directory *os.File) (*LocalDataStore, error) {
	if directory == nil {
		return nil, errors.New("TTC bulk-object directory descriptor is nil")
	}
	fd, err := unix.Dup(int(directory.Fd()))
	if err != nil {
		return nil, err
	}
	return &LocalDataStore{rootFD: os.NewFile(uintptr(fd), "ttc-graph-objects")}, nil
}

var dataTemporarySequence atomic.Uint64

const wholeFileCopyBufferSize = 1 << 20

// copyWholeFile deliberately uses Read and Write instead of io.Copy. The
// latter selects copy_file_range for two *os.File values, which would require
// broadening the isolated Sentry's host syscall policy for an operation that
// is needed only when reflinks are unavailable.
func copyWholeFile(destination, source *os.File) error {
	buffer := make([]byte, wholeFileCopyBufferSize)
	for {
		read, readErr := source.Read(buffer)
		if read > 0 {
			written := 0
			for written < read {
				n, writeErr := destination.Write(buffer[written:read])
				if writeErr != nil {
					return writeErr
				}
				if n == 0 {
					return io.ErrShortWrite
				}
				written += n
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// Close releases a donated directory. Path-opened stores own no persistent
// descriptor and Close is a no-op.
func (store *LocalDataStore) Close() error {
	if store == nil || store.rootFD == nil {
		return nil
	}
	err := store.rootFD.Close()
	store.rootFD = nil
	return err
}

func objectKindName(kind ObjectKind) (string, error) {
	switch kind {
	case ObjectKindPage:
		return "pages", nil
	case ObjectKindMemoryChunk:
		return "memory-chunks", nil
	case ObjectKindFile:
		return "files", nil
	default:
		return "", fmt.Errorf("object kind %d has no standalone bulk data", kind)
	}
}

func (store *LocalDataStore) objectPath(id ObjectID) (string, error) {
	if store.root == "" {
		return "", errors.New("donated TTC bulk-object store has no host path")
	}
	relative, err := objectRelativePath(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(store.root, relative), nil
}

func (store *LocalDataStore) mkdirAllRelative(relative string) error {
	if store.rootFD == nil {
		return errors.New("TTC bulk-object store has no directory descriptor")
	}
	current := ""
	for _, component := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("invalid TTC bulk-object directory %q", relative)
		}
		if current == "" {
			current = component
		} else {
			current = filepath.Join(current, component)
		}
		if err := unix.Mkdirat(int(store.rootFD.Fd()), current, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	return nil
}

func (store *LocalDataStore) openRelative(relative string, flags int, mode uint32) (*os.File, error) {
	if store.rootFD == nil {
		return nil, errors.New("TTC bulk-object store has no directory descriptor")
	}
	fd, err := unix.Openat(int(store.rootFD.Fd()), relative, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), relative), nil
}

func (store *LocalDataStore) syncRelativeDirectory(relative string) error {
	directory, err := store.openRelative(relative, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (store *LocalDataStore) createRelativeTemporary(directory, prefix string) (*os.File, string, error) {
	for attempts := 0; attempts < 100; attempts++ {
		name := filepath.Join(directory, fmt.Sprintf("%s%016x", prefix, dataTemporarySequence.Add(1)))
		file, err := store.openRelative(name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		return file, name, err
	}
	return nil, "", errors.New("allocating temporary TTC bulk-object file")
}

func objectRelativePath(id ObjectID) (string, error) {
	name, err := objectKindName(id.Kind)
	if err != nil {
		return "", err
	}
	if err := id.validate(); err != nil {
		return "", err
	}
	return filepath.Join(name, snapshotKey(id.Snapshot), fmt.Sprintf("%02x", byte(id.Ordinal)), id.key()+".data"), nil
}

// Path returns the trusted host path for a bulk object. It is used by the
// standalone filesystem backend while a working-origin file is exclusively
// mutable. Callers must not mutate objects from sealed origins.
func (store *LocalDataStore) Path(id ObjectID) (string, error) {
	return store.objectPath(id)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func installTemporary(temporary *os.File, temporaryPath, destination string, length int64, durable bool) error {
	if durable {
		if err := temporary.Sync(); err != nil {
			return err
		}
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
	if durable {
		if err := syncDirectory(filepath.Dir(destination)); err != nil {
			return err
		}
	}
	return nil
}

// Install writes exactly length bytes and atomically publishes id.
func (store *LocalDataStore) Install(id ObjectID, source io.Reader, length int64) error {
	return store.install(id, source, length, true)
}

// InstallDeferred writes and atomically publishes id without imposing a
// durability barrier. Filesystem repositories use this for ordinary guest
// writes, then call SyncObjects before making the graph transaction durable at
// fsync/sync or a lifecycle fence. Other LocalDataStore users retain Install's
// immediate-durability contract.
func (store *LocalDataStore) InstallDeferred(id ObjectID, source io.Reader, length int64) error {
	return store.install(id, source, length, false)
}

// ReplaceDeferred writes a complete cache object and atomically replaces any
// incomplete placeholder for id. It is intentionally non-durable: distributed
// cache contents are disposable and can be fetched again after a crash.
// Authoritative callers must continue using Install or InstallDeferred, whose
// create-if-absent semantics protect immutable objects from replacement.
func (store *LocalDataStore) ReplaceDeferred(id ObjectID, source io.Reader, length int64) error {
	if length < 0 {
		return errors.New("negative TTC bulk-object length")
	}
	relative, err := objectRelativePath(id)
	if err != nil {
		return err
	}
	if store.rootFD != nil {
		directory := filepath.Dir(relative)
		if err := store.mkdirAllRelative(directory); err != nil {
			return err
		}
		temporary, temporaryName, err := store.createRelativeTemporary(directory, ".replace-")
		if err != nil {
			return err
		}
		defer unix.Unlinkat(int(store.rootFD.Fd()), temporaryName, 0)
		written, copyErr := io.CopyN(temporary, source, length)
		closeErr := temporary.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("writing TTC replacement object after %d bytes: %w", written, errors.Join(copyErr, closeErr))
		}
		return unix.Renameat(int(store.rootFD.Fd()), temporaryName, int(store.rootFD.Fd()), relative)
	}
	destination, err := store.objectPath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".replace-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, copyErr := io.CopyN(temporary, source, length)
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("writing TTC replacement object after %d bytes: %w", written, errors.Join(copyErr, closeErr))
	}
	return os.Rename(temporaryPath, destination)
}

// CreateSparsePlaceholder creates an incomplete, zero-block cache file with
// the final object's logical length. Callers must track incompleteness
// separately and never expose placeholder bytes. It exists only so mmap/open
// paths have a stable inode that can later be atomically replaced.
func (store *LocalDataStore) CreateSparsePlaceholder(id ObjectID, length int64) error {
	if length < 0 {
		return errors.New("negative TTC bulk-object placeholder length")
	}
	relative, err := objectRelativePath(id)
	if err != nil {
		return err
	}
	if store.rootFD != nil {
		directory := filepath.Dir(relative)
		if err := store.mkdirAllRelative(directory); err != nil {
			return err
		}
		temporary, temporaryName, err := store.createRelativeTemporary(directory, ".placeholder-")
		if err != nil {
			return err
		}
		defer unix.Unlinkat(int(store.rootFD.Fd()), temporaryName, 0)
		truncateErr := temporary.Truncate(length)
		closeErr := temporary.Close()
		if truncateErr != nil || closeErr != nil {
			return errors.Join(truncateErr, closeErr)
		}
		if err := unix.Linkat(int(store.rootFD.Fd()), temporaryName, int(store.rootFD.Fd()), relative, 0); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return nil
	}
	destination, err := store.objectPath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".placeholder-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	truncateErr := temporary.Truncate(length)
	closeErr := temporary.Close()
	if truncateErr != nil || closeErr != nil {
		return errors.Join(truncateErr, closeErr)
	}
	if err := os.Link(temporaryPath, destination); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func (store *LocalDataStore) install(id ObjectID, source io.Reader, length int64, durable bool) error {
	if length < 0 {
		return errors.New("negative TTC bulk-object length")
	}
	relative, err := objectRelativePath(id)
	if err != nil {
		return err
	}
	if store.rootFD != nil {
		directory := filepath.Dir(relative)
		if err := store.mkdirAllRelative(directory); err != nil {
			return err
		}
		temporary, temporaryName, err := store.createRelativeTemporary(directory, ".object-")
		if err != nil {
			return err
		}
		defer unix.Unlinkat(int(store.rootFD.Fd()), temporaryName, 0)
		written, err := io.CopyN(temporary, source, length)
		if err != nil {
			temporary.Close()
			return fmt.Errorf("writing TTC bulk object after %d bytes: %w", written, err)
		}
		if durable {
			if err := temporary.Sync(); err != nil {
				temporary.Close()
				return err
			}
		}
		if err := temporary.Close(); err != nil {
			return err
		}
		if err := unix.Linkat(int(store.rootFD.Fd()), temporaryName, int(store.rootFD.Fd()), relative, 0); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return err
			}
			existing, openErr := store.openRelative(relative, unix.O_RDONLY, 0)
			if openErr != nil {
				return openErr
			}
			info, statErr := existing.Stat()
			closeErr := existing.Close()
			if statErr != nil || closeErr != nil || info.Size() != length {
				return fmt.Errorf("existing immutable object %q has unexpected length: %w", relative, errors.Join(statErr, closeErr))
			}
		}
		if durable {
			return store.syncRelativeDirectory(directory)
		}
		return nil
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
	return installTemporary(temporary, temporaryPath, destination, length, durable)
}

// Clone installs destination from a complete source object. If tryReflink is
// true, it first attempts FICLONE and transparently uses a whole-file copy when
// unavailable. Callers should pass false after probing a filesystem that does
// not support reflinks, avoiding a failing ioctl for every privatized file.
func (store *LocalDataStore) Clone(sourceID, destinationID ObjectID, tryReflink bool) error {
	return store.clone(sourceID, destinationID, tryReflink, true)
}

// CloneDeferred installs a complete clone without synchronizing it. The
// enclosing filesystem repository must pass destinationID to SyncObjects
// before synchronizing the graph transaction that names it.
func (store *LocalDataStore) CloneDeferred(sourceID, destinationID ObjectID, tryReflink bool) error {
	return store.clone(sourceID, destinationID, tryReflink, false)
}

func (store *LocalDataStore) clone(sourceID, destinationID ObjectID, tryReflink, durable bool) error {
	if store.rootFD != nil {
		source, err := store.Open(sourceID, -1)
		if err != nil {
			return err
		}
		defer source.Close()
		info, err := source.Stat()
		if err != nil {
			return err
		}
		relative, err := objectRelativePath(destinationID)
		if err != nil {
			return err
		}
		directory := filepath.Dir(relative)
		if err := store.mkdirAllRelative(directory); err != nil {
			return err
		}
		temporary, temporaryName, err := store.createRelativeTemporary(directory, ".clone-")
		if err != nil {
			return err
		}
		defer unix.Unlinkat(int(store.rootFD.Fd()), temporaryName, 0)
		var cloneErr error
		if tryReflink {
			cloneErr = unix.IoctlFileClone(int(temporary.Fd()), int(source.Fd()))
		} else {
			cloneErr = unix.EOPNOTSUPP
		}
		if cloneErr != nil {
			if err := temporary.Truncate(0); err != nil {
				temporary.Close()
				return err
			}
			if _, err := source.Seek(0, io.SeekStart); err != nil {
				temporary.Close()
				return err
			}
			if err := copyWholeFile(temporary, source); err != nil {
				temporary.Close()
				return err
			}
		}
		if durable {
			if err := temporary.Sync(); err != nil {
				temporary.Close()
				return err
			}
		}
		if err := temporary.Close(); err != nil {
			return err
		}
		if err := unix.Linkat(int(store.rootFD.Fd()), temporaryName, int(store.rootFD.Fd()), relative, 0); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return err
			}
			existing, openErr := store.openRelative(relative, unix.O_RDONLY, 0)
			if openErr != nil {
				return openErr
			}
			existingInfo, statErr := existing.Stat()
			closeErr := existing.Close()
			if statErr != nil || closeErr != nil || existingInfo.Size() != info.Size() {
				return fmt.Errorf("existing immutable object %q has unexpected length: %w", relative, errors.Join(statErr, closeErr))
			}
		}
		if durable {
			return store.syncRelativeDirectory(directory)
		}
		return nil
	}
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
	var cloneErr error
	if tryReflink {
		cloneErr = unix.IoctlFileClone(int(temporary.Fd()), int(source.Fd()))
	} else {
		cloneErr = unix.EOPNOTSUPP
	}
	if cloneErr != nil {
		if err := temporary.Truncate(0); err != nil {
			temporary.Close()
			return err
		}
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			temporary.Close()
			return err
		}
		if err := copyWholeFile(temporary, source); err != nil {
			temporary.Close()
			return err
		}
	}
	return installTemporary(temporary, temporaryPath, destination, info.Size(), durable)
}

// SyncObjects makes complete object contents and every directory entry in
// their descriptor-relative path durable. Files precede leaf directories and
// directories precede their parents, so a subsequently synchronized graph can
// never name an object omitted by a completed durability fence.
func (store *LocalDataStore) SyncObjects(ids []ObjectID) error {
	if len(ids) == 0 {
		return nil
	}
	directories := make(map[string]struct{})
	for _, id := range ids {
		relative, err := objectRelativePath(id)
		if err != nil {
			return err
		}
		file, err := store.Open(id, -1)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil || closeErr != nil {
			return errors.Join(syncErr, closeErr)
		}
		for directory := filepath.Dir(relative); directory != "."; directory = filepath.Dir(directory) {
			directories[directory] = struct{}{}
		}
		directories["."] = struct{}{}
	}
	ordered := make([]string, 0, len(directories))
	for directory := range directories {
		ordered = append(ordered, directory)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return strings.Count(ordered[i], string(filepath.Separator)) > strings.Count(ordered[j], string(filepath.Separator))
	})
	for _, directory := range ordered {
		if store.rootFD != nil {
			if err := store.syncRelativeDirectory(directory); err != nil {
				return err
			}
			continue
		}
		if err := syncDirectory(filepath.Join(store.root, directory)); err != nil {
			return err
		}
	}
	return nil
}

// Open returns a read-only complete object after verifying its exact length.
func (store *LocalDataStore) Open(id ObjectID, length int64) (*os.File, error) {
	return store.OpenFile(id, unix.O_RDONLY, length)
}

// OpenFile opens a complete bulk object using descriptor-relative access when
// the store was donated to an isolated Sentry.
func (store *LocalDataStore) OpenFile(id ObjectID, flags int, length int64) (*os.File, error) {
	var file *os.File
	var err error
	if store.rootFD != nil {
		relative, pathErr := objectRelativePath(id)
		if pathErr != nil {
			return nil, pathErr
		}
		file, err = store.openRelative(relative, flags, 0)
	} else {
		path, pathErr := store.objectPath(id)
		if pathErr != nil {
			return nil, pathErr
		}
		file, err = os.OpenFile(path, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || length >= 0 && info.Size() != length || !info.Mode().IsRegular() {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("TTC bulk object %s has length %d, want %d", id.key(), info.Size(), length)
	}
	return file, nil
}

// Fetch implements ObjectSource for authoritative standalone objects and
// node-local caches. The graph-independent source API intentionally carries
// the length with the reader so callers can reject incomplete providers.
func (store *LocalDataStore) Fetch(_ context.Context, id ObjectID) (io.ReadCloser, uint64, error) {
	file, err := store.Open(id, -1)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, 0, fmt.Errorf("TTC bulk object %s is not a regular file", id.key())
	}
	return file, uint64(info.Size()), nil
}

// Remove deletes an object whose authoritative graph record was reclaimed.
func (store *LocalDataStore) Remove(id ObjectID) error {
	if store.rootFD != nil {
		relative, err := objectRelativePath(id)
		if err != nil {
			return err
		}
		if err := unix.Unlinkat(int(store.rootFD.Fd()), relative, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	}
	path, err := store.objectPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// VerifyReferenced rejects graph metadata that names missing, truncated, or
// non-regular bulk data. Install publishes bulk bytes before the graph WAL can
// reference them, so any failure here indicates storage loss or a violated
// transaction boundary and must not be hidden by graph recovery.
func (store *LocalDataStore) VerifyReferenced(graph *Store) error {
	if graph == nil {
		return errors.New("TTC snapshot graph is nil")
	}
	for _, object := range graph.Inspect().Objects {
		if object.BulkLength == 0 {
			continue
		}
		if object.BulkLength > math.MaxInt64 {
			return fmt.Errorf("validating referenced TTC bulk object %s: length %d exceeds host file limit", object.ID.key(), object.BulkLength)
		}
		file, err := store.Open(object.ID, int64(object.BulkLength))
		if err != nil {
			return fmt.Errorf("validating referenced TTC bulk object %s: %w", object.ID.key(), err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("closing referenced TTC bulk object %s: %w", object.ID.key(), err)
		}
	}
	return nil
}

func quarantineDataFile(path, quarantine string) error {
	if err := os.MkdirAll(quarantine, 0700); err != nil {
		return err
	}
	for attempts := 0; attempts < 100; attempts++ {
		destination := filepath.Join(quarantine, fmt.Sprintf("unknown-%016x-%s", dataTemporarySequence.Add(1), filepath.Base(path)))
		if err := os.Rename(path, destination); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("allocating quarantine name for unknown TTC object %q", path)
}

// ReclaimUnreferenced removes complete bulk objects absent from graph and
// quarantines unknown names. Callers serialize it with graph/data commits.
func (store *LocalDataStore) ReclaimUnreferenced(graph *Store) (removed, quarantined int, err error) {
	quarantine := filepath.Join(store.root, "quarantine")
	for _, kind := range []string{"pages", "memory-chunks", "files"} {
		base := filepath.Join(store.root, kind)
		var directories []string
		walkErr := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, fs.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if entry.IsDir() {
				if path != base {
					directories = append(directories, path)
				}
				return nil
			}
			if strings.HasPrefix(entry.Name(), ".object-") || strings.HasPrefix(entry.Name(), ".clone-") || strings.HasPrefix(entry.Name(), ".replace-") {
				// Install and Clone publish with link(2) only after the temporary
				// file is complete and durable. A named temporary is therefore
				// never authoritative and is safe to remove while reconciliation
				// excludes active graph/data transactions.
				if err := os.Remove(path); err != nil {
					return err
				}
				removed++
				return nil
			}
			name := strings.TrimSuffix(entry.Name(), ".data")
			id, parseErr := ParseObjectID(name)
			if parseErr != nil || name == entry.Name() {
				if err := quarantineDataFile(path, quarantine); err != nil {
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
		sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
		for _, directory := range directories {
			if err := os.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, unix.ENOTEMPTY) {
				return removed, quarantined, err
			}
		}
		if err := os.Remove(base); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, unix.ENOTEMPTY) {
			return removed, quarantined, err
		}
	}
	return removed, quarantined, nil
}
