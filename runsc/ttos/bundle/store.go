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
	"fmt"
	"os"
	"path/filepath"
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

type containerMetadata struct {
	Name      string    `json:"name"`
	Reference string    `json:"reference"`
	Digest    string    `json:"digest"`
	Platform  string    `json:"platform"`
	BundleDir string    `json:"bundle_dir"`
	RootFS    string    `json:"rootfs"`
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
	metadata := containerMetadata{
		Name:      opts.Name,
		Reference: imageInfo.Reference,
		Digest:    pulled.Digest,
		Platform:  imageInfo.Platform,
		BundleDir: bundleDir,
		RootFS:    rootfs,
		CreatedAt: time.Now().UTC(),
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &metadata); err != nil {
		return Prepared{}, fmt.Errorf("writing container metadata: %w", err)
	}
	keep = true
	return Prepared{BundleDir: bundleDir, RootFS: rootfs, Image: imageInfo}, nil
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
	if name == "" {
		return fmt.Errorf("container name must not be empty")
	}
	for i, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || (i > 0 && strings.ContainsRune("_.-", r)) {
			continue
		}
		return fmt.Errorf("invalid container name %q", name)
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
