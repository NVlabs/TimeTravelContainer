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

// Package ownership defines the rootless image ownership metadata shared by
// the TTOS image materializer and the gofer serving an immutable image root.
package ownership

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// Annotation names the ownership manifest for the OCI root filesystem.
	Annotation = "dev.timetravelos.rootfs.ownership"

	// ManifestName is stored beside, rather than inside, the image rootfs.
	ManifestName = "ownership.json"

	currentVersion = 1
)

// Owner is the guest-visible numeric owner of one image path.
type Owner struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// Manifest maps clean root-relative paths to guest-visible owners. The root
// directory is represented by ".".
type Manifest struct {
	Version int              `json:"version"`
	Entries map[string]Owner `json:"entries"`
}

// NewManifest returns an empty manifest using the current format version.
func NewManifest() *Manifest {
	return &Manifest{Version: currentVersion, Entries: make(map[string]Owner)}
}

// ManifestPath returns the metadata path associated with rootfs.
func ManifestPath(rootfs string) string {
	return filepath.Join(filepath.Dir(rootfs), ManifestName)
}

// CleanPath validates and canonicalizes a root-relative manifest path.
func CleanPath(name string) (string, error) {
	name = filepath.ToSlash(name)
	clean := path.Clean(name)
	clean = strings.TrimPrefix(clean, "./")
	if clean == "" {
		clean = "."
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("ownership path %q escapes the root", name)
	}
	return clean, nil
}

// Lookup returns the virtual owner for name.
func (m *Manifest) Lookup(name string) (Owner, bool) {
	if m == nil {
		return Owner{}, false
	}
	clean, err := CleanPath(name)
	if err != nil {
		return Owner{}, false
	}
	owner, ok := m.Entries[clean]
	return owner, ok
}

// Read loads and validates a manifest.
func Read(name string) (*Manifest, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decoding ownership manifest %q: %w", name, err)
	}
	if manifest.Version != currentVersion {
		return nil, fmt.Errorf("ownership manifest %q has version %d, want %d", name, manifest.Version, currentVersion)
	}
	if manifest.Entries == nil {
		return nil, fmt.Errorf("ownership manifest %q has no entries", name)
	}
	for entry := range manifest.Entries {
		clean, err := CleanPath(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid ownership manifest %q: %w", name, err)
		}
		if clean != entry {
			return nil, fmt.Errorf("ownership manifest %q contains non-canonical path %q", name, entry)
		}
	}
	if _, ok := manifest.Entries["."]; !ok {
		return nil, fmt.Errorf("ownership manifest %q does not describe the root", name)
	}
	return &manifest, nil
}

// Write atomically writes manifest to name.
func Write(name string, manifest *Manifest) error {
	if manifest == nil {
		return fmt.Errorf("ownership manifest must not be nil")
	}
	manifest.Version = currentVersion
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding ownership manifest: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(name), ".ownership-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, name)
}
