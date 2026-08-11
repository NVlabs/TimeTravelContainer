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
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	metadataSetInode    = "set-inode"
	metadataDeleteInode = "delete-inode"
	metadataSetEntry    = "set-entry"
	metadataDeleteEntry = "delete-entry"

	metadataJournalHeaderSize = 16
	maxMetadataRecordSize     = 4 << 20
	inodeReservationSize      = 4096
)

var metadataJournalMagic = [4]byte{'T', 'T', 'C', 'J'}

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

type metadataTransaction struct {
	Sequence uint64       `json:"sequence"`
	Ops      []metadataOp `json:"ops"`
}

type branchMetadata struct {
	mu           sync.RWMutex
	branch       Branch
	layers       []string
	inodes       map[uint64]Inode
	entries      map[metadataEntryKey]DirectoryEntry
	directories  map[uint64]map[string]DirectoryEntry
	nextSequence uint64
	layerSizes   map[string]int64
}

func newBranchMetadata(branch Branch) *branchMetadata {
	return &branchMetadata{
		branch: branch, inodes: make(map[uint64]Inode),
		entries: make(map[metadataEntryKey]DirectoryEntry), directories: make(map[uint64]map[string]DirectoryEntry),
		// Sequence numbers are local to a layer and start at one.
		nextSequence: 1, layerSizes: make(map[string]int64),
	}
}

type metadataLayer struct {
	// Parent links immutable metadata layers into a persistent sharing DAG. It
	// is not a container-branch parent: every branch independently names one
	// Root, and deleting or rehydrating any branch never traverses UI lineage.
	Parent string `json:"parent,omitempty"`
}

func (r *Repository) createMetadataLayer(parent string) (string, error) {
	if parent != "" && !validID(parent) {
		return "", fmt.Errorf("invalid parent metadata layer %q", parent)
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	name := filepath.Join("layers", id)
	if err := r.mkdir(name, 0700); err != nil {
		return "", err
	}
	if err := r.writeJSONAtomic(filepath.Join(name, "layer.json"), metadataLayer{Parent: parent}); err != nil {
		return "", err
	}
	journal, err := r.openFile(filepath.Join(name, "journal"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	if err := journal.Close(); err != nil {
		return "", err
	}
	r.markDirtyDirectory(name)
	r.markDirtyDirectory("layers")
	return id, nil
}

func normalizeBranchLayers(branch *Branch) error {
	if !validID(branch.Root) {
		return fmt.Errorf("branch %q has invalid metadata root %q", branch.ID, branch.Root)
	}
	return nil
}

func (r *Repository) metadataLayers(root string) ([]string, error) {
	seen := make(map[string]struct{})
	var reversed []string
	for root != "" {
		if !validID(root) {
			return nil, fmt.Errorf("invalid metadata layer %q", root)
		}
		if _, ok := seen[root]; ok {
			return nil, fmt.Errorf("metadata layer cycle at %q", root)
		}
		seen[root] = struct{}{}
		reversed = append(reversed, root)
		var layer metadataLayer
		if err := r.readJSON(filepath.Join("layers", root, "layer.json"), &layer); err != nil {
			return nil, err
		}
		root = layer.Parent
	}
	layers := make([]string, len(reversed))
	for i := range reversed {
		layers[len(reversed)-1-i] = reversed[i]
	}
	return layers, nil
}

func (r *Repository) metadataForBranch(branch Branch) (*branchMetadata, error) {
	if err := normalizeBranchLayers(&branch); err != nil {
		return nil, err
	}
	r.metadataMu.Lock()
	defer r.metadataMu.Unlock()
	if r.metadata == nil {
		r.metadata = make(map[string]*branchMetadata)
	}
	if state := r.metadata[branch.ID]; state != nil {
		state.mu.RLock()
		sameRoot := state.branch.Root == branch.Root
		knownSize := state.layerSizes[branch.Root]
		layers := append([]string(nil), state.layers...)
		state.mu.RUnlock()
		if sameRoot {
			current, err := r.metadataLayerSizes([]string{branch.Root})
			if err != nil {
				return nil, err
			}
			if current[branch.Root] == knownSize {
				state.mu.Lock()
				state.branch = branch
				state.mu.Unlock()
				return state, nil
			}
			return r.reloadBranchMetadata(branch, layers)
		}
	}
	layers, err := r.metadataLayers(branch.Root)
	if err != nil {
		return nil, err
	}
	return r.reloadBranchMetadata(branch, layers)
}

func (r *Repository) reloadBranchMetadata(branch Branch, layers []string) (*branchMetadata, error) {
	var replacedLayers []string
	if previous := r.metadata[branch.ID]; previous != nil {
		previous.mu.RLock()
		replacedLayers = append(replacedLayers, previous.layers...)
		previous.mu.RUnlock()
	}
	state := newBranchMetadata(branch)
	state.layers = layers
	for _, layer := range layers {
		size, sequence, err := r.replayMetadataLayer(layer, state)
		if err != nil {
			return nil, fmt.Errorf("replaying metadata layer %q: %w", layer, err)
		}
		state.layerSizes[layer] = size
		if sequence >= state.nextSequence {
			state.nextSequence = sequence + 1
		}
	}
	if state.nextSequence == 0 {
		state.nextSequence = 1
	}
	r.metadata[branch.ID] = state
	r.pruneUnreferencedLayerCachesLocked(replacedLayers)
	return state, nil
}

// pruneUnreferencedLayerCachesLocked drops process-local state for metadata
// layers that no decoded branch still uses. r.metadataMu must be held. Closing
// a journal for a layer used only by an uncached branch is harmless: a later
// mutation reopens it after reconstructing that branch from its pointer.
func (r *Repository) pruneUnreferencedLayerCachesLocked(candidates []string) {
	if len(candidates) == 0 {
		return
	}
	live := make(map[string]struct{})
	for _, state := range r.metadata {
		state.mu.RLock()
		for _, layer := range state.layers {
			live[layer] = struct{}{}
		}
		state.mu.RUnlock()
	}
	stale := make(map[string]struct{})
	for _, layer := range candidates {
		if _, ok := live[layer]; !ok {
			stale[layer] = struct{}{}
		}
	}
	if len(stale) == 0 {
		return
	}
	r.journalMu.Lock()
	for layer := range stale {
		if journal := r.journals[layer]; journal != nil {
			_ = journal.Close()
			delete(r.journals, layer)
		}
	}
	r.journalMu.Unlock()
	r.dirtyMu.Lock()
	for name := range r.dirtyFiles {
		if pathUsesMetadataLayer(name, stale) {
			delete(r.dirtyFiles, name)
		}
	}
	for name := range r.dirtyDirs {
		if pathUsesMetadataLayer(name, stale) {
			delete(r.dirtyDirs, name)
		}
	}
	for name := range r.knownDirs {
		if pathUsesMetadataLayer(name, stale) {
			delete(r.knownDirs, name)
		}
	}
	r.dirtyMu.Unlock()
}

func pathUsesMetadataLayer(name string, layers map[string]struct{}) bool {
	const prefix = "layers/"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rest := strings.TrimPrefix(name, prefix)
	layer, _, _ := strings.Cut(rest, "/")
	_, ok := layers[layer]
	return ok
}

func (r *Repository) metadataLayerSizes(layers []string) (map[string]int64, error) {
	sizes := make(map[string]int64, len(layers))
	for _, layer := range layers {
		file, err := r.metadataJournal(layer)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		sizes[layer] = info.Size()
	}
	return sizes, nil
}

func (r *Repository) replayMetadataLayer(layer string, state *branchMetadata) (int64, uint64, error) {
	file, err := r.metadataJournal(layer)
	if err != nil {
		return 0, 0, err
	}
	var offset int64
	var sequence uint64
	for {
		transaction, next, err := readMetadataTransaction(file, offset)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return offset, sequence, nil
		}
		if err != nil {
			return 0, 0, err
		}
		if transaction.Sequence <= sequence {
			return 0, 0, fmt.Errorf("non-increasing transaction sequence %d", transaction.Sequence)
		}
		if err := applyMetadataOps(state, transaction.Ops); err != nil {
			return 0, 0, err
		}
		sequence = transaction.Sequence
		offset = next
	}
}

func (r *Repository) metadataJournal(layer string) (*os.File, error) {
	if !validID(layer) {
		return nil, fmt.Errorf("invalid metadata layer %q", layer)
	}
	r.journalMu.Lock()
	defer r.journalMu.Unlock()
	if journal := r.journals[layer]; journal != nil {
		return journal, nil
	}
	journal, err := r.openFile(filepath.Join("layers", layer, "journal"), os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if r.journals == nil {
		r.journals = make(map[string]*os.File)
	}
	r.journals[layer] = journal
	return journal, nil
}

func readMetadataTransaction(file *os.File, offset int64) (metadataTransaction, int64, error) {
	var header [metadataJournalHeaderSize]byte
	if _, err := file.ReadAt(header[:], offset); err != nil {
		return metadataTransaction{}, offset, err
	}
	if !bytes.Equal(header[:4], metadataJournalMagic[:]) {
		return metadataTransaction{}, offset, fmt.Errorf("invalid metadata journal magic at offset %d", offset)
	}
	length := binary.LittleEndian.Uint32(header[4:8])
	if length == 0 || length > maxMetadataRecordSize {
		return metadataTransaction{}, offset, fmt.Errorf("invalid metadata transaction length %d at offset %d", length, offset)
	}
	payload := make([]byte, length)
	if _, err := file.ReadAt(payload, offset+metadataJournalHeaderSize); err != nil {
		return metadataTransaction{}, offset, err
	}
	if got, want := crc32.ChecksumIEEE(payload), binary.LittleEndian.Uint32(header[8:12]); got != want {
		return metadataTransaction{}, offset, fmt.Errorf("metadata transaction checksum mismatch at offset %d", offset)
	}
	var transaction metadataTransaction
	if err := json.Unmarshal(payload, &transaction); err != nil {
		return metadataTransaction{}, offset, err
	}
	if transaction.Sequence == 0 || len(transaction.Ops) == 0 {
		return metadataTransaction{}, offset, errors.New("empty metadata transaction")
	}
	return transaction, offset + metadataJournalHeaderSize + int64(length), nil
}

func (r *Repository) commitMetadata(branch *Branch, state *branchMetadata, ops []metadataOp) error {
	if len(ops) == 0 {
		return nil
	}
	state.mu.RLock()
	sequence := state.nextSequence
	durableEnd := state.layerSizes[branch.Root]
	state.mu.RUnlock()
	transaction := metadataTransaction{Sequence: sequence, Ops: ops}
	payload, err := json.Marshal(transaction)
	if err != nil {
		return err
	}
	if len(payload) > maxMetadataRecordSize {
		return syscall.E2BIG
	}
	record := make([]byte, metadataJournalHeaderSize+len(payload))
	copy(record[:4], metadataJournalMagic[:])
	binary.LittleEndian.PutUint32(record[4:8], uint32(len(payload)))
	binary.LittleEndian.PutUint32(record[8:12], crc32.ChecksumIEEE(payload))
	copy(record[metadataJournalHeaderSize:], payload)
	journalName := filepath.Join("layers", branch.Root, "journal")
	journal, err := r.metadataJournal(branch.Root)
	if err != nil {
		return err
	}
	info, err := journal.Stat()
	if err != nil {
		return err
	}
	if info.Size() != durableEnd {
		// Recovery deliberately ignores a torn final record. Remove that tail
		// before appending so it cannot hide all subsequent transactions.
		if info.Size() < durableEnd {
			return fmt.Errorf("metadata journal shrank from %d to %d", durableEnd, info.Size())
		}
		if err := journal.Truncate(durableEnd); err != nil {
			return err
		}
	}
	if _, err := journal.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	written, err := journal.Write(record)
	if err != nil {
		return err
	}
	if written != len(record) {
		return io.ErrShortWrite
	}
	state.mu.Lock()
	if err := applyMetadataOps(state, ops); err != nil {
		state.mu.Unlock()
		return err
	}
	state.nextSequence++
	state.layerSizes[branch.Root] += int64(len(record))
	state.branch = *branch
	state.mu.Unlock()
	r.markDirtyFile(journalName)
	return nil
}

func applyMetadataOps(state *branchMetadata, ops []metadataOp) error {
	for _, op := range ops {
		switch op.Kind {
		case metadataSetInode:
			if op.Inode == nil || op.Inode.Number == 0 {
				return errors.New("invalid set-inode operation")
			}
			state.inodes[op.Inode.Number] = *op.Inode
		case metadataDeleteInode:
			if op.Number == 0 {
				return errors.New("invalid delete-inode operation")
			}
			delete(state.inodes, op.Number)
		case metadataSetEntry:
			if op.Entry == nil || op.Parent == 0 || validateEntryName(op.Entry.Name) != nil || op.Entry.Inode == 0 {
				return errors.New("invalid set-entry operation")
			}
			key := metadataEntryKey{Parent: op.Parent, Name: op.Entry.Name}
			state.entries[key] = *op.Entry
			directory := state.directories[op.Parent]
			if directory == nil {
				directory = make(map[string]DirectoryEntry)
				state.directories[op.Parent] = directory
			}
			directory[op.Entry.Name] = *op.Entry
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
	defer state.mu.RUnlock()
	inode, ok := state.inodes[ino]
	if !ok {
		return Inode{}, fs.ErrNotExist
	}
	return inode, nil
}

func (r *Repository) metadataLookup(state *branchMetadata, parent uint64, name string) (uint64, error) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	entry, ok := state.entries[metadataEntryKey{Parent: parent, Name: name}]
	if !ok {
		return 0, fs.ErrNotExist
	}
	return entry.Inode, nil
}

func (r *Repository) metadataDirectory(state *branchMetadata, parent uint64) []DirectoryEntry {
	state.mu.RLock()
	defer state.mu.RUnlock()
	directory := state.directories[parent]
	entries := make([]DirectoryEntry, 0, len(directory))
	for _, entry := range directory {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
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
	return filepath.Join("objects", id[:2], id[2:4], id)
}

func (r *Repository) openObject(id string, flags int) (*os.File, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid object ID %q", id)
	}
	file, err := r.openFile(r.objectPath(id), flags, 0)
	if errors.Is(err, os.ErrNotExist) {
		// Version-1 repositories and snapshots used a flat object directory.
		return r.openFile(filepath.Join("objects", id), flags, 0)
	}
	return file, err
}
