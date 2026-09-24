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
	"bytes"
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
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

// syncParallelism bounds the fsyncs one durability fence runs at once.
// Filesystems write concurrently synced files out together and fold their
// fsyncs into shared journal commits, where one at a time waits for a
// commit each.
const syncParallelism = 16

// LocalDataStore stores authoritative standalone whole-file objects. Reflink
// is opportunistic; correctness always falls back to a complete copy.
type LocalDataStore struct {
	root   string
	rootFD *os.File
	gcLock *os.File
	// gcMu serializes directory reconciliation with other reconciliation
	// goroutines using this store. Graph mutations are transactional, but the
	// subsequent physical sweep is deliberately outside the graph lock; two
	// independent lifecycle completions may therefore reach it concurrently.
	gcMu sync.RWMutex
	// unsyncedNames are objects installed without a durability barrier whose
	// directory entries a later fence must make durable, keyed by object with
	// a generation so a concurrent reinstall is not forgotten by a sync that
	// began before it. Removal forgets them: a removed object has no name to
	// make durable.
	namesMu       sync.Mutex
	unsyncedNames map[string]unsyncedName
}

type unsyncedName struct {
	id         ObjectID
	generation uint64
}

func (store *LocalDataStore) noteUnsyncedName(id ObjectID) {
	store.namesMu.Lock()
	defer store.namesMu.Unlock()
	if store.unsyncedNames == nil {
		store.unsyncedNames = make(map[string]unsyncedName)
	}
	current := store.unsyncedNames[id.key()]
	store.unsyncedNames[id.key()] = unsyncedName{id: id, generation: current.generation + 1}
}

func (store *LocalDataStore) forgetUnsyncedName(id ObjectID) {
	store.namesMu.Lock()
	delete(store.unsyncedNames, id.key())
	store.namesMu.Unlock()
}

// SyncNewNames makes durable the directory entries of every object installed
// without a durability barrier since the last call, and nothing else. It is
// the part of a durability fence that a WAL naming those objects depends on;
// their payloads are the fence's separate concern.
func (store *LocalDataStore) SyncNewNames() error {
	store.namesMu.Lock()
	snapshot := make(map[string]unsyncedName, len(store.unsyncedNames))
	ids := make([]ObjectID, 0, len(store.unsyncedNames))
	for key, name := range store.unsyncedNames {
		snapshot[key] = name
		ids = append(ids, name.id)
	}
	store.namesMu.Unlock()
	if err := store.SyncObjectDirectories(ids); err != nil {
		return err
	}
	store.forgetSyncedNames(snapshot)
	return nil
}

// WithGraphMutation excludes physical garbage collection while installed bulk
// bytes are being committed into their authoritative graph. Immutable object
// construction is intentionally ordered as:
//
//	install bytes -> commit graph object
//
// because a graph record must never name missing data after a crash. Without
// this fence, a concurrent collector could observe the final object filename
// in the short interval before the graph commit and unlink it as unreachable.
// The caller must include both steps in mutate. A per-store RW lock covers
// goroutines sharing one handle; .gc.lock extends the fence across independent
// handles and donated directory descriptors in other TTC processes.
func (store *LocalDataStore) WithGraphMutation(mutate func() error) error {
	if store == nil || store.gcLock == nil || mutate == nil {
		return errors.New("invalid TTC graph-data mutation")
	}
	store.gcMu.RLock()
	defer store.gcMu.RUnlock()
	if err := syscall.Flock(int(store.gcLock.Fd()), syscall.LOCK_SH); err != nil {
		return err
	}
	defer syscall.Flock(int(store.gcLock.Fd()), syscall.LOCK_UN)
	return mutate()
}

// OpenLocalDataStore opens a standalone bulk-object directory.
func OpenLocalDataStore(root string) (*LocalDataStore, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("TTC bulk-object directory must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".gc.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	return &LocalDataStore{root: root, gcLock: lock}, nil
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
	rootFD := os.NewFile(uintptr(fd), "ttc-graph-objects")
	lockFD, err := unix.Openat(int(rootFD.Fd()), ".gc.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		rootFD.Close()
		return nil, err
	}
	return &LocalDataStore{rootFD: rootFD, gcLock: os.NewFile(uintptr(lockFD), "ttc-graph-objects-gc-lock")}, nil
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

// Close releases the collector fence and any donated directory descriptor.
func (store *LocalDataStore) Close() error {
	if store == nil {
		return nil
	}
	var result error
	if store.gcLock != nil {
		result = errors.Join(result, store.gcLock.Close())
		store.gcLock = nil
	}
	if store.rootFD != nil {
		result = errors.Join(result, store.rootFD.Close())
		store.rootFD = nil
	}
	return result
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
	if err := store.install(id, source, length, false); err != nil {
		return err
	}
	store.noteUnsyncedName(id)
	return nil
}

// ReplaceDeferred writes a complete cache object and atomically replaces any
// incomplete placeholder for id. It is intentionally non-durable: distributed
// cache contents are disposable and can be fetched again after a crash.
// Authoritative callers must continue using Install or InstallDeferred, whose
// create-if-absent semantics protect immutable objects from replacement.
func (store *LocalDataStore) ReplaceDeferred(id ObjectID, source io.Reader, length int64) error {
	if err := store.replaceDeferred(id, source, length); err != nil {
		return err
	}
	store.noteUnsyncedName(id)
	return nil
}

func (store *LocalDataStore) replaceDeferred(id ObjectID, source io.Reader, length int64) error {
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

// Extent is Data at Offset of an object that is zero everywhere else.
type Extent struct {
	Offset int64
	Data   []byte
}

// InstallExtentsDeferred is InstallDeferred for an object of length bytes
// holding extents, in ascending order, and zeros elsewhere. The zeros are
// left as holes, and nothing is copied or scanned for them. Writeback of the
// object starts at once, so a sync of many such objects finds most of them
// written already.
func (store *LocalDataStore) InstallExtentsDeferred(id ObjectID, length int64, extents []Extent) error {
	end := int64(0)
	for _, extent := range extents {
		if extent.Offset < end || extent.Offset+int64(len(extent.Data)) > length {
			return fmt.Errorf("TTC bulk object extent at %d of %d bytes is out of order or past %d", extent.Offset, len(extent.Data), length)
		}
		end = extent.Offset + int64(len(extent.Data))
	}
	if err := store.installWith(id, length, false, func(file *os.File) error {
		return writeExtents(file, length, extents)
	}); err != nil {
		return err
	}
	store.noteUnsyncedName(id)
	return nil
}

// writeExtents writes extents to the empty file, a run of contiguous ones per
// system call, and sizes it to length.
func writeExtents(file *os.File, length int64, extents []Extent) error {
	for begin := 0; begin < len(extents); {
		end := begin + 1
		for end < len(extents) && extents[end].Offset == extents[end-1].Offset+int64(len(extents[end-1].Data)) {
			end++
		}
		buffers := make([][]byte, 0, end-begin)
		for _, extent := range extents[begin:end] {
			buffers = append(buffers, extent.Data)
		}
		offset := extents[begin].Offset
		for len(buffers) != 0 {
			n, err := unix.Pwritev(int(file.Fd()), buffers, offset)
			if err != nil {
				return fmt.Errorf("writing TTC bulk object at %d: %w", offset, err)
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			offset += int64(n)
			for n != 0 {
				if n >= len(buffers[0]) {
					n -= len(buffers[0])
					buffers = buffers[1:]
					continue
				}
				buffers[0] = buffers[0][n:]
				n = 0
			}
		}
		begin = end
	}
	if err := file.Truncate(length); err != nil {
		return err
	}
	// Only a hint: a later sync is what makes the object durable, so a
	// filesystem that does not take it loses nothing.
	_ = unix.SyncFileRange(int(file.Fd()), 0, 0, unix.SYNC_FILE_RANGE_WRITE)
	return nil
}

func (store *LocalDataStore) install(id ObjectID, source io.Reader, length int64, durable bool) error {
	return store.installWith(id, length, durable, func(file *os.File) error {
		if written, err := copySparse(file, source, length); err != nil {
			return fmt.Errorf("writing TTC bulk object after %d bytes: %w", written, err)
		}
		return nil
	})
}

// installWith atomically publishes id, which fill writes into an empty
// temporary file, as an object of length bytes.
func (store *LocalDataStore) installWith(id ObjectID, length int64, durable bool, fill func(*os.File) error) error {
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
		if err := fill(temporary); err != nil {
			temporary.Close()
			return err
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
	if err := fill(temporary); err != nil {
		temporary.Close()
		return err
	}
	return installTemporary(temporary, temporaryPath, destination, length, durable)
}

// sparsePageSize is the granularity at which copySparse leaves holes.
const sparsePageSize = 4096

var zeroSparsePage [sparsePageSize]byte

// copySparse copies exactly length bytes of source into the empty file, and
// leaves a hole wherever a page of them is all zeros, as much of a memory
// chunk is: a chunk holds only the pages its graph did not have yet. It
// returns how many bytes it consumed from source.
func copySparse(file *os.File, source io.Reader, length int64) (int64, error) {
	buffer := make([]byte, min(length, 1<<20))
	var offset int64
	for offset < length {
		n, err := io.ReadFull(source, buffer[:min(int64(len(buffer)), length-offset)])
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			return offset + int64(n), err
		}
		for start := 0; start < n; {
			end := min(start+sparsePageSize, n)
			if bytes.Equal(buffer[start:end], zeroSparsePage[:end-start]) {
				start = end
				continue
			}
			for end < n {
				next := min(end+sparsePageSize, n)
				if bytes.Equal(buffer[end:next], zeroSparsePage[:next-end]) {
					break
				}
				end = next
			}
			if _, err := file.WriteAt(buffer[start:end], offset+int64(start)); err != nil {
				return offset + int64(start), err
			}
			start = end
		}
		offset += int64(n)
	}
	return offset, file.Truncate(length)
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
	if err := store.clone(sourceID, destinationID, tryReflink, false); err != nil {
		return err
	}
	store.noteUnsyncedName(destinationID)
	return nil
}

// LinkDeferred installs destination as a second name for source's bytes,
// without synchronizing it. From then on a write through either name is a
// write to both, so the caller must be about to release source and must know
// that nothing else can read it. The enclosing repository must make
// destination's name durable, as SyncNewNames does, before synchronizing the
// graph transaction that names it.
func (store *LocalDataStore) LinkDeferred(sourceID, destinationID ObjectID) error {
	source, err := objectRelativePath(sourceID)
	if err != nil {
		return err
	}
	destination, err := objectRelativePath(destinationID)
	if err != nil {
		return err
	}
	if store.rootFD != nil {
		if err := store.mkdirAllRelative(filepath.Dir(destination)); err != nil {
			return err
		}
		if err := unix.Linkat(int(store.rootFD.Fd()), source, int(store.rootFD.Fd()), destination, 0); err != nil {
			return err
		}
	} else {
		if store.root == "" {
			return errors.New("donated TTC bulk-object store has no host path")
		}
		if err := os.MkdirAll(filepath.Join(store.root, filepath.Dir(destination)), 0700); err != nil {
			return err
		}
		if err := os.Link(filepath.Join(store.root, source), filepath.Join(store.root, destination)); err != nil {
			return err
		}
	}
	store.noteUnsyncedName(destinationID)
	return nil
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
	names := store.unsyncedNamesOf(ids)
	directories := make(map[string]struct{})
	for _, id := range ids {
		relative, err := objectRelativePath(id)
		if err != nil {
			return err
		}
		for directory := filepath.Dir(relative); directory != "."; directory = filepath.Dir(directory) {
			directories[directory] = struct{}{}
		}
		directories["."] = struct{}{}
	}
	var group errgroup.Group
	group.SetLimit(syncParallelism)
	for _, id := range ids {
		group.Go(func() error {
			file, err := store.Open(id, -1)
			if err != nil {
				return err
			}
			return errors.Join(file.Sync(), file.Close())
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	if err := store.syncDirectoriesDeepestFirst(directories); err != nil {
		return err
	}
	// Their names are durable now too, so a later SyncNewNames need not
	// sync them again.
	store.forgetSyncedNames(names)
	return nil
}

// unsyncedNamesOf returns the entries of unsyncedNames for ids, as they are
// before a sync that covers them begins.
func (store *LocalDataStore) unsyncedNamesOf(ids []ObjectID) map[string]unsyncedName {
	store.namesMu.Lock()
	defer store.namesMu.Unlock()
	names := make(map[string]unsyncedName)
	for _, id := range ids {
		if name, ok := store.unsyncedNames[id.key()]; ok {
			names[id.key()] = name
		}
	}
	return names
}

// forgetSyncedNames drops the entries of synced from unsyncedNames, unless the
// object was installed again after the sync began.
func (store *LocalDataStore) forgetSyncedNames(synced map[string]unsyncedName) {
	store.namesMu.Lock()
	defer store.namesMu.Unlock()
	for key, name := range synced {
		if current, ok := store.unsyncedNames[key]; ok && current.generation == name.generation {
			delete(store.unsyncedNames, key)
		}
	}
}

// SyncObjectDirectories makes the names of objects durable without syncing
// their payloads.
//
// A guest fsync of one file must make the metadata WAL durable, and the WAL
// names every object any branch has published since the last full fence.
// Those objects must still be openable if the WAL survives a crash, so their
// directory entries have to be durable first. Their bytes need not be: a file
// nobody fsynced may come back short, which is what a crash promises it.
func (store *LocalDataStore) SyncObjectDirectories(ids []ObjectID) error {
	if len(ids) == 0 {
		return nil
	}
	directories := make(map[string]struct{})
	for _, id := range ids {
		relative, err := objectRelativePath(id)
		if err != nil {
			return err
		}
		for directory := filepath.Dir(relative); directory != "."; directory = filepath.Dir(directory) {
			directories[directory] = struct{}{}
		}
		directories["."] = struct{}{}
	}
	return store.syncDirectoriesDeepestFirst(directories)
}

// syncDirectoriesDeepestFirst syncs directories a level at a time, deepest
// first, so that no directory's entry is made durable before the entries it
// holds; those of one level are synced concurrently.
func (store *LocalDataStore) syncDirectoriesDeepestFirst(directories map[string]struct{}) error {
	levels := make(map[int][]string)
	for directory := range directories {
		depth := 0
		if directory != "." {
			depth = strings.Count(directory, string(filepath.Separator)) + 1
		}
		levels[depth] = append(levels[depth], directory)
	}
	depths := make([]int, 0, len(levels))
	for depth := range levels {
		depths = append(depths, depth)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(depths)))
	for _, depth := range depths {
		var group errgroup.Group
		group.SetLimit(syncParallelism)
		for _, directory := range levels[depth] {
			group.Go(func() error {
				if store.rootFD != nil {
					return store.syncRelativeDirectory(directory)
				}
				return syncDirectory(filepath.Join(store.root, directory))
			})
		}
		if err := group.Wait(); err != nil {
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
	store.forgetUnsyncedName(id)
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

func (store *LocalDataStore) quarantineRelative(relative string) error {
	if err := unix.Mkdirat(int(store.rootFD.Fd()), "quarantine", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	quarantine, err := store.openRelative("quarantine", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("opening TTC object quarantine: %w", err)
	}
	defer quarantine.Close()
	for attempts := 0; attempts < 100; attempts++ {
		destination := fmt.Sprintf("unknown-%016x-%s", dataTemporarySequence.Add(1), filepath.Base(relative))
		if err := unix.Renameat(int(store.rootFD.Fd()), relative, int(quarantine.Fd()), destination); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("allocating quarantine name for unknown TTC object %q", relative)
}

// reclaimRelative walks one known bulk-data subtree using only the donated
// repository descriptor. It intentionally never follows symlinks. Unknown
// non-directory entries are quarantined just like the path-backed collector,
// while empty implementation-owned shard directories are removed bottom-up.
func (store *LocalDataStore) reclaimRelative(graph *Store, relative string) (removed, quarantined int, err error) {
	directory, err := store.openRelative(relative, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if errors.Is(err, unix.ENOENT) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return 0, 0, errors.Join(readErr, closeErr)
	}
	for _, entry := range entries {
		child := filepath.Join(relative, entry.Name())
		var stat unix.Stat_t
		if err := unix.Fstatat(int(store.rootFD.Fd()), child, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return removed, quarantined, err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			r, q, err := store.reclaimRelative(graph, child)
			removed += r
			quarantined += q
			if err != nil {
				return removed, quarantined, err
			}
			if err := unix.Unlinkat(int(store.rootFD.Fd()), child, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTEMPTY) {
				return removed, quarantined, err
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), ".object-") || strings.HasPrefix(entry.Name(), ".clone-") || strings.HasPrefix(entry.Name(), ".replace-") {
			if err := unix.Unlinkat(int(store.rootFD.Fd()), child, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return removed, quarantined, err
			}
			removed++
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".data")
		id, parseErr := ParseObjectID(name)
		if parseErr != nil || name == entry.Name() || stat.Mode&unix.S_IFMT != unix.S_IFREG {
			if err := store.quarantineRelative(child); err != nil {
				return removed, quarantined, err
			}
			quarantined++
			continue
		}
		if _, ok := graph.Object(id); ok {
			continue
		}
		store.forgetUnsyncedName(id)
		if err := unix.Unlinkat(int(store.rootFD.Fd()), child, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return removed, quarantined, err
		}
		removed++
	}
	return removed, quarantined, nil
}

// ReclaimUnreferenced removes complete bulk objects absent from graph and
// quarantines unknown names. Callers serialize it with graph/data commits.
func (store *LocalDataStore) ReclaimUnreferenced(graph *Store) (removed, quarantined int, err error) {
	store.gcMu.Lock()
	defer store.gcMu.Unlock()
	if store.gcLock == nil {
		return 0, 0, errors.New("TTC bulk-object store is closed")
	}
	if err := syscall.Flock(int(store.gcLock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, 0, err
	}
	defer syscall.Flock(int(store.gcLock.Fd()), syscall.LOCK_UN)
	if store.rootFD != nil {
		for _, kind := range []string{"pages", "memory-chunks", "files"} {
			r, q, reclaimErr := store.reclaimRelative(graph, kind)
			removed += r
			quarantined += q
			if reclaimErr != nil {
				return removed, quarantined, reclaimErr
			}
			if err := unix.Unlinkat(int(store.rootFD.Fd()), kind, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTEMPTY) {
				return removed, quarantined, err
			}
		}
		return removed, quarantined, nil
	}
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
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
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
			store.forgetUnsyncedName(id)
			if err := os.Remove(path); err != nil {
				// A second LocalDataStore handle may have completed the same
				// idempotent sweep after WalkDir observed the entry. The graph
				// record is already absent, so ENOENT is the committed outcome.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
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
