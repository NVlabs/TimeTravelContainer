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

type publicationBulkSource struct {
	id     ObjectID
	source ObjectSource
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
	if err := ctx.Err(); err != nil {
		return err
	}
	directory := store.snapshotDirectory(snapshot)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	// More than one retry may exist on the authorized provider. Serialize the
	// whole publication with readers, compaction, and retirement, then check
	// the manifest under that lease. In particular, a failed retry must never
	// remove a namespace or pack committed by another retry.
	lease, err := store.acquireSnapshotWriteLease(ctx, snapshot)
	if err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		_ = lease.Close()
	}()
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
	if handoff {
		objects, references, _, err = graph.BeginSnapshotLedgerHandoff(snapshot)
	} else {
		objects, references, err = graph.SnapshotLedger(snapshot)
	}
	if err != nil {
		return err
	}
	bulkSources := make(map[string]publicationBulkSource)
	for index := range objects {
		object := objects[index]
		if !object.Deferred {
			if object.BulkLength != 0 {
				bulkSources[object.ID.key()] = publicationBulkSource{id: object.ID, source: source}
			}
			continue
		}
		resolved, bulk, err := store.resolveDeferredForPublication(graph, source, object)
		if err != nil {
			return fmt.Errorf("resolving deferred object %s for publication: %w", object.ID, err)
		}
		objects[index] = resolved
		if resolved.BulkLength != 0 {
			bulkSources[resolved.ID.key()] = bulk
		}
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
			bulk := bulkSources[object.ID.key()]
			if bulk.source == nil || bulk.id.IsZero() {
				temporary.Close()
				return fmt.Errorf("object %s has no publication bulk source", object.ID)
			}
			reader, length, err := bulk.source.Fetch(ctx, bulk.id)
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

// resolveDeferredForPublication path-compresses an inherited immutable stub
// into the pack being published. This prevents a cold snapshot from depending
// on a manager-side resolver that disappears with its Sentry. The alias keeps
// its current stable ID and reference count, while its immutable payload and
// child edges come from the terminal source object. Cross-origin child edges
// are then visible to the shared reference ledger.
func (store *SharedPackStore) resolveDeferredForPublication(graph *Store, local ObjectSource, alias Object) (Object, publicationBulkSource, error) {
	requested := alias.ID
	current := alias.SourceObject
	seen := make(map[ObjectID]struct{})
	for !current.IsZero() {
		if _, duplicate := seen[current]; duplicate {
			return Object{}, publicationBulkSource{}, errors.New("deferred object source cycle")
		}
		seen[current] = struct{}{}
		var object Object
		var bulk ObjectSource
		if shared, err := store.Object(current); err == nil {
			object, bulk = shared, store
		} else {
			localObject, ok := graph.Object(current)
			if !ok {
				return Object{}, publicationBulkSource{}, fmt.Errorf("source object %s is unavailable", current)
			}
			object, bulk = localObject, local
		}
		if object.State != ObjectLive {
			return Object{}, publicationBulkSource{}, fmt.Errorf("source object %s is not live", current)
		}
		if object.Deferred {
			current = object.SourceObject
			continue
		}
		terminal := object.ID
		object.ID = requested
		object.Refs = alias.Refs
		object.SourceCatalog = alias.SourceCatalog
		object.SourceObject = alias.SourceObject
		object.Deferred = false
		if err := validateObject(object); err != nil {
			return Object{}, publicationBulkSource{}, err
		}
		return object, publicationBulkSource{id: terminal, source: bulk}, nil
	}
	return Object{}, publicationBulkSource{}, errors.New("deferred object has no source identity")
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
	if cached, ok := store.manifests[snapshot]; ok && cached.manifest != nil && cached.info != nil && os.SameFile(cached.info, info) && cached.info.Size() == info.Size() && cached.info.ModTime().Equal(info.ModTime()) {
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

func packedObjectByID(manifest *SnapshotManifest, id ObjectID) (PackedObject, bool) {
	index := sort.Search(len(manifest.Objects), func(index int) bool {
		return manifest.Objects[index].Object.ID.key() >= id.key()
	})
	if index == len(manifest.Objects) || manifest.Objects[index].Object.ID != id {
		return PackedObject{}, false
	}
	return manifest.Objects[index], true
}

// Object returns immutable metadata for id without reading its bulk payload.
// It is the shared-storage counterpart of Store.ResolveObject and is used by
// lazy filesystem catalogs.
func (store *SharedPackStore) Object(id ObjectID) (Object, error) {
	manifest, err := store.Manifest(id.Snapshot)
	if err != nil {
		return Object{}, err
	}
	packed, ok := packedObjectByID(manifest, id)
	if !ok {
		return Object{}, os.ErrNotExist
	}
	return cloneObject(packed.Object), nil
}

// Objects returns immutable metadata for ids while opening each snapshot
// manifest at most once. Callers that validate a complete memory graph may
// name thousands of objects; repeatedly calling Object would clone the entire
// manifest for every ID and turn that validation into quadratic work.
func (store *SharedPackStore) Objects(ids []ObjectID) (map[ObjectID]Object, error) {
	result := make(map[ObjectID]Object, len(ids))
	bySnapshot := make(map[SnapshotID][]ObjectID)
	for _, id := range ids {
		bySnapshot[id.Snapshot] = append(bySnapshot[id.Snapshot], id)
	}
	for snapshot, group := range bySnapshot {
		manifest, err := store.Manifest(snapshot)
		if err != nil {
			return nil, err
		}
		for _, id := range group {
			packed, ok := packedObjectByID(manifest, id)
			if !ok {
				return nil, fmt.Errorf("shared snapshot object %s: %w", id, os.ErrNotExist)
			}
			result[id] = cloneObject(packed.Object)
		}
	}
	return result, nil
}

// ReadObjectRange reads only the extents intersecting [offset, offset+len).
// Guest reads remain arbitrary-sized even though shared packs use 1 MiB
// extents, and no preceding bytes in a large file are scanned.
func (store *SharedPackStore) ReadObjectRange(ctx context.Context, id ObjectID, destination []byte, offset int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, os.ErrInvalid
	}
	lease, err := store.acquireSnapshotReadLease(ctx, id.Snapshot)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		_ = lease.Close()
	}()
	manifest, err := store.Manifest(id.Snapshot)
	if err != nil {
		return 0, err
	}
	packed, ok := packedObjectByID(manifest, id)
	if !ok {
		return 0, os.ErrNotExist
	}
	if uint64(offset) >= packed.Object.BulkLength {
		if len(destination) == 0 && uint64(offset) == packed.Object.BulkLength {
			return 0, nil
		}
		return 0, io.EOF
	}
	want := len(destination)
	if remaining := packed.Object.BulkLength - uint64(offset); uint64(want) > remaining {
		want = int(remaining)
	}
	written := 0
	objectOffset := int64(0)
	for _, extent := range packed.Extents {
		if written == want {
			break
		}
		extentEnd := objectOffset + extent.Length
		if offset >= extentEnd {
			objectOffset = extentEnd
			continue
		}
		within := int64(0)
		if offset > objectOffset {
			within = offset - objectOffset
		}
		amount := min(int64(want-written), extent.Length-within)
		file, err := os.Open(filepath.Join(store.snapshotDirectory(id.Snapshot), packName(extent.Pack)))
		if err != nil {
			return written, err
		}
		n, readErr := file.ReadAt(destination[written:written+int(amount)], extent.Offset+within)
		closeErr := file.Close()
		written += n
		offset += int64(n)
		if readErr != nil && !(errors.Is(readErr, io.EOF) && n == int(amount)) {
			return written, errors.Join(readErr, closeErr)
		}
		if closeErr != nil {
			return written, closeErr
		}
		if n != int(amount) {
			return written, io.ErrUnexpectedEOF
		}
		objectOffset = extentEnd
	}
	if written != len(destination) {
		return written, io.EOF
	}
	return written, nil
}

// Manifests enumerates committed snapshot manifests without treating staging,
// retired, or operator-created entries as authority. Callers use this to
// rebuild disposable coordinator state; a directory without manifest.json is
// never returned merely because pack-shaped files exist there.
func (store *SharedPackStore) Manifests() ([]*SnapshotManifest, error) {
	if store == nil || store.root == "" {
		return nil, errors.New("shared pack store is unavailable")
	}
	entries, err := os.ReadDir(filepath.Join(store.root, "snapshots"))
	if err != nil {
		return nil, err
	}
	var snapshots []SnapshotID
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		value, err := strconv.ParseUint(entry.Name(), 16, 64)
		if err != nil || value == 0 || snapshotKey(SnapshotID(value)) != entry.Name() {
			continue
		}
		if _, err := os.Stat(filepath.Join(store.root, "snapshots", entry.Name(), sharedManifestName)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		snapshots = append(snapshots, SnapshotID(value))
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i] < snapshots[j] })
	manifests := make([]*SnapshotManifest, 0, len(snapshots))
	for _, snapshot := range snapshots {
		manifest, err := store.Manifest(snapshot)
		if errors.Is(err, fs.ErrNotExist) {
			// Enumeration is only a physical discovery pass. A preceding GC
			// coordinator may have permanently fenced this origin in Raft and be
			// finishing its directory rename/removal while a successor rebuilds
			// disposable reference state. The absent directory is therefore no
			// longer a candidate manifest, not evidence of a partial pack.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("opening shared snapshot %d during enumeration: %w", snapshot, err)
		}
		manifests = append(manifests, manifest)
	}
	return manifests, nil
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

// acquireSnapshotLease locks a writable file inside the snapshot directory.
// NFS rejects exclusive flock on read-only descriptors (including an opened
// directory) with EBADF. Retirement first renames the whole directory, then
// closes this descriptor before recursive removal, so the lease itself cannot
// become an NFS unlink tombstone.
func (store *SharedPackStore) acquireSnapshotLease(snapshot SnapshotID, operation int) (*os.File, error) {
	directory, err := os.OpenFile(filepath.Join(store.snapshotDirectory(snapshot), ".lease"), os.O_CREATE|os.O_RDWR, 0600)
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

func (store *SharedPackStore) acquireSnapshotWriteLease(ctx context.Context, snapshot SnapshotID) (*os.File, error) {
	for {
		lease, err := store.acquireSnapshotLease(snapshot, syscall.LOCK_EX|syscall.LOCK_NB)
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
	packed, ok := packedObjectByID(manifest, id)
	if !ok {
		syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		lease.Close()
		return nil, 0, os.ErrNotExist
	}
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

// DiscardHandoff removes a pack written by PublishHandoff before its
// provider-to-shared authority transition committed. The caller must first
// prove that the snapshot is still routed to its volatile provider. In that
// state no shared-pack reader can be authoritative; the lease nevertheless
// prevents racing a diagnostic or an interrupted publisher that still has an
// extent open.
func (store *SharedPackStore) DiscardHandoff(snapshot SnapshotID) (busy bool, err error) {
	if snapshot == 0 {
		return false, errors.New("cannot discard snapshot zero")
	}
	if _, err := os.Stat(store.snapshotDirectory(snapshot)); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	lease, err := store.acquireSnapshotLease(snapshot, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	directory := store.snapshotDirectory(snapshot)
	parent := filepath.Dir(directory)
	retired := filepath.Join(parent, fmt.Sprintf(".discarded-%s-%d-%d", snapshotKey(snapshot), os.Getpid(), time.Now().UnixNano()))
	renameErr := os.Rename(directory, retired)
	if errors.Is(renameErr, fs.ErrNotExist) {
		renameErr = nil
	}
	unlockErr := syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
	closeErr := lease.Close()
	if renameErr != nil {
		return false, errors.Join(renameErr, unlockErr, closeErr)
	}
	syncErr := syncDirectory(parent)
	removeErr := os.RemoveAll(retired)
	syncErr = errors.Join(syncErr, syncDirectory(parent))
	store.mu.Lock()
	delete(store.manifests, snapshot)
	store.mu.Unlock()
	return false, errors.Join(unlockErr, closeErr, removeErr, syncErr)
}

// RetirementReady reports whether retirement can acquire the snapshot's
// exclusive physical lease. It does not alter the snapshot. Distributed GC
// calls this immediately before permanently fencing the origin in Raft: a
// busy result proves that some data-plane reader still observes the origin,
// which is inconsistent with retiring it even if the reference ledger has
// already reached zero.
func (store *SharedPackStore) RetirementReady(snapshot SnapshotID) (bool, error) {
	if snapshot == 0 {
		return false, errors.New("cannot inspect retirement of snapshot zero")
	}
	lease, err := store.acquireSnapshotLease(snapshot, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, errors.Join(syscall.Flock(int(lease.Fd()), syscall.LOCK_UN), lease.Close())
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
	// Publication creates the snapshot directory before its temporary pack and
	// commits the manifest last. A canceled or crashed publisher may therefore
	// leave an empty (or temporary-only) namespace. Once Raft has permanently
	// retired this origin, that namespace is no more authoritative than an
	// interrupted handoff and must not survive as shared-root residue.
	if _, err := os.Stat(filepath.Join(store.snapshotDirectory(snapshot), sharedManifestName)); errors.Is(err, fs.ErrNotExist) {
		return store.DiscardHandoff(snapshot)
	} else if err != nil {
		return false, err
	}
	return store.Retire(snapshot)
}
