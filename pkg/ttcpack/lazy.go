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

package ttcpack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	// LazyAnnotation names the local descriptor consumed by the rootfs gofer.
	LazyAnnotation = "dev.timetravelcontainer.rootfs.image-pack"
	// LazyDescriptorName is stored beside the cached rootfs and is not visible
	// inside the guest.
	LazyDescriptorName = "image-pack.json"
	// LazyStateDirectory stores node-local completion records and locks.
	LazyStateDirectory   = "image-pack-state"
	lazyDescriptorFormat = 1
)

// LazyDescriptor identifies an immutable shared image pack backing a local
// skeleton. Repository is an absolute host path and is opened before the gofer
// enters its rootfs chroot.
type LazyDescriptor struct {
	Format     int    `json:"format"`
	Repository string `json:"repository"`
	Pack       string `json:"pack"`
	Prefix     string `json:"prefix,omitempty"`
}

// LazyDescriptorPath returns the sidecar associated with rootfs.
func LazyDescriptorPath(rootfs string) string {
	return filepath.Join(filepath.Dir(rootfs), LazyDescriptorName)
}

// WriteLazyDescriptor atomically records a local skeleton's shared backing.
func WriteLazyDescriptor(rootfs, repository string, id ID, prefix string) error {
	if !filepath.IsAbs(rootfs) || !filepath.IsAbs(repository) {
		return errors.New("lazy image root and repository must be absolute")
	}
	if err := validateRelative(prefix); err != nil {
		return err
	}
	descriptor := LazyDescriptor{Format: lazyDescriptorFormat, Repository: filepath.Clean(repository), Pack: id.String(), Prefix: filepath.ToSlash(filepath.Clean(prefix))}
	if descriptor.Prefix == "." {
		descriptor.Prefix = ""
	}
	data, err := json.MarshalIndent(&descriptor, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := LazyDescriptorPath(rootfs)
	temporary, err := os.CreateTemp(filepath.Dir(path), ".image-pack.tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// ReadLazyDescriptor validates the descriptor associated with rootfs.
func ReadLazyDescriptor(rootfs string) (LazyDescriptor, error) {
	path := LazyDescriptorPath(rootfs)
	data, err := os.ReadFile(path)
	if err != nil {
		return LazyDescriptor{}, err
	}
	var descriptor LazyDescriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		return LazyDescriptor{}, err
	}
	if descriptor.Format != lazyDescriptorFormat || !filepath.IsAbs(descriptor.Repository) {
		return LazyDescriptor{}, fmt.Errorf("invalid lazy image descriptor format %d", descriptor.Format)
	}
	if _, err := ParseID(descriptor.Pack); err != nil {
		return LazyDescriptor{}, err
	}
	if err := validateRelative(descriptor.Prefix); err != nil {
		return LazyDescriptor{}, err
	}
	descriptor.Repository = filepath.Clean(descriptor.Repository)
	return descriptor, nil
}

// FileMetadata is the pack-backed metadata needed before file contents have
// been populated in the local skeleton.
type FileMetadata struct {
	Size int64
	Mode uint32
}

// LazyCacheUsage summarizes immutable regular-file data represented by one
// local skeleton. CompleteBytes is the logical size of files whose durable
// completion marker is visible; TotalBytes is the fully populated cache size.
// Hardlinks are counted once by canonical file identity.
type LazyCacheUsage struct {
	CompleteFiles uint64 `json:"complete_files"`
	TotalFiles    uint64 `json:"total_files"`
	CompleteBytes uint64 `json:"complete_bytes"`
	TotalBytes    uint64 `json:"total_bytes"`
}

// LazyCacheEviction summarizes complete immutable files removed from a
// node-local image cache. Eviction leaves the metadata skeleton intact, so a
// later guest open can repopulate the file from the shared pack.
type LazyCacheEviction struct {
	Files uint64 `json:"files"`
	Bytes uint64 `json:"bytes"`
}

// ErrLazyCacheBusy means that a gofer currently has the cache open. TTC never
// evicts from an active image root: an already-donated host descriptor must
// continue to name stable bytes for the lifetime of the sandbox.
var ErrLazyCacheBusy = errors.New("lazy image cache is in use")

// LazyDirectory populates whole regular files from a pre-opened pack. It owns
// descriptors for the pack, local root, and completion-record directory so it
// remains usable after the gofer chroots.
type LazyDirectory struct {
	pack       *os.File
	rootFD     int
	stateFD    int
	prefix     string
	files      map[string][]Entry
	metadata   map[string]FileMetadata
	canonical  map[string]string
	paths      []string
	stop       chan struct{}
	closeOnce  sync.Once
	prefetchWG sync.WaitGroup
	stateLock  bool
}

// OpenLazyDirectory opens descriptor and the skeleton root. It reads only the
// pack header/footer/index, never regular-file payloads.
func OpenLazyDirectory(rootfs string, descriptor LazyDescriptor) (*LazyDirectory, error) {
	id, err := ParseID(descriptor.Pack)
	if err != nil {
		return nil, err
	}
	pack, _, err := OpenIndexed(descriptor.Repository, id)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*LazyDirectory, error) {
		pack.Close()
		return nil, err
	}
	rootFD, err := unix.Open(rootfs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	statePath := filepath.Join(filepath.Dir(rootfs), LazyStateDirectory)
	if err := os.MkdirAll(statePath, 0700); err != nil {
		unix.Close(rootFD)
		return fail(err)
	}
	stateFD, err := unix.Open(statePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(rootFD)
		return fail(err)
	}
	return openLazyDirectory(pack, rootFD, stateFD, descriptor.Prefix, true)
}

// OpenLazyDirectoryFromFDs reconstructs a lazy directory after the rootless
// gofer capability-changing re-exec. The descriptors retain the pre-reexec
// mount and shared-pack objects even though their host paths are no longer
// reachable in the gofer's private root.
func OpenLazyDirectoryFromFDs(packFD, rootFD, stateFD int, prefix string) (*LazyDirectory, error) {
	if packFD < 0 || rootFD < 0 || stateFD < 0 {
		return nil, errors.New("lazy image inherited descriptors are incomplete")
	}
	if err := validateRelative(prefix); err != nil {
		return nil, err
	}
	pack := os.NewFile(uintptr(packFD), "TTC lazy image pack")
	if pack == nil {
		return nil, errors.New("invalid inherited lazy image pack descriptor")
	}
	return openLazyDirectory(pack, rootFD, stateFD, prefix, true)
}

func openLazyDirectory(pack *os.File, rootFD, stateFD int, prefix string, lockState bool) (*LazyDirectory, error) {
	if lockState {
		if err := unix.Flock(stateFD, unix.LOCK_SH); err != nil {
			_ = pack.Close()
			_ = unix.Close(rootFD)
			_ = unix.Close(stateFD)
			return nil, fmt.Errorf("locking lazy image cache: %w", err)
		}
	}
	index, err := ReadIndex(pack)
	if err != nil {
		_ = pack.Close()
		_ = unix.Close(rootFD)
		_ = unix.Close(stateFD)
		return nil, err
	}
	d := &LazyDirectory{
		pack: pack, rootFD: rootFD, stateFD: stateFD, prefix: prefix,
		files: make(map[string][]Entry), metadata: make(map[string]FileMetadata), canonical: make(map[string]string),
		stop: make(chan struct{}), stateLock: lockState,
	}
	logicalPath := func(packPath string) (string, bool) {
		if prefix == "" {
			return packPath, true
		}
		pathPrefix := strings.TrimSuffix(prefix, "/") + "/"
		if !strings.HasPrefix(packPath, pathPrefix) {
			return "", false
		}
		return strings.TrimPrefix(packPath, pathPrefix), true
	}
	for _, entry := range index.Entries {
		path, include := logicalPath(entry.Path)
		if !include {
			continue
		}
		switch entry.Kind {
		case "file":
			d.files[path] = append(d.files[path], entry)
			metadata := d.metadata[path]
			if end := entry.FileOffset + entry.Length; end > metadata.Size {
				metadata.Size = end
			}
			metadata.Mode = entry.Mode
			d.metadata[path] = metadata
			d.canonical[path] = path
		case "hardlink":
			target, ok := logicalPath(entry.Hardlink)
			if !ok {
				return nil, fmt.Errorf("lazy hardlink %q escapes prefix %q", entry.Path, prefix)
			}
			d.canonical[path] = target
		}
	}
	for path := range d.files {
		sort.Slice(d.files[path], func(i, j int) bool { return d.files[path][i].FileOffset < d.files[path][j].FileOffset })
		d.paths = append(d.paths, path)
	}
	sort.Strings(d.paths)
	return d, nil
}

// InheritedFDs returns the descriptors and prefix that must survive the
// rootless gofer re-exec. LazyDirectory retains ownership of the descriptors.
func (d *LazyDirectory) InheritedFDs() (packFD, rootFD, stateFD int, prefix string) {
	return int(d.pack.Fd()), d.rootFD, d.stateFD, d.prefix
}

// Metadata returns immutable regular-file metadata for virtualPath.
func (d *LazyDirectory) Metadata(virtualPath string) (size int64, mode uint32, ok bool) {
	if d == nil {
		return 0, 0, false
	}
	path, err := cleanVirtualPath(virtualPath)
	if err != nil {
		return 0, 0, false
	}
	canonical, ok := d.canonical[path]
	if !ok {
		return 0, 0, false
	}
	metadata, ok := d.metadata[canonical]
	return metadata.Size, metadata.Mode, ok
}

func cleanVirtualPath(path string) (string, error) {
	if path == "." {
		return "", nil
	}
	if err := validateRelative(path); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))), nil
}

func lazyLockName(path string) (string, string) {
	digest := sha256.Sum256([]byte(path))
	encoded := hex.EncodeToString(digest[:])
	return encoded[:2], encoded[2:]
}

func (d *LazyDirectory) complete(canonical string) bool {
	shard, name := lazyLockName(canonical)
	shardFD, err := unix.Openat(d.stateFD, shard, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(shardFD)
	markerFD, err := unix.Openat(shardFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer unix.Close(markerFD)
	marker := make([]byte, len("complete\n"))
	n, err := unix.Pread(markerFD, marker, 0)
	return err == nil && n == len(marker) && string(marker) == "complete\n"
}

// Usage returns a lock-free point-in-time view of cache completion. A file
// racing publication is conservatively absent until its complete marker is
// fully visible.
func (d *LazyDirectory) Usage() LazyCacheUsage {
	var usage LazyCacheUsage
	if d == nil {
		return usage
	}
	for _, canonical := range d.paths {
		metadata := d.metadata[canonical]
		usage.TotalFiles++
		if metadata.Size > 0 {
			usage.TotalBytes += uint64(metadata.Size)
		}
		if d.complete(canonical) {
			usage.CompleteFiles++
			if metadata.Size > 0 {
				usage.CompleteBytes += uint64(metadata.Size)
			}
		}
	}
	return usage
}

// InspectLazyCache opens a skeleton's descriptor and reports its current
// completion without reading regular-file payloads.
func InspectLazyCache(rootfs string) (LazyCacheUsage, error) {
	descriptor, err := ReadLazyDescriptor(rootfs)
	if err != nil {
		return LazyCacheUsage{}, err
	}
	directory, err := OpenLazyDirectory(rootfs, descriptor)
	if err != nil {
		return LazyCacheUsage{}, err
	}
	usage := directory.Usage()
	if err := directory.Close(); err != nil {
		return LazyCacheUsage{}, err
	}
	return usage, nil
}

// EvictLazyCache reduces the complete regular-file content in rootfs to at
// most targetBytes. Files are selected oldest-completion-first. It returns
// ErrLazyCacheBusy without changing anything when a gofer has the image root
// open. The target is best effort when one file is larger than the remaining
// budget or another process has already changed a completion record.
func EvictLazyCache(rootfs string, targetBytes uint64) (LazyCacheUsage, LazyCacheEviction, error) {
	descriptor, err := ReadLazyDescriptor(rootfs)
	if err != nil {
		return LazyCacheUsage{}, LazyCacheEviction{}, err
	}
	id, err := ParseID(descriptor.Pack)
	if err != nil {
		return LazyCacheUsage{}, LazyCacheEviction{}, err
	}
	pack, _, err := OpenIndexed(descriptor.Repository, id)
	if err != nil {
		return LazyCacheUsage{}, LazyCacheEviction{}, err
	}
	rootFD, err := unix.Open(rootfs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = pack.Close()
		return LazyCacheUsage{}, LazyCacheEviction{}, err
	}
	statePath := filepath.Join(filepath.Dir(rootfs), LazyStateDirectory)
	stateFD, err := unix.Open(statePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = pack.Close()
		_ = unix.Close(rootFD)
		return LazyCacheUsage{}, LazyCacheEviction{}, err
	}
	directory, err := openLazyDirectory(pack, rootFD, stateFD, descriptor.Prefix, false)
	if err != nil {
		return LazyCacheUsage{}, LazyCacheEviction{}, err
	}
	defer directory.Close()
	if err := unix.Flock(stateFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return directory.Usage(), LazyCacheEviction{}, ErrLazyCacheBusy
		}
		return LazyCacheUsage{}, LazyCacheEviction{}, fmt.Errorf("locking lazy image cache for eviction: %w", err)
	}
	directory.stateLock = true

	type candidate struct {
		path      string
		shard     string
		name      string
		size      uint64
		completed unix.Timespec
	}
	usage := directory.Usage()
	if usage.CompleteBytes <= targetBytes {
		return usage, LazyCacheEviction{}, nil
	}
	var candidates []candidate
	for _, path := range directory.paths {
		shard, name := lazyLockName(path)
		shardFD, err := unix.Openat(stateFD, shard, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return usage, LazyCacheEviction{}, err
		}
		var stat unix.Stat_t
		err = unix.Fstatat(shardFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		_ = unix.Close(shardFD)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return usage, LazyCacheEviction{}, err
		}
		size := uint64(0)
		if metadata := directory.metadata[path]; metadata.Size > 0 {
			size = uint64(metadata.Size)
		}
		candidates = append(candidates, candidate{path: path, shard: shard, name: name, size: size, completed: stat.Mtim})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].completed.Sec != candidates[j].completed.Sec {
			return candidates[i].completed.Sec < candidates[j].completed.Sec
		}
		if candidates[i].completed.Nsec != candidates[j].completed.Nsec {
			return candidates[i].completed.Nsec < candidates[j].completed.Nsec
		}
		return candidates[i].path < candidates[j].path
	})
	var evicted LazyCacheEviction
	for _, item := range candidates {
		if usage.CompleteBytes <= targetBytes {
			break
		}
		shardFD, err := unix.Openat(stateFD, item.shard, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return usage, evicted, err
		}
		if err := unix.Unlinkat(shardFD, item.name, 0); err != nil {
			_ = unix.Close(shardFD)
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return usage, evicted, err
		}
		// The absent completion record must be durable before the cached bytes
		// can disappear. After a crash, Ensure may safely repeat the fetch.
		if err := unix.Fsync(shardFD); err != nil {
			_ = unix.Close(shardFD)
			return usage, evicted, err
		}
		_ = unix.Close(shardFD)
		targetFD, err := unix.Openat(rootFD, item.path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return usage, evicted, err
		}
		if err := unix.Fchmod(targetFD, 0600); err != nil {
			_ = unix.Close(targetFD)
			return usage, evicted, err
		}
		_ = unix.Close(targetFD)
		targetFD, err = unix.Openat(rootFD, item.path, unix.O_WRONLY|unix.O_TRUNC|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return usage, evicted, err
		}
		if err := unix.Fsync(targetFD); err != nil {
			_ = unix.Close(targetFD)
			return usage, evicted, err
		}
		if err := unix.Close(targetFD); err != nil {
			return usage, evicted, err
		}
		usage.CompleteFiles--
		usage.CompleteBytes -= item.size
		evicted.Files++
		evicted.Bytes += item.size
	}
	return usage, evicted, nil
}

// Ensure makes virtualPath complete before any guest descriptor can observe
// it. The completion record is written only after file data and metadata have
// been synced. A crash before then causes the next caller to rewrite the file.
func (d *LazyDirectory) Ensure(virtualPath string) error {
	if d == nil {
		return nil
	}
	path, err := cleanVirtualPath(virtualPath)
	if err != nil {
		return err
	}
	canonical, ok := d.canonical[path]
	if !ok {
		return nil
	}
	entries, ok := d.files[canonical]
	if !ok {
		return fmt.Errorf("lazy image file %q has no data records", canonical)
	}
	shard, name := lazyLockName(canonical)
	if err := unix.Mkdirat(d.stateFD, shard, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	shardFD, err := unix.Openat(d.stateFD, shard, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(shardFD)
	lockFD, err := unix.Openat(shardFD, name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(lockFD)
	if err := unix.Flock(lockFD, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(lockFD, unix.LOCK_UN)
	marker := make([]byte, len("complete\n"))
	if n, err := unix.Pread(lockFD, marker, 0); err == nil && n == len(marker) && string(marker) == "complete\n" {
		return nil
	}
	// Incomplete skeleton files are normally writable. Eviction deliberately
	// leaves an immutable completed file at size zero, so restore owner-write
	// permission before reopening it for replacement.
	targetFD, err := unix.Openat(d.rootFD, canonical, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if err := unix.Fchmod(targetFD, 0600); err != nil {
		_ = unix.Close(targetFD)
		return err
	}
	if err := unix.Close(targetFD); err != nil {
		return err
	}
	targetFD, err = unix.Openat(d.rootFD, canonical, unix.O_WRONLY|unix.O_TRUNC|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(targetFD)
	buffer := make([]byte, MaxDataExtent)
	var size int64
	for _, entry := range entries {
		if entry.Length < 0 || entry.Length > int64(len(buffer)) {
			return fmt.Errorf("lazy image extent for %q has invalid length %d", canonical, entry.Length)
		}
		n, err := d.pack.ReadAt(buffer[:entry.Length], entry.RecordOffset+Alignment)
		if err != nil && !(err == io.EOF && int64(n) == entry.Length) {
			return err
		}
		if int64(n) != entry.Length {
			return io.ErrUnexpectedEOF
		}
		if n != 0 {
			if written, err := unix.Pwrite(targetFD, buffer[:n], entry.FileOffset); err != nil || written != n {
				if err == nil {
					err = io.ErrShortWrite
				}
				return err
			}
		}
		if end := entry.FileOffset + entry.Length; end > size {
			size = end
		}
	}
	if err := unix.Ftruncate(targetFD, size); err != nil {
		return err
	}
	if err := unix.Fchmod(targetFD, entries[0].Mode&07777); err != nil {
		return err
	}
	if err := unix.Fsync(targetFD); err != nil {
		return err
	}
	if err := unix.Ftruncate(lockFD, 0); err != nil {
		return err
	}
	if n, err := unix.Pwrite(lockFD, []byte("complete\n"), 0); err != nil || n != len("complete\n") {
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	return unix.Fsync(lockFD)
}

// StartPrefetch begins low-priority sequential whole-file population. Demand
// calls to Ensure share the same per-file locks and can safely race it.
func (d *LazyDirectory) StartPrefetch() {
	if d == nil {
		return
	}
	d.prefetchWG.Add(1)
	go func() {
		defer d.prefetchWG.Done()
		for _, path := range d.paths {
			select {
			case <-d.stop:
				return
			default:
			}
			_ = d.Ensure(path)
		}
	}()
}

// Close stops background work and releases pre-chroot descriptors.
func (d *LazyDirectory) Close() error {
	if d == nil {
		return nil
	}
	var result error
	d.closeOnce.Do(func() {
		close(d.stop)
		d.prefetchWG.Wait()
		if d.stateLock {
			_ = unix.Flock(d.stateFD, unix.LOCK_UN)
		}
		result = errors.Join(d.pack.Close(), unix.Close(d.rootFD), unix.Close(d.stateFD))
	})
	return result
}
