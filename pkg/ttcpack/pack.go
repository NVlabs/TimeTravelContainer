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

// Package ttcpack stores immutable filesystem generations in range-readable,
// logically identified pack files. The format deliberately requires only ordinary
// filesystem operations; it does not depend on reflinks, sparse files, xattrs,
// or atomic writes larger than a pathname rename.
package ttcpack

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	Alignment     = int64(4096)
	MaxDataExtent = int64(1 << 20)
	formatVersion = 2
	headerMagic   = "TTCPACK2"
	recordMagic   = "TTCREC02"
	trailerMagic  = "TTCIDX02"
)

// ID is the stable logical identity of a committed pack. It is derived from
// small, transaction-fenced metadata rather than from bulk pack contents.
type ID [sha256.Size]byte

func (id ID) String() string { return hex.EncodeToString(id[:]) }

// ParseID validates a textual pack ID.
func ParseID(value string) (ID, error) {
	var id ID
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(id) {
		return id, fmt.Errorf("invalid pack ID %q", value)
	}
	copy(id[:], decoded)
	return id, nil
}

// IDForKey deterministically derives a pack ID from a namespace and logical
// identity components. Length-prefixing prevents delimiter ambiguity. This
// hashes only small control metadata; normal pack reads and writes never hash
// bulk data.
func IDForKey(namespace string, components ...string) ID {
	hash := sha256.New()
	write := func(value string) {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	write(namespace)
	for _, component := range components {
		write(component)
	}
	var id ID
	copy(id[:], hash.Sum(nil))
	return id
}

// Entry identifies one metadata or data record. Offset and StoredLength make
// the index independently range-readable from NFS or an HTTP peer.
type Entry struct {
	Path         string            `json:"path"`
	Kind         string            `json:"kind"`
	Mode         uint32            `json:"mode,omitempty"`
	UID          uint32            `json:"uid,omitempty"`
	GID          uint32            `json:"gid,omitempty"`
	MtimeNS      int64             `json:"mtime_ns,omitempty"`
	Link         string            `json:"link,omitempty"`
	Hardlink     string            `json:"hardlink,omitempty"`
	Device       uint64            `json:"device,omitempty"`
	Xattrs       map[string][]byte `json:"xattrs,omitempty"`
	FileOffset   int64             `json:"file_offset,omitempty"`
	Length       int64             `json:"length,omitempty"`
	RecordOffset int64             `json:"record_offset"`
	StoredLength int64             `json:"stored_length"`
}

type packHeader struct {
	Magic   string `json:"magic"`
	Version int    `json:"version"`
}

type recordHeader struct {
	Magic string `json:"magic"`
	Entry Entry  `json:"entry"`
}

type trailer struct {
	Magic       string `json:"magic"`
	Version     int    `json:"version"`
	IndexOffset int64  `json:"index_offset"`
	IndexLength int64  `json:"index_length"`
	RecordCount int    `json:"record_count"`
}

// Index is the structurally validated footer of an opened pack.
type Index struct {
	Entries []Entry `json:"entries"`
}

// WriteDirectory writes source as the immutable pack id in repository/packs.
// An existing structurally valid pack with the same transaction-fenced ID is
// reused, making publication retries idempotent.
func WriteDirectory(source, repository string, id ID) (ID, error) {
	if !filepath.IsAbs(source) || !filepath.IsAbs(repository) {
		return ID{}, errors.New("pack source and repository must be absolute")
	}
	if id == (ID{}) {
		return ID{}, errors.New("pack ID must not be zero")
	}
	if err := os.MkdirAll(filepath.Join(repository, "packs"), 0700); err != nil {
		return ID{}, err
	}
	temporary, err := os.CreateTemp(filepath.Join(repository, "packs"), ".pack-*.tmp")
	if err != nil {
		return ID{}, err
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	writer := &writer{file: temporary, hardlinks: make(map[fileIdentity]string)}
	if err := writer.writeBlock(packHeader{Magic: headerMagic, Version: formatVersion}); err != nil {
		return ID{}, err
	}
	if err := writer.walk(source); err != nil {
		return ID{}, err
	}
	indexOffset, _ := temporary.Seek(0, io.SeekCurrent)
	indexData, err := json.Marshal(Index{Entries: writer.entries})
	if err != nil {
		return ID{}, err
	}
	if err := writer.writeBytes(indexData); err != nil {
		return ID{}, err
	}
	if err := writer.pad(); err != nil {
		return ID{}, err
	}
	if err := writer.writeBlock(trailer{Magic: trailerMagic, Version: formatVersion, IndexOffset: indexOffset, IndexLength: int64(len(indexData)), RecordCount: len(writer.entries)}); err != nil {
		return ID{}, err
	}
	if err := temporary.Sync(); err != nil {
		return ID{}, err
	}
	if err := temporary.Close(); err != nil {
		return ID{}, err
	}
	destination := Path(repository, id)
	if err := os.Link(temporaryName, destination); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return ID{}, err
		}
		existing, _, openErr := Open(repository, id)
		if openErr != nil {
			return ID{}, fmt.Errorf("verifying existing pack: %w", openErr)
		}
		if closeErr := existing.Close(); closeErr != nil {
			return ID{}, closeErr
		}
	}
	if err := os.Remove(temporaryName); err != nil {
		return ID{}, err
	}
	committed = true
	if dir, err := os.Open(filepath.Dir(destination)); err == nil {
		err = errors.Join(dir.Sync(), dir.Close())
		if err != nil {
			return ID{}, err
		}
	}
	return id, nil
}

// Path returns the canonical pathname of id.
func Path(repository string, id ID) string {
	return filepath.Join(repository, "packs", id.String()+".pack")
}

type fileIdentity struct{ dev, ino uint64 }

type writer struct {
	file      *os.File
	entries   []Entry
	hardlinks map[fileIdentity]string
}

func (w *writer) walk(root string) error {
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			relative = ""
		}
		if err := validateRelative(relative); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		entry := Entry{Path: filepath.ToSlash(relative), Mode: uint32(info.Mode()), MtimeNS: info.ModTime().UnixNano()}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			entry.UID, entry.GID = stat.Uid, stat.Gid
			entry.Device = uint64(stat.Rdev)
			identity := fileIdentity{uint64(stat.Dev), stat.Ino}
			if info.Mode().IsRegular() && stat.Nlink > 1 {
				if first, ok := w.hardlinks[identity]; ok {
					entry.Kind, entry.Hardlink = "hardlink", first
					if err := w.record(entry, nil); err != nil {
						return err
					}
					continue
				}
				w.hardlinks[identity] = entry.Path
			}
		}
		xattrs, err := readXattrs(path, info.Mode()&os.ModeSymlink != 0)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(xattrs))
		for name := range xattrs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := w.record(Entry{Path: entry.Path, Kind: "xattr", Link: name, Length: int64(len(xattrs[name]))}, xattrs[name]); err != nil {
				return err
			}
		}
		switch {
		case info.IsDir():
			entry.Kind = "directory"
			err = w.record(entry, nil)
		case info.Mode().IsRegular():
			entry.Kind = "file"
			err = w.regular(path, entry)
		case info.Mode()&os.ModeSymlink != 0:
			entry.Kind = "symlink"
			entry.Link, err = os.Readlink(path)
			if err == nil {
				err = w.record(entry, nil)
			}
		case info.Mode()&os.ModeNamedPipe != 0:
			entry.Kind = "fifo"
			err = w.record(entry, nil)
		case info.Mode()&os.ModeDevice != 0:
			entry.Kind = "device"
			err = w.record(entry, nil)
		case info.Mode()&os.ModeSocket != 0:
			return fmt.Errorf("cannot publish live socket %q", path)
		default:
			return fmt.Errorf("unsupported file type %s at %q", info.Mode().Type(), path)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *writer) regular(path string, metadata Entry) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := make([]byte, MaxDataExtent)
	offset := int64(0)
	for {
		n, readErr := io.ReadFull(file, buffer)
		if readErr == io.ErrUnexpectedEOF || readErr == io.EOF {
			if n == 0 && offset != 0 {
				return nil
			}
		} else if readErr != nil {
			return readErr
		}
		entry := metadata
		entry.FileOffset, entry.Length = offset, int64(n)
		if err := w.record(entry, buffer[:n]); err != nil {
			return err
		}
		offset += int64(n)
		if readErr == io.ErrUnexpectedEOF || readErr == io.EOF {
			return nil
		}
	}
}

func (w *writer) record(entry Entry, data []byte) error {
	if int64(len(data)) > MaxDataExtent {
		return errors.New("pack data extent exceeds 1 MiB")
	}
	offset, err := w.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if offset%Alignment != 0 {
		return errors.New("unaligned pack record")
	}
	entry.RecordOffset = offset
	entry.StoredLength = Alignment + align(int64(len(data)))
	if err := w.writeBlock(recordHeader{Magic: recordMagic, Entry: entry}); err != nil {
		return err
	}
	if err := w.writeBytes(data); err != nil {
		return err
	}
	if err := w.pad(); err != nil {
		return err
	}
	w.entries = append(w.entries, entry)
	return nil
}

func (w *writer) writeBlock(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(data)+1) > Alignment {
		return errors.New("pack metadata record exceeds 4 KiB")
	}
	block := make([]byte, Alignment)
	copy(block, data)
	block[len(data)] = '\n'
	return w.writeBytes(block)
}

func (w *writer) writeBytes(data []byte) error {
	if n, err := w.file.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (w *writer) pad() error {
	offset, err := w.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	padding := align(offset) - offset
	if padding == 0 {
		return nil
	}
	return w.writeBytes(make([]byte, padding))
}

func align(value int64) int64 { return (value + Alignment - 1) &^ (Alignment - 1) }

func validateRelative(path string) error {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid pack path %q", path)
	}
	return nil
}

func readXattrs(path string, symlink bool) (map[string][]byte, error) {
	list := unix.Listxattr
	get := unix.Getxattr
	if symlink {
		list, get = unix.Llistxattr, unix.Lgetxattr
	}
	size, err := list(path, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, nil
		}
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	names := make([]byte, size)
	n, err := list(path, names)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte)
	for _, name := range bytes.Split(names[:n], []byte{0}) {
		if len(name) == 0 {
			continue
		}
		valueSize, err := get(path, string(name), nil)
		if err != nil {
			return nil, err
		}
		value := make([]byte, valueSize)
		if _, err := get(path, string(name), value); err != nil {
			return nil, err
		}
		result[string(name)] = value
	}
	return result, nil
}

// Open validates the structural header, footer, and index of a pack.
func Open(repository string, id ID) (*os.File, Index, error) {
	return open(repository, id)
}

// OpenIndexed is an explicit alias for Open used by demand-paged callers. Both
// read only fixed framing and indexed metadata, never the bulk payload.
func OpenIndexed(repository string, id ID) (*os.File, Index, error) {
	return open(repository, id)
}

func open(repository string, id ID) (*os.File, Index, error) {
	file, err := os.Open(Path(repository, id))
	if err != nil {
		return nil, Index{}, err
	}
	fail := func(err error) (*os.File, Index, error) { file.Close(); return nil, Index{}, err }
	if err := unix.Flock(int(file.Fd()), unix.LOCK_SH); err != nil {
		return fail(err)
	}
	stat, err := file.Stat()
	if err != nil || stat.Size() < 2*Alignment || stat.Size()%Alignment != 0 {
		return fail(fmt.Errorf("invalid pack size: %w", err))
	}
	index, err := ReadIndex(file)
	if err != nil {
		return fail(err)
	}
	return file, index, nil
}

// ReadIndex validates and reads pack metadata from an already-open pack. The
// caller owns file and is responsible for retaining any reader lease or lock.
func ReadIndex(file *os.File) (Index, error) {
	var header packHeader
	if err := readJSONBlock(file, 0, &header); err != nil || header.Magic != headerMagic || header.Version != formatVersion {
		return Index{}, fmt.Errorf("invalid pack header: %w", err)
	}
	stat, err := file.Stat()
	if err != nil || stat.Size() < 2*Alignment || stat.Size()%Alignment != 0 {
		return Index{}, fmt.Errorf("invalid pack size: %w", err)
	}
	var tail trailer
	if err := readJSONBlock(file, stat.Size()-Alignment, &tail); err != nil || tail.Magic != trailerMagic || tail.Version != formatVersion {
		return Index{}, fmt.Errorf("invalid pack trailer: %w", err)
	}
	if tail.IndexOffset < Alignment || tail.IndexLength < 0 || tail.IndexOffset+tail.IndexLength > stat.Size()-Alignment {
		return Index{}, errors.New("pack index range is invalid")
	}
	indexData := make([]byte, tail.IndexLength)
	if _, err := file.ReadAt(indexData, tail.IndexOffset); err != nil {
		return Index{}, err
	}
	var index Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return Index{}, err
	}
	if len(index.Entries) != tail.RecordCount {
		return Index{}, errors.New("pack record count mismatch")
	}
	for _, entry := range index.Entries {
		if entry.RecordOffset < Alignment || entry.RecordOffset%Alignment != 0 ||
			entry.StoredLength < Alignment || entry.StoredLength%Alignment != 0 ||
			entry.RecordOffset > tail.IndexOffset-entry.StoredLength {
			return Index{}, fmt.Errorf("pack record %q has an invalid extent", entry.Path)
		}
		if entry.Length < 0 || entry.Length > MaxDataExtent ||
			entry.Length > entry.StoredLength-Alignment {
			return Index{}, fmt.Errorf("pack record %q has an invalid payload length", entry.Path)
		}
	}
	return index, nil
}

// Collect removes old packs absent from keep. Open readers hold shared locks,
// so collection skips rather than unlinks an object being range-read on this
// or another cooperating NFS client.
func Collect(repository string, keep map[ID]struct{}, olderThan int64) (int, error) {
	directory := filepath.Join(repository, "packs")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".pack") {
			continue
		}
		id, err := ParseID(strings.TrimSuffix(name, ".pack"))
		if err != nil {
			return removed, err
		}
		if _, ok := keep[id]; ok {
			continue
		}
		path := filepath.Join(directory, name)
		info, err := entry.Info()
		if err != nil {
			return removed, err
		}
		if olderThan != 0 && info.ModTime().UnixNano() >= olderThan {
			continue
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		lockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, unix.EAGAIN) {
			file.Close()
			continue
		}
		if lockErr != nil {
			file.Close()
			return removed, lockErr
		}
		removeErr := os.Remove(path)
		closeErr := file.Close()
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removed, errors.Join(removeErr, closeErr)
		}
		if closeErr != nil {
			return removed, closeErr
		}
		removed++
	}
	return removed, nil
}

func readJSONBlock(file *os.File, offset int64, value any) error {
	block := make([]byte, Alignment)
	if _, err := file.ReadAt(block, offset); err != nil {
		return err
	}
	if end := bytes.IndexByte(block, '\n'); end >= 0 {
		block = block[:end]
	}
	return json.Unmarshal(block, value)
}

// ReadEntry returns one structurally bounded data extent without reading
// adjacent extents or hashing payload data.
func ReadEntry(file *os.File, entry Entry, destination []byte) (int, error) {
	if entry.Length < 0 || entry.Length > MaxDataExtent || int64(len(destination)) < entry.Length {
		return 0, errors.New("invalid pack extent buffer")
	}
	if entry.Length == 0 {
		return 0, nil
	}
	return file.ReadAt(destination[:entry.Length], entry.RecordOffset+Alignment)
}

// Materialize reconstructs a pack below an existing empty destination. It is
// primarily the compatibility fallback while Sentry-side lazy object
// resolution is unavailable; pack readers can instead call ReadEntry on
// demand and cache individual 1 MiB ranges.
func Materialize(repository string, id ID, destination string) error {
	return materialize(repository, id, destination, nil)
}

// MaterializeSkeleton reconstructs directory entries and non-regular objects,
// but leaves ordinary files as empty writable placeholders unless their path
// occurs in eagerPaths. A LazyDirectory must mediate access to the resulting
// root until every placeholder has been populated.
func MaterializeSkeleton(repository string, id ID, destination string, eagerPaths []string) error {
	eager := make(map[string]bool, len(eagerPaths))
	for _, path := range eagerPaths {
		if err := validateRelative(path); err != nil {
			return err
		}
		eager[filepath.ToSlash(path)] = true
	}
	return materialize(repository, id, destination, eager)
}

func materialize(repository string, id ID, destination string, eager map[string]bool) error {
	file, index, err := OpenIndexed(repository, id)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	created := make(map[string]bool)
	finalModes := make(map[string]os.FileMode)
	buffer := make([]byte, MaxDataExtent)
	for _, entry := range index.Entries {
		if err := validateRelative(entry.Path); err != nil {
			return err
		}
		path := filepath.Join(destination, filepath.FromSlash(entry.Path))
		if entry.Path == "" {
			path = destination
		}
		if entry.Path != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
		}
		switch entry.Kind {
		case "directory":
			// Keep directories searchable and writable until every descendant has
			// been installed. Image and snapshot modes such as 0550 and 0000 are
			// guest-visible final metadata, not extraction permissions.
			if err := os.MkdirAll(path, 0700); err != nil {
				return err
			}
			if err := os.Chmod(path, 0700); err != nil {
				return err
			}
			finalModes[path] = os.FileMode(entry.Mode).Perm()
		case "file":
			lazy := eager != nil && !eager[entry.Path]
			flags := os.O_WRONLY
			if !created[entry.Path] {
				flags |= os.O_CREATE | os.O_EXCL
			}
			out, err := os.OpenFile(path, flags, 0600)
			if err != nil {
				return err
			}
			var n int
			if !lazy {
				n, err = ReadEntry(file, entry, buffer)
				if err == nil && n != 0 {
					_, err = out.WriteAt(buffer[:n], entry.FileOffset)
				}
			}
			err = errors.Join(err, out.Close())
			if err != nil {
				return err
			}
			created[entry.Path] = true
			if !lazy {
				finalModes[path] = os.FileMode(entry.Mode).Perm()
			}
		case "hardlink":
			if err := os.Link(filepath.Join(destination, filepath.FromSlash(entry.Hardlink)), path); err != nil {
				return err
			}
			if eager == nil || eager[entry.Hardlink] {
				finalModes[path] = os.FileMode(entry.Mode).Perm()
			}
		case "symlink":
			if err := os.Symlink(entry.Link, path); err != nil {
				return err
			}
		case "fifo":
			if err := unix.Mkfifo(path, entry.Mode&07777); err != nil {
				return err
			}
			finalModes[path] = os.FileMode(entry.Mode).Perm()
		case "device":
			if err := unix.Mknod(path, entry.Mode, int(entry.Device)); err != nil {
				return err
			}
			finalModes[path] = os.FileMode(entry.Mode).Perm()
		case "xattr":
			n, err := ReadEntry(file, entry, buffer)
			if err != nil {
				return err
			}
			if err := unix.Lsetxattr(path, entry.Link, buffer[:n], 0); err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
				return err
			}
		default:
			return fmt.Errorf("unknown pack entry kind %q", entry.Kind)
		}
		if entry.Kind != "xattr" && (entry.Kind != "file" || entry.FileOffset == 0) {
			for name, value := range entry.Xattrs {
				if err := unix.Lsetxattr(path, name, value, 0); err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
					return err
				}
			}
		}
	}
	// Restore children before directories so restrictive parent permissions do
	// not prevent metadata restoration below them.
	paths := make([]string, 0, len(finalModes))
	for path := range finalModes {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		return strings.Count(paths[i], string(filepath.Separator)) > strings.Count(paths[j], string(filepath.Separator))
	})
	for _, path := range paths {
		if err := os.Chmod(path, finalModes[path]); err != nil {
			return err
		}
	}
	return nil
}
