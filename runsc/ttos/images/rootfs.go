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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// RootFSOptions identifies an image and an empty destination directory.
type RootFSOptions struct {
	Address   string
	Namespace string
	Reference string
	Platform  string
	Root      string
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
// directory. It does not mount a containerd snapshot, so it can run without
// host mount privileges. The first rootless slice maps all file ownership to
// the invoking user; preserving distinct image UIDs and GIDs is tracked as a
// compatibility requirement for the image-root format.
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

	client, err := containerd.New(opts.Address, containerd.WithDefaultNamespace(opts.Namespace))
	if err != nil {
		return RootFSInfo{}, fmt.Errorf("connecting to containerd at %q: %w", opts.Address, err)
	}
	defer client.Close()

	image, err := client.GetImage(ctx, reference)
	if err != nil {
		return RootFSInfo{}, fmt.Errorf("finding image %q: %w", reference, err)
	}
	matcher := platforms.DefaultStrict()
	if opts.Platform != "" {
		platform, err := platforms.Parse(opts.Platform)
		if err != nil {
			return RootFSInfo{}, fmt.Errorf("invalid platform %q: %w", opts.Platform, err)
		}
		matcher = platforms.OnlyStrict(platform)
	}
	store := client.ContentStore()
	manifest, err := containerdimages.Manifest(ctx, store, image.Target(), matcher)
	if err != nil {
		return RootFSInfo{}, fmt.Errorf("resolving manifest for %q: %w", reference, err)
	}
	configData, err := content.ReadBlob(ctx, store, manifest.Config)
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

	for _, layer := range manifest.Layers {
		if err := applyContentLayer(ctx, store, opts.Root, layer); err != nil {
			return RootFSInfo{}, fmt.Errorf("applying layer %s: %w", layer.Digest, err)
		}
	}
	platform := strings.TrimSuffix(imageConfig.OS+"/"+imageConfig.Architecture+"/"+imageConfig.Variant, "/")
	return RootFSInfo{
		Reference:    image.Name(),
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
	return nil
}

func applyContentLayer(ctx context.Context, store content.Store, root string, layer ocispec.Descriptor) error {
	readerAt, err := store.ReaderAt(ctx, layer)
	if err != nil {
		return err
	}
	defer readerAt.Close()
	return applyLayer(ctx, root, content.NewReader(readerAt))
}

func applyLayer(ctx context.Context, root string, compressed io.Reader) error {
	stream, err := compression.DecompressStream(compressed)
	if err != nil {
		return fmt.Errorf("decompressing layer: %w", err)
	}
	defer stream.Close()
	if _, err := archive.Apply(ctx, root, stream, archive.WithNoSameOwner()); err != nil {
		return fmt.Errorf("extracting layer: %w", err)
	}
	return nil
}
