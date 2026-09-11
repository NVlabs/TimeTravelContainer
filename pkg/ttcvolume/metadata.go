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

package ttcvolume

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

const (
	metadataSetInode    = "set-inode"
	metadataDeleteInode = "delete-inode"
	metadataSetEntry    = "set-entry"
	metadataDeleteEntry = "delete-entry"

	inodeReservationSize = 4096
)

type metadataEntryKey struct {
	Parent uint64
	Name   string
}

type metadataOp struct {
	Kind   string          `json:"kind"`
	Inode  *Inode          `json:"inode,omitempty"`
	Parent uint64          `json:"parent,omitempty"`
	Entry  *DirectoryEntry `json:"entry,omitempty"`
	Number uint64          `json:"number,omitempty"`
	Name   string          `json:"name,omitempty"`
}

type branchMetadata struct {
	mu                sync.RWMutex
	branch            Branch
	inodes            map[uint64]Inode
	entries           map[metadataEntryKey]DirectoryEntry
	directories       map[uint64]map[string]DirectoryEntry
	loadedDirectories map[uint64]bool
	complete          bool
}

func newBranchMetadata(branch Branch) *branchMetadata {
	return &branchMetadata{
		branch: branch, inodes: make(map[uint64]Inode),
		entries: make(map[metadataEntryKey]DirectoryEntry), directories: make(map[uint64]map[string]DirectoryEntry),
		loadedDirectories: make(map[uint64]bool),
	}
}

func normalizeBranchLayers(branch *Branch) error {
	if _, err := ttcsnapshot.ParseObjectID(branch.Root); err != nil {
		return fmt.Errorf("branch %q has invalid snapshot graph root %q: %w", branch.ID, branch.Root, err)
	}
	if branch.WorkingSnapshot == 0 {
		return fmt.Errorf("branch %q has no working snapshot", branch.ID)
	}
	return nil
}

func graphRootName(branchID string) string {
	return "filesystem/" + branchID
}

func graphBaseRootName(branchID string) string {
	return "filesystem-base/" + branchID
}

func graphConstructionPin(id ttcsnapshot.ObjectID) string {
	return "construct/" + id.String()
}

func (r *Repository) metadataForBranch(branch Branch) (*branchMetadata, error) {
	if err := normalizeBranchLayers(&branch); err != nil {
		return nil, err
	}
	if err := r.graph.Reload(); err != nil {
		return nil, err
	}
	root, ok := r.graph.Root(graphRootName(branch.ID))
	if !ok {
		return nil, fmt.Errorf("branch %q has no snapshot graph root", branch.ID)
	}
	// The graph transaction precedes the branch pointer rename. Reading the
	// graph root here makes the two-file publication appear atomically at the
	// graph commit point, including to a reader in the small rename window.
	branch.Root = root.String()
	r.metadataMu.Lock()
	defer r.metadataMu.Unlock()
	if r.metadata == nil {
		r.metadata = make(map[string]*branchMetadata)
	}
	if state := r.metadata[branch.ID]; state != nil {
		state.mu.RLock()
		sameRoot := state.branch.Root == branch.Root
		state.mu.RUnlock()
		if sameRoot {
			state.mu.Lock()
			state.branch = branch
			state.mu.Unlock()
			return state, nil
		}
	}
	state := newBranchMetadata(branch)
	r.metadata[branch.ID] = state
	return state, nil
}

func (r *Repository) completeMetadata(state *branchMetadata) error {
	state.mu.RLock()
	if state.complete {
		state.mu.RUnlock()
		return nil
	}
	branch := state.branch
	state.mu.RUnlock()
	root, err := ttcsnapshot.ParseObjectID(branch.Root)
	if err != nil {
		return err
	}
	inodes, entries, err := r.graph.FilesystemRecords(root)
	if err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.branch.Root != branch.Root {
		return errors.New("filesystem root changed while materializing metadata")
	}
	state.inodes = make(map[uint64]Inode, len(inodes))
	state.entries = make(map[metadataEntryKey]DirectoryEntry, len(entries))
	state.directories = make(map[uint64]map[string]DirectoryEntry)
	state.loadedDirectories = make(map[uint64]bool)
	for _, record := range inodes {
		inode := inodeFromGraph(record)
		state.inodes[inode.Number] = inode
	}
	for _, record := range entries {
		entry := DirectoryEntry{Name: record.Name, Inode: record.Inode}
		key := metadataEntryKey{Parent: record.Parent, Name: record.Name}
		state.entries[key] = entry
		directory := state.directories[record.Parent]
		if directory == nil {
			directory = make(map[string]DirectoryEntry)
			state.directories[record.Parent] = directory
		}
		directory[record.Name] = entry
		state.loadedDirectories[record.Parent] = true
	}
	for _, inode := range state.inodes {
		if inode.Type == "directory" {
			state.loadedDirectories[inode.Number] = true
		}
	}
	state.complete = true
	return nil
}

func (r *Repository) commitMetadata(branch *Branch, state *branchMetadata, ops []metadataOp) error {
	if len(ops) == 0 {
		return nil
	}
	graphOps := make([]ttcsnapshot.FilesystemOperation, 0, len(ops))
	for _, op := range ops {
		switch op.Kind {
		case metadataSetInode:
			if op.Inode == nil {
				return errors.New("invalid set-inode operation")
			}
			record, err := inodeToGraph(*op.Inode)
			if err != nil {
				return err
			}
			if !record.File.IsZero() && uint64(record.File.Snapshot) == branch.WorkingSnapshot {
				if err := r.graph.UpdateWorkingBulkLength(record.File, uint64(max(op.Inode.Size, 0))); err != nil {
					return err
				}
			}
			graphOps = append(graphOps, ttcsnapshot.FilesystemOperation{PutInode: &record})
		case metadataDeleteInode:
			graphOps = append(graphOps, ttcsnapshot.FilesystemOperation{DeleteInode: op.Number})
		case metadataSetEntry:
			if op.Entry == nil {
				return errors.New("invalid set-entry operation")
			}
			record := ttcsnapshot.DirectoryEntryRecord{Parent: op.Parent, Name: op.Entry.Name, Inode: op.Entry.Inode}
			graphOps = append(graphOps, ttcsnapshot.FilesystemOperation{PutEntry: &record})
		case metadataDeleteEntry:
			record := ttcsnapshot.DirectoryEntryRecord{Parent: op.Parent, Name: op.Name}
			graphOps = append(graphOps, ttcsnapshot.FilesystemOperation{DeleteEntry: &record})
		default:
			return fmt.Errorf("unknown metadata operation %q", op.Kind)
		}
	}
	var releasePins []string
	for _, op := range ops {
		if op.Inode != nil && op.Inode.Object != "" {
			if id, err := ttcsnapshot.ParseObjectID(op.Inode.Object); err == nil {
				pin := graphConstructionPin(id)
				if r.graph.HasPin(pin) {
					releasePins = append(releasePins, pin)
				}
			}
		}
	}
	root, err := r.graph.ApplyFilesystemAndReleasePins(ttcsnapshot.SnapshotID(branch.WorkingSnapshot), graphRootName(branch.ID), graphOps, releasePins)
	if err != nil {
		return err
	}
	branch.Root = root.String()
	state.mu.Lock()
	if err := applyMetadataOps(state, ops); err != nil {
		state.mu.Unlock()
		return err
	}
	state.branch = *branch
	state.mu.Unlock()
	r.dirtyMu.Lock()
	if r.dirtyBranches == nil {
		r.dirtyBranches = make(map[string]*branchMetadata)
	}
	r.dirtyBranches[branch.ID] = state
	r.dirtyMu.Unlock()
	return nil
}

func inodeToGraph(inode Inode) (ttcsnapshot.InodeRecord, error) {
	record := ttcsnapshot.InodeRecord{
		Number: inode.Number, Type: inode.Type, Mode: inode.Mode, UID: inode.UID, GID: inode.GID,
		Nlink: inode.Nlink, Size: inode.Size, Target: inode.Target, DeviceMajor: inode.DevMajor,
		DeviceMinor: inode.DevMinor, AtimeNsec: inode.AtimeNsec, BtimeNsec: inode.BtimeNsec,
		CtimeNsec: inode.CtimeNsec, MtimeNsec: inode.MtimeNsec,
	}
	if inode.Object != "" {
		id, err := ttcsnapshot.ParseObjectID(inode.Object)
		if err != nil || id.Kind != ttcsnapshot.ObjectKindFile {
			return record, fmt.Errorf("inode %d has invalid whole-file object %q", inode.Number, inode.Object)
		}
		record.File = id
	}
	if len(inode.Xattrs) != 0 {
		record.Xattrs = make(map[string][]byte, len(inode.Xattrs))
		for name, value := range inode.Xattrs {
			record.Xattrs[name] = []byte(value)
		}
	}
	return record, nil
}

func inodeFromGraph(record ttcsnapshot.InodeRecord) Inode {
	inode := Inode{
		Number: record.Number, Type: record.Type, Mode: record.Mode, UID: record.UID, GID: record.GID,
		Nlink: record.Nlink, Size: record.Size, Target: record.Target, DevMajor: record.DeviceMajor,
		DevMinor: record.DeviceMinor, AtimeNsec: record.AtimeNsec, BtimeNsec: record.BtimeNsec,
		CtimeNsec: record.CtimeNsec, MtimeNsec: record.MtimeNsec,
	}
	if !record.File.IsZero() {
		inode.Object = record.File.String()
		inode.DataGeneration = uint64(record.File.Snapshot)
	}
	if len(record.Xattrs) != 0 {
		inode.Xattrs = make(map[string]string, len(record.Xattrs))
		for name, value := range record.Xattrs {
			inode.Xattrs[name] = string(value)
		}
	}
	return inode
}

func applyMetadataOps(state *branchMetadata, ops []metadataOp) error {
	for _, op := range ops {
		switch op.Kind {
		case metadataSetInode:
			if op.Inode == nil || op.Inode.Number == 0 {
				return errors.New("invalid set-inode operation")
			}
			state.inodes[op.Inode.Number] = *op.Inode
			if state.complete && op.Inode.Type == "directory" {
				state.loadedDirectories[op.Inode.Number] = true
				if state.directories[op.Inode.Number] == nil {
					state.directories[op.Inode.Number] = make(map[string]DirectoryEntry)
				}
			}
		case metadataDeleteInode:
			if op.Number == 0 {
				return errors.New("invalid delete-inode operation")
			}
			delete(state.inodes, op.Number)
			delete(state.directories, op.Number)
			delete(state.loadedDirectories, op.Number)
		case metadataSetEntry:
			if op.Entry == nil || op.Parent == 0 || validateEntryName(op.Entry.Name) != nil || op.Entry.Inode == 0 {
				return errors.New("invalid set-entry operation")
			}
			key := metadataEntryKey{Parent: op.Parent, Name: op.Entry.Name}
			state.entries[key] = *op.Entry
			if state.loadedDirectories[op.Parent] {
				directory := state.directories[op.Parent]
				if directory == nil {
					directory = make(map[string]DirectoryEntry)
					state.directories[op.Parent] = directory
				}
				directory[op.Entry.Name] = *op.Entry
			}
		case metadataDeleteEntry:
			if op.Parent == 0 || validateEntryName(op.Name) != nil {
				return errors.New("invalid delete-entry operation")
			}
			delete(state.entries, metadataEntryKey{Parent: op.Parent, Name: op.Name})
			if directory := state.directories[op.Parent]; directory != nil {
				delete(directory, op.Name)
				if len(directory) == 0 {
					delete(state.directories, op.Parent)
				}
			}
		default:
			return fmt.Errorf("unknown metadata operation %q", op.Kind)
		}
	}
	return nil
}

func (r *Repository) metadataInode(state *branchMetadata, ino uint64) (Inode, error) {
	state.mu.RLock()
	inode, ok := state.inodes[ino]
	branch := state.branch
	state.mu.RUnlock()
	if ok {
		return inode, nil
	}
	root, err := ttcsnapshot.ParseObjectID(branch.Root)
	if err != nil {
		return Inode{}, err
	}
	record, ok, err := r.graph.FilesystemInode(root, ino)
	if err != nil {
		return Inode{}, err
	}
	if !ok {
		return Inode{}, fs.ErrNotExist
	}
	inode = inodeFromGraph(record)
	state.mu.Lock()
	if state.branch.Root == branch.Root {
		state.inodes[ino] = inode
	}
	state.mu.Unlock()
	return inode, nil
}

func (r *Repository) metadataLookup(state *branchMetadata, parent uint64, name string) (uint64, error) {
	state.mu.RLock()
	entry, ok := state.entries[metadataEntryKey{Parent: parent, Name: name}]
	branch := state.branch
	state.mu.RUnlock()
	if ok {
		return entry.Inode, nil
	}
	root, err := ttcsnapshot.ParseObjectID(branch.Root)
	if err != nil {
		return 0, err
	}
	record, ok, err := r.graph.FilesystemLookup(root, parent, name)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fs.ErrNotExist
	}
	entry = DirectoryEntry{Name: record.Name, Inode: record.Inode}
	state.mu.Lock()
	if state.branch.Root == branch.Root {
		state.entries[metadataEntryKey{Parent: parent, Name: name}] = entry
	}
	state.mu.Unlock()
	return entry.Inode, nil
}

func (r *Repository) metadataDirectory(state *branchMetadata, parent uint64) ([]DirectoryEntry, error) {
	state.mu.RLock()
	loaded := state.loadedDirectories[parent]
	branch := state.branch
	if loaded {
		directory := state.directories[parent]
		entries := make([]DirectoryEntry, 0, len(directory))
		for _, entry := range directory {
			entries = append(entries, entry)
		}
		state.mu.RUnlock()
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		return entries, nil
	}
	state.mu.RUnlock()
	root, err := ttcsnapshot.ParseObjectID(branch.Root)
	if err != nil {
		return nil, err
	}
	records, err := r.graph.FilesystemDirectory(root, parent)
	if err != nil {
		return nil, err
	}
	directory := make(map[string]DirectoryEntry, len(records))
	entries := make([]DirectoryEntry, 0, len(records))
	for _, record := range records {
		entry := DirectoryEntry{Name: record.Name, Inode: record.Inode}
		directory[entry.Name] = entry
		entries = append(entries, entry)
	}
	state.mu.Lock()
	if state.branch.Root == branch.Root {
		state.directories[parent] = directory
		state.loadedDirectories[parent] = true
		for _, entry := range entries {
			state.entries[metadataEntryKey{Parent: parent, Name: entry.Name}] = entry
		}
	}
	state.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (r *Repository) allocateInodeLocked() (uint64, error) {
	if r.allocNext == r.allocEnd {
		// Refresh only when reserving a new lease. Ordinary metadata operations
		// do not need to decode the repository-wide allocator record.
		if err := r.readJSON("volume.json", &r.info); err != nil {
			return 0, err
		}
		start := r.info.NextInode
		if start <= rootInode {
			start = rootInode + 1
		}
		end := start + inodeReservationSize
		if end < start {
			return 0, syscall.ENOSPC
		}
		r.info.NextInode = end
		if err := r.writeJSONAtomic("volume.json", r.info); err != nil {
			return 0, err
		}
		r.allocNext, r.allocEnd = start, end
	}
	ino := r.allocNext
	r.allocNext++
	return ino, nil
}

func (r *Repository) markDirtyFile(name string) {
	r.dirtyMu.Lock()
	defer r.dirtyMu.Unlock()
	if r.dirtyFiles == nil {
		r.dirtyFiles = make(map[string]struct{})
	}
	r.dirtyFiles[name] = struct{}{}
}

func (r *Repository) markDirtyDirectory(name string) {
	r.dirtyMu.Lock()
	defer r.dirtyMu.Unlock()
	if r.dirtyDirs == nil {
		r.dirtyDirs = make(map[string]struct{})
	}
	r.dirtyDirs[name] = struct{}{}
}

func (r *Repository) mkdir(name string, mode fs.FileMode) error {
	if err := validateInternalPath(name); err != nil {
		return err
	}
	r.dirtyMu.Lock()
	if _, ok := r.knownDirs[name]; ok {
		r.dirtyMu.Unlock()
		return nil
	}
	r.dirtyMu.Unlock()
	err := unix.Mkdirat(int(r.dir.Fd()), name, uint32(mode.Perm()))
	created := err == nil
	if err != nil && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	r.dirtyMu.Lock()
	if r.knownDirs == nil {
		r.knownDirs = make(map[string]struct{})
	}
	r.knownDirs[name] = struct{}{}
	if created {
		if r.dirtyDirs == nil {
			r.dirtyDirs = make(map[string]struct{})
		}
		r.dirtyDirs[filepath.Dir(name)] = struct{}{}
	}
	r.dirtyMu.Unlock()
	return nil
}

func (r *Repository) mkdirAll(name string, mode fs.FileMode) error {
	if err := validateInternalPath(name); err != nil {
		return err
	}
	current := ""
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}
		if err := r.mkdir(current, mode); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) objectPath(id string) string {
	parsed, err := ttcsnapshot.ParseObjectID(id)
	if err != nil || r.data == nil {
		return filepath.Join("data", "invalid")
	}
	path, err := r.data.Path(parsed)
	if err != nil {
		return filepath.Join("data", "invalid")
	}
	if r.path != "" {
		if relative, err := filepath.Rel(r.path, path); err == nil {
			return relative
		}
	}
	return path
}

func (r *Repository) openObject(id string, flags int) (*os.File, error) {
	parsed, err := ttcsnapshot.ParseObjectID(id)
	if err != nil || parsed.Kind != ttcsnapshot.ObjectKindFile {
		return nil, fmt.Errorf("invalid whole-file object ID %q", id)
	}
	return r.data.OpenFile(parsed, flags, -1)
}
