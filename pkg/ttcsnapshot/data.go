// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostthread"
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
	unsyncedNames map[ObjectID]unsyncedName
	// chunkCache makes sweeps keep, within memoryChunkCacheTarget, memory
	// chunks their graph no longer holds: those of origins the graph does not
	// hold as its own, whose images the shared store has, and which a restore
	// on this node reads again rather than fetch. A publication releases its
	// node's roots of the image it hands to the shared store, and rehydrating
	// the branch on the node then fetched every chunk back. The oldest are
	// removed first.
	chunkCache bool
	// chunkCacheDead, if not nil, reports the origins no one will read again,
	// as those shared GC retired: sweeps keep none of their chunks.
	chunkCacheDead func(SnapshotID) bool
	// chunkCacheLimit, if nonzero, replaces memoryChunkCacheTarget, for tests.
	chunkCacheLimit int64
}

// EnableMemoryChunkCache makes sweeps keep memory chunks of origins their
// graph does not hold as its own, as a node's cache of shared images, but for
// those of origins dead reports gone, if it is not nil.
func (store *LocalDataStore) EnableMemoryChunkCache(dead func(SnapshotID) bool) {
	store.gcMu.Lock()
	defer store.gcMu.Unlock()
	store.chunkCache = true
	store.chunkCacheDead = dead
}

// memoryChunkCacheTarget bounds the cached chunks a sweep keeps: an eighth of
// the store's filesystem, as a node's image cache takes a quarter, from
// 256 MiB to 32 GiB, and no more than a quarter of its free space, which
// other stores on the filesystem may be caching into.
func (store *LocalDataStore) memoryChunkCacheTarget() uint64 {
	var stat unix.Statfs_t
	var err error
	if store.rootFD != nil {
		err = unix.Fstatfs(int(store.rootFD.Fd()), &stat)
	} else {
		err = unix.Statfs(store.root, &stat)
	}
	if err != nil {
		return 0
	}
	capacity := stat.Blocks * uint64(stat.Bsize)
	target := capacity / 8
	const (
		minimum = uint64(256 << 20)
		maximum = uint64(32 << 30)
	)
	if target < minimum && capacity >= minimum {
		target = minimum
	}
	return min(target, maximum, stat.Bavail*uint64(stat.Bsize)/4)
}

// cachedChunk is a memory chunk a sweep keeps though its graph no longer
// holds it.
type cachedChunk struct {
	id ObjectID
	// name is the chunk's file, relative to the store's root descriptor if it
	// has one.
	name     string
	modified int64
	bytes    int64
}

// keepsAsCache reports whether a sweep keeps id, which graph does not hold,
// as a cached chunk: a memory chunk of an origin graph does not hold as its
// own, and that is not dead. A retired origin's chunks stayed until newer ones
// outgrew the cache's bound, of up to 32 GiB, though no image names them.
func (store *LocalDataStore) keepsAsCache(graph *Store, id ObjectID) bool {
	if !store.chunkCache || id.Kind != ObjectKindMemoryChunk {
		return false
	}
	if snapshot, ok := graph.Snapshot(id.Snapshot); ok && snapshot.State != SnapshotShared {
		return false
	}
	return store.chunkCacheDead == nil || !store.chunkCacheDead(id.Snapshot)
}

// evictCachedChunks removes the oldest of chunks until those left fit the
// cache's bound, each with remove.
func (store *LocalDataStore) evictCachedChunks(chunks []cachedChunk, remove func(string) error) (int, error) {
	if len(chunks) == 0 {
		return 0, nil
	}
	limit := store.chunkCacheLimit
	if limit == 0 {
		limit = int64(store.memoryChunkCacheTarget())
	}
	var total int64
	for _, chunk := range chunks {
		total += chunk.bytes
	}
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].modified < chunks[j].modified })
	removed := 0
	for _, chunk := range chunks {
		if total <= limit {
			break
		}
		store.forgetUnsyncedName(chunk.id)
		if err := remove(chunk.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		total -= chunk.bytes
		removed++
	}
	return removed, nil
}

type unsyncedName struct {
	id         ObjectID
	generation uint64
}

func (store *LocalDataStore) noteUnsyncedName(id ObjectID) {
	store.namesMu.Lock()
	defer store.namesMu.Unlock()
	if store.unsyncedNames == nil {
		store.unsyncedNames = make(map[ObjectID]unsyncedName)
	}
	current := store.unsyncedNames[id]
	store.unsyncedNames[id] = unsyncedName{id: id, generation: current.generation + 1}
}

func (store *LocalDataStore) forgetUnsyncedName(id ObjectID) {
	store.namesMu.Lock()
	delete(store.unsyncedNames, id)
	store.namesMu.Unlock()
}

// SyncNewNames makes durable the directory entries of every object installed
// without a durability barrier since the last call, and nothing else. It is
// the part of a durability fence that a WAL naming those objects depends on;
// their payloads are the fence's separate concern.
func (store *LocalDataStore) SyncNewNames() error {
	store.namesMu.Lock()
	snapshot := make(map[ObjectID]unsyncedName, len(store.unsyncedNames))
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
	if err := hostthread.Flock(int(store.gcLock.Fd()), syscall.LOCK_SH); err != nil {
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
		file, err := store.createRelative(name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		return file, name, err
	}
	return nil, "", errors.New("allocating temporary TTC bulk-object file")
}

// createRelative is openRelative for a file it may create, whose directories
// it makes if they are missing. Most exist already, so it tries first: the
// objects of an origin share a few directories, which reclamation may remove
// once they are empty, as it may between making one and creating in it.
func (store *LocalDataStore) createRelative(relative string, flags int, mode uint32) (*os.File, error) {
	for attempt := 0; ; attempt++ {
		file, err := store.openRelative(relative, flags, mode)
		if !errors.Is(err, unix.ENOENT) || attempt == 3 {
			return file, err
		}
		if err := store.mkdirAllRelative(filepath.Dir(relative)); err != nil {
			return nil, err
		}
	}
}

// linkRelative links source to destination, relative to the store, making
// destination's directories if they are missing, as createRelative does.
func (store *LocalDataStore) linkRelative(source, destination string) error {
	for attempt := 0; ; attempt++ {
		err := unix.Linkat(int(store.rootFD.Fd()), source, int(store.rootFD.Fd()), destination, 0)
		if !errors.Is(err, unix.ENOENT) || attempt == 3 {
			return err
		}
		if err := store.mkdirAllRelative(filepath.Dir(destination)); err != nil {
			return err
		}
	}
}

func objectRelativePath(id ObjectID) (string, error) {
	name, err := objectKindName(id.Kind)
	if err != nil {
		return "", err
	}
	if err := id.validate(); err != nil {
		return "", err
	}
	// Built by hand: formatted, a path took 2-3 us, and every object's
	// operation names its path.
	path := make([]byte, 0, len(name)+1+16+1+2+1+36+len(".data"))
	path = append(path, name...)
	path = append(path, '/')
	path = appendHex(path, uint64(id.Snapshot), 16)
	path = append(path, '/')
	path = appendHex(path, id.Ordinal, 2)
	path = append(path, '/')
	path = appendObjectKey(path, id)
	path = append(path, ".data"...)
	return string(path), nil
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

// InstallEmptyDeferred is InstallDeferred for an empty object: it creates
// it if it is absent, and reports whether it did. An existing one is checked
// empty, as it must be.
func (store *LocalDataStore) InstallEmptyDeferred(id ObjectID) (bool, error) {
	created, err := store.createEmpty(id)
	if err != nil {
		return false, err
	}
	if created {
		store.noteUnsyncedName(id)
		return true, nil
	}
	return false, store.checkExistingEmpty(id)
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
	if length == 0 {
		// Commonly nothing holds its name yet.
		if created, err := store.createEmpty(id); err != nil || created {
			return err
		}
	}
	relative, err := objectRelativePath(id)
	if err != nil {
		return err
	}
	if store.rootFD != nil {
		directory := filepath.Dir(relative)
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
	var dataEnd int64
	for _, extent := range extents {
		punchHole(file, dataEnd, extent.Offset-dataEnd)
		dataEnd = extent.Offset + int64(len(extent.Data))
	}
	punchHole(file, dataEnd, length-dataEnd)
	// Only a hint: a later sync is what makes the object durable, so a
	// filesystem that does not take it loses nothing.
	startWriteback(file)
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

// createEmpty creates the empty object id exclusively: its name publishes it
// whole, without a temporary file. It reports false if something holds the
// name already.
func (store *LocalDataStore) createEmpty(id ObjectID) (bool, error) {
	relative, err := objectRelativePath(id)
	if err != nil {
		return false, err
	}
	directoryFD, name := unix.AT_FDCWD, relative
	if store.rootFD != nil {
		directoryFD = int(store.rootFD.Fd())
	} else if name, err = store.objectPath(id); err != nil {
		return false, err
	}
	for attempt := 0; ; attempt++ {
		// Reclamation may remove an emptied directory, as createRelative says.
		err = createEmptyFile(directoryFD, name)
		if !errors.Is(err, unix.ENOENT) || attempt == 3 {
			break
		}
		if store.rootFD != nil {
			err = store.mkdirAllRelative(filepath.Dir(relative))
		} else {
			err = os.MkdirAll(filepath.Dir(name), 0700)
		}
		if err != nil {
			return false, err
		}
	}
	if errors.Is(err, unix.EEXIST) {
		return false, nil
	}
	return err == nil, err
}

// checkExistingEmpty checks that the object id, which createEmpty found
// there already, is empty, as it must be. One stat does it.
func (store *LocalDataStore) checkExistingEmpty(id ObjectID) error {
	var size int64
	var regular bool
	if store.rootFD != nil {
		relative, err := objectRelativePath(id)
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(store.rootFD.Fd()), relative, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("existing immutable object: %w", err)
		}
		size, regular = stat.Size, stat.Mode&unix.S_IFMT == unix.S_IFREG
	} else {
		path, err := store.objectPath(id)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("existing immutable object: %w", err)
		}
		size, regular = info.Size(), info.Mode().IsRegular()
	}
	if size != 0 || !regular {
		return fmt.Errorf("existing immutable object: TTC bulk object %s has length %d, want 0", id.key(), size)
	}
	return nil
}

// installEmpty publishes the empty object id, which may be there already.
func (store *LocalDataStore) installEmpty(id ObjectID, durable bool) error {
	created, err := store.createEmpty(id)
	if err != nil {
		return err
	}
	if !created {
		return store.checkExistingEmpty(id)
	}
	if !durable {
		return nil
	}
	if store.rootFD != nil {
		relative, err := objectRelativePath(id)
		if err != nil {
			return err
		}
		return store.syncRelativeDirectory(filepath.Dir(relative))
	}
	destination, err := store.objectPath(id)
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

// installWith atomically publishes id, which fill writes into an empty
// temporary file, as an object of length bytes.
func (store *LocalDataStore) installWith(id ObjectID, length int64, durable bool, fill func(*os.File) error) error {
	if length < 0 {
		return errors.New("negative TTC bulk-object length")
	}
	if length == 0 {
		return store.installEmpty(id, durable)
	}
	relative, err := objectRelativePath(id)
	if err != nil {
		return err
	}
	if store.rootFD != nil {
		directory := filepath.Dir(relative)
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
	var offset, dataEnd int64
	type hole struct{ offset, length int64 }
	var holes []hole
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
			if gap := offset + int64(start) - dataEnd; gap != 0 {
				holes = append(holes, hole{dataEnd, gap})
			}
			dataEnd = offset + int64(end)
			start = end
		}
		offset += int64(n)
	}
	if err := file.Truncate(length); err != nil {
		return offset, err
	}
	holes = append(holes, hole{dataEnd, length - dataEnd})
	for _, h := range holes {
		punchHole(file, h.offset, h.length)
	}
	return offset, nil
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
		if err := store.linkRelative(source, destination); err != nil {
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
		temporary, temporaryName, err := store.createRelativeTemporary(directory, ".clone-")
		if err != nil {
			return err
		}
		defer unix.Unlinkat(int(store.rootFD.Fd()), temporaryName, 0)
		var cloneErr error
		if tryReflink {
			var cloned *os.File
			cloned, cloneErr = cloneFile(source, temporary, int(store.rootFD.Fd()), temporaryName)
			if cloned == nil {
				return cloneErr
			}
			temporary = cloned
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
		var cloned *os.File
		cloned, cloneErr = cloneFile(source, temporary, unix.AT_FDCWD, temporaryPath)
		if cloned == nil {
			return cloneErr
		}
		temporary = cloned
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
			hostthread.SyscallsEnd()
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
func (store *LocalDataStore) unsyncedNamesOf(ids []ObjectID) map[ObjectID]unsyncedName {
	store.namesMu.Lock()
	defer store.namesMu.Unlock()
	names := make(map[ObjectID]unsyncedName)
	for _, id := range ids {
		if name, ok := store.unsyncedNames[id]; ok {
			names[id] = name
		}
	}
	return names
}

// forgetSyncedNames drops the entries of synced from unsyncedNames, unless the
// object was installed again after the sync began.
func (store *LocalDataStore) forgetSyncedNames(synced map[ObjectID]unsyncedName) {
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
				hostthread.SyscallsEnd()
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
	// Not os.File.Stat, which allocates what it returns.
	var stat unix.Stat_t
	err = unix.Fstat(int(file.Fd()), &stat)
	if err != nil || length >= 0 && stat.Size != length || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("TTC bulk object %s has length %d, want %d", id.key(), stat.Size, length)
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
func (store *LocalDataStore) reclaimRelative(graph *Store, relative string, cache *[]cachedChunk) (removed, quarantined int, err error) {
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
	// The directory's entries carry their types: a file is looked at only if
	// it is kept, and stating each one took as long as removing it.
	for _, entry := range entries {
		child := filepath.Join(relative, entry.Name())
		if entry.IsDir() {
			r, q, err := store.reclaimRelative(graph, child, cache)
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
		if parseErr != nil || name == entry.Name() || !entry.Type().IsRegular() {
			if err := store.quarantineRelative(child); err != nil {
				return removed, quarantined, err
			}
			quarantined++
			continue
		}
		if _, ok := graph.Object(id); ok {
			continue
		}
		if store.keepsAsCache(graph, id) {
			var stat unix.Stat_t
			if err := unix.Fstatat(int(store.rootFD.Fd()), child, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue
				}
				return removed, quarantined, err
			}
			*cache = append(*cache, cachedChunk{id: id, name: child, modified: stat.Mtim.Nano(), bytes: stat.Blocks * 512})
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

// ReclaimCollected removes the data of objects graph's collection deleted,
// as TakeCollectedBulk returns them: what ReclaimUnreferenced would find of
// one collection, without reading every object's directory, which cost as
// much as removing a branch of 100,000 files each time a fork of it went. It
// leaves what only a whole reading finds, such as a crash's leftovers, to
// ReclaimUnreferenced. Callers serialize it with graph/data commits.
func (store *LocalDataStore) ReclaimCollected(graph *Store, ids []ObjectID) (removed int, err error) {
	store.gcMu.Lock()
	defer store.gcMu.Unlock()
	if store.gcLock == nil {
		return 0, errors.New("TTC bulk-object store is closed")
	}
	if err := hostthread.Flock(int(store.gcLock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(store.gcLock.Fd()), syscall.LOCK_UN)
	for _, id := range ids {
		if _, ok := graph.Object(id); ok || store.keepsAsCache(graph, id) {
			continue
		}
		relative, err := objectRelativePath(id)
		if err != nil {
			return removed, err
		}
		store.forgetUnsyncedName(id)
		if store.rootFD != nil {
			err = unix.Unlinkat(int(store.rootFD.Fd()), relative, 0)
		} else {
			err = os.Remove(filepath.Join(store.root, relative))
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// EvictDeadCachedChunks removes the cached chunks of the origins the cache's
// dead reports gone, which graph does not hold, reading no other origin's.
// Shared GC retires an origin only after the removal that left its chunks
// cached, and its sweeps, so no sweep found them dead: they stayed until newer
// chunks outgrew the cache's bound. It returns how many it removed.
func (store *LocalDataStore) EvictDeadCachedChunks(graph *Store) (int, error) {
	store.gcMu.Lock()
	dead := store.chunkCacheDead
	caching := store.chunkCache && store.gcLock != nil
	store.gcMu.Unlock()
	if !caching || dead == nil {
		return 0, nil
	}
	origins, err := store.readDirNames("memory-chunks")
	if err != nil {
		return 0, err
	}
	var gone []string
	for _, name := range origins {
		value, err := strconv.ParseUint(name, 16, 64)
		if err != nil || len(name) != 16 {
			continue
		}
		origin := SnapshotID(value)
		if snapshot, ok := graph.Snapshot(origin); ok && snapshot.State != SnapshotShared {
			continue
		}
		// Asked without the store's lock: the answer may take a round trip.
		if dead(origin) {
			gone = append(gone, name)
		}
	}
	if len(gone) == 0 {
		return 0, nil
	}
	store.gcMu.Lock()
	defer store.gcMu.Unlock()
	if store.gcLock == nil {
		return 0, errors.New("TTC bulk-object store is closed")
	}
	if err := hostthread.Flock(int(store.gcLock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(store.gcLock.Fd()), syscall.LOCK_UN)
	removed := 0
	for _, origin := range gone {
		originDir := filepath.Join("memory-chunks", origin)
		shards, err := store.readDirNames(originDir)
		if err != nil {
			return removed, err
		}
		for _, shard := range shards {
			shardDir := filepath.Join(originDir, shard)
			names, err := store.readDirNames(shardDir)
			if err != nil {
				return removed, err
			}
			for _, name := range names {
				id, err := ParseObjectID(strings.TrimSuffix(name, ".data"))
				if err != nil || !strings.HasSuffix(name, ".data") || id.Kind != ObjectKindMemoryChunk {
					// Left to a sweep, which quarantines what it cannot name.
					continue
				}
				if _, ok := graph.Object(id); ok {
					continue
				}
				store.forgetUnsyncedName(id)
				if err := store.removeRelative(filepath.Join(shardDir, name), false); err != nil {
					return removed, err
				}
				removed++
			}
			if err := store.removeRelative(shardDir, true); err != nil {
				return removed, err
			}
		}
		if err := store.removeRelative(originDir, true); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// readDirNames returns the names in the store's directory relative, none if
// it is gone.
func (store *LocalDataStore) readDirNames(relative string) ([]string, error) {
	var file *os.File
	var err error
	if store.rootFD != nil {
		var fd int
		fd, err = unix.Openat(int(store.rootFD.Fd()), relative, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err == nil {
			file = os.NewFile(uintptr(fd), relative)
		}
	} else {
		file, err = os.Open(filepath.Join(store.root, relative))
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Readdirnames(-1)
}

// removeRelative removes the store's file relative, or, if directory is set,
// the directory, unless something is in it. That it is gone is no error.
func (store *LocalDataStore) removeRelative(relative string, directory bool) error {
	var err error
	if store.rootFD != nil {
		flags := 0
		if directory {
			flags = unix.AT_REMOVEDIR
		}
		err = unix.Unlinkat(int(store.rootFD.Fd()), relative, flags)
	} else {
		err = os.Remove(filepath.Join(store.root, relative))
	}
	if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOENT) || (directory && (errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST))) {
		return nil
	}
	return err
}

// ReclaimUnreferenced removes complete bulk objects absent from graph and
// quarantines unknown names. Callers serialize it with graph/data commits.
func (store *LocalDataStore) ReclaimUnreferenced(graph *Store) (removed, quarantined int, err error) {
	store.gcMu.Lock()
	defer store.gcMu.Unlock()
	if store.gcLock == nil {
		return 0, 0, errors.New("TTC bulk-object store is closed")
	}
	if err := hostthread.Flock(int(store.gcLock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, 0, err
	}
	defer syscall.Flock(int(store.gcLock.Fd()), syscall.LOCK_UN)
	var cache []cachedChunk
	if store.rootFD != nil {
		for _, kind := range []string{"pages", "memory-chunks", "files"} {
			r, q, reclaimErr := store.reclaimRelative(graph, kind, &cache)
			removed += r
			quarantined += q
			if reclaimErr != nil {
				return removed, quarantined, reclaimErr
			}
			if err := unix.Unlinkat(int(store.rootFD.Fd()), kind, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTEMPTY) {
				return removed, quarantined, err
			}
		}
		evicted, err := store.evictCachedChunks(cache, func(name string) error {
			return unix.Unlinkat(int(store.rootFD.Fd()), name, 0)
		})
		return removed + evicted, quarantined, err
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
			if store.keepsAsCache(graph, id) {
				if info, err := entry.Info(); err == nil {
					var blocks int64
					if stat, ok := info.Sys().(*syscall.Stat_t); ok {
						blocks = stat.Blocks
					}
					cache = append(cache, cachedChunk{id: id, name: path, modified: info.ModTime().UnixNano(), bytes: blocks * 512})
					return nil
				}
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
	evicted, err := store.evictCachedChunks(cache, os.Remove)
	return removed + evicted, quarantined, err
}
