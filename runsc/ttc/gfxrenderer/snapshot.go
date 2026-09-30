// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// A renderer's snapshot is an archive of named entries: the files of
// gfxstream's snapshot, and what gfxstream does not save, the renderer's
// resources and the backings of its classic resources, which the renderer
// allocates and the guest maps.

// archiveMagic starts an archive.
const archiveMagic = "TTCGFXS1"

const (
	// archiveResources is the entry that lists the renderer's resources:
	// for each, its ID and whether it is a blob.
	archiveResources = "ttc-resources"
	// archiveBackingPrefix, and a resource's ID, name the entry of a classic
	// resource's backing.
	archiveBackingPrefix = "ttc-backing."
)

// archiveEntry is an entry of an archive.
type archiveEntry struct {
	name string
	data []byte
}

// encodeArchive returns an archive of entries.
func encodeArchive(entries []archiveEntry) []byte {
	size := len(archiveMagic) + 4
	for _, e := range entries {
		size += 4 + len(e.name) + 8 + len(e.data)
	}
	b := make([]byte, 0, size)
	b = append(b, archiveMagic...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(entries)))
	for _, e := range entries {
		b = binary.LittleEndian.AppendUint32(b, uint32(len(e.name)))
		b = append(b, e.name...)
		b = binary.LittleEndian.AppendUint64(b, uint64(len(e.data)))
		b = append(b, e.data...)
	}
	return b
}

// decodeArchive returns an archive's entries, whose data alias b. An entry's
// name is a file name, and unique.
func decodeArchive(b []byte) ([]archiveEntry, error) {
	if !strings.HasPrefix(string(b[:min(len(b), len(archiveMagic))]), archiveMagic) {
		return nil, errors.New("not a renderer snapshot")
	}
	b = b[len(archiveMagic):]
	if len(b) < 4 {
		return nil, errors.New("truncated renderer snapshot")
	}
	count := binary.LittleEndian.Uint32(b)
	b = b[4:]
	var entries []archiveEntry
	names := make(map[string]bool)
	for i := uint32(0); i < count; i++ {
		if len(b) < 4 {
			return nil, errors.New("truncated renderer snapshot")
		}
		n := uint64(binary.LittleEndian.Uint32(b))
		b = b[4:]
		if uint64(len(b)) < n+8 {
			return nil, errors.New("truncated renderer snapshot")
		}
		name := string(b[:n])
		b = b[n:]
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || names[name] {
			return nil, fmt.Errorf("renderer snapshot entry %q", name)
		}
		names[name] = true
		size := binary.LittleEndian.Uint64(b)
		b = b[8:]
		if uint64(len(b)) < size {
			return nil, errors.New("truncated renderer snapshot")
		}
		entries = append(entries, archiveEntry{name: name, data: b[:size:size]})
		b = b[size:]
	}
	if len(b) != 0 {
		return nil, errors.New("renderer snapshot has trailing bytes")
	}
	return entries, nil
}

// archivedResource is a resource in a snapshot.
type archivedResource struct {
	id   uint32
	blob bool
}

// encodeResources returns the entry of archiveResources.
func encodeResources(resources []archivedResource) []byte {
	slices.SortFunc(resources, func(a, b archivedResource) int { return int(a.id) - int(b.id) })
	b := make([]byte, 0, 5*len(resources))
	for _, r := range resources {
		b = binary.LittleEndian.AppendUint32(b, r.id)
		blob := byte(0)
		if r.blob {
			blob = 1
		}
		b = append(b, blob)
	}
	return b
}

// decodeResources decodes the entry of archiveResources.
func decodeResources(b []byte) ([]archivedResource, error) {
	if len(b)%5 != 0 {
		return nil, errors.New("malformed resource list")
	}
	resources := make([]archivedResource, 0, len(b)/5)
	for ; len(b) != 0; b = b[5:] {
		resources = append(resources, archivedResource{id: binary.LittleEndian.Uint32(b), blob: b[4] != 0})
	}
	return resources, nil
}
