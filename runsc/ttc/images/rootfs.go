// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package images

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/content"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

// RootFSOptions identifies an image and an empty destination directory.
type RootFSOptions struct {
	StoreRoot string
	Reference string
	Platform  string
	Root      string
	Progress  ProgressFunc
}

// RootFSInfo describes a materialized immutable image root.
type RootFSInfo struct {
	Reference    string        `json:"reference"`
	ConfigDigest string        `json:"config_digest"`
	Platform     string        `json:"platform"`
	LayerCount   int           `json:"layer_count"`
	Image        ocispec.Image `json:"image"`
}

// MaterializeRootFS applies the selected image's layers directly to an empty
// directory without a host mount or snapshotter. Physical files remain owned
// by the invoking user;
// their image UIDs and GIDs are persisted in a sidecar manifest for the gofer
// to expose to the Sentry.
func MaterializeRootFS(ctx context.Context, opts RootFSOptions) (RootFSInfo, error) {
	reference, err := NormalizeReference(opts.Reference)
	if err != nil {
		return RootFSInfo{}, err
	}
	store, err := openNativeStore(opts.StoreRoot)
	if err != nil {
		return RootFSInfo{}, err
	}
	defer store.close()
	record, ok := store.catalog.Images[reference]
	if !ok {
		return RootFSInfo{}, fmt.Errorf("finding image %q: %w", reference, ErrNotFound)
	}
	return materializeRootFSRecord(ctx, opts, store, record)
}

// MaterializeImportedRootFS materializes an archive entry before its final tag
// is published by PublishLoaded.
func MaterializeImportedRootFS(ctx context.Context, opts RootFSOptions, imported ImportedImage) (RootFSInfo, error) {
	store, err := openNativeStore(opts.StoreRoot)
	if err != nil {
		return RootFSInfo{}, err
	}
	defer store.close()
	return materializeRootFSRecord(ctx, opts, store, imageRecord{Info: imported.Info, Target: imported.target})
}

func materializeRootFSRecord(ctx context.Context, opts RootFSOptions, store *nativeStore, record imageRecord) (RootFSInfo, error) {
	if opts.Root == "" {
		return RootFSInfo{}, fmt.Errorf("rootfs destination must not be empty")
	}
	if err := prepareEmptyRoot(opts.Root); err != nil {
		return RootFSInfo{}, err
	}
	reference := record.Info.Reference
	matcher, err := platformMatcher(opts.Platform)
	if err != nil {
		return RootFSInfo{}, err
	}
	manifest, err := containerdimages.Manifest(ctx, store.content, record.Target, matcher)
	if err != nil {
		return RootFSInfo{}, fmt.Errorf("resolving manifest for %q: %w", reference, err)
	}
	configData, err := content.ReadBlob(ctx, store.content, manifest.Config)
	if err != nil {
		return RootFSInfo{}, fmt.Errorf("reading image config for %q: %w", reference, err)
	}
	var imageConfig ocispec.Image
	if err := json.Unmarshal(configData, &imageConfig); err != nil {
		return RootFSInfo{}, fmt.Errorf("decoding image config for %q: %w", reference, err)
	}
	if imageConfig.OS != "linux" {
		return RootFSInfo{}, fmt.Errorf("image %q has unsupported OS %q", reference, imageConfig.OS)
	}

	owners := newOwnershipState()
	for index, layer := range manifest.Layers {
		if err := applyContentLayer(ctx, store.content, opts.Root, layer, owners, opts.Progress, index+1, len(manifest.Layers)); err != nil {
			return RootFSInfo{}, fmt.Errorf("applying layer %s: %w", layer.Digest, err)
		}
	}
	ownerManifest, err := owners.finalize(opts.Root)
	if err != nil {
		return RootFSInfo{}, fmt.Errorf("finalizing image ownership: %w", err)
	}
	if err := ownership.Write(ownership.ManifestPath(opts.Root), ownerManifest); err != nil {
		return RootFSInfo{}, fmt.Errorf("writing image ownership: %w", err)
	}
	platform := strings.TrimSuffix(imageConfig.OS+"/"+imageConfig.Architecture+"/"+imageConfig.Variant, "/")
	return RootFSInfo{
		Reference:    record.Info.Reference,
		ConfigDigest: manifest.Config.Digest.String(),
		Platform:     platform,
		LayerCount:   len(manifest.Layers),
		Image:        imageConfig,
	}, nil
}

func prepareEmptyRoot(root string) error {
	if err := os.MkdirAll(root, 0755); err != nil {
		return fmt.Errorf("creating rootfs destination %q: %w", root, err)
	}
	dir, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("opening rootfs destination %q: %w", root, err)
	}
	defer dir.Close()
	if _, err := dir.Readdirnames(1); err == nil {
		return fmt.Errorf("rootfs destination %q is not empty", root)
	} else if err != io.EOF {
		return fmt.Errorf("checking rootfs destination %q: %w", root, err)
	}
	// Storage parents are private, but the directory itself becomes guest /.
	// Normalize an existing cache directory so host metadata does not make the
	// guest root unsearchable by non-root users. A layer entry for . may still
	// override this default during extraction.
	if err := os.Chmod(root, 0755); err != nil {
		return fmt.Errorf("setting rootfs destination mode: %w", err)
	}
	return nil
}

func applyContentLayer(ctx context.Context, store content.Store, root string, layer ocispec.Descriptor, owners *ownershipState, progress ProgressFunc, index, count int) error {
	readerAt, err := store.ReaderAt(ctx, layer)
	if err != nil {
		return err
	}
	defer readerAt.Close()
	reader := io.Reader(content.NewReader(readerAt))
	if progress != nil {
		progress(ProgressEvent{Phase: ProgressExtracting, Descriptor: layer, Total: layer.Size, Index: index, Count: count})
		reader = &progressReader{reader: reader, desc: layer, progress: progress, index: index, count: count}
	}
	if err := applyLayerWithOwnership(ctx, root, reader, owners); err != nil {
		return err
	}
	if progress != nil {
		progress(ProgressEvent{Phase: ProgressExtracted, Descriptor: layer, Current: layer.Size, Total: layer.Size, Index: index, Count: count})
	}
	return nil
}

type progressReader struct {
	reader   io.Reader
	desc     ocispec.Descriptor
	progress ProgressFunc
	current  int64
	index    int
	count    int
}

func (r *progressReader) Read(dst []byte) (int, error) {
	n, err := r.reader.Read(dst)
	r.current += int64(n)
	if n != 0 {
		r.progress(ProgressEvent{Phase: ProgressExtracting, Descriptor: r.desc, Current: r.current, Total: r.desc.Size, Index: r.index, Count: r.count})
	}
	return n, err
}

func applyLayer(ctx context.Context, root string, compressed io.Reader) error {
	return applyLayerWithOwnership(ctx, root, compressed, nil)
}

func applyLayerWithOwnership(ctx context.Context, root string, compressed io.Reader, owners *ownershipState) error {
	stream, err := compression.DecompressStream(compressed)
	if err != nil {
		return fmt.Errorf("decompressing layer: %w", err)
	}
	defer stream.Close()

	// OCI directory modes describe the completed layer, not permissions that
	// should prevent extraction of later entries. A privileged extractor can
	// traverse and write through modes such as 0550 using CAP_DAC_OVERRIDE,
	// but TTC's rootless extractor cannot. Temporarily grant the invoking user
	// access to existing and newly declared directories, then restore the
	// image's exact modes after the complete layer has been applied.
	directoryModes := make(layerDirectoryModes)
	if err := directoryModes.relaxExisting(root); err != nil {
		return fmt.Errorf("preparing layer directories: %w", err)
	}
	filter := func(header *tar.Header) (bool, error) {
		// containerd skips device nodes only when it detects that extraction is
		// running inside a host user namespace. TTC is rootless in both daemon
		// isolation modes, so unsafe-no-host-isolation would otherwise attempt a
		// host mknod(2) and fail with EPERM. Container /dev is supplied by the
		// Sentry, not by immutable image contents.
		if header.Typeflag == tar.TypeBlock || header.Typeflag == tar.TypeChar {
			return false, nil
		}
		if owners != nil {
			accepted, err := owners.record(header)
			if err != nil || !accepted {
				return accepted, err
			}
		}
		return directoryModes.record(header)
	}
	_, applyErr := archive.Apply(ctx, root, stream, archive.WithNoSameOwner(), archive.WithFilter(filter))
	// containerd defers directory timestamps until the complete tar stream has
	// been applied. A valid diff may declare a directory and then remove it with
	// a later whiteout (dpkg-generated TTC layers do this for transient
	// *.dpkg-new trees). In that case extraction is complete and the only
	// failure is the deferred timestamp syscall observing the intended final
	// absence. Do not reject the otherwise fully applied layer. Keep the string
	// qualification narrow: an unrelated ENOENT during entry extraction still
	// indicates a malformed layer.
	if applyErr != nil && errors.Is(applyErr, os.ErrNotExist) &&
		strings.Contains(applyErr.Error(), "failed call to UtimesNanoAt for ") {
		applyErr = nil
	}
	restoreErr := directoryModes.restore(root)
	if applyErr != nil {
		if restoreErr != nil {
			return fmt.Errorf("extracting layer: %w (also restoring directory modes: %v)", applyErr, restoreErr)
		}
		return fmt.Errorf("extracting layer: %w", applyErr)
	}
	if restoreErr != nil {
		return fmt.Errorf("restoring layer directory modes: %w", restoreErr)
	}
	return nil
}

type layerDirectoryModes map[string]os.FileMode

func (m layerDirectoryModes) relaxExisting(root string) (retErr error) {
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		m[relative] = info.Mode()
		if info.Mode().Perm()&0700 == 0700 {
			return nil
		}
		return os.Chmod(current, info.Mode()|0700)
	})
	if err == nil {
		return nil
	}
	if restoreErr := m.restore(root); restoreErr != nil {
		return fmt.Errorf("%w (also rolling back directory modes: %v)", err, restoreErr)
	}
	return err
}

func (m layerDirectoryModes) record(header *tar.Header) (bool, error) {
	if header.Typeflag != tar.TypeDir {
		return true, nil
	}
	relative, err := ownership.CleanPath(header.Name)
	if err != nil {
		return false, err
	}
	m[relative] = header.FileInfo().Mode()
	header.Mode |= 0700
	return true, nil
}

func (m layerDirectoryModes) restore(root string) error {
	paths := make([]string, 0, len(m))
	for relative := range m {
		paths = append(paths, relative)
	}
	// Restore children before parents so a final non-searchable parent cannot
	// prevent restoration of entries beneath it.
	depth := func(relative string) int {
		if relative == "." {
			return 0
		}
		return strings.Count(relative, "/") + 1
	}
	sort.Slice(paths, func(i, j int) bool {
		iDepth := depth(paths[i])
		jDepth := depth(paths[j])
		if iDepth != jDepth {
			return iDepth > jDepth
		}
		return paths[i] < paths[j]
	})
	for _, relative := range paths {
		current := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		}
		if err := os.Chmod(current, m[relative]); err != nil {
			return err
		}
	}
	return nil
}

type ownershipRecord struct {
	owner         ownership.Owner
	virtualXattrs map[string][]byte
	sequence      uint64
}

type ownershipState struct {
	entries map[string]ownershipRecord
	groups  map[string]uint64
	// members holds the paths of each hardlink group, and children the
	// recorded paths directly beneath each path, recorded or implied. With
	// them, relinking a group or deleting a subtree costs what the group or
	// subtree holds, not a scan of every path recorded so far: each file of
	// a layer deletes the subtree it may replace, and a layer of 200,000
	// files took seven minutes.
	members   map[uint64]map[string]struct{}
	children  map[string]map[string]struct{}
	sequence  uint64
	nextGroup uint64
}

func newOwnershipState() *ownershipState {
	return &ownershipState{
		entries: map[string]ownershipRecord{
			".": {owner: ownership.Owner{}},
		},
		groups:   make(map[string]uint64),
		members:  make(map[uint64]map[string]struct{}),
		children: make(map[string]map[string]struct{}),
	}
}

// index records rel beneath each of its ancestors. Every path beneath an
// indexed path is indexed up to the root, so indexing stops at the first
// ancestor that already is.
func (s *ownershipState) index(rel string) {
	for rel != "." {
		parent := path.Dir(rel)
		siblings := s.children[parent]
		if siblings == nil {
			siblings = make(map[string]struct{})
			s.children[parent] = siblings
		}
		if _, ok := siblings[rel]; ok {
			return
		}
		siblings[rel] = struct{}{}
		rel = parent
	}
}

// join puts rel in hardlink group, and out of any other.
func (s *ownershipState) join(rel string, group uint64) {
	s.leave(rel)
	s.groups[rel] = group
	members := s.members[group]
	if members == nil {
		members = make(map[string]struct{})
		s.members[group] = members
	}
	members[rel] = struct{}{}
	s.index(rel)
}

// leave takes rel out of its hardlink group, if it is in one.
func (s *ownershipState) leave(rel string) {
	group, ok := s.groups[rel]
	if !ok {
		return
	}
	delete(s.groups, rel)
	delete(s.members[group], rel)
	if len(s.members[group]) == 0 {
		delete(s.members, group)
	}
}

// forget deletes what is recorded for rel itself.
func (s *ownershipState) forget(rel string) {
	delete(s.entries, rel)
	s.leave(rel)
}

func (s *ownershipState) record(header *tar.Header) (bool, error) {
	rel, err := ownership.CleanPath(header.Name)
	if err != nil {
		return false, err
	}
	base := path.Base(rel)
	dir := path.Dir(rel)
	if base == ".wh..wh..opq" {
		s.deleteChildren(dir)
		return true, nil
	}
	if strings.HasPrefix(base, ".wh.") {
		target, err := ownership.CleanPath(path.Join(dir, strings.TrimPrefix(base, ".wh.")))
		if err != nil {
			return false, err
		}
		s.deleteSubtree(target)
		return true, nil
	}
	if header.Typeflag == tar.TypeXGlobalHeader {
		return true, nil
	}
	if header.Uid < 0 || uint64(header.Uid) > math.MaxUint32 || header.Gid < 0 || uint64(header.Gid) > math.MaxUint32 {
		return false, fmt.Errorf("image path %q has invalid owner %d:%d", header.Name, header.Uid, header.Gid)
	}
	virtualXattrs := takeVirtualXattrs(header)
	s.sequence++
	record := ownershipRecord{
		owner:         ownership.Owner{UID: uint32(header.Uid), GID: uint32(header.Gid)},
		virtualXattrs: virtualXattrs,
		sequence:      s.sequence,
	}
	switch header.Typeflag {
	case tar.TypeDir:
		// A directory header merges with an existing directory, but replaces a
		// non-directory at the same path. Either way it is not a hardlink.
		_, replacesNonDirectory := s.groups[rel]
		if previous, ok := s.entries[rel]; ok && !replacesNonDirectory {
			record.virtualXattrs = mergeVirtualXattrs(previous.virtualXattrs, virtualXattrs)
		}
		s.leave(rel)
		s.entries[rel] = record
		s.index(rel)
	case tar.TypeLink:
		s.deleteSubtree(rel)
		target, err := ownership.CleanPath(header.Linkname)
		if err != nil {
			return false, fmt.Errorf("invalid hardlink target for %q: %w", header.Name, err)
		}
		if targetRecord, ok := s.entries[target]; ok {
			record.virtualXattrs = mergeVirtualXattrs(targetRecord.virtualXattrs, virtualXattrs)
		}
		group, ok := s.groups[target]
		if !ok {
			s.nextGroup++
			group = s.nextGroup
			s.join(target, group)
		}
		// The archive extractor applies the link header owner with lchown(2), which changes
		// the shared inode and therefore every existing alias.
		for name := range s.members[group] {
			s.entries[name] = record
		}
		s.join(rel, group)
		s.entries[rel] = record
	default:
		s.deleteSubtree(rel)
		s.nextGroup++
		s.join(rel, s.nextGroup)
		s.entries[rel] = record
	}
	return true, nil
}

func takeVirtualXattrs(header *tar.Header) map[string][]byte {
	var virtual map[string][]byte
	for key, value := range header.PAXRecords {
		if !strings.HasPrefix(key, "SCHILY.xattr.") {
			continue
		}
		attribute := strings.TrimPrefix(key, "SCHILY.xattr.")
		if virtual == nil {
			virtual = make(map[string][]byte)
		}
		virtual[attribute] = []byte(value)
		// Immutable image xattrs are guest metadata. Keeping every namespace in
		// TTC's manifest avoids depending on host xattr support (notably NFS) and
		// avoids privileged security.*, trusted.*, and ACL operations. The image
		// gofer merges these values with any host-backed attributes when serving
		// the guest.
		delete(header.PAXRecords, key)
	}
	return virtual
}

func mergeVirtualXattrs(base, updates map[string][]byte) map[string][]byte {
	if len(base) == 0 && len(updates) == 0 {
		return nil
	}
	merged := make(map[string][]byte, len(base)+len(updates))
	for name, value := range base {
		merged[name] = append([]byte(nil), value...)
	}
	for name, value := range updates {
		merged[name] = append([]byte(nil), value...)
	}
	return merged
}

// deleteSubtree deletes what is recorded for rel and every path beneath it.
func (s *ownershipState) deleteSubtree(rel string) {
	if rel != "." {
		s.deleteChildren(rel)
	}
	s.forget(rel)
}

// deleteChildren deletes what is recorded for every path beneath rel.
func (s *ownershipState) deleteChildren(rel string) {
	for child := range s.children[rel] {
		s.deleteChildren(child)
		s.forget(child)
	}
	delete(s.children, rel)
}

type inodeKey struct {
	dev uint64
	ino uint64
}

type inodeOwners struct {
	paths    []string
	winner   ownershipRecord
	hasOwner bool
}

func (s *ownershipState) finalize(root string) (*ownership.Manifest, error) {
	manifest := ownership.NewManifest()
	inodes := make(map[inodeKey]*inodeOwners)
	// chmod(2) on one hard link changes every alias. Retain the first observed
	// image mode so all aliases receive the same guest-visible mode even after
	// the first path is made host-readable.
	inodeModes := make(map[inodeKey]uint32)
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel, err = ownership.CleanPath(rel)
		if err != nil {
			return err
		}
		record, ok := s.entries[rel]
		if !ok {
			record = ownershipRecord{owner: ownership.Owner{}}
		}
		manifest.Entries[rel] = record.owner
		if len(record.virtualXattrs) != 0 {
			manifest.Xattrs[rel] = mergeVirtualXattrs(nil, record.virtualXattrs)
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("image path %q has unsupported stat metadata", name)
		}
		key := inodeKey{dev: uint64(stat.Dev), ino: stat.Ino}
		guestMode, seenMode := inodeModes[key]
		if !seenMode {
			guestMode = uint32(stat.Mode) & 07777
			inodeModes[key] = guestMode
		}
		relaxedMode := info.Mode()
		switch {
		case entry.IsDir() && guestMode&0500 != 0500:
			relaxedMode |= 0500
		case info.Mode().IsRegular() && guestMode&0400 == 0:
			relaxedMode |= 0400
		}
		if relaxedMode != info.Mode() {
			// The immutable image gofer is read-only, so these extra host bits
			// provide backing access without granting guest writes. fsgofer
			// reports guestMode from the manifest instead.
			manifest.Modes[rel] = guestMode
			if err := os.Chmod(name, relaxedMode); err != nil {
				return fmt.Errorf("making immutable image path %q host-readable: %w", name, err)
			}
		} else if seenMode {
			// A prior hard-link alias may already have relaxed the shared inode.
			// Preserve the common pre-relaxation mode for this alias too.
			actualMode := uint32(stat.Mode) & 07777
			if actualMode != guestMode {
				manifest.Modes[rel] = guestMode
			}
		}
		if stat.Nlink < 2 || entry.IsDir() {
			return nil
		}
		group := inodes[key]
		if group == nil {
			group = &inodeOwners{}
			inodes[key] = group
		}
		group.paths = append(group.paths, rel)
		if !group.hasOwner || record.sequence > group.winner.sequence {
			group.winner = record
			group.hasOwner = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, group := range inodes {
		for _, rel := range group.paths {
			manifest.Entries[rel] = group.winner.owner
			if len(group.winner.virtualXattrs) != 0 {
				manifest.Xattrs[rel] = mergeVirtualXattrs(nil, group.winner.virtualXattrs)
			} else {
				delete(manifest.Xattrs, rel)
			}
		}
	}
	return manifest, nil
}
