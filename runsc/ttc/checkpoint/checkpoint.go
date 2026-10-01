// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package checkpoint defines TTC's standalone checkpoint bundle.
package checkpoint

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/ttc/bundle"
)

const (
	// FormatVersion is the current TTC checkpoint container format. Its state
	// directory holds a dehydration of the branch: its capture, its kernel
	// state, and a stream of the pages it uses.
	FormatVersion = 2
	// kernelCheckpointFormat is the format before: its state directory held a
	// gVisor checkpoint of the branch's kernel, with the whole memory file the
	// branch shared with its family, which only the build that wrote it could
	// restore.
	kernelCheckpointFormat = 1
	// ManifestName is the checkpoint manifest path inside the archive.
	ManifestName = "manifest.json"
)

// RepositorySnapshot associates an OCI ttcvol source with an archived
// repository snapshot directory.
type RepositorySnapshot struct {
	Source string `json:"source"`
	Path   string `json:"path"`
}

// HostMountSnapshot records a manager-owned readonly bind copied into the
// checkpoint.
type HostMountSnapshot struct {
	MountIndex int    `json:"mount_index"`
	Source     string `json:"source"`
	Path       string `json:"path"`
}

// Manifest is sufficient to create one independent TTC family. Immutable image
// content is intentionally omitted and locked by Container.Digest.
type Manifest struct {
	FormatVersion int                  `json:"format_version"`
	CreatedAt     time.Time            `json:"created_at"`
	SourceBranch  string               `json:"source_branch"`
	RestorePaused bool                 `json:"restore_paused"`
	Container     bundle.ContainerInfo `json:"container"`
	Spec          specs.Spec           `json:"spec"`
	Repositories  []RepositorySnapshot `json:"repositories,omitempty"`
	HostMounts    []HostMountSnapshot  `json:"host_mounts,omitempty"`
	// MemoryCompression is how the dehydration's page stream is compressed:
	// "flate-best-speed", or empty for none.
	MemoryCompression string `json:"memory_compression,omitempty"`
}

// WriteManifest writes manifest into an unpacked checkpoint staging tree.
func WriteManifest(root string, manifest Manifest) error {
	manifest.FormatVersion = FormatVersion
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(root, ManifestName), data, 0600)
}

// ReadManifest validates a manifest in an unpacked checkpoint tree.
func ReadManifest(root string) (Manifest, error) {
	manifestPath := filepath.Join(root, ManifestName)
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	if !info.Mode().IsRegular() {
		return Manifest{}, fmt.Errorf("TTC checkpoint manifest is not a regular file")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decoding TTC checkpoint manifest: %w", err)
	}
	if manifest.FormatVersion == kernelCheckpointFormat {
		return Manifest{}, errors.New("this checkpoint was made by an earlier TTC build, and only that build can restore it")
	}
	if manifest.FormatVersion != FormatVersion {
		return Manifest{}, fmt.Errorf("unsupported TTC checkpoint format %d", manifest.FormatVersion)
	}
	if manifest.Container.Digest == "" || manifest.Container.Platform == "" || manifest.SourceBranch == "" {
		return Manifest{}, errors.New("TTC checkpoint manifest is incomplete")
	}
	repositorySources := make(map[string]struct{})
	repositoryPaths := make(map[string]struct{})
	for _, repository := range manifest.Repositories {
		if repository.Source == "" || !validArchiveSubpath(repository.Path, "repositories") {
			return Manifest{}, fmt.Errorf("invalid repository snapshot %+v", repository)
		}
		if _, ok := repositorySources[repository.Source]; ok {
			return Manifest{}, fmt.Errorf("duplicate repository source %q", repository.Source)
		}
		if _, ok := repositoryPaths[repository.Path]; ok {
			return Manifest{}, fmt.Errorf("duplicate repository snapshot path %q", repository.Path)
		}
		repositorySources[repository.Source] = struct{}{}
		repositoryPaths[repository.Path] = struct{}{}
		if err := requireArchivedRoot(root, repository.Path); err != nil {
			return Manifest{}, fmt.Errorf("invalid repository snapshot %q: %w", repository.Path, err)
		}
	}
	hostPaths := make(map[string]struct{})
	hostIndices := make(map[int]struct{})
	for _, mount := range manifest.HostMounts {
		if mount.MountIndex < 0 || mount.Source == "" || !validArchiveSubpath(mount.Path, "host-mounts") {
			return Manifest{}, fmt.Errorf("invalid host-mount snapshot %+v", mount)
		}
		if _, ok := hostPaths[mount.Path]; ok {
			return Manifest{}, fmt.Errorf("duplicate host-mount snapshot path %q", mount.Path)
		}
		if _, ok := hostIndices[mount.MountIndex]; ok {
			return Manifest{}, fmt.Errorf("duplicate host-mount index %d", mount.MountIndex)
		}
		hostPaths[mount.Path] = struct{}{}
		hostIndices[mount.MountIndex] = struct{}{}
		if err := requireArchivedRoot(root, mount.Path); err != nil {
			return Manifest{}, fmt.Errorf("invalid host-mount snapshot %q: %w", mount.Path, err)
		}
	}
	return manifest, nil
}

// Pack atomically publishes a staging tree containing regular files,
// directories, and symbolic links as one tar file.
func Pack(root, destination string) error {
	archive, err := CreateArchive(destination)
	if err != nil {
		return err
	}
	defer archive.Abort()
	return archive.Finish(root)
}

// blockSize is the size of a tar block.
const blockSize = 512

// Archive is a checkpoint archive being written: first the members whose
// content is streamed, each as it is made, then, by Finish, the other entries
// of a staging tree, as Pack writes them.
type Archive struct {
	destination string
	temp        *os.File
	// end is where the next member begins.
	end       int64
	streamed  map[string]struct{}
	published bool
}

// CreateArchive begins an archive that Finish publishes at destination.
func CreateArchive(destination string) (*Archive, error) {
	if _, err := os.Lstat(destination); err == nil {
		return nil, fmt.Errorf("checkpoint destination %q already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), ".ttc-checkpoint-*")
	if err != nil {
		return nil, err
	}
	return &Archive{destination: destination, temp: temp, streamed: make(map[string]struct{})}, nil
}

// Stream writes name, a regular file of mode, whose content write writes to
// the archive's file from the file's offset on, by itself or by another
// process it hands the file to. The member's header, which records its size,
// is written once its content is: the content is neither staged nor copied.
func (a *Archive) Stream(name string, mode fs.FileMode, write func(*os.File) error) error {
	if !validArchivePath(filepath.FromSlash(name)) {
		return fmt.Errorf("invalid checkpoint archive path %q", name)
	}
	if _, ok := a.streamed[name]; ok {
		return fmt.Errorf("duplicate checkpoint archive path %q", name)
	}
	start := a.end + blockSize
	if _, err := a.temp.Seek(start, io.SeekStart); err != nil {
		return err
	}
	if err := write(a.temp); err != nil {
		return err
	}
	end, err := a.temp.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	size := end - start
	header, err := streamedHeader(name, mode, size)
	if err != nil {
		return err
	}
	if _, err := a.temp.WriteAt(header, a.end); err != nil {
		return err
	}
	padding := (blockSize - size%blockSize) % blockSize
	if _, err := a.temp.WriteAt(make([]byte, padding), end); err != nil {
		return err
	}
	a.end = end + padding
	a.streamed[name] = struct{}{}
	return nil
}

// streamedHeader returns the one block of the header of a streamed member: the
// GNU format records a size of any length in it.
func streamedHeader(name string, mode fs.FileMode, size int64) ([]byte, error) {
	var header bytes.Buffer
	if err := tar.NewWriter(&header).WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg, Name: name, Mode: int64(mode.Perm()), Size: size,
		ModTime: time.Unix(time.Now().Unix(), 0), Format: tar.FormatGNU,
	}); err != nil {
		return nil, err
	}
	if header.Len() != blockSize {
		return nil, fmt.Errorf("checkpoint archive header of %q has %d bytes", name, header.Len())
	}
	return header.Bytes(), nil
}

// Abort removes the archive, unless Finish published it.
func (a *Archive) Abort() {
	if a.published {
		return
	}
	_ = a.temp.Close()
	_ = os.Remove(a.temp.Name())
}

// Finish writes the entries of the staging tree at root that were not
// streamed, as Pack does, and publishes the archive.
func (a *Archive) Finish(root string) error {
	if _, err := a.temp.Seek(a.end, io.SeekStart); err != nil {
		return err
	}
	temp, tempName, destination := a.temp, a.temp.Name(), a.destination
	writer := tar.NewWriter(temp)
	type packReadResult struct {
		data []byte
		err  error
	}
	type packEntry struct {
		relative   string
		info       fs.FileInfo
		link       string
		prefetched chan packReadResult
	}
	var entries []packEntry
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !validArchivePath(relative) {
			return fmt.Errorf("invalid checkpoint staging path %q", relative)
		}
		if _, ok := a.streamed[filepath.ToSlash(relative)]; ok {
			return fmt.Errorf("checkpoint staging path %q was streamed", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported checkpoint staging file %q with mode %v", relative, info.Mode())
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		entries = append(entries, packEntry{relative: relative, info: info, link: link})
		return nil
	}); err != nil {
		writer.Close()
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].relative < entries[j].relative })

	// APFS clone reads can incur per-file extent-resolution latency. Reading
	// tens of thousands of package objects serially makes a checkpoint appear
	// hung even when aggregate data is modest. Prefetch bounded-size files in
	// parallel while the single tar writer preserves deterministic ordering.
	const maxPrefetchFileSize = int64(1 << 20)
	// File opens, rather than CPU, dominate APFS clone reads. Permit more
	// readers than GOMAXPROCS while keeping the queue and memory window bounded.
	workers := min(max(runtime.GOMAXPROCS(0), 32), 64)
	if workers > len(entries) {
		workers = len(entries)
	}
	jobs := make(chan *packEntry, workers*4)
	var readers sync.WaitGroup
	readers.Add(workers)
	for range workers {
		go func() {
			defer readers.Done()
			for entry := range jobs {
				data, err := os.ReadFile(filepath.Join(root, entry.relative))
				entry.prefetched <- packReadResult{data: data, err: err}
			}
		}()
	}
	closeReaders := func() {
		close(jobs)
		readers.Wait()
	}
	readersClosed := false
	defer func() {
		if !readersClosed {
			closeReaders()
		}
	}()
	nextPrefetch := 0
	prefetchWindow := workers * 4
	queuePrefetch := func(limit int) {
		if limit > len(entries) {
			limit = len(entries)
		}
		for nextPrefetch < limit {
			entry := &entries[nextPrefetch]
			nextPrefetch++
			if !entry.info.Mode().IsRegular() || entry.info.Size() > maxPrefetchFileSize {
				continue
			}
			entry.prefetched = make(chan packReadResult, 1)
			jobs <- entry
		}
	}

	for index := range entries {
		queuePrefetch(index + prefetchWindow)
		entry := &entries[index]
		header, err := tar.FileInfoHeader(entry.info, entry.link)
		if err != nil {
			writer.Close()
			return err
		}
		header.Name = filepath.ToSlash(entry.relative)
		header.Uid = 0
		header.Gid = 0
		if err := writer.WriteHeader(header); err != nil {
			writer.Close()
			return err
		}
		if !entry.info.Mode().IsRegular() {
			continue
		}
		if entry.prefetched != nil {
			result := <-entry.prefetched
			if result.err != nil {
				writer.Close()
				return result.err
			}
			if _, err := writer.Write(result.data); err != nil {
				writer.Close()
				return err
			}
			continue
		}
		file, err := os.Open(filepath.Join(root, entry.relative))
		if err != nil {
			writer.Close()
			return err
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			writer.Close()
			return copyErr
		}
		if closeErr != nil {
			writer.Close()
			return closeErr
		}
	}
	closeReaders()
	readersClosed = true
	if err := writer.Close(); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Link(tempName, destination); err != nil {
		return fmt.Errorf("publishing checkpoint: %w", err)
	}
	if err := os.Remove(tempName); err != nil {
		_ = os.Remove(destination)
		return err
	}
	a.published = true
	return nil
}

// Section is where a regular file Unpack left in its archive is: its content
// is the archive's Size bytes from Offset.
type Section struct {
	Offset, Size int64
}

// Unpack extracts the TTC checkpoint archive into an empty destination, but
// for the regular files named in leave, which it leaves in the archive, and
// returns the sections of. It accepts safe symbolic links and rejects hard
// links, special files, duplicate paths, and traversal.
func Unpack(archive *os.File, destination string, leave ...string) (map[string]Section, error) {
	if err := os.Mkdir(destination, 0700); err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(destination)
		}
	}()
	reader := tar.NewReader(archive)
	sections := make(map[string]Section, len(leave))
	seen := make(map[string]struct{})
	type archivedSymlink struct {
		name   string
		target string
	}
	var symlinks []archivedSymlink
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := filepath.FromSlash(header.Name)
		if !validArchivePath(name) {
			return nil, fmt.Errorf("invalid checkpoint archive path %q", header.Name)
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("duplicate checkpoint archive path %q", header.Name)
		}
		seen[name] = struct{}{}
		path := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := ensureArchiveParents(destination, name); err != nil {
				return nil, err
			}
			mode := fs.FileMode(header.Mode).Perm()
			if mode == 0 {
				mode = 0700
			}
			if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return nil, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if slices.Contains(leave, header.Name) {
				// tar reads no further than a header: the member's content
				// begins where the archive is read to.
				offset, err := archive.Seek(0, io.SeekCurrent)
				if err != nil {
					return nil, err
				}
				sections[header.Name] = Section{Offset: offset, Size: header.Size}
				continue
			}
			if err := ensureArchiveParents(destination, name); err != nil {
				return nil, err
			}
			mode := fs.FileMode(header.Mode).Perm()
			if mode == 0 {
				mode = 0600
			}
			output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return nil, err
			}
			_, copyErr := io.CopyN(output, reader, header.Size)
			closeErr := output.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
		case tar.TypeSymlink:
			if header.Linkname == "" {
				return nil, fmt.Errorf("checkpoint archive symlink %q has an empty target", header.Name)
			}
			symlinks = append(symlinks, archivedSymlink{name: name, target: header.Linkname})
		default:
			return nil, fmt.Errorf("unsupported checkpoint archive entry %q type %d", header.Name, header.Typeflag)
		}
	}
	sort.Slice(symlinks, func(i, j int) bool { return symlinks[i].name < symlinks[j].name })
	for _, link := range symlinks {
		prefix := link.name + string(filepath.Separator)
		for name := range seen {
			if strings.HasPrefix(name, prefix) {
				return nil, fmt.Errorf("checkpoint archive path %q is beneath symlink %q", name, link.name)
			}
		}
		if err := ensureArchiveParents(destination, link.name); err != nil {
			return nil, err
		}
		if err := os.Symlink(link.target, filepath.Join(destination, link.name)); err != nil {
			return nil, err
		}
	}
	if _, err := ReadManifest(destination); err != nil {
		return nil, err
	}
	keep = true
	return sections, nil
}

func ensureArchiveParents(root, name string) error {
	parent := filepath.Dir(name)
	if parent == "." {
		return nil
	}
	current := root
	for _, component := range strings.Split(parent, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("checkpoint archive parent %q is not a directory", current)
		}
	}
	return nil
}

func validArchivePath(path string) bool {
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	return clean == path && clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func validArchiveSubpath(path, directory string) bool {
	if !validArchivePath(path) {
		return false
	}
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func requireArchivedRoot(root, name string) error {
	current := root
	components := strings.Split(name, string(filepath.Separator))
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if index != len(components)-1 {
			if !info.IsDir() {
				return fmt.Errorf("parent %q is not a directory", strings.Join(components[:index+1], string(filepath.Separator)))
			}
			continue
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("path is neither a directory nor a regular file")
		}
	}
	return nil
}
