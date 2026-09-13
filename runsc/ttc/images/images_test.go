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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gvisor.dev/gvisor/pkg/ttcvolume"
)

func TestContentReuse(t *testing.T) {
	before := map[string]int64{"sha256:old": 60}
	after := map[string]int64{"sha256:old": 60, "sha256:new": 40}
	if got, want := contentReuse(before, after, 100), (PullStats{DownloadedBytes: 40, ReusedBytes: 60}); got != want {
		t.Fatalf("contentReuse=%+v, want %+v", got, want)
	}
	if got := contentReuse(after, after, 100); got.DownloadedBytes != 0 || got.ReusedBytes != 100 {
		t.Fatalf("full reuse=%+v", got)
	}
	if got := contentReuse(nil, map[string]int64{"extra": 120}, 100); got.DownloadedBytes != 100 || got.ReusedBytes != 0 {
		t.Fatalf("capped download=%+v", got)
	}
}

func TestNativeCatalogPinsContentUntilFamilyRemoval(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	target := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		Size:      5,
	}
	store, err := openNativeStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.WriteBlob(ctx, store.content, "test-config", bytes.NewReader([]byte("hello")), target); err != nil {
		store.close()
		t.Fatal(err)
	}
	reference := "docker.io/library/test:latest"
	store.catalog.Images[reference] = imageRecord{
		Info:   Info{Reference: reference, Digest: target.Digest.String(), Platform: "linux/amd64"},
		Target: target,
	}
	if err := store.saveCatalog(); err != nil {
		store.close()
		t.Fatal(err)
	}
	store.close()

	leaseID := ContainerLeaseID("family")
	if err := CreateContentLease(ctx, ContentLeaseOptions{Root: root, LeaseID: leaseID, Digest: target.Digest.String()}); err != nil {
		t.Fatalf("CreateContentLease: %v", err)
	}
	if err := Delete(ctx, DeleteOptions{Root: root, Reference: reference, Digest: target.Digest.String()}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	store, err = openNativeStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.content.Info(ctx, target.Digest); err != nil {
		store.close()
		t.Fatalf("leased content was collected: %v", err)
	}
	store.close()
	if err := DeleteContentLease(ctx, ContentLeaseOptions{Root: root, LeaseID: leaseID}); err != nil {
		t.Fatalf("DeleteContentLease: %v", err)
	}
	store, err = openNativeStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if _, err := store.content.Info(ctx, target.Digest); err == nil {
		t.Fatal("unreferenced content remains after family pin removal")
	}
}

func TestNativeDeleteGuardsAgainstRetargetedTag(t *testing.T) {
	root := t.TempDir()
	store, err := openNativeStore(root)
	if err != nil {
		t.Fatal(err)
	}
	reference := "docker.io/library/test:latest"
	store.catalog.Images[reference] = imageRecord{
		Info: Info{Reference: reference, Digest: "sha256:" + strings.Repeat("a", 64)},
	}
	if err := store.saveCatalog(); err != nil {
		store.close()
		t.Fatal(err)
	}
	store.close()
	err = Delete(context.Background(), DeleteOptions{Root: root, Reference: reference, Digest: "sha256:" + strings.Repeat("b", 64)})
	if err == nil || !strings.Contains(err.Error(), "changed from") {
		t.Fatalf("Delete retarget guard error=%v", err)
	}
}

func TestCommitAppliesDockerChangesAndArchiveRoundTrips(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	created := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	config := ocispec.Image{
		Created: &created, Platform: ocispec.Platform{Architecture: "amd64", OS: "linux"},
		Config: ocispec.ImageConfig{Labels: map[string]string{"base": "kept", "override": "old"}},
		RootFS: ocispec.RootFS{Type: "layers"},
	}
	configData, err := json.Marshal(&config)
	if err != nil {
		t.Fatal(err)
	}
	algorithm := ocispec.Descriptor{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}.Digest.Algorithm()
	configDescriptor := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: algorithm.FromBytes(configData), Size: int64(len(configData))}
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest, Config: configDescriptor,
	}
	manifest.SchemaVersion = 2
	manifestData, err := json.Marshal(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	target := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: algorithm.FromBytes(manifestData), Size: int64(len(manifestData))}
	store, err := openNativeStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.WriteBlob(ctx, store.content, "base-config", bytes.NewReader(configData), configDescriptor); err != nil {
		store.close()
		t.Fatal(err)
	}
	if err := content.WriteBlob(ctx, store.content, "base-manifest", bytes.NewReader(manifestData), target); err != nil {
		store.close()
		t.Fatal(err)
	}
	baseReference := "docker.io/library/base:latest"
	store.catalog.Images[baseReference] = imageRecord{Info: Info{Reference: baseReference, Digest: target.Digest.String(), Platform: "linux/amd64"}, Target: target}
	if err := store.saveCatalog(); err != nil {
		store.close()
		t.Fatal(err)
	}
	store.close()

	layerData := make([]byte, 1024)
	info, err := Commit(ctx, CommitOptions{
		Root: root, BaseDigest: target.Digest.String(), Reference: "session:v1",
		Layer: bytes.NewReader(layerData), Author: "TTC Harness <ttc@example.com>", Message: "prepared session",
		Changes: []string{
			"LABEL override=new timetravel.usage=prompt",
			"ENV SESSION_MODE=review",
			`CMD ["agent","run"]`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Labels["base"] != "kept" || info.Labels["override"] != "new" || info.Labels["timetravel.usage"] != "prompt" {
		t.Fatalf("committed labels=%v", info.Labels)
	}
	store, err = openNativeStore(root)
	if err != nil {
		t.Fatal(err)
	}
	record := store.catalog.Images["docker.io/library/session:v1"]
	committedManifest, err := containerdimages.Manifest(ctx, store.content, record.Target, platforms.DefaultStrict())
	if err != nil {
		t.Fatal(err)
	}
	if len(committedManifest.Layers) != 1 {
		t.Fatalf("committed layers=%d, want 1", len(committedManifest.Layers))
	}
	committedLayer := committedManifest.Layers[0]
	if committedLayer.MediaType != ocispec.MediaTypeImageLayerGzip || committedLayer.Size >= int64(len(layerData)) {
		t.Fatalf("committed layer=%+v, want compressed layer smaller than %d bytes", committedLayer, len(layerData))
	}
	compressedLayer, err := content.ReadBlob(ctx, store.content, committedLayer)
	if err != nil {
		t.Fatal(err)
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(compressedLayer))
	if err != nil {
		t.Fatal(err)
	}
	uncompressedLayer, err := io.ReadAll(gzipReader)
	if err != nil {
		t.Fatal(err)
	}
	if err := gzipReader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(uncompressedLayer, layerData) {
		t.Fatal("committed layer changed while compressing")
	}
	committedConfigData, err := content.ReadBlob(ctx, store.content, committedManifest.Config)
	if err != nil {
		t.Fatal(err)
	}
	var committedConfig ocispec.Image
	if err := json.Unmarshal(committedConfigData, &committedConfig); err != nil {
		t.Fatal(err)
	}
	if committedConfig.Config.Labels["base"] != "kept" || committedConfig.Config.Labels["override"] != "new" ||
		committedConfig.Config.Labels["timetravel.usage"] != "prompt" || committedConfig.Author != "TTC Harness <ttc@example.com>" ||
		!reflect.DeepEqual(committedConfig.Config.Cmd, []string{"agent", "run"}) || !slices.Contains(committedConfig.Config.Env, "SESSION_MODE=review") ||
		len(committedConfig.RootFS.DiffIDs) != 1 || committedConfig.RootFS.DiffIDs[0] != algorithm.FromBytes(layerData) ||
		committedConfig.History[len(committedConfig.History)-1].Comment != "prepared session" {
		t.Fatalf("committed config=%+v", committedConfig)
	}
	alias := record
	alias.Info.Reference = "docker.io/library/session:review"
	store.catalog.Images[alias.Info.Reference] = alias
	if err := store.saveCatalog(); err != nil {
		store.close()
		t.Fatal(err)
	}
	store.close()

	var archive bytes.Buffer
	if err := Save(ctx, SaveOptions{Root: root, Archive: &archive, Images: []string{"session"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	loadedRoot := t.TempDir()
	existingTarget := target.Digest.Algorithm().FromString("existing image")
	existingDigest := existingTarget.String()
	loadedStore, err := openNativeStore(loadedRoot)
	if err != nil {
		t.Fatal(err)
	}
	existingReference := "docker.io/library/session:v1"
	loadedStore.catalog.Images[existingReference] = imageRecord{
		Info: Info{Reference: existingReference, Digest: existingDigest, Platform: "linux/amd64"},
		Target: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    existingTarget,
		},
	}
	if err := loadedStore.saveCatalog(); err != nil {
		loadedStore.close()
		t.Fatal(err)
	}
	loadedStore.close()
	loaded, err := Load(ctx, LoadOptions{Root: loadedRoot, Archive: bytes.NewReader(compressed.Bytes()), Platform: "linux/amd64"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loadedStore, err = openNativeStore(loadedRoot)
	if err != nil {
		t.Fatal(err)
	}
	visible := loadedStore.catalog.Images
	loadedStore.close()
	if len(visible) != 1 || visible[existingReference].Info.Digest != existingDigest {
		t.Fatalf("unpublished load changed the visible catalog: %+v", visible)
	}
	if err := PublishLoaded(ctx, loadedRoot, loaded); err != nil {
		t.Fatalf("PublishLoaded: %v", err)
	}
	published, err := List(ctx, ListOptions{Root: loadedRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 {
		t.Fatalf("published images=%+v, want two archive tags", published)
	}
	for _, image := range published {
		if image.Reference == existingReference && image.Digest == existingDigest {
			t.Fatalf("published load retained superseded image: %+v", image)
		}
	}
	var references []string
	for _, loadedInfo := range loaded {
		references = append(references, loadedInfo.Info.Reference)
	}
	wantReferences := []string{"docker.io/library/session:review", "docker.io/library/session:v1"}
	if !reflect.DeepEqual(references, wantReferences) {
		t.Fatalf("loaded references=%q, want %q", references, wantReferences)
	}
}

func TestCommittedWhiteoutsSurviveSaveLoadAndMaterialize(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	algorithm := ocispec.Descriptor{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}.Digest.Algorithm()
	baseLayerData := makeLayer(t, false, []tarEntry{
		{name: "keep", body: "base", mode: 0644},
		{name: "deleted", body: "remove me", mode: 0644},
		{name: "tree/", mode: 0755},
		{name: "tree/old", body: "remove tree", mode: 0644},
		{name: "opaque/", mode: 0755},
		{name: "opaque/old", body: "remove child", mode: 0644},
	})
	baseLayer := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    algorithm.FromBytes(baseLayerData),
		Size:      int64(len(baseLayerData)),
	}
	config := ocispec.Image{
		Platform: ocispec.Platform{Architecture: "amd64", OS: "linux"},
		RootFS:   ocispec.RootFS{Type: "layers"},
	}
	config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, baseLayer.Digest)
	configData, err := json.Marshal(&config)
	if err != nil {
		t.Fatal(err)
	}
	configDescriptor := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    algorithm.FromBytes(configData),
		Size:      int64(len(configData)),
	}
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDescriptor,
		Layers:    []ocispec.Descriptor{baseLayer},
	}
	manifest.SchemaVersion = 2
	manifestData, err := json.Marshal(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	target := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    algorithm.FromBytes(manifestData),
		Size:      int64(len(manifestData)),
	}
	store, err := openNativeStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, blob := range map[string]struct {
		data       []byte
		descriptor ocispec.Descriptor
	}{
		"base-layer":    {baseLayerData, baseLayer},
		"base-config":   {configData, configDescriptor},
		"base-manifest": {manifestData, target},
	} {
		if err := content.WriteBlob(ctx, store.content, name, bytes.NewReader(blob.data), blob.descriptor); err != nil {
			store.close()
			t.Fatal(err)
		}
	}
	baseReference := "docker.io/library/whiteout-base:latest"
	store.catalog.Images[baseReference] = imageRecord{
		Info:   Info{Reference: baseReference, Digest: target.Digest.String(), Platform: "linux/amd64"},
		Target: target,
	}
	if err := store.saveCatalog(); err != nil {
		store.close()
		t.Fatal(err)
	}
	store.close()

	manager, err := ttcvolume.OpenManager(filepath.Join(t.TempDir(), "volumes"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("upper")
	if err != nil {
		t.Fatal(err)
	}
	deletions := []string{"deleted", "tree"}
	for _, name := range deletions {
		if _, err := repository.CreateSpecial(ttcvolume.MainBranch, ttcvolume.RootInode, name, "char", 0, 0, 0, 0, 0); err != nil {
			repository.Close()
			t.Fatal(err)
		}
	}
	opaque, err := repository.CreateDirectory(ttcvolume.MainBranch, ttcvolume.RootInode, "opaque", 0755, 0, 0)
	if err != nil {
		repository.Close()
		t.Fatal(err)
	}
	opaqueMeta, err := repository.Inode(ttcvolume.MainBranch, opaque)
	if err != nil {
		repository.Close()
		t.Fatal(err)
	}
	opaqueMeta.Xattrs = map[string]string{"trusted.overlay.opaque": "y"}
	if err := repository.UpdateMetadata(ttcvolume.MainBranch, opaqueMeta); err != nil {
		repository.Close()
		t.Fatal(err)
	}
	newFile, err := repository.CreateRegular(ttcvolume.MainBranch, opaque, "new", 0644, 0, 0)
	if err != nil {
		repository.Close()
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(ttcvolume.MainBranch, newFile, []byte("new child"), 0); err != nil {
		repository.Close()
		t.Fatal(err)
	}
	var upperLayer bytes.Buffer
	if err := repository.WriteLayerTar(ttcvolume.MainBranch, &upperLayer); err != nil {
		repository.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(ctx, CommitOptions{
		Root: root, BaseDigest: target.Digest.String(), Reference: "whiteout-roundtrip:latest",
		Layer: bytes.NewReader(upperLayer.Bytes()),
	}); err != nil {
		t.Fatal(err)
	}

	var imageArchive bytes.Buffer
	if err := Save(ctx, SaveOptions{Root: root, Archive: &imageArchive, Images: []string{"whiteout-roundtrip:latest"}}); err != nil {
		t.Fatal(err)
	}
	loadedRoot := t.TempDir()
	loaded, err := Load(ctx, LoadOptions{Root: loadedRoot, Archive: bytes.NewReader(imageArchive.Bytes()), Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded images=%d, want 1", len(loaded))
	}
	materialized := filepath.Join(t.TempDir(), "rootfs")
	if _, err := MaterializeImportedRootFS(ctx, RootFSOptions{
		StoreRoot: loadedRoot, Root: materialized, Platform: "linux/amd64",
	}, loaded[0]); err != nil {
		t.Fatal(err)
	}
	if err := PublishLoaded(ctx, loadedRoot, loaded); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(materialized, "keep")); err != nil || string(got) != "base" {
		t.Fatalf("unchanged base file=%q, err=%v", got, err)
	}
	for _, removed := range []string{"deleted", "tree", "opaque/old"} {
		if _, err := os.Lstat(filepath.Join(materialized, removed)); !os.IsNotExist(err) {
			t.Fatalf("deleted path %q survived commit/load: %v", removed, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(materialized, "opaque/new")); err != nil || string(got) != "new child" {
		t.Fatalf("opaque replacement file=%q, err=%v", got, err)
	}
}

func TestApplyCommitChangeDockerForms(t *testing.T) {
	config := ocispec.ImageConfig{
		Env:    []string{"KEPT=yes", "REPLACED=old"},
		Labels: map[string]string{"base": "kept"},
	}
	for _, change := range []string{
		"ENV REPLACED=new ADDED='two words'",
		"EXPOSE 8080 8443/udp",
		"LABEL timetravel.usage 'agent session'",
		`ENTRYPOINT ["agent"]`,
		"CMD run --foreground",
		"USER 1000:1000",
		`VOLUME ["/work","/cache"]`,
		"WORKDIR /work",
	} {
		if err := applyCommitChange(&config, change); err != nil {
			t.Fatalf("applyCommitChange(%q): %v", change, err)
		}
	}
	if want := []string{"KEPT=yes", "REPLACED=new", "ADDED=two words"}; !reflect.DeepEqual(config.Env, want) {
		t.Errorf("Env=%q, want %q", config.Env, want)
	}
	if _, ok := config.ExposedPorts["8080/tcp"]; !ok {
		t.Errorf("ExposedPorts=%v", config.ExposedPorts)
	}
	if _, ok := config.ExposedPorts["8443/udp"]; !ok {
		t.Errorf("ExposedPorts=%v", config.ExposedPorts)
	}
	if config.Labels["base"] != "kept" || config.Labels["timetravel.usage"] != "agent session" {
		t.Errorf("Labels=%v", config.Labels)
	}
	if want := []string{"agent"}; !reflect.DeepEqual(config.Entrypoint, want) {
		t.Errorf("Entrypoint=%q, want %q", config.Entrypoint, want)
	}
	if want := []string{"/bin/sh", "-c", "run --foreground"}; !reflect.DeepEqual(config.Cmd, want) {
		t.Errorf("Cmd=%q, want %q", config.Cmd, want)
	}
	if config.User != "1000:1000" || config.WorkingDir != "/work" {
		t.Errorf("User=%q WorkingDir=%q", config.User, config.WorkingDir)
	}
	for _, volume := range []string{"/work", "/cache"} {
		if _, ok := config.Volumes[volume]; !ok {
			t.Errorf("Volumes=%v", config.Volumes)
		}
	}
	if err := applyCommitChange(&config, "RUN echo unsupported"); err == nil {
		t.Fatal("RUN unexpectedly accepted")
	}
}

func TestContainerLeaseID(t *testing.T) {
	containerID := strings.Repeat("a", 64)
	if got, want := ContainerLeaseID(containerID), "ttc-"+containerID; got != want {
		t.Fatalf("ContainerLeaseID=%q, want %q", got, want)
	}
}

func TestNormalizeReference(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "ubuntu", want: "docker.io/library/ubuntu:latest"},
		{input: "example/app", want: "docker.io/example/app:latest"},
		{input: "ubuntu:24.04", want: "docker.io/library/ubuntu:24.04"},
		{input: "quay.io/example/app:v1", want: "quay.io/example/app:v1"},
		{input: "localhost:5000/app", want: "localhost:5000/app:latest"},
		{input: "busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662", want: "docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := NormalizeReference(test.input)
			if err != nil {
				t.Fatalf("NormalizeReference(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Errorf("NormalizeReference(%q)=%q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeReferenceRejectsInvalid(t *testing.T) {
	for _, input := range []string{"", "UPPERCASE", "bad reference"} {
		if got, err := NormalizeReference(input); err == nil {
			t.Errorf("NormalizeReference(%q)=%q, want error", input, got)
		}
	}
}

func TestResolveNameDigestAndAliases(t *testing.T) {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	infos := []Info{
		{Reference: "docker.io/library/ubuntu:latest", Digest: digestA},
		{Reference: "docker.io/library/ubuntu:stable", Digest: digestA},
		{Reference: "quay.io/example/tool:v1", Digest: digestB},
	}
	matches, err := Resolve(infos, "ubuntu")
	if err != nil {
		t.Fatalf("Resolve(ubuntu): %v", err)
	}
	if len(matches) != 1 || matches[0].Reference != "docker.io/library/ubuntu:latest" {
		t.Errorf("Resolve(ubuntu)=%+v", matches)
	}
	matches, err = Resolve(infos, strings.Repeat("a", 12))
	if err != nil {
		t.Fatalf("Resolve(short digest): %v", err)
	}
	wantAliases := []Info{infos[0], infos[1]}
	if !reflect.DeepEqual(matches, wantAliases) {
		t.Errorf("Resolve(short digest)=%+v, want %+v", matches, wantAliases)
	}
	matches, err = Resolve(infos, "sha256:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatalf("Resolve(full digest): %v", err)
	}
	if len(matches) != 1 || matches[0].Reference != infos[2].Reference {
		t.Errorf("Resolve(full digest)=%+v", matches)
	}
}

func TestResolveRejectsUnknownAndAmbiguousPrefixes(t *testing.T) {
	infos := []Info{
		{Reference: "example/a:v1", Digest: "sha256:abcd" + strings.Repeat("1", 60)},
		{Reference: "example/b:v1", Digest: "sha256:abcd" + strings.Repeat("2", 60)},
	}
	for _, identifier := range []string{"abcd", "missing", "1234"} {
		if matches, err := Resolve(infos, identifier); err == nil {
			t.Errorf("Resolve(%q)=%+v, want error", identifier, matches)
		}
	}
}

func TestImageCreatedAtPrefersOCIConfiguration(t *testing.T) {
	stored := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	configured := stored.Add(-24 * time.Hour)
	if got := imageCreatedAt(stored, &configured); !got.Equal(configured) {
		t.Errorf("imageCreatedAt with config=%v, want %v", got, configured)
	}
	if got := imageCreatedAt(stored, nil); !got.Equal(stored) {
		t.Errorf("imageCreatedAt without config=%v, want %v", got, stored)
	}
}

func TestDockerCredentialsInlineAndDockerHubAlias(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("test-user:test-secret"))
	config := dockerConfigFile{Auths: map[string]dockerAuthConfig{
		"https://index.docker.io/v1/": {Auth: auth},
	}}
	username, secret, err := config.credentials("registry-1.docker.io")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if username != "test-user" || secret != "test-secret" {
		t.Errorf("credentials=%q:%q, want test-user:test-secret", username, secret)
	}
}

func TestDockerCredentialHelper(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "docker-credential-fixture")
	script := "#!/bin/sh\nread server\nprintf \"{\\\"Username\\\":\\\"helper-user\\\",\\\"Secret\\\":\\\"helper-secret\\\"}\n\"\n"
	if err := os.WriteFile(helper, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	config := dockerConfigFile{CredHelpers: map[string]string{"registry.example": "fixture"}}
	username, secret, err := config.credentials("registry.example")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if username != "helper-user" || secret != "helper-secret" {
		t.Errorf("credentials=%q:%q, want helper-user:helper-secret", username, secret)
	}
}

func TestDockerCredentialCallbackUsesDockerConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	auth := base64.StdEncoding.EncodeToString([]byte("configured:secret"))
	data := []byte(fmt.Sprintf(`{"auths":{"registry.example":{"auth":%q}}}`, auth))
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	callback, configured, err := dockerCredentialCallback()
	if err != nil {
		t.Fatalf("dockerCredentialCallback: %v", err)
	}
	if !configured {
		t.Fatal("Docker configuration was not detected")
	}
	username, secret, err := callback("registry.example")
	if err != nil || username != "configured" || secret != "secret" {
		t.Fatalf("callback credentials=%q:%q err=%v", username, secret, err)
	}
}

func TestPruneEmptyNativeIngestDirectories(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	nonempty := filepath.Join(root, "nonempty")
	if err := os.Mkdir(empty, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nonempty, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonempty, "data"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := pruneEmptyNativeIngestDirectories(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty ingest directory remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nonempty, "data")); err != nil {
		t.Fatalf("nonempty ingest transaction was modified: %v", err)
	}
}
