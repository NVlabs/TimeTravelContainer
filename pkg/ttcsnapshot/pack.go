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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	sharedPackVersion   = 1
	sharedPackAlignment = int64(4096)
	sharedPackExtent    = int64(1 << 20)
	sharedPackMagic     = "TTCSGPK1"
	sharedManifestName  = "manifest.json"
)

// PackExtent is one independently range-readable portion of an immutable
// object. Extents are at most 1 MiB and begin at a 4 KiB-aligned pack offset.
type PackExtent struct {
	Pack   uint32 `json:"pack"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

// PackedObject binds graph metadata to its bulk extents. Metadata-only graph
// objects have no extents.
type PackedObject struct {
	Object  Object       `json:"object"`
	Extents []PackExtent `json:"extents,omitempty"`
}

// PackInfo describes one immutable physical pack.
type PackInfo struct {
	Number uint32 `json:"number"`
	Size   int64  `json:"size"`
}

// SnapshotManifest is the sole authority pointer for a published snapshot.
// Pack files without this manifest are uncommitted staging residue.
type SnapshotManifest struct {
	Version    int                 `json:"version"`
	Snapshot   SnapshotID          `json:"snapshot"`
	Packs      []PackInfo          `json:"packs"`
	Objects    []PackedObject      `json:"objects"`
	References map[string]ObjectID `json:"references,omitempty"`
}

// SharedPackStore stores immutable snapshot-origin packs. It intentionally
// uses no content checksums: durability and authority come from sync followed
// by manifest-last publication, not from treating corruption as a protocol.
type SharedPackStore struct {
	root string

	mu        sync.Mutex
	manifests map[SnapshotID]cachedSharedManifest
}

type cachedSharedManifest struct {
	manifest *SnapshotManifest
	info     os.FileInfo
}

// OpenSharedPackStore opens a shared pack repository.
func OpenSharedPackStore(root string) (*SharedPackStore, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("TTC shared pack directory must be absolute")
	}
	if err := os.MkdirAll(filepath.Join(root, "snapshots"), 0700); err != nil {
		return nil, err
	}
	return &SharedPackStore{root: root, manifests: make(map[SnapshotID]cachedSharedManifest)}, nil
}

func (store *SharedPackStore) snapshotDirectory(id SnapshotID) string {
	return filepath.Join(store.root, "snapshots", snapshotKey(id))
}

func packName(number uint32) string { return fmt.Sprintf("pack-%08x.data", number) }

func parsePackName(name string) (uint32, bool) {
	if !strings.HasPrefix(name, "pack-") || !strings.HasSuffix(name, ".data") {
		return 0, false
	}
	value, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "pack-"), ".data"), 16, 32)
	if err != nil || packName(uint32(value)) != name {
		return 0, false
	}
	return uint32(value), true
}

func alignPack(value int64) int64 {
	return (value + sharedPackAlignment - 1) &^ (sharedPackAlignment - 1)
}

func writePackHeader(file *os.File, snapshot SnapshotID, number uint32) error {
	header := make([]byte, sharedPackAlignment)
	copy(header, sharedPackMagic)
	binary.LittleEndian.PutUint32(header[8:12], sharedPackVersion)
	binary.LittleEndian.PutUint64(header[16:24], uint64(snapshot))
	binary.LittleEndian.PutUint32(header[24:28], number)
	_, err := file.Write(header)
	return err
}

func validatePackHeader(file *os.File, snapshot SnapshotID, number uint32, expectedSize int64) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != expectedSize || info.Size() < sharedPackAlignment || info.Size()%sharedPackAlignment != 0 {
		return fmt.Errorf("shared pack %d has size %d, want %d", number, info.Size(), expectedSize)
	}
	header := make([]byte, sharedPackAlignment)
	if _, err := file.ReadAt(header, 0); err != nil {
		return err
	}
	if !bytes.Equal(header[:8], []byte(sharedPackMagic)) ||
		binary.LittleEndian.Uint32(header[8:12]) != sharedPackVersion ||
		SnapshotID(binary.LittleEndian.Uint64(header[16:24])) != snapshot ||
		binary.LittleEndian.Uint32(header[24:28]) != number {
		return fmt.Errorf("shared pack %d has an invalid header", number)
	}
	return nil
}

func writePadding(file *os.File) error {
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	padding := alignPack(offset) - offset
	if padding == 0 {
		return nil
	}
	_, err = file.Write(make([]byte, padding))
	return err
}

// Publish writes every object allocated by snapshot into a range-readable
// pack and commits the manifest last. Retrying an already committed snapshot
// is idempotent and never replaces its authority.
func (store *SharedPackStore) Publish(ctx context.Context, graph *Store, snapshot SnapshotID, source ObjectSource) error {
	return store.publish(ctx, graph, snapshot, source, false)
}

// PublishHandoff publishes a provider-to-shared handoff baseline. The graph
// durably records reference mutations that race the bulk pack write, allowing
// the caller to catch the shared ledger up before the short final freeze.
func (store *SharedPackStore) PublishHandoff(ctx context.Context, graph *Store, snapshot SnapshotID, source ObjectSource) error {
	return store.publish(ctx, graph, snapshot, source, true)
}

func (store *SharedPackStore) publish(ctx context.Context, graph *Store, snapshot SnapshotID, source ObjectSource, handoff bool) error {
	if graph == nil || source == nil || snapshot == 0 {
		return errors.New("invalid shared snapshot publication")
	}
	if _, err := store.Manifest(snapshot); err == nil {
		// The manifest is the immutable authority point. Provider reference
		// counts may already have moved to the shared ledger, so a post-handoff
		// retry must not compare them with the former local graph.
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	snapshotRecord, ok := graph.Snapshot(snapshot)
	if !ok {
		return os.ErrNotExist
	}
	if snapshotRecord.State != SnapshotSealedVolatile && snapshotRecord.State != SnapshotSealedLocal && snapshotRecord.State != SnapshotShared {
		return fmt.Errorf("snapshot %d is %q, not immutable", snapshot, snapshotRecord.State)
	}
	var objects []Object
	var references map[string]ObjectID
	var err error
	if handoff {
		objects, references, _, err = graph.BeginSnapshotLedgerHandoff(snapshot)
	} else {
		objects, references, err = graph.SnapshotLedger(snapshot)
	}
	if err != nil {
		return err
	}
	directory := store.snapshotDirectory(snapshot)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".pack-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	const packNumber = uint32(0)
	if err := writePackHeader(temporary, snapshot, packNumber); err != nil {
		temporary.Close()
		return err
	}
	manifest := SnapshotManifest{Version: sharedPackVersion, Snapshot: snapshot, References: references}
	buffer := make([]byte, sharedPackExtent)
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			temporary.Close()
			return err
		}
		packed := PackedObject{Object: object}
		if object.BulkLength != 0 {
			reader, length, err := source.Fetch(ctx, object.ID)
			if err != nil {
				temporary.Close()
				return fmt.Errorf("fetching %s for publication: %w", object.ID.key(), err)
			}
			if length != object.BulkLength {
				reader.Close()
				temporary.Close()
				return fmt.Errorf("object %s source length is %d, want %d", object.ID.key(), length, object.BulkLength)
			}
			remaining := int64(length)
			for remaining != 0 {
				amount := min(remaining, sharedPackExtent)
				offset, _ := temporary.Seek(0, io.SeekCurrent)
				n, copyErr := io.ReadFull(reader, buffer[:amount])
				if copyErr != nil {
					reader.Close()
					temporary.Close()
					return fmt.Errorf("reading object %s: %w", object.ID.key(), copyErr)
				}
				if _, err := temporary.Write(buffer[:n]); err != nil {
					reader.Close()
					temporary.Close()
					return err
				}
				packed.Extents = append(packed.Extents, PackExtent{Pack: packNumber, Offset: offset, Length: int64(n)})
				remaining -= int64(n)
				if err := writePadding(temporary); err != nil {
					reader.Close()
					temporary.Close()
					return err
				}
			}
			if extra := make([]byte, 1); true {
				n, readErr := reader.Read(extra)
				if n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
					reader.Close()
					temporary.Close()
					return fmt.Errorf("object %s source exceeds declared length", object.ID.key())
				}
			}
			if err := reader.Close(); err != nil {
				temporary.Close()
				return err
			}
		}
		manifest.Objects = append(manifest.Objects, packed)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	info, err := temporary.Stat()
	if err != nil {
		temporary.Close()
		return err
	}
	manifest.Packs = []PackInfo{{Number: packNumber, Size: info.Size()}}
	if err := temporary.Close(); err != nil {
		return err
	}
	destination := filepath.Join(directory, packName(packNumber))
	if err := os.Link(temporaryPath, destination); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		existing, openErr := os.Open(destination)
		if openErr != nil {
			return openErr
		}
		validateErr := validatePackHeader(existing, snapshot, packNumber, info.Size())
		existing.Close()
		if validateErr != nil {
			return fmt.Errorf("validating existing immutable pack: %w", validateErr)
		}
	}
	if err := syncDirectory(directory); err != nil {
		return err
	}
	if err := writeAtomicJSON(filepath.Join(directory, sharedManifestName), manifest); err != nil {
		return err
	}
	store.mu.Lock()
	delete(store.manifests, snapshot)
	store.mu.Unlock()
	return nil
}

func writeAtomicJSON(path string, value any) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".manifest-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
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
	if err := os.Link(temporaryPath, path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return syncDirectory(directory)
}

func replaceAtomicJSON(path string, value any) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".manifest-replacement-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func cloneManifest(manifest *SnapshotManifest) *SnapshotManifest {
	if manifest == nil {
		return nil
	}
	copy := *manifest
	copy.Packs = append([]PackInfo(nil), manifest.Packs...)
	copy.References = make(map[string]ObjectID, len(manifest.References))
	for name, object := range manifest.References {
		copy.References[name] = object
	}
	copy.Objects = make([]PackedObject, len(manifest.Objects))
	for index, object := range manifest.Objects {
		copy.Objects[index] = PackedObject{Object: cloneObject(object.Object), Extents: append([]PackExtent(nil), object.Extents...)}
	}
	return &copy
}

// Manifest opens and structurally validates an authoritative manifest and its
// pack headers without reading bulk data.
func (store *SharedPackStore) Manifest(snapshot SnapshotID) (*SnapshotManifest, error) {
	path := filepath.Join(store.snapshotDirectory(snapshot), sharedManifestName)
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	store.mu.Lock()
	if cached, ok := store.manifests[snapshot]; ok && cached.manifest != nil && cached.info != nil && os.SameFile(cached.info, info) && cached.info.Size() == info.Size() && cached.info.ModTime() == info.ModTime() {
		result := cloneManifest(cached.manifest)
		store.mu.Unlock()
		file.Close()
		return result, nil
	}
	store.mu.Unlock()
	data, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	var manifest SnapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decoding shared snapshot manifest: %w", err)
	}
	if err := store.validateManifest(&manifest); err != nil {
		return nil, err
	}
	store.mu.Lock()
	store.manifests[snapshot] = cachedSharedManifest{manifest: cloneManifest(&manifest), info: info}
	store.mu.Unlock()
	return cloneManifest(&manifest), nil
}

func (store *SharedPackStore) validateManifest(manifest *SnapshotManifest) error {
	if manifest.Version != sharedPackVersion || manifest.Snapshot == 0 {
		return errors.New("invalid shared snapshot manifest header")
	}
	packs := make(map[uint32]PackInfo, len(manifest.Packs))
	for _, pack := range manifest.Packs {
		if pack.Size < sharedPackAlignment || pack.Size%sharedPackAlignment != 0 {
			return fmt.Errorf("shared pack %d has invalid declared size", pack.Number)
		}
		if _, exists := packs[pack.Number]; exists {
			return fmt.Errorf("shared manifest repeats pack %d", pack.Number)
		}
		packs[pack.Number] = pack
		file, err := os.Open(filepath.Join(store.snapshotDirectory(manifest.Snapshot), packName(pack.Number)))
		if err != nil {
			return err
		}
		err = validatePackHeader(file, manifest.Snapshot, pack.Number, pack.Size)
		file.Close()
		if err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(manifest.Objects))
	objects := make(map[string]Object, len(manifest.Objects))
	for _, packed := range manifest.Objects {
		if err := validateObject(packed.Object); err != nil {
			return err
		}
		if packed.Object.ID.Snapshot != manifest.Snapshot {
			return fmt.Errorf("object %s is in the wrong snapshot manifest", packed.Object.ID.key())
		}
		if _, exists := seen[packed.Object.ID.key()]; exists {
			return fmt.Errorf("shared manifest repeats object %s", packed.Object.ID.key())
		}
		seen[packed.Object.ID.key()] = struct{}{}
		objects[packed.Object.ID.key()] = packed.Object
		var length uint64
		for _, extent := range packed.Extents {
			pack, ok := packs[extent.Pack]
			if !ok || extent.Offset < sharedPackAlignment || extent.Offset%sharedPackAlignment != 0 ||
				extent.Length <= 0 || extent.Length > sharedPackExtent || extent.Offset > pack.Size-extent.Length {
				return fmt.Errorf("object %s has an invalid pack extent", packed.Object.ID.key())
			}
			length += uint64(extent.Length)
		}
		if length != packed.Object.BulkLength {
			return fmt.Errorf("object %s extents total %d, want %d", packed.Object.ID.key(), length, packed.Object.BulkLength)
		}
	}
	for name, object := range manifest.References {
		if err := validateReferenceName(name); err != nil {
			return fmt.Errorf("shared manifest reference %q: %w", name, err)
		}
		if object.Snapshot != manifest.Snapshot {
			return fmt.Errorf("shared manifest reference %q selects foreign object %s", name, object)
		}
		if _, ok := objects[object.key()]; !ok {
			return fmt.Errorf("shared manifest reference %q selects absent object %s", name, object)
		}
	}
	counts := make(map[string]uint64, len(objects))
	for _, packed := range manifest.Objects {
		for _, edge := range packed.Object.Children {
			if edge.Child.Snapshot == manifest.Snapshot {
				if _, ok := objects[edge.Child.key()]; !ok {
					return fmt.Errorf("shared object %s selects absent same-origin child %s", packed.Object.ID, edge.Child)
				}
				counts[edge.Child.key()]++
			}
		}
	}
	for _, object := range manifest.References {
		counts[object.key()]++
	}
	for _, object := range objects {
		if counts[object.ID.key()] != object.Refs {
			return fmt.Errorf("shared object %s baseline has %d references, manifest records %d", object.ID, counts[object.ID.key()], object.Refs)
		}
	}
	sort.Slice(manifest.Objects, func(i, j int) bool {
		return manifest.Objects[i].Object.ID.key() < manifest.Objects[j].Object.ID.key()
	})
	return nil
}

type extentReadCloser struct {
	reader io.Reader
	files  []*os.File
	lease  *os.File
}

func (reader *extentReadCloser) Read(destination []byte) (int, error) {
	return reader.reader.Read(destination)
}

func (reader *extentReadCloser) Close() error {
	var result error
	for _, file := range reader.files {
		result = errors.Join(result, file.Close())
	}
	reader.files = nil
	if reader.lease != nil {
		result = errors.Join(result, syscall.Flock(int(reader.lease.Fd()), syscall.LOCK_UN), reader.lease.Close())
		reader.lease = nil
	}
	return result
}

// acquireSnapshotLease locks the snapshot directory itself rather than a
// regular lock file. This matters on NFS: removing an open regular lock file
// creates a .nfs tombstone. An open directory can be renamed and removed after
// the lock is released without creating hidden files.
func (store *SharedPackStore) acquireSnapshotLease(snapshot SnapshotID, operation int) (*os.File, error) {
	directory, err := os.Open(store.snapshotDirectory(snapshot))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(directory.Fd()), operation); err != nil {
		directory.Close()
		return nil, err
	}
	return directory, nil
}

func (store *SharedPackStore) acquireSnapshotReadLease(ctx context.Context, snapshot SnapshotID) (*os.File, error) {
	for {
		lease, err := store.acquireSnapshotLease(snapshot, syscall.LOCK_SH|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return lease, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// Fetch opens a complete immutable object by stable ID. Only the manifest and
// requested extents are read; unrelated objects remain untouched.
func (store *SharedPackStore) Fetch(ctx context.Context, id ObjectID) (io.ReadCloser, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	lease, err := store.acquireSnapshotReadLease(ctx, id.Snapshot)
	if err != nil {
		return nil, 0, err
	}
	manifest, err := store.Manifest(id.Snapshot)
	if err != nil {
		syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		lease.Close()
		return nil, 0, err
	}
	index := sort.Search(len(manifest.Objects), func(index int) bool {
		return manifest.Objects[index].Object.ID.key() >= id.key()
	})
	if index == len(manifest.Objects) || manifest.Objects[index].Object.ID != id {
		syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		lease.Close()
		return nil, 0, os.ErrNotExist
	}
	packed := manifest.Objects[index]
	readers := make([]io.Reader, 0, len(packed.Extents))
	files := make([]*os.File, 0, len(packed.Extents))
	for _, extent := range packed.Extents {
		file, err := os.Open(filepath.Join(store.snapshotDirectory(id.Snapshot), packName(extent.Pack)))
		if err != nil {
			for _, opened := range files {
				opened.Close()
			}
			syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
			lease.Close()
			return nil, 0, err
		}
		files = append(files, file)
		readers = append(readers, io.NewSectionReader(file, extent.Offset, extent.Length))
	}
	return &extentReadCloser{reader: io.MultiReader(readers...), files: files, lease: lease}, packed.Object.BulkLength, nil
}

// PackCompactionStats describes one physical representation rewrite. Object
// identities and logical reference counts do not change.
type PackCompactionStats struct {
	BeforePacks   int
	BeforeObjects int
	BeforeBytes   int64
	AfterPacks    int
	AfterObjects  int
	AfterBytes    int64
}

// Compact rewrites one partially-live origin into a single pack using the
// exact object/reference view supplied by its authoritative reference ledger.
// It takes the same directory lease as retirement, so no cooperating reader
// can retain an old pack FD while it is unlinked (important on NFS).
func (store *SharedPackStore) Compact(snapshot SnapshotID, objects []Object, references map[string]ObjectID) (PackCompactionStats, bool, error) {
	var stats PackCompactionStats
	if snapshot == 0 || len(objects) == 0 {
		return stats, false, errors.New("shared pack compaction requires a nonempty origin")
	}
	lease, err := store.acquireSnapshotLease(snapshot, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return stats, true, nil
	}
	if err != nil {
		return stats, false, err
	}
	defer func() {
		_ = syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		_ = lease.Close()
	}()
	manifest, err := store.Manifest(snapshot)
	if err != nil {
		return stats, false, err
	}
	stats.BeforePacks, stats.BeforeObjects = len(manifest.Packs), len(manifest.Objects)
	for _, pack := range manifest.Packs {
		stats.BeforeBytes += pack.Size
	}
	old := make(map[string]PackedObject, len(manifest.Objects))
	for _, object := range manifest.Objects {
		old[object.Object.ID.key()] = object
	}
	objects = append([]Object(nil), objects...)
	sort.Slice(objects, func(i, j int) bool { return objects[i].ID.key() < objects[j].ID.key() })
	for _, object := range objects {
		packed, ok := old[object.ID.key()]
		if !ok || object.ID.Snapshot != snapshot || object.State != ObjectLive || packed.Object.BulkLength != object.BulkLength {
			return stats, false, fmt.Errorf("shared pack compaction view contains invalid object %s", object.ID)
		}
		if err := validateObject(object); err != nil {
			return stats, false, err
		}
	}

	directory := store.snapshotDirectory(snapshot)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return stats, false, err
	}
	var nextPack uint64
	for _, entry := range entries {
		name := entry.Name()
		value, ok := parsePackName(name)
		if ok && uint64(value) >= nextPack {
			nextPack = uint64(value) + 1
		}
	}
	if nextPack > uint64(^uint32(0)) {
		return stats, false, errors.New("shared pack number space exhausted")
	}
	packNumber := uint32(nextPack)
	temporary, err := os.CreateTemp(directory, ".compact-pack-")
	if err != nil {
		return stats, false, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(err error) (PackCompactionStats, bool, error) {
		_ = temporary.Close()
		return stats, false, err
	}
	if err := writePackHeader(temporary, snapshot, packNumber); err != nil {
		return fail(err)
	}
	replacement := SnapshotManifest{Version: sharedPackVersion, Snapshot: snapshot, References: make(map[string]ObjectID, len(references))}
	for name, object := range references {
		replacement.References[name] = object
	}
	for _, object := range objects {
		previous := old[object.ID.key()]
		packed := PackedObject{Object: cloneObject(object)}
		for _, extent := range previous.Extents {
			source, err := os.Open(filepath.Join(directory, packName(extent.Pack)))
			if err != nil {
				return fail(err)
			}
			offset, err := temporary.Seek(0, io.SeekCurrent)
			if err == nil {
				_, err = io.CopyN(temporary, io.NewSectionReader(source, extent.Offset, extent.Length), extent.Length)
			}
			closeErr := source.Close()
			if err != nil || closeErr != nil {
				return fail(errors.Join(err, closeErr))
			}
			packed.Extents = append(packed.Extents, PackExtent{Pack: packNumber, Offset: offset, Length: extent.Length})
			if err := writePadding(temporary); err != nil {
				return fail(err)
			}
		}
		replacement.Objects = append(replacement.Objects, packed)
	}
	if err := temporary.Sync(); err != nil {
		return fail(err)
	}
	info, err := temporary.Stat()
	if err != nil {
		return fail(err)
	}
	replacement.Packs = []PackInfo{{Number: packNumber, Size: info.Size()}}
	if err := temporary.Close(); err != nil {
		return stats, false, err
	}
	destination := filepath.Join(directory, packName(packNumber))
	if err := os.Rename(temporaryPath, destination); err != nil {
		return stats, false, err
	}
	if err := syncDirectory(directory); err != nil {
		return stats, false, err
	}
	if err := store.validateManifest(&replacement); err != nil {
		return stats, false, fmt.Errorf("validating compacted shared pack: %w", err)
	}
	if err := replaceAtomicJSON(filepath.Join(directory, sharedManifestName), replacement); err != nil {
		return stats, false, err
	}
	store.mu.Lock()
	delete(store.manifests, snapshot)
	store.mu.Unlock()
	var removeErr error
	for _, pack := range manifest.Packs {
		if pack.Number != packNumber {
			removeErr = errors.Join(removeErr, os.Remove(filepath.Join(directory, packName(pack.Number))))
		}
	}
	removeErr = errors.Join(removeErr, syncDirectory(directory))
	stats.AfterPacks, stats.AfterObjects, stats.AfterBytes = 1, len(objects), info.Size()
	return stats, false, removeErr
}

// Reconcile removes finalized pack files that are not selected by the current
// authoritative manifest. This rolls forward a crash or transient failure
// after a compaction manifest switch. Unknown and temporary files are left
// untouched for the higher-level transaction reconciler; they never make the
// pack unreadable.
func (store *SharedPackStore) Reconcile(snapshot SnapshotID) (int, bool, error) {
	if snapshot == 0 {
		return 0, false, errors.New("cannot reconcile snapshot zero")
	}
	lease, err := store.acquireSnapshotLease(snapshot, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer func() {
		_ = syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		_ = lease.Close()
	}()
	manifest, err := store.Manifest(snapshot)
	if err != nil {
		return 0, false, err
	}
	live := make(map[uint32]struct{}, len(manifest.Packs))
	for _, pack := range manifest.Packs {
		live[pack.Number] = struct{}{}
	}
	entries, err := os.ReadDir(store.snapshotDirectory(snapshot))
	if err != nil {
		return 0, false, err
	}
	removed := 0
	for _, entry := range entries {
		number, ok := parsePackName(entry.Name())
		if !ok {
			continue
		}
		if _, ok := live[number]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(store.snapshotDirectory(snapshot), entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, false, err
		}
		removed++
	}
	if removed != 0 {
		if err := syncDirectory(store.snapshotDirectory(snapshot)); err != nil {
			return removed, false, err
		}
	}
	return removed, false, nil
}

// Retire removes one unreachable committed snapshot pack without racing a
// cooperating reader. The caller must hold the current Raft shared-GC lease
// and must have proved that no authoritative shared root or immutable edge
// references this origin. busy=true means a reader still has an extent open.
func (store *SharedPackStore) Retire(snapshot SnapshotID) (busy bool, err error) {
	if snapshot == 0 {
		return false, errors.New("cannot retire snapshot zero")
	}
	lease, err := store.acquireSnapshotLease(snapshot, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	directory := store.snapshotDirectory(snapshot)
	if _, err := os.Stat(filepath.Join(directory, sharedManifestName)); err != nil {
		syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		lease.Close()
		return false, err
	}
	parent := filepath.Dir(directory)
	var retired string
	for attempt := 0; attempt != 16; attempt++ {
		retired = filepath.Join(parent, fmt.Sprintf(".retired-%s-%d-%d-%d", snapshotKey(snapshot), os.Getpid(), time.Now().UnixNano(), attempt))
		if err = os.Rename(directory, retired); err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	unlockErr := syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
	closeErr := lease.Close()
	if err != nil {
		return false, errors.Join(err, unlockErr, closeErr)
	}
	if err := syncDirectory(parent); err != nil {
		return false, errors.Join(err, unlockErr, closeErr)
	}
	removeErr := os.RemoveAll(retired)
	syncErr := syncDirectory(parent)
	store.mu.Lock()
	delete(store.manifests, snapshot)
	store.mu.Unlock()
	return false, errors.Join(unlockErr, closeErr, removeErr, syncErr)
}

// CompleteRetirement idempotently finishes physical deletion after Raft has
// permanently retired snapshot. It also removes directories left after a
// crash between the namespace rename and RemoveAll.
func (store *SharedPackStore) CompleteRetirement(snapshot SnapshotID) (busy bool, err error) {
	if snapshot == 0 {
		return false, errors.New("cannot complete retirement of snapshot zero")
	}
	parent := filepath.Join(store.root, "snapshots")
	prefix := ".retired-" + snapshotKey(snapshot) + "-"
	entries, err := os.ReadDir(parent)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(parent, entry.Name())); err != nil {
			return false, err
		}
	}
	if _, err := os.Stat(store.snapshotDirectory(snapshot)); errors.Is(err, fs.ErrNotExist) {
		store.mu.Lock()
		delete(store.manifests, snapshot)
		store.mu.Unlock()
		return false, syncDirectory(parent)
	} else if err != nil {
		return false, err
	}
	return store.Retire(snapshot)
}
