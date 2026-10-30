// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcvolume

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

const metadataBatchOperations = 256

type metadataBatch struct {
	state       *branchMetadata
	branch      Branch
	inodes      map[uint64]ttcsnapshot.FilesystemOperation
	entries     map[metadataEntryKey]ttcsnapshot.FilesystemOperation
	releasePins map[string]struct{}
}

func (batch *metadataBatch) add(operations []ttcsnapshot.FilesystemOperation) {
	for _, operation := range operations {
		switch {
		case operation.PutInode != nil:
			batch.inodes[operation.PutInode.Number] = operation
		case operation.DeleteInode != 0:
			batch.inodes[operation.DeleteInode] = operation
		case operation.PutEntry != nil:
			batch.entries[metadataEntryKey{Parent: operation.PutEntry.Parent, Name: operation.PutEntry.Name}] = operation
		case operation.DeleteEntry != nil:
			batch.entries[metadataEntryKey{Parent: operation.DeleteEntry.Parent, Name: operation.DeleteEntry.Name}] = operation
		}
	}
}

func (batch *metadataBatch) len() int {
	return len(batch.inodes) + len(batch.entries)
}

// operations returns the final metadata delta in dependency-safe canonical
// order. Repeated chmod/chown/utimens/write-size updates to one inode and
// repeated changes to one name collapse to their final values before any
// immutable B-tree path is copied.
func (batch *metadataBatch) operations() []ttcsnapshot.FilesystemOperation {
	inodeNumbers := make([]uint64, 0, len(batch.inodes))
	for number := range batch.inodes {
		inodeNumbers = append(inodeNumbers, number)
	}
	sort.Slice(inodeNumbers, func(i, j int) bool { return inodeNumbers[i] < inodeNumbers[j] })
	entryKeys := make([]metadataEntryKey, 0, len(batch.entries))
	for key := range batch.entries {
		if parent, ok := batch.inodes[key.Parent]; ok && parent.DeleteInode != 0 {
			// The complete directory subtree disappears with its inode; child
			// entry deltas are neither reachable nor valid graph operations.
			continue
		}
		entryKeys = append(entryKeys, key)
	}
	sort.Slice(entryKeys, func(i, j int) bool {
		if entryKeys[i].Parent != entryKeys[j].Parent {
			return entryKeys[i].Parent < entryKeys[j].Parent
		}
		return entryKeys[i].Name < entryKeys[j].Name
	})
	operations := make([]ttcsnapshot.FilesystemOperation, 0, batch.len())
	// Materialize final inode records before names reference them.
	for _, number := range inodeNumbers {
		operation := batch.inodes[number]
		if operation.PutInode != nil {
			operations = append(operations, operation)
		}
	}
	// Remove old names before publishing final names, then remove unreachable
	// inode records last.
	for _, key := range entryKeys {
		operation := batch.entries[key]
		if operation.DeleteEntry != nil {
			operations = append(operations, operation)
		}
	}
	for _, key := range entryKeys {
		operation := batch.entries[key]
		if operation.PutEntry != nil {
			operations = append(operations, operation)
		}
	}
	for _, number := range inodeNumbers {
		operation := batch.inodes[number]
		if operation.DeleteInode != 0 {
			operations = append(operations, operation)
		}
	}
	return operations
}

type branchMetadata struct {
	mu                sync.RWMutex
	branch            Branch
	inodes            map[uint64]Inode
	entries           map[metadataEntryKey]DirectoryEntry
	directories       map[uint64]map[string]DirectoryEntry
	loadedDirectories map[uint64]bool
	deletedInodes     map[uint64]bool
	deletedEntries    map[metadataEntryKey]bool
	complete          bool
}

func newBranchMetadata(branch Branch) *branchMetadata {
	return &branchMetadata{
		branch: branch, inodes: make(map[uint64]Inode),
		entries: make(map[metadataEntryKey]DirectoryEntry), directories: make(map[uint64]map[string]DirectoryEntry),
		loadedDirectories: make(map[uint64]bool), deletedInodes: make(map[uint64]bool),
		deletedEntries: make(map[metadataEntryKey]bool),
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
	// Expand the files' objects many to a journal record, not one each as
	// preparing them below would.
	files := make([]ttcsnapshot.ObjectID, 0, len(inodes))
	for _, record := range inodes {
		if record.Type == "regular" && !record.File.IsZero() {
			files = append(files, record.File)
		}
	}
	if err := r.graph.ResolveObjects(files); err != nil {
		return err
	}
	resolvedInodes := make([]Inode, 0, len(inodes))
	for _, record := range inodes {
		inode := inodeFromGraph(record)
		if err := r.prepareDeferredInode(&inode); err != nil {
			return err
		}
		resolvedInodes = append(resolvedInodes, inode)
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
	state.deletedInodes = make(map[uint64]bool)
	state.deletedEntries = make(map[metadataEntryKey]bool)
	for _, inode := range resolvedInodes {
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
	// metadataForBranch receives Branch by value, so its authoritative graph
	// root correction is stored in state rather than the caller's stale branch
	// summary. Carry that state forward while a bounded batch is pending.
	state.mu.RLock()
	currentBranch := state.branch
	state.mu.RUnlock()
	if currentBranch.ID != branch.ID || currentBranch.WorkingSnapshot != branch.WorkingSnapshot {
		return fmt.Errorf("branch %q metadata generation changed during mutation", branch.ID)
	}
	*branch = currentBranch
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
			if !record.File.IsZero() {
				r.dirtyMu.Lock()
				if pending, ok := r.pendingBulk[record.File.String()]; ok {
					pending.BulkLength = uint64(max(op.Inode.Size, 0))
					r.pendingBulk[record.File.String()] = pending
				}
				r.dirtyMu.Unlock()
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
	releasePins := make(map[string]struct{})
	for _, op := range ops {
		if op.Inode != nil && op.Inode.Object != "" {
			if id, err := ttcsnapshot.ParseObjectID(op.Inode.Object); err == nil {
				pin := graphConstructionPin(id)
				r.dirtyMu.Lock()
				_, pending := r.pendingBulk[id.String()]
				r.dirtyMu.Unlock()
				if pending || r.graph.HasPin(pin) {
					releasePins[pin] = struct{}{}
				}
			}
		}
	}
	state.mu.Lock()
	if err := applyMetadataOps(state, ops); err != nil {
		state.mu.Unlock()
		return err
	}
	state.branch = *branch
	state.mu.Unlock()
	r.dirtyMu.Lock()
	if r.pendingMetadata == nil {
		r.pendingMetadata = make(map[string]*metadataBatch)
	}
	batch := r.pendingMetadata[branch.ID]
	if batch == nil {
		batch = &metadataBatch{
			state: state, branch: *branch, inodes: make(map[uint64]ttcsnapshot.FilesystemOperation),
			entries: make(map[metadataEntryKey]ttcsnapshot.FilesystemOperation), releasePins: make(map[string]struct{}),
		}
		r.pendingMetadata[branch.ID] = batch
	}
	batch.add(graphOps)
	for pin := range releasePins {
		batch.releasePins[pin] = struct{}{}
	}
	// Construction pins are also bounded tail work even when create+unlink
	// pairs collapse to no final namespace entry.
	flush := batch.len()+len(batch.releasePins) >= metadataBatchOperations ||
		len(r.pendingReclaims)+len(batch.releasePins) >= reclaimBatchObjects
	r.dirtyMu.Unlock()
	if flush {
		if err := r.flushMetadataBatchLocked(branch.ID); err != nil {
			// The operation is applied, and its batch keeps it. While an
			// authority handoff has frozen a snapshot, the graph refuses
			// any batch, and a later operation flushes this one once the
			// handoff is over: failing an operation already made visible
			// left the file system's caller unable to undo it, and a live
			// sibling of a branch being dehydrated or stopped panicked.
			if errors.Is(err, ttcsnapshot.ErrSnapshotLedgerFrozen) {
				return nil
			}
			return fmt.Errorf("flushing bounded filesystem metadata batch: %w", err)
		}
		r.dirtyMu.Lock()
		durabilityFence := len(r.pendingReclaims) >= reclaimBatchObjects || r.graph.CheckpointDue(reclaimJournalBytes)
		r.dirtyMu.Unlock()
		if durabilityFence {
			if err := r.reclaimFenceLocked(); err != nil {
				return fmt.Errorf("flushing bounded filesystem garbage: %w", err)
			}
		}
	}
	return nil
}

// flushMetadataBatchLocked publishes one branch's bounded live metadata tail
// into the immutable snapshot graph. Repository.operationMu and the repository
// flock must be held by the caller.
func (r *Repository) flushMetadataBatchLocked(branchID string) error {
	r.dirtyMu.Lock()
	batch := r.pendingMetadata[branchID]
	r.dirtyMu.Unlock()
	if batch == nil || batch.len() == 0 {
		return nil
	}
	// Another process may have taken the branch over since the batch began:
	// ttcd forks a branch whose Kernel is gone to publish it, and removes a
	// deleted one. The batch's origin is then sealed, and refused every later
	// fence of this repository, or the branch's roots are gone, and the batch
	// alone became its root.
	if superseded, err := r.branchSupersededLocked(branchID, batch.branch); err != nil {
		return err
	} else if superseded {
		r.forgetSupersededBranchLocked(branchID)
		return nil
	}
	// A filesystem transaction path-copies the branch's tree, and so changes
	// the references of the snapshots it shares with other branches; while
	// one of them is frozen for an authority handoff, the graph refuses it.
	// Refuse it before publishing any bulk object below. Refused after, the
	// objects were left held only by construction pins, and the next process
	// to open the repository released them as a crashed transaction's.
	if snapshot, frozen := r.graph.FrozenSnapshotLedger(); frozen {
		return fmt.Errorf("snapshot %d references are frozen for authority handoff: %w", snapshot, ttcsnapshot.ErrSnapshotLedgerFrozen)
	}
	pins := make([]string, 0, len(batch.releasePins))
	for pin := range batch.releasePins {
		pins = append(pins, pin)
	}
	sort.Strings(pins)
	// Publish complete bulk payload metadata in one graph transaction before
	// the filesystem transaction converts their construction pins into inode
	// edges. A retry after an uncertain later step recognizes already-published
	// objects and proceeds idempotently.
	r.dirtyMu.Lock()
	pending := make([]ttcsnapshot.ReservedObject, 0, len(pins))
	for _, pin := range pins {
		for _, object := range r.pendingBulk {
			if object.Pin == pin {
				pending = append(pending, object)
				break
			}
		}
	}
	r.dirtyMu.Unlock()
	slices.SortFunc(pending, func(a, b ttcsnapshot.ReservedObject) int { return a.ID.Compare(b.ID) })
	var unpublished []ttcsnapshot.ReservedObject
	for _, object := range pending {
		if _, exists := r.graph.Object(object.ID); exists && r.graph.HasPin(object.Pin) {
			continue
		}
		unpublished = append(unpublished, object)
	}
	if len(unpublished) != 0 {
		if err := r.graph.CommitReservedObjects(unpublished); err != nil {
			return fmt.Errorf("publishing filesystem bulk objects: %w", err)
		}
	}
	root, reclaimed, err := r.graph.ApplyFilesystemAndReleasePinsReclaimed(
		ttcsnapshot.SnapshotID(batch.branch.WorkingSnapshot), graphRootName(branchID), batch.operations(), pins)
	if err != nil {
		return err
	}
	// Until the inodes name them, a retry must still find the objects above.
	if len(pending) != 0 {
		r.dirtyMu.Lock()
		for _, object := range pending {
			delete(r.pendingBulk, object.ID.String())
		}
		r.dirtyMu.Unlock()
	}
	batch.branch.Root = root.String()
	batch.state.mu.Lock()
	batch.state.branch.Root = root.String()
	batch.state.mu.Unlock()
	r.dirtyMu.Lock()
	// The operation lock ensures nobody can replace this batch while it is
	// being published. Keep the identity check to make that invariant explicit.
	if r.pendingMetadata[branchID] != batch {
		r.dirtyMu.Unlock()
		return errors.New("TTC filesystem metadata batch changed during publication")
	}
	delete(r.pendingMetadata, branchID)
	if r.dirtyBranches == nil {
		r.dirtyBranches = make(map[string]*branchMetadata)
	}
	r.dirtyBranches[branchID] = batch.state
	if r.pendingReclaims == nil {
		r.pendingReclaims = make(map[string]ttcsnapshot.ObjectID)
	}
	for _, id := range reclaimed {
		r.pendingReclaims[id.String()] = id
	}
	r.dirtyMu.Unlock()
	return nil
}

// branchSupersededLocked reports whether another process has removed branch
// id, or forked it, since base, this process's view of its pointer, was read:
// the pointer is gone, or has moved to another generation. A branch this
// process is still making has no pointer yet, and was never read: base has no
// root. The repository flock must be held by the caller.
func (r *Repository) branchSupersededLocked(id string, base Branch) (bool, error) {
	current, err := r.readBranchRecord(id, false)
	if errors.Is(err, os.ErrNotExist) {
		return base.Root != "", nil
	}
	if err != nil {
		return false, err
	}
	return current.Generation != base.Generation || current.WorkingSnapshot != base.WorkingSnapshot, nil
}

// forgetSupersededBranchLocked drops what this process has of branch id
// that another process superseded (see branchSupersededLocked): its batch,
// whose objects' construction pins the next open of the repository releases
// as a crashed transaction's, its unpublished pointer summary, and its
// metadata view, which the next access reads again from the graph.
// Repository.operationMu must be held by the caller.
func (r *Repository) forgetSupersededBranchLocked(id string) {
	r.dirtyMu.Lock()
	if batch := r.pendingMetadata[id]; batch != nil {
		for pin := range batch.releasePins {
			for name, object := range r.pendingBulk {
				if object.Pin == pin {
					delete(r.pendingBulk, name)
				}
			}
		}
		delete(r.reservedFileIDs, batch.branch.WorkingSnapshot)
	}
	delete(r.pendingMetadata, id)
	delete(r.dirtyBranches, id)
	r.dirtyMu.Unlock()
	r.metadataMu.Lock()
	delete(r.metadata, id)
	r.metadataMu.Unlock()
	r.branchMu.Lock()
	delete(r.branchCache, id)
	r.branchMu.Unlock()
}

func (r *Repository) flushAllMetadataBatchesLocked() error {
	r.dirtyMu.Lock()
	branches := make([]string, 0, len(r.pendingMetadata))
	for branch := range r.pendingMetadata {
		branches = append(branches, branch)
	}
	r.dirtyMu.Unlock()
	sort.Strings(branches)
	for _, branch := range branches {
		if err := r.flushMetadataBatchLocked(branch); err != nil {
			return err
		}
	}
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
			delete(state.deletedInodes, op.Inode.Number)
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
			state.deletedInodes[op.Number] = true
			delete(state.directories, op.Number)
			delete(state.loadedDirectories, op.Number)
			for key := range state.entries {
				if key.Parent == op.Number {
					delete(state.entries, key)
					state.deletedEntries[key] = true
				}
			}
		case metadataSetEntry:
			if op.Entry == nil || op.Parent == 0 || validateEntryName(op.Entry.Name) != nil || op.Entry.Inode == 0 {
				return errors.New("invalid set-entry operation")
			}
			key := metadataEntryKey{Parent: op.Parent, Name: op.Entry.Name}
			state.entries[key] = *op.Entry
			delete(state.deletedEntries, key)
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
			key := metadataEntryKey{Parent: op.Parent, Name: op.Name}
			delete(state.entries, key)
			state.deletedEntries[key] = true
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
	deleted := state.deletedInodes[ino]
	branch := state.branch
	state.mu.RUnlock()
	if ok {
		if err := r.prepareDeferredInode(&inode); err != nil {
			return Inode{}, err
		}
		return inode, nil
	}
	if deleted {
		return Inode{}, fs.ErrNotExist
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
	if err := r.prepareDeferredInode(&inode); err != nil {
		return Inode{}, err
	}
	state.mu.Lock()
	if state.branch.Root == branch.Root {
		state.inodes[ino] = inode
	}
	state.mu.Unlock()
	return inode, nil
}

func (r *Repository) metadataLookup(state *branchMetadata, parent uint64, name string) (uint64, error) {
	state.mu.RLock()
	key := metadataEntryKey{Parent: parent, Name: name}
	entry, ok := state.entries[key]
	deleted := state.deletedEntries[key]
	// A loaded directory has all its entries in state.entries.
	loaded := state.loadedDirectories[parent]
	branch := state.branch
	state.mu.RUnlock()
	if ok {
		return entry.Inode, nil
	}
	if deleted || loaded {
		return 0, fs.ErrNotExist
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

// markNewDirectory records that the directory number, which the branch of
// state has just made, has no entries but those state holds, so that looking
// a name up in it need not read the graph.
func (state *branchMetadata) markNewDirectory(number uint64) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, ok := state.inodes[number]; !ok {
		return
	}
	state.loadedDirectories[number] = true
	if state.directories[number] == nil {
		state.directories[number] = make(map[string]DirectoryEntry)
	}
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
		// Overlay process-local batched mutations on the immutable graph view.
		// Negative entries are explicit so a pending unlink cannot fall back to
		// the still-current graph root and resurrect a name.
		for key := range state.deletedEntries {
			if key.Parent == parent {
				delete(directory, key.Name)
			}
		}
		for key, entry := range state.entries {
			if key.Parent == parent {
				directory[key.Name] = entry
			}
		}
		entries = entries[:0]
		for _, entry := range directory {
			entries = append(entries, entry)
		}
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
	file, err := r.data.OpenFile(parsed, flags, -1)
	if err != nil && retryOpen(err) {
		file, err = r.data.OpenFile(parsed, flags, -1)
	}
	return file, err
}

// OpenObject opens the host file of object, a whole-file object of r, for
// writing too if writable is set. It takes no lock of r: the Sentry opens
// again, under its memory locks, objects whose mappings it holds (whose
// objects r keeps until the Sentry drops them, see CoWCallback), after it
// closed their descriptors to stay within its limit on them.
func (r *Repository) OpenObject(object string, writable bool) (*os.File, error) {
	flags := os.O_RDONLY
	if writable {
		flags = os.O_RDWR
	}
	return r.openObject(object, flags)
}

// FDShortageHook, if set, is called when the host refuses the process another
// file descriptor (EMFILE, or ENFILE) as a repository opens one of its files,
// and reports whether to try again. The Sentry sets it to close descriptors
// that it does not use (see fdbudget).
var FDShortageHook func(error) bool

// retryOpen reports whether to try again an open that failed with err.
func retryOpen(err error) bool {
	if hook := FDShortageHook; hook != nil {
		return hook(err)
	}
	return false
}
