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
// the TTC image materializer and the gofer serving an immutable image root.
package ownership

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// Annotation names the ownership manifest for the OCI root filesystem.
	Annotation = "dev.timetravelcontainer.rootfs.ownership"

	// ManifestName is stored beside, rather than inside, the image rootfs.
	ManifestName = "ownership.json"

	currentVersion = 1
)

// AnnotationValue returns the rootfs ownership annotation.
func AnnotationValue(annotations map[string]string) string {
	return annotations[Annotation]
}

// Owner is the guest-visible numeric owner of one image path.
type Owner struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// Manifest maps clean root-relative paths to guest-visible metadata that an
// unprivileged host cannot represent directly. The root directory is
// represented by ".".
type Manifest struct {
	Version int              `json:"version"`
	Entries map[string]Owner `json:"entries"`
	// Modes contains guest-visible Unix mode bits for immutable image paths
	// whose host backing permissions were relaxed so an unprivileged gofer can
	// serve them. Presence in this map is significant: mode 0000 is valid.
	Modes  map[string]uint32            `json:"modes,omitempty"`
	Xattrs map[string]map[string][]byte `json:"xattrs,omitempty"`
}

// NewManifest returns an empty manifest using the current format version.
func NewManifest() *Manifest {
	return &Manifest{
		Version: currentVersion,
		Entries: make(map[string]Owner),
		Modes:   make(map[string]uint32),
		Xattrs:  make(map[string]map[string][]byte),
	}
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

// LookupMode returns guest-visible Unix permission and special bits for name.
func (m *Manifest) LookupMode(name string) (uint32, bool) {
	if m == nil {
		return 0, false
	}
	clean, err := CleanPath(name)
	if err != nil {
		return 0, false
	}
	mode, ok := m.Modes[clean]
	return mode, ok
}

// LookupXattr returns a copy of a virtual extended attribute.
func (m *Manifest) LookupXattr(name, attribute string) ([]byte, bool) {
	if m == nil {
		return nil, false
	}
	clean, err := CleanPath(name)
	if err != nil {
		return nil, false
	}
	attributes := m.Xattrs[clean]
	value, ok := attributes[attribute]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), value...), true
}

// ListXattrs returns the virtual extended-attribute names for name.
func (m *Manifest) ListXattrs(name string) []string {
	if m == nil {
		return nil
	}
	clean, err := CleanPath(name)
	if err != nil {
		return nil
	}
	attributes := m.Xattrs[clean]
	result := make([]string, 0, len(attributes))
	for attribute := range attributes {
		result = append(result, attribute)
	}
	sort.Strings(result)
	return result
}

// Read loads and validates a manifest.
func Read(name string) (*Manifest, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return ReadFrom(name, file)
}

// ReadFrom loads and validates a manifest from r. name identifies the source
// in validation errors.
func ReadFrom(name string, r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading ownership manifest %q: %w", name, err)
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
	for entry, mode := range manifest.Modes {
		clean, err := CleanPath(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid ownership manifest %q modes: %w", name, err)
		}
		if clean != entry {
			return nil, fmt.Errorf("ownership manifest %q contains non-canonical mode path %q", name, entry)
		}
		if _, ok := manifest.Entries[entry]; !ok {
			return nil, fmt.Errorf("ownership manifest %q contains a mode for unknown path %q", name, entry)
		}
		if mode&^uint32(07777) != 0 {
			return nil, fmt.Errorf("ownership manifest %q contains invalid mode %#o for %q", name, mode, entry)
		}
	}
	for entry, attributes := range manifest.Xattrs {
		clean, err := CleanPath(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid ownership manifest %q xattrs: %w", name, err)
		}
		if clean != entry {
			return nil, fmt.Errorf("ownership manifest %q contains non-canonical xattr path %q", name, entry)
		}
		if _, ok := manifest.Entries[entry]; !ok {
			return nil, fmt.Errorf("ownership manifest %q contains xattrs for unknown path %q", name, entry)
		}
		for attribute := range attributes {
			if attribute == "" || len(attribute) > 255 || strings.ContainsRune(attribute, 0) {
				return nil, fmt.Errorf("ownership manifest %q contains invalid xattr name %q", name, attribute)
			}
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
