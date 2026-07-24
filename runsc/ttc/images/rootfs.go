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

package images

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/content"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/platforms"
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
	if opts.Root == "" {
		return RootFSInfo{}, fmt.Errorf("rootfs destination must not be empty")
	}
	reference, err := NormalizeReference(opts.Reference)
	if err != nil {
		return RootFSInfo{}, err
	}
	if err := prepareEmptyRoot(opts.Root); err != nil {
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
	matcher := platforms.DefaultStrict()
	if opts.Platform != "" {
		platform, err := platforms.Parse(opts.Platform)
		if err != nil {
			return RootFSInfo{}, fmt.Errorf("invalid platform %q: %w", opts.Platform, err)
		}
		matcher = platforms.OnlyStrict(platform)
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
	options := []archive.ApplyOpt{archive.WithNoSameOwner()}
	if owners != nil {
		options = append(options, archive.WithFilter(owners.record))
	}
	if _, err := archive.Apply(ctx, root, stream, options...); err != nil {
		return fmt.Errorf("extracting layer: %w", err)
	}
	return nil
}

type ownershipRecord struct {
	owner    ownership.Owner
	sequence uint64
}

type ownershipState struct {
	entries   map[string]ownershipRecord
	groups    map[string]uint64
	sequence  uint64
	nextGroup uint64
}

func newOwnershipState() *ownershipState {
	return &ownershipState{
		entries: map[string]ownershipRecord{
			".": {owner: ownership.Owner{}},
		},
		groups: make(map[string]uint64),
	}
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
	s.sequence++
	record := ownershipRecord{
		owner:    ownership.Owner{UID: uint32(header.Uid), GID: uint32(header.Gid)},
		sequence: s.sequence,
	}
	switch header.Typeflag {
	case tar.TypeDir:
		// A directory header merges with an existing directory, but replaces a
		// non-directory at the same path. Either way it is not a hardlink.
		delete(s.groups, rel)
		s.entries[rel] = record
	case tar.TypeLink:
		s.deleteSubtree(rel)
		target, err := ownership.CleanPath(header.Linkname)
		if err != nil {
			return false, fmt.Errorf("invalid hardlink target for %q: %w", header.Name, err)
		}
		group, ok := s.groups[target]
		if !ok {
			s.nextGroup++
			group = s.nextGroup
			s.groups[target] = group
		}
		// The archive extractor applies the link header owner with lchown(2), which changes
		// the shared inode and therefore every existing alias.
		for name, candidate := range s.groups {
			if candidate == group {
				s.entries[name] = record
			}
		}
		s.groups[rel] = group
		s.entries[rel] = record
	default:
		s.deleteSubtree(rel)
		s.nextGroup++
		s.groups[rel] = s.nextGroup
		s.entries[rel] = record
	}
	return true, nil
}

func (s *ownershipState) deleteSubtree(rel string) {
	prefix := rel + "/"
	for name := range s.entries {
		if name == rel || strings.HasPrefix(name, prefix) {
			delete(s.entries, name)
			delete(s.groups, name)
		}
	}
}

func (s *ownershipState) deleteChildren(rel string) {
	prefix := ""
	if rel != "." {
		prefix = rel + "/"
	}
	for name := range s.entries {
		if name != rel && strings.HasPrefix(name, prefix) {
			delete(s.entries, name)
			delete(s.groups, name)
		}
	}
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

		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink < 2 || entry.IsDir() {
			return nil
		}
		key := inodeKey{dev: uint64(stat.Dev), ino: stat.Ino}
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
		}
	}
	return manifest, nil
}
