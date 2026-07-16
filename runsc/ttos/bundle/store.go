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

package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"gvisor.dev/gvisor/runsc/ttos/images"
)

// Store owns persistent image roots and generated container bundles.
type Store struct {
	Root      string
	Address   string
	Namespace string
}

// PrepareOptions describes an image-backed OCI bundle.
type PrepareOptions struct {
	SpecOptions
	Reference string
	Platform  string
}

// Prepared is a generated OCI bundle ready for runsc.
type Prepared struct {
	BundleDir string
	RootFS    string
	Image     images.RootFSInfo
}

// ContainerInfo is the persistent configuration needed to start a container.
type ContainerInfo struct {
	Name       string          `json:"name"`
	Reference  string          `json:"reference"`
	Digest     string          `json:"digest"`
	Platform   string          `json:"platform"`
	BundleDir  string          `json:"bundle_dir"`
	RootFS     string          `json:"rootfs"`
	Volumes    []ManagedVolume `json:"volumes,omitempty"`
	State      string          `json:"state"`
	ExitStatus *int            `json:"exit_status,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	ExitedAt   *time.Time      `json:"exited_at,omitempty"`
}

// ConsolePaths are persistent host-side files for one branch's init console.
// None of these files are donated into the sandbox.
type ConsolePaths struct {
	Log       string
	Lock      string
	StdinLock string
	Error     string
}

// VolumeInfo is persistent metadata for a family-local managed volume. Volume
// contents live in Sentry tmpfs and are not checkpointed to this store.
type VolumeInfo struct {
	Name      string    `json:"name"`
	Driver    string    `json:"driver"`
	CreatedAt time.Time `json:"created_at"`
}

// Prepare pulls content, creates or reuses an immutable root, and writes a new
// container bundle. Existing container names are rejected.
func (s Store) Prepare(ctx context.Context, opts PrepareOptions) (Prepared, error) {
	if err := validateName(opts.Name); err != nil {
		return Prepared{}, err
	}
	pulled, err := images.Pull(ctx, images.PullOptions{
		Address:   s.Address,
		Namespace: s.Namespace,
		Reference: opts.Reference,
		Platform:  opts.Platform,
		Unpack:    false,
	})
	if err != nil {
		return Prepared{}, err
	}
	imageInfo, rootfs, err := s.prepareImageRoot(ctx, pulled, opts.Platform)
	if err != nil {
		return Prepared{}, err
	}
	opts.SpecOptions.RootFS = rootfs
	spec, err := BuildSpec(imageInfo.Image, opts.SpecOptions)
	if err != nil {
		return Prepared{}, err
	}
	for _, volume := range opts.ManagedVolumes {
		if _, err := s.EnsureVolume(volume.Name); err != nil {
			return Prepared{}, err
		}
	}

	containersDir := filepath.Join(s.Root, "containers")
	if err := os.MkdirAll(containersDir, 0700); err != nil {
		return Prepared{}, fmt.Errorf("creating container state root: %w", err)
	}
	containerDir := filepath.Join(containersDir, opts.Name)
	if err := os.Mkdir(containerDir, 0700); err != nil {
		if os.IsExist(err) {
			return Prepared{}, fmt.Errorf("container %q already exists", opts.Name)
		}
		return Prepared{}, fmt.Errorf("creating state for container %q: %w", opts.Name, err)
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(containerDir)
		}
	}()
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.Mkdir(bundleDir, 0700); err != nil {
		return Prepared{}, fmt.Errorf("creating OCI bundle: %w", err)
	}
	if err := writeJSON(filepath.Join(bundleDir, "config.json"), spec); err != nil {
		return Prepared{}, fmt.Errorf("writing OCI config: %w", err)
	}
	metadata := ContainerInfo{
		Name:      opts.Name,
		Reference: imageInfo.Reference,
		Digest:    pulled.Digest,
		Platform:  imageInfo.Platform,
		BundleDir: bundleDir,
		RootFS:    rootfs,
		Volumes:   append([]ManagedVolume(nil), opts.ManagedVolumes...),
		State:     "created",
		CreatedAt: time.Now().UTC(),
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &metadata); err != nil {
		return Prepared{}, fmt.Errorf("writing container metadata: %w", err)
	}
	keep = true
	return Prepared{BundleDir: bundleDir, RootFS: rootfs, Image: imageInfo}, nil
}

// LoadContainer returns persistent configuration for name.
func (s Store) LoadContainer(name string) (ContainerInfo, error) {
	if err := validateName(name); err != nil {
		return ContainerInfo{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.Root, "containers", name, "container.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return ContainerInfo{}, fmt.Errorf("container %q does not exist", name)
		}
		return ContainerInfo{}, fmt.Errorf("reading container %q: %w", name, err)
	}
	var info ContainerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return ContainerInfo{}, fmt.Errorf("decoding container %q: %w", name, err)
	}
	if info.Name != name || info.BundleDir == "" {
		return ContainerInfo{}, fmt.Errorf("container %q has invalid metadata", name)
	}
	if info.State == "" {
		info.State = "created"
	}
	if _, err := os.Stat(filepath.Join(info.BundleDir, "config.json")); err != nil {
		return ContainerInfo{}, fmt.Errorf("container %q bundle is unavailable: %w", name, err)
	}
	return info, nil
}

// ListContainers returns all persistent containers ordered by name.
func (s Store) ListContainers() ([]ContainerInfo, error) {
	containersDir := filepath.Join(s.Root, "containers")
	entries, err := os.ReadDir(containersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ContainerInfo{}, nil
		}
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	containers := make([]ContainerInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := s.LoadContainer(entry.Name())
		if err != nil {
			return nil, err
		}
		containers = append(containers, info)
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	return containers, nil
}

// UpdateState records the last known base-container lifecycle state.
func (s Store) UpdateState(name, state string, exitStatus *int) error {
	info, err := s.LoadContainer(name)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	info.State = state
	info.ExitStatus = exitStatus
	switch state {
	case "running":
		info.StartedAt = &now
		info.ExitedAt = nil
	case "exited":
		info.ExitedAt = &now
	}
	path := filepath.Join(s.Root, "containers", name, "container.json")
	if err := writeJSON(path, &info); err != nil {
		return fmt.Errorf("updating container %q: %w", name, err)
	}
	return nil
}

// RemoveContainer removes persistent configuration for name. Shared image
// roots remain until image garbage collection proves that no container uses
// them.
func (s Store) RemoveContainer(name string) error {
	if _, err := s.LoadContainer(name); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.Root, "containers", name)); err != nil {
		return fmt.Errorf("removing container %q: %w", name, err)
	}
	return nil
}

// BranchConsolePaths returns persistent console paths for one branch, creating
// their private parent directory if needed.
func (s Store) BranchConsolePaths(container, branch string) (ConsolePaths, error) {
	if err := validateName(container); err != nil {
		return ConsolePaths{}, err
	}
	if err := validateObjectName("branch", branch); err != nil {
		return ConsolePaths{}, err
	}
	if _, err := s.LoadContainer(container); err != nil {
		return ConsolePaths{}, err
	}
	dir := filepath.Join(s.Root, "containers", container, "console")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ConsolePaths{}, fmt.Errorf("creating console state for %q: %w", container, err)
	}
	base := filepath.Join(dir, branch)
	return ConsolePaths{
		Log:       base + ".log",
		Lock:      base + ".lock",
		StdinLock: base + ".stdin.lock",
		Error:     base + ".error.log",
	}, nil
}

// CreateVolume creates metadata for a family-local managed tmpfs volume.
func (s Store) CreateVolume(name string) (VolumeInfo, error) {
	if err := validateVolumeName(name); err != nil {
		return VolumeInfo{}, err
	}
	volumesDir := filepath.Join(s.Root, "volumes")
	if err := os.MkdirAll(volumesDir, 0700); err != nil {
		return VolumeInfo{}, fmt.Errorf("creating volume state root: %w", err)
	}
	volumeDir := filepath.Join(volumesDir, name)
	if err := os.Mkdir(volumeDir, 0700); err != nil {
		return VolumeInfo{}, fmt.Errorf("creating volume %q: %w", name, err)
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(volumeDir)
		}
	}()
	info := VolumeInfo{Name: name, Driver: "local-tmpfs", CreatedAt: time.Now().UTC()}
	if err := writeJSON(filepath.Join(volumeDir, "volume.json"), &info); err != nil {
		return VolumeInfo{}, fmt.Errorf("writing volume %q metadata: %w", name, err)
	}
	keep = true
	return info, nil
}

// EnsureVolume returns an existing volume or creates it idempotently.
func (s Store) EnsureVolume(name string) (VolumeInfo, error) {
	info, err := s.LoadVolume(name)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return VolumeInfo{}, err
	}
	info, err = s.CreateVolume(name)
	if err == nil {
		return info, nil
	}
	if errors.Is(err, os.ErrExist) {
		return s.LoadVolume(name)
	}
	return VolumeInfo{}, err
}

// LoadVolume returns persistent managed-volume metadata.
func (s Store) LoadVolume(name string) (VolumeInfo, error) {
	if err := validateVolumeName(name); err != nil {
		return VolumeInfo{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.Root, "volumes", name, "volume.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return VolumeInfo{}, fmt.Errorf("volume %q does not exist: %w", name, err)
		}
		return VolumeInfo{}, fmt.Errorf("reading volume %q: %w", name, err)
	}
	var info VolumeInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return VolumeInfo{}, fmt.Errorf("decoding volume %q: %w", name, err)
	}
	if info.Name != name || info.Driver != "local-tmpfs" {
		return VolumeInfo{}, fmt.Errorf("volume %q has invalid metadata", name)
	}
	return info, nil
}

// ListVolumes returns managed volumes ordered by name.
func (s Store) ListVolumes() ([]VolumeInfo, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "volumes"))
	if err != nil {
		if os.IsNotExist(err) {
			return []VolumeInfo{}, nil
		}
		return nil, fmt.Errorf("listing volumes: %w", err)
	}
	volumes := make([]VolumeInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := s.LoadVolume(entry.Name())
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, info)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	return volumes, nil
}

// RemoveVolume removes an unused managed volume's metadata.
func (s Store) RemoveVolume(name string) error {
	if _, err := s.LoadVolume(name); err != nil {
		return err
	}
	containers, err := s.ListContainers()
	if err != nil {
		return err
	}
	for _, container := range containers {
		for _, volume := range container.Volumes {
			if volume.Name == name {
				return fmt.Errorf("volume %q is used by container %q", name, container.Name)
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(s.Root, "volumes", name)); err != nil {
		return fmt.Errorf("removing volume %q: %w", name, err)
	}
	return nil
}

func (s Store) prepareImageRoot(ctx context.Context, pulled images.Info, platform string) (images.RootFSInfo, string, error) {
	imagesDir := filepath.Join(s.Root, "images")
	if err := os.MkdirAll(imagesDir, 0700); err != nil {
		return images.RootFSInfo{}, "", fmt.Errorf("creating image cache: %w", err)
	}
	selectedPlatform := platform
	if selectedPlatform == "" {
		selectedPlatform = pulled.Platform
	}
	key := safeComponent(pulled.Digest + "-" + selectedPlatform)
	finalDir := filepath.Join(imagesDir, key)
	if info, err := loadImageRoot(finalDir); err == nil {
		return info, filepath.Join(finalDir, "rootfs"), nil
	}
	tempDir, err := os.MkdirTemp(imagesDir, key+".tmp-")
	if err != nil {
		return images.RootFSInfo{}, "", fmt.Errorf("creating temporary image root: %w", err)
	}
	defer os.RemoveAll(tempDir)
	rootfs := filepath.Join(tempDir, "rootfs")
	info, err := images.MaterializeRootFS(ctx, images.RootFSOptions{
		Address:   s.Address,
		Namespace: s.Namespace,
		Reference: pulled.Reference,
		Platform:  platform,
		Root:      rootfs,
	})
	if err != nil {
		return images.RootFSInfo{}, "", err
	}
	if err := writeJSON(filepath.Join(tempDir, "image.json"), &info); err != nil {
		return images.RootFSInfo{}, "", fmt.Errorf("writing image-root metadata: %w", err)
	}
	if err := os.Rename(tempDir, finalDir); err != nil {
		if existing, loadErr := loadImageRoot(finalDir); loadErr == nil {
			return existing, filepath.Join(finalDir, "rootfs"), nil
		}
		return images.RootFSInfo{}, "", fmt.Errorf("publishing image root: %w", err)
	}
	return info, filepath.Join(finalDir, "rootfs"), nil
}

func loadImageRoot(dir string) (images.RootFSInfo, error) {
	data, err := os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		return images.RootFSInfo{}, err
	}
	var info images.RootFSInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return images.RootFSInfo{}, err
	}
	if stat, err := os.Stat(filepath.Join(dir, "rootfs")); err != nil || !stat.IsDir() {
		return images.RootFSInfo{}, fmt.Errorf("cached rootfs is missing")
	}
	return info, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0600)
}

func validateName(name string) error {
	return validateObjectName("container", name)
}

func validateVolumeName(name string) error {
	return validateObjectName("volume", name)
}

func validateObjectName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name must not be empty", kind)
	}
	for i, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || (i > 0 && strings.ContainsRune("_.-", r)) {
			continue
		}
		return fmt.Errorf("invalid %s name %q", kind, name)
	}
	return nil
}

func safeComponent(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, value)
}
