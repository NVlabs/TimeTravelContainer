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

// Package images implements TTC's native OCI image operations.
package images

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/errdefs"
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

// LoadOptions controls loading a Docker or OCI image archive into the native
// store.
type LoadOptions struct {
	Root     string
	Archive  io.Reader
	Platform string
}

// ImportedImage is an image archive entry whose content is present in TTC's
// native store but whose tag has not yet been published. Callers materialize
// all imported roots before publishing the batch, so a failed load never
// replaces existing tags or exposes an unusable image.
type ImportedImage struct {
	Info   Info
	target ocispec.Descriptor
}

// SaveOptions controls saving named images as a Docker/OCI-compatible archive.
type SaveOptions struct {
	Root     string
	Archive  io.Writer
	Images   []string
	Platform string
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

// ContentLeaseOptions identifies a durable TTC family lease over one image target.
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
	Reference string            `json:"reference"`
	Digest    string            `json:"digest"`
	MediaType string            `json:"media_type"`
	Size      int64             `json:"size"`
	Platform  string            `json:"platform"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Labels    map[string]string `json:"labels,omitempty"`
	PullStats *PullStats        `json:"pull_stats,omitempty"`
}

// CommitOptions creates a new OCI image by appending an uncompressed rootfs
// upper layer to an existing image. Layer must be an OCI layer tar stream.
type CommitOptions struct {
	Root       string
	BaseDigest string
	Reference  string
	Layer      io.Reader
	Author     string
	Message    string
	Changes    []string
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
		// TTC always executes a Linux guest ABI. containerd's DefaultStrict
		// derives the OS from the host, which would select darwin/* when TTC is
		// running natively on macOS.
		guest := platforms.Normalize(ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH})
		return platforms.OnlyStrict(guest), nil
	}
	parsed, err := platforms.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid platform %q: %w", value, err)
	}
	return platforms.OnlyStrict(parsed), nil
}

// Pull resolves and stores one image in TTC's native content store.
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
		Labels:    cloneLabels(config.Config.Labels),
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

// Load reads an OCI or Docker image archive into TTC's native content store
// without publishing its tags. The caller must invoke PublishLoaded after all
// image roots are ready, or DiscardLoaded after a failure.
func Load(ctx context.Context, opts LoadOptions) ([]ImportedImage, error) {
	if opts.Archive == nil {
		return nil, errors.New("image load requires an archive stream")
	}
	matcher, err := platformMatcher(opts.Platform)
	if err != nil {
		return nil, err
	}
	store, err := openNativeStore(opts.Root)
	if err != nil {
		return nil, err
	}
	defer store.close()
	complete := false
	defer func() {
		if !complete {
			_ = store.garbageCollect(context.Background())
		}
	}()

	decompressed, err := compression.DecompressStream(opts.Archive)
	if err != nil {
		return nil, fmt.Errorf("detecting image archive compression: %w", err)
	}
	defer decompressed.Close()
	index, err := archive.ImportIndex(ctx, store.content, decompressed)
	if err != nil {
		return nil, fmt.Errorf("reading image archive: %w", err)
	}
	indexData, err := content.ReadBlob(ctx, store.content, index)
	if err != nil {
		return nil, fmt.Errorf("reading archive index: %w", err)
	}
	var parsed ocispec.Index
	if err := json.Unmarshal(indexData, &parsed); err != nil {
		return nil, fmt.Errorf("decoding archive index: %w", err)
	}
	now := time.Now().UTC()
	loaded := make(map[string]ImportedImage)
	for _, descriptor := range parsed.Manifests {
		reference := descriptor.Annotations[containerdimages.AnnotationImageName]
		if reference == "" {
			reference = descriptor.Annotations[ocispec.AnnotationRefName]
		}
		if reference == "" {
			continue
		}
		reference, err = NormalizeReference(reference)
		if err != nil {
			return nil, err
		}
		manifest, err := containerdimages.Manifest(ctx, store.content, descriptor, matcher)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("selecting platform for %q: %w", reference, err)
		}
		configData, err := content.ReadBlob(ctx, store.content, manifest.Config)
		if err != nil {
			return nil, fmt.Errorf("reading config for %q: %w", reference, err)
		}
		var config ocispec.Image
		if err := json.Unmarshal(configData, &config); err != nil {
			return nil, fmt.Errorf("decoding config for %q: %w", reference, err)
		}
		if config.OS != "linux" {
			continue
		}
		image := containerdimages.Image{Name: reference, Target: descriptor}
		size, err := image.Size(ctx, store.content, matcher)
		if err != nil {
			return nil, fmt.Errorf("measuring %q: %w", reference, err)
		}
		info := Info{
			Reference: reference,
			Digest:    descriptor.Digest.String(),
			MediaType: descriptor.MediaType,
			Size:      size,
			Platform:  imagePlatform(config.OS, config.Architecture, config.Variant),
			CreatedAt: imageCreatedAt(now, config.Created),
			UpdatedAt: now,
			Labels:    cloneLabels(config.Config.Labels),
		}
		loaded[reference] = ImportedImage{Info: info, target: descriptor}
	}
	if len(loaded) == 0 {
		if opts.Platform != "" {
			return nil, fmt.Errorf("requested platform %q not found: image might be filtered out", opts.Platform)
		}
		return nil, errors.New("image archive contains no named Linux image for this platform")
	}
	images := make([]ImportedImage, 0, len(loaded))
	for _, image := range loaded {
		images = append(images, image)
	}
	complete = true
	sort.Slice(images, func(i, j int) bool { return images[i].Info.Reference < images[j].Info.Reference })
	return images, nil
}

// PublishLoaded atomically makes every imported tag visible. Image content is
// already durable; superseded unreferenced blobs are collected afterward.
func PublishLoaded(ctx context.Context, root string, images []ImportedImage) error {
	if len(images) == 0 {
		return errors.New("publishing image load requires at least one image")
	}
	store, err := openNativeStore(root)
	if err != nil {
		return err
	}
	defer store.close()
	for _, image := range images {
		if image.Info.Reference == "" || image.target.Digest == "" {
			return errors.New("cannot publish an incomplete imported image")
		}
		store.catalog.Images[image.Info.Reference] = imageRecord{Info: image.Info, Target: image.target}
	}
	if err := store.saveCatalog(); err != nil {
		return fmt.Errorf("saving loaded image records: %w", err)
	}
	// Catalog publication is the commit point. Garbage collection is cleanup,
	// not a reason to report that an otherwise usable load failed.
	_ = store.garbageCollect(ctx)
	return nil
}

// DiscardLoaded removes content that is not referenced by a published image
// or active container lease.
func DiscardLoaded(ctx context.Context, root string) error {
	store, err := openNativeStore(root)
	if err != nil {
		return err
	}
	defer store.close()
	return store.garbageCollect(ctx)
}

// Save writes named native images as a Docker/OCI-compatible archive.
func Save(ctx context.Context, opts SaveOptions) error {
	if opts.Archive == nil {
		return errors.New("image save requires an archive writer")
	}
	if len(opts.Images) == 0 {
		return errors.New("image save requires at least one image")
	}
	store, err := openNativeStore(opts.Root)
	if err != nil {
		return err
	}
	defer store.close()
	selected := make(map[string]imageRecord)
	for _, identifier := range opts.Images {
		records, err := resolveSaveRecords(store.catalog.Images, identifier)
		if err != nil {
			return err
		}
		for _, record := range records {
			selected[record.Info.Reference] = record
		}
	}
	images := make([]containerdimages.Image, 0, len(selected))
	for _, record := range selected {
		images = append(images, containerdimages.Image{Name: record.Info.Reference, Target: record.Target})
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Name < images[j].Name })
	exportOptions := []archive.ExportOpt{
		archive.WithImages(images),
		archive.WithSkipMissing(store.content),
	}
	if opts.Platform == "" {
		exportOptions = append(exportOptions, archive.WithAllPlatforms())
	} else {
		matcher, err := platformMatcher(opts.Platform)
		if err != nil {
			return err
		}
		exportOptions = append(exportOptions, archive.WithPlatform(matcher))
	}
	if err := archive.Export(ctx, store.content, opts.Archive, exportOptions...); err != nil {
		return fmt.Errorf("writing image archive: %w", err)
	}
	return nil
}

func resolveSaveRecords(catalog map[string]imageRecord, identifier string) ([]imageRecord, error) {
	if named, err := distref.ParseNormalizedNamed(identifier); err == nil && distref.IsNameOnly(named) {
		repository := distref.TrimNamed(named).String()
		var records []imageRecord
		for reference, record := range catalog {
			parsed, err := distref.ParseNormalizedNamed(reference)
			if err == nil && distref.TrimNamed(parsed).String() == repository {
				records = append(records, record)
			}
		}
		if len(records) == 0 {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, identifier)
		}
		sort.Slice(records, func(i, j int) bool { return records[i].Info.Reference < records[j].Info.Reference })
		return records, nil
	}
	infos := make([]Info, 0, len(catalog))
	byReference := make(map[string]imageRecord, len(catalog))
	for reference, record := range catalog {
		infos = append(infos, record.Info)
		byReference[reference] = record
	}
	matches, err := Resolve(infos, identifier)
	if err != nil {
		return nil, err
	}
	records := make([]imageRecord, 0, len(matches))
	for _, match := range matches {
		records = append(records, byReference[match.Reference])
	}
	return records, nil
}

func applyCommitChange(config *ocispec.ImageConfig, change string) error {
	instruction, value, ok := strings.Cut(strings.TrimSpace(change), " ")
	if !ok || strings.TrimSpace(value) == "" {
		return errors.New("change must contain a Dockerfile instruction and value")
	}
	instruction = strings.ToUpper(instruction)
	value = strings.TrimSpace(value)
	switch instruction {
	case "CMD":
		command, err := parseCommandChange(value)
		if err != nil {
			return err
		}
		config.Cmd = command
	case "ENTRYPOINT":
		command, err := parseCommandChange(value)
		if err != nil {
			return err
		}
		config.Entrypoint = command
	case "ENV":
		words, err := splitChangeWords(value)
		if err != nil {
			return err
		}
		if len(words) == 0 {
			return errors.New("ENV requires a key and value")
		}
		assignments := words
		if !strings.Contains(words[0], "=") {
			if len(words) < 2 {
				return errors.New("ENV requires a value")
			}
			assignments = []string{words[0] + "=" + strings.Join(words[1:], " ")}
		}
		for _, assignment := range assignments {
			key, _, ok := strings.Cut(assignment, "=")
			if !ok || key == "" {
				return fmt.Errorf("invalid ENV assignment %q", assignment)
			}
			config.Env = replaceKeyValue(config.Env, key, assignment)
		}
	case "EXPOSE":
		words, err := splitChangeWords(value)
		if err != nil {
			return err
		}
		if config.ExposedPorts == nil {
			config.ExposedPorts = make(map[string]struct{})
		}
		for _, port := range words {
			if !strings.Contains(port, "/") {
				port += "/tcp"
			}
			config.ExposedPorts[port] = struct{}{}
		}
	case "LABEL":
		words, err := splitChangeWords(value)
		if err != nil {
			return err
		}
		if config.Labels == nil {
			config.Labels = make(map[string]string)
		}
		assignments := words
		if len(words) != 0 && !strings.Contains(words[0], "=") {
			if len(words) < 2 {
				return errors.New("LABEL requires a value")
			}
			assignments = []string{words[0] + "=" + strings.Join(words[1:], " ")}
		}
		for _, assignment := range assignments {
			key, labelValue, ok := strings.Cut(assignment, "=")
			if !ok || key == "" {
				return fmt.Errorf("invalid LABEL assignment %q", assignment)
			}
			config.Labels[key] = labelValue
		}
	case "USER":
		config.User = value
	case "VOLUME":
		volumes, err := parseStringListChange(value)
		if err != nil {
			return err
		}
		if config.Volumes == nil {
			config.Volumes = make(map[string]struct{})
		}
		for _, volume := range volumes {
			config.Volumes[volume] = struct{}{}
		}
	case "WORKDIR":
		config.WorkingDir = value
	default:
		return fmt.Errorf("unsupported Dockerfile instruction %q", instruction)
	}
	return nil
}

func replaceKeyValue(values []string, key, replacement string) []string {
	prefix := key + "="
	for i := range values {
		if strings.HasPrefix(values[i], prefix) {
			values[i] = replacement
			return values
		}
	}
	return append(values, replacement)
}

func parseCommandChange(value string) ([]string, error) {
	if strings.HasPrefix(strings.TrimSpace(value), "[") {
		var command []string
		if err := json.Unmarshal([]byte(value), &command); err != nil {
			return nil, fmt.Errorf("decoding JSON command: %w", err)
		}
		if command == nil {
			return nil, errors.New("JSON command must be an array of strings")
		}
		return command, nil
	}
	return []string{"/bin/sh", "-c", value}, nil
}

func parseStringListChange(value string) ([]string, error) {
	if strings.HasPrefix(strings.TrimSpace(value), "[") {
		var values []string
		if err := json.Unmarshal([]byte(value), &values); err != nil {
			return nil, fmt.Errorf("decoding JSON string list: %w", err)
		}
		return values, nil
	}
	return splitChangeWords(value)
}

func splitChangeWords(value string) ([]string, error) {
	var words []string
	var word strings.Builder
	quote := rune(0)
	escaped := false
	haveWord := false
	flush := func() {
		if haveWord {
			words = append(words, word.String())
			word.Reset()
			haveWord = false
		}
	}
	for _, r := range value {
		if escaped {
			word.WriteRune(r)
			haveWord = true
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			haveWord = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			haveWord = true
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			haveWord = true
		case ' ', '\t', '\r', '\n':
			flush()
		default:
			word.WriteRune(r)
			haveWord = true
		}
	}
	if escaped {
		return nil, errors.New("unfinished escape in change")
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in change")
	}
	flush()
	return words, nil
}

// Commit appends a TTC rootfs-upper tar stream to an existing image and stores
// a new OCI manifest. The base image configuration is retained, including all
// existing OCI configuration; opts.Changes apply Docker-style changes.
func Commit(ctx context.Context, opts CommitOptions) (Info, error) {
	if opts.Layer == nil {
		return Info{}, errors.New("image commit requires a layer stream")
	}
	reference, err := NormalizeReference(opts.Reference)
	if err != nil {
		return Info{}, err
	}
	store, err := openNativeStore(opts.Root)
	if err != nil {
		return Info{}, err
	}
	defer store.close()
	var base imageRecord
	found := false
	for _, record := range store.catalog.Images {
		if record.Info.Digest == opts.BaseDigest {
			base = record
			found = true
			break
		}
	}
	if !found {
		return Info{}, fmt.Errorf("base image %s is not present", opts.BaseDigest)
	}
	matcher, err := platformMatcher(base.Info.Platform)
	if err != nil {
		return Info{}, err
	}
	baseManifest, err := containerdimages.Manifest(ctx, store.content, base.Target, matcher)
	if err != nil {
		return Info{}, fmt.Errorf("reading base image manifest: %w", err)
	}
	configData, err := content.ReadBlob(ctx, store.content, baseManifest.Config)
	if err != nil {
		return Info{}, fmt.Errorf("reading base image config: %w", err)
	}
	var config ocispec.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return Info{}, fmt.Errorf("decoding base image config: %w", err)
	}
	for _, change := range opts.Changes {
		if err := applyCommitChange(&config.Config, change); err != nil {
			return Info{}, fmt.Errorf("applying change %q: %w", change, err)
		}
	}

	temporary, err := os.CreateTemp(store.root, ".commit-layer-")
	if err != nil {
		return Info{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	algorithm := base.Target.Digest.Algorithm()
	if !algorithm.Available() {
		temporary.Close()
		return Info{}, fmt.Errorf("base image uses unavailable digest algorithm %q", algorithm)
	}
	blobDigester := algorithm.Digester()
	diffDigester := algorithm.Digester()
	// OCI identifies the stored gzip blob separately from the uncompressed tar
	// stream recorded in config.RootFS.DiffIDs.
	compressor := gzip.NewWriter(io.MultiWriter(temporary, blobDigester.Hash()))
	_, copyErr := io.Copy(compressor, io.TeeReader(opts.Layer, diffDigester.Hash()))
	closeErr := compressor.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		temporary.Close()
		return Info{}, fmt.Errorf("compressing committed layer: %w", err)
	}
	layerSize, err := temporary.Seek(0, io.SeekCurrent)
	if err != nil {
		temporary.Close()
		return Info{}, err
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		temporary.Close()
		return Info{}, err
	}
	layer := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip, Digest: blobDigester.Digest(), Size: layerSize,
	}
	if err := content.WriteBlob(ctx, store.content, "ttc-commit-layer-"+layer.Digest.Encoded(), temporary, layer); err != nil {
		temporary.Close()
		return Info{}, fmt.Errorf("storing committed layer: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Info{}, err
	}

	now := time.Now().UTC()
	config.Created = &now
	config.RootFS.Type = "layers"
	config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, diffDigester.Digest())
	if opts.Author != "" {
		config.Author = opts.Author
	}
	comment := opts.Message
	if comment == "" {
		comment = "TimeTravelContainer branch commit"
	}
	config.History = append(config.History, ocispec.History{Created: &now, CreatedBy: "ttc commit", Author: opts.Author, Comment: comment})
	encodedConfig, err := json.Marshal(&config)
	if err != nil {
		return Info{}, err
	}
	configDescriptor := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    algorithm.FromBytes(encodedConfig), Size: int64(len(encodedConfig)),
	}
	if err := content.WriteBlob(ctx, store.content, "ttc-commit-config-"+configDescriptor.Digest.Encoded(), bytes.NewReader(encodedConfig), configDescriptor); err != nil {
		return Info{}, fmt.Errorf("storing committed image config: %w", err)
	}
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDescriptor, Layers: append(append([]ocispec.Descriptor(nil), baseManifest.Layers...), layer),
	}
	manifest.SchemaVersion = 2
	encodedManifest, err := json.Marshal(&manifest)
	if err != nil {
		return Info{}, err
	}
	target := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    algorithm.FromBytes(encodedManifest), Size: int64(len(encodedManifest)),
	}
	if err := content.WriteBlob(ctx, store.content, "ttc-commit-manifest-"+target.Digest.Encoded(), bytes.NewReader(encodedManifest), target); err != nil {
		return Info{}, fmt.Errorf("storing committed image manifest: %w", err)
	}
	image := containerdimages.Image{Name: reference, Target: target}
	size, err := image.Size(ctx, store.content, matcher)
	if err != nil {
		return Info{}, fmt.Errorf("measuring committed image: %w", err)
	}
	info := Info{
		Reference: reference, Digest: target.Digest.String(), MediaType: target.MediaType,
		Size: size, Platform: imagePlatform(config.OS, config.Architecture, config.Variant),
		CreatedAt: now, UpdatedAt: now, Labels: cloneLabels(config.Config.Labels),
	}
	store.catalog.Images[reference] = imageRecord{Info: info, Target: target}
	if err := store.saveCatalog(); err != nil {
		return Info{}, fmt.Errorf("saving committed image record: %w", err)
	}
	if err := store.garbageCollect(ctx); err != nil {
		return Info{}, err
	}
	return info, nil
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
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

// ContainerLeaseID returns the deterministic native content pin for a TTC family.
func ContainerLeaseID(containerID string) string {
	return "ttc-" + containerID
}

// CreateContentLease pins an image target and its referenced content for a TTC family.
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
