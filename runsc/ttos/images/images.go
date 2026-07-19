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

// Package images implements TTOS's native OCI image operations.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/platforms"
	distref "github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var (
	// ErrNotFound indicates that an image selector matched no stored record.
	ErrNotFound = errors.New("image not found")
	// ErrAmbiguous indicates that an image ID prefix matched multiple digests.
	ErrAmbiguous = errors.New("ambiguous image identifier")
)

// PullOptions controls image resolution and download into the native store.
type PullOptions struct {
	Root      string
	Reference string
	Platform  string
	Progress  ProgressFunc
}

// ProgressEvent reports human-facing image transfer and extraction progress.
type ProgressEvent struct {
	Phase      string
	Descriptor ocispec.Descriptor
	Current    int64
	Total      int64
	Index      int
	Count      int
}

// ProgressFunc consumes image progress events. Calls may be concurrent.
type ProgressFunc func(ProgressEvent)

const (
	ProgressResolving     = "resolving"
	ProgressDownloading   = "downloading"
	ProgressDownloadDone  = "download-complete"
	ProgressAlreadyExists = "already-exists"
	ProgressExtracting    = "extracting"
	ProgressExtracted     = "extract-complete"
)

func isLayerDescriptor(desc ocispec.Descriptor) bool {
	return strings.Contains(strings.ToLower(desc.MediaType), "layer")
}

type progressFetcher struct {
	remotes.Fetcher
	progress ProgressFunc
}

func (f progressFetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	reader, err := f.Fetcher.Fetch(ctx, desc)
	if err != nil || f.progress == nil || !isLayerDescriptor(desc) {
		return reader, err
	}
	f.progress(ProgressEvent{Phase: ProgressDownloading, Descriptor: desc, Total: desc.Size})
	return &progressReadCloser{ReadCloser: reader, desc: desc, progress: f.progress}, nil
}

type progressReadCloser struct {
	io.ReadCloser
	desc     ocispec.Descriptor
	progress ProgressFunc
	current  int64
	done     sync.Once
}

func (r *progressReadCloser) Read(dst []byte) (int, error) {
	n, err := r.ReadCloser.Read(dst)
	r.current += int64(n)
	if n != 0 {
		r.progress(ProgressEvent{Phase: ProgressDownloading, Descriptor: r.desc, Current: r.current, Total: r.desc.Size})
	}
	if err == io.EOF || (r.desc.Size > 0 && r.current >= r.desc.Size) {
		r.complete()
	}
	return n, err
}

func (r *progressReadCloser) Close() error {
	if r.desc.Size > 0 && r.current >= r.desc.Size {
		r.complete()
	}
	return r.ReadCloser.Close()
}

func (r *progressReadCloser) complete() {
	r.done.Do(func() {
		r.progress(ProgressEvent{Phase: ProgressDownloadDone, Descriptor: r.desc, Current: r.current, Total: r.desc.Size})
	})
}

// ContentLeaseOptions identifies a durable TTOS family lease over one image target.
type ContentLeaseOptions struct {
	Root    string
	LeaseID string
	Digest  string
}

// ListOptions identifies the native image store to query.
type ListOptions struct {
	Root string
}

// DeleteOptions identifies one exact native image record to remove. Digest
// protects against deleting a tag that was concurrently retargeted.
type DeleteOptions struct {
	Root      string
	Reference string
	Digest    string
}

// Info describes a pulled platform-specific image.
type Info struct {
	Reference string     `json:"reference"`
	Digest    string     `json:"digest"`
	MediaType string     `json:"media_type"`
	Size      int64      `json:"size"`
	Platform  string     `json:"platform"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	PullStats *PullStats `json:"pull_stats,omitempty"`
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

func platformMatcher(value string) (platforms.MatchComparer, error) {
	if value == "" {
		return platforms.DefaultStrict(), nil
	}
	parsed, err := platforms.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid platform %q: %w", value, err)
	}
	return platforms.OnlyStrict(parsed), nil
}

// Pull resolves and stores one image in TTOS's native content store.
func Pull(ctx context.Context, opts PullOptions) (Info, error) {
	if opts.Progress != nil {
		opts.Progress(ProgressEvent{Phase: ProgressResolving})
	}
	reference, err := NormalizeReference(opts.Reference)
	if err != nil {
		return Info{}, err
	}
	matcher, err := platformMatcher(opts.Platform)
	if err != nil {
		return Info{}, err
	}
	store, err := openNativeStore(opts.Root)
	if err != nil {
		return Info{}, err
	}
	defer store.close()
	complete := false
	defer func() {
		if !complete {
			_ = store.garbageCollect(context.Background())
		}
	}()
	contentBefore, snapshotErr := snapshotContent(ctx, store.content)
	credentials, configured, err := dockerCredentialCallback()
	if err != nil {
		return Info{}, err
	}
	resolverOptions := docker.ResolverOptions{}
	if configured {
		resolverOptions.Credentials = credentials
	}
	resolver := docker.NewResolver(resolverOptions)
	name, target, err := resolver.Resolve(ctx, reference)
	if err != nil {
		return Info{}, fmt.Errorf("resolving %q: %w", reference, err)
	}
	if target.MediaType == containerdimages.MediaTypeDockerSchema1Manifest {
		return Info{}, fmt.Errorf("pulling %q: legacy Docker schema 1 images are unsupported", reference)
	}
	fetcher, err := resolver.Fetcher(ctx, name)
	if err != nil {
		return Info{}, fmt.Errorf("creating fetcher for %q: %w", reference, err)
	}
	children := containerdimages.FilterPlatforms(containerdimages.ChildrenHandler(store.content), matcher)
	children = containerdimages.LimitManifests(children, matcher, 1)
	if opts.Progress != nil {
		fetcher = progressFetcher{Fetcher: fetcher, progress: opts.Progress}
	}
	handler := containerdimages.Handlers(remotes.FetchHandler(store.content, fetcher), children)
	if err := containerdimages.Dispatch(ctx, handler, nil, target); err != nil {
		return Info{}, fmt.Errorf("pulling %q: %w", reference, err)
	}
	manifest, err := containerdimages.Manifest(ctx, store.content, target, matcher)
	if err != nil {
		return Info{}, fmt.Errorf("selecting manifest for %q: %w", reference, err)
	}
	if opts.Progress != nil {
		for _, layer := range manifest.Layers {
			if _, existed := contentBefore[layer.Digest.String()]; existed {
				opts.Progress(ProgressEvent{Phase: ProgressAlreadyExists, Descriptor: layer, Current: layer.Size, Total: layer.Size})
			}
		}
	}
	configData, err := content.ReadBlob(ctx, store.content, manifest.Config)
	if err != nil {
		return Info{}, fmt.Errorf("reading config for %q: %w", reference, err)
	}
	var config ocispec.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return Info{}, fmt.Errorf("decoding config for %q: %w", reference, err)
	}
	if config.OS != "linux" {
		return Info{}, fmt.Errorf("image %q has unsupported OS %q", reference, config.OS)
	}
	image := containerdimages.Image{Name: reference, Target: target}
	size, err := image.Size(ctx, store.content, matcher)
	if err != nil {
		return Info{}, fmt.Errorf("measuring %q: %w", reference, err)
	}
	now := time.Now().UTC()
	info := Info{
		Reference: reference,
		Digest:    target.Digest.String(),
		MediaType: target.MediaType,
		Size:      size,
		Platform:  imagePlatform(config.OS, config.Architecture, config.Variant),
		CreatedAt: imageCreatedAt(now, config.Created),
		UpdatedAt: now,
	}
	if snapshotErr == nil {
		if contentAfter, err := snapshotContent(ctx, store.content); err == nil {
			stats := contentReuse(contentBefore, contentAfter, size)
			info.PullStats = &stats
		}
	}
	store.catalog.Images[reference] = imageRecord{Info: info, Target: target}
	if err := store.saveCatalog(); err != nil {
		return Info{}, fmt.Errorf("saving native image record: %w", err)
	}
	if err := store.garbageCollect(ctx); err != nil {
		return Info{}, fmt.Errorf("collecting superseded image content: %w", err)
	}
	complete = true
	return info, nil
}

// List returns all named image records ordered by reference. Multiple
// references may intentionally point at the same digest.
func List(ctx context.Context, opts ListOptions) ([]Info, error) {
	store, err := openNativeStore(opts.Root)
	if err != nil {
		return nil, err
	}
	defer store.close()
	infos := make([]Info, 0, len(store.catalog.Images))
	for _, record := range store.catalog.Images {
		if _, err := store.content.Info(ctx, record.Target.Digest); err != nil {
			return nil, fmt.Errorf("checking image %q content: %w", record.Info.Reference, err)
		}
		infos = append(infos, record.Info)
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

// ContainerLeaseID returns the deterministic native content pin for a TTOS family.
func ContainerLeaseID(containerID string) string {
	return "ttos-" + containerID
}

// CreateContentLease pins an image target and its referenced content for a TTOS family.
func CreateContentLease(ctx context.Context, opts ContentLeaseOptions) error {
	if opts.LeaseID == "" || opts.Digest == "" {
		return errors.New("content lease requires an ID and image digest")
	}
	store, err := openNativeStore(opts.Root)
	if err != nil {
		return err
	}
	defer store.close()
	if _, exists := store.catalog.Leases[opts.LeaseID]; exists {
		return fmt.Errorf("content lease %q already exists", opts.LeaseID)
	}
	var target ocispec.Descriptor
	for _, image := range store.catalog.Images {
		if image.Info.Digest == opts.Digest {
			target = image.Target
			break
		}
	}
	if target.Digest == "" {
		return fmt.Errorf("cannot pin unknown image target %s", opts.Digest)
	}
	store.catalog.Leases[opts.LeaseID] = leaseRecord{Digest: opts.Digest, Target: target}
	if err := store.saveCatalog(); err != nil {
		return fmt.Errorf("saving content lease %q: %w", opts.LeaseID, err)
	}
	return nil
}

// DeleteContentLease releases one family pin and collects unreachable content.
func DeleteContentLease(ctx context.Context, opts ContentLeaseOptions) error {
	if opts.LeaseID == "" {
		return nil
	}
	store, err := openNativeStore(opts.Root)
	if err != nil {
		return err
	}
	defer store.close()
	if _, exists := store.catalog.Leases[opts.LeaseID]; !exists {
		return nil
	}
	delete(store.catalog.Leases, opts.LeaseID)
	if err := store.saveCatalog(); err != nil {
		return fmt.Errorf("deleting content lease %q: %w", opts.LeaseID, err)
	}
	return store.garbageCollect(ctx)
}

// Delete removes one exact named image record and collects unreachable content.
func Delete(ctx context.Context, opts DeleteOptions) error {
	store, err := openNativeStore(opts.Root)
	if err != nil {
		return err
	}
	defer store.close()
	record, exists := store.catalog.Images[opts.Reference]
	if !exists {
		return fmt.Errorf("finding image %q: %w", opts.Reference, ErrNotFound)
	}
	if opts.Digest != "" && record.Info.Digest != opts.Digest {
		return fmt.Errorf("image %q changed from %s to %s", opts.Reference, opts.Digest, record.Info.Digest)
	}
	delete(store.catalog.Images, opts.Reference)
	if err := store.saveCatalog(); err != nil {
		return fmt.Errorf("deleting image %q: %w", opts.Reference, err)
	}
	return store.garbageCollect(ctx)
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
