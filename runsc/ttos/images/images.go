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

// Package images implements TTOS image operations backed by containerd.
package images

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	distref "github.com/distribution/reference"
)

var (
	// ErrNotFound indicates that an image selector matched no stored record.
	ErrNotFound = errors.New("image not found")
	// ErrAmbiguous indicates that an image ID prefix matched multiple digests.
	ErrAmbiguous = errors.New("ambiguous image identifier")
)

// PullOptions controls image resolution, download, and unpacking.
type PullOptions struct {
	Address     string
	Namespace   string
	Reference   string
	Platform    string
	Snapshotter string
	Unpack      bool
}

// ListOptions identifies the containerd image store to query.
type ListOptions struct {
	Address     string
	Namespace   string
	Snapshotter string
}

// DeleteOptions identifies one exact containerd image record to remove. Digest
// protects against deleting a tag that was concurrently retargeted.
type DeleteOptions struct {
	Address   string
	Namespace string
	Reference string
	Digest    string
}

// Info describes a pulled platform-specific image.
type Info struct {
	Reference   string     `json:"reference"`
	Digest      string     `json:"digest"`
	MediaType   string     `json:"media_type"`
	Size        int64      `json:"size"`
	Platform    string     `json:"platform"`
	Snapshotter string     `json:"snapshotter,omitempty"`
	Unpacked    bool       `json:"unpacked"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PullStats   *PullStats `json:"pull_stats,omitempty"`
}

// PullStats summarizes committed content reuse observed during one pull.
type PullStats struct {
	DownloadedBytes int64 `json:"downloaded_bytes"`
	ReusedBytes     int64 `json:"reused_bytes"`
}

func snapshotContent(ctx context.Context, store content.Store) (map[string]int64, error) {
	snapshot := make(map[string]int64)
	if err := store.Walk(ctx, func(info content.Info) error {
		snapshot[info.Digest.String()] = info.Size
		return nil
	}); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func contentReuse(before, after map[string]int64, total int64) PullStats {
	var downloaded int64
	for digest, size := range after {
		if _, existed := before[digest]; !existed && size > 0 {
			downloaded += size
		}
	}
	if downloaded > total {
		downloaded = total
	}
	if downloaded < 0 {
		downloaded = 0
	}
	reused := total - downloaded
	if reused < 0 {
		reused = 0
	}
	return PullStats{DownloadedBytes: downloaded, ReusedBytes: reused}
}

// NormalizeReference applies Docker's familiar-name and default-tag rules.
func NormalizeReference(input string) (string, error) {
	named, err := distref.ParseNormalizedNamed(input)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", input, err)
	}
	return distref.TagNameOnly(named).String(), nil
}

// Pull resolves and stores one image in containerd.
func Pull(ctx context.Context, opts PullOptions) (Info, error) {
	reference, err := NormalizeReference(opts.Reference)
	if err != nil {
		return Info{}, err
	}
	client, err := containerd.New(opts.Address, containerd.WithDefaultNamespace(opts.Namespace))
	if err != nil {
		return Info{}, fmt.Errorf("connecting to containerd at %q: %w", opts.Address, err)
	}
	defer client.Close()

	contentBefore, snapshotErr := snapshotContent(ctx, client.ContentStore())

	var remoteOpts []containerd.RemoteOpt
	credentials, configured, err := dockerCredentialCallback()
	if err != nil {
		return Info{}, err
	}
	if configured {
		resolver := docker.NewResolver(docker.ResolverOptions{Credentials: credentials})
		remoteOpts = append(remoteOpts, containerd.WithResolver(resolver))
	}
	if opts.Platform != "" {
		remoteOpts = append(remoteOpts, containerd.WithPlatform(opts.Platform))
	}
	if opts.Unpack {
		remoteOpts = append(remoteOpts, containerd.WithPullUnpack)
		if opts.Snapshotter != "" {
			remoteOpts = append(remoteOpts, containerd.WithPullSnapshotter(opts.Snapshotter))
		}
	}
	image, err := client.Pull(ctx, reference, remoteOpts...)
	if err != nil {
		return Info{}, fmt.Errorf("pulling %q: %w", reference, err)
	}
	size, err := image.Size(ctx)
	if err != nil {
		return Info{}, fmt.Errorf("measuring %q: %w", reference, err)
	}
	spec, err := image.Spec(ctx)
	if err != nil {
		return Info{}, fmt.Errorf("reading config for %q: %w", reference, err)
	}
	platform := strings.TrimSuffix(spec.OS+"/"+spec.Architecture+"/"+spec.Variant, "/")
	var pullStats *PullStats
	if snapshotErr == nil {
		if contentAfter, err := snapshotContent(ctx, client.ContentStore()); err == nil {
			stats := contentReuse(contentBefore, contentAfter, size)
			pullStats = &stats
		}
	}
	target := image.Target()
	metadata := image.Metadata()
	return Info{
		Reference:   image.Name(),
		Digest:      target.Digest.String(),
		MediaType:   target.MediaType,
		Size:        size,
		Platform:    platform,
		Snapshotter: opts.Snapshotter,
		Unpacked:    opts.Unpack,
		CreatedAt:   imageCreatedAt(metadata.CreatedAt, spec.Created),
		UpdatedAt:   metadata.UpdatedAt,
		PullStats:   pullStats,
	}, nil
}

// List returns all named image records ordered by reference. Multiple
// references may intentionally point at the same digest.
func List(ctx context.Context, opts ListOptions) ([]Info, error) {
	client, err := containerd.New(opts.Address, containerd.WithDefaultNamespace(opts.Namespace))
	if err != nil {
		return nil, fmt.Errorf("connecting to containerd at %q: %w", opts.Address, err)
	}
	defer client.Close()

	stored, err := client.ListImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containerd images: %w", err)
	}
	infos := make([]Info, 0, len(stored))
	for _, image := range stored {
		size, err := image.Size(ctx)
		if err != nil {
			return nil, fmt.Errorf("measuring %q: %w", image.Name(), err)
		}
		spec, err := image.Spec(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading config for %q: %w", image.Name(), err)
		}
		unpacked := false
		if opts.Snapshotter != "" {
			// TTOS materializes roots itself and does not require a containerd
			// snapshot. Failure to query an optional snapshotter must not hide
			// otherwise usable image records.
			unpacked, _ = image.IsUnpacked(ctx, opts.Snapshotter)
		}
		target := image.Target()
		metadata := image.Metadata()
		infos = append(infos, Info{
			Reference:   image.Name(),
			Digest:      target.Digest.String(),
			MediaType:   target.MediaType,
			Size:        size,
			Platform:    imagePlatform(spec.OS, spec.Architecture, spec.Variant),
			Snapshotter: opts.Snapshotter,
			Unpacked:    unpacked,
			CreatedAt:   imageCreatedAt(metadata.CreatedAt, spec.Created),
			UpdatedAt:   metadata.UpdatedAt,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Reference < infos[j].Reference })
	return infos, nil
}

// Resolve selects image records using a Docker-style name, a full digest, or
// a unique hexadecimal digest prefix. A digest selector returns every alias
// for that target; a name selector returns only that named record.
func Resolve(infos []Info, identifier string) ([]Info, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, fmt.Errorf("image identifier must not be empty")
	}
	for _, info := range infos {
		if info.Reference == identifier {
			return []Info{info}, nil
		}
	}
	if normalized, err := NormalizeReference(identifier); err == nil {
		for _, info := range infos {
			if info.Reference == normalized {
				return []Info{info}, nil
			}
		}
	}
	prefix, digestSelector := digestPrefix(identifier)
	if !digestSelector {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, identifier)
	}
	byDigest := make(map[string][]Info)
	for _, info := range infos {
		if strings.HasPrefix(strings.TrimPrefix(info.Digest, "sha256:"), prefix) {
			byDigest[info.Digest] = append(byDigest[info.Digest], info)
		}
	}
	if len(byDigest) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, identifier)
	}
	if len(byDigest) > 1 {
		return nil, fmt.Errorf("%w: %q", ErrAmbiguous, identifier)
	}
	for _, matches := range byDigest {
		sort.Slice(matches, func(i, j int) bool { return matches[i].Reference < matches[j].Reference })
		return matches, nil
	}
	panic("unreachable")
}

// Delete removes one exact named image record and synchronously asks
// containerd to garbage-collect newly unreachable content.
func Delete(ctx context.Context, opts DeleteOptions) error {
	client, err := containerd.New(opts.Address, containerd.WithDefaultNamespace(opts.Namespace))
	if err != nil {
		return fmt.Errorf("connecting to containerd at %q: %w", opts.Address, err)
	}
	defer client.Close()
	image, err := client.GetImage(ctx, opts.Reference)
	if err != nil {
		return fmt.Errorf("finding image %q: %w", opts.Reference, err)
	}
	target := image.Target()
	if opts.Digest != "" && target.Digest.String() != opts.Digest {
		return fmt.Errorf("image %q changed from %s to %s", opts.Reference, opts.Digest, target.Digest)
	}
	if err := client.ImageService().Delete(ctx, opts.Reference, containerdimages.SynchronousDelete(), containerdimages.DeleteTarget(&target)); err != nil {
		return fmt.Errorf("deleting image %q: %w", opts.Reference, err)
	}
	return nil
}

func imagePlatform(os, architecture, variant string) string {
	return strings.TrimSuffix(os+"/"+architecture+"/"+variant, "/")
}

func imageCreatedAt(stored time.Time, configured *time.Time) time.Time {
	if configured != nil {
		return *configured
	}
	return stored
}

func digestPrefix(identifier string) (string, bool) {
	value := strings.ToLower(identifier)
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) < 4 || len(value) > 64 {
		return "", false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return "", false
		}
	}
	return value, true
}
