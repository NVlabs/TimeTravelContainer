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
	return openLazyDirectory(pack, rootFD, stateFD, descriptor.Prefix)
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
	return openLazyDirectory(pack, rootFD, stateFD, prefix)
}

func openLazyDirectory(pack *os.File, rootFD, stateFD int, prefix string) (*LazyDirectory, error) {
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
		stop: make(chan struct{}),
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
	targetFD, err := unix.Openat(d.rootFD, canonical, unix.O_WRONLY|unix.O_TRUNC|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
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
		result = errors.Join(d.pack.Close(), unix.Close(d.rootFD), unix.Close(d.stateFD))
	})
	return result
}
