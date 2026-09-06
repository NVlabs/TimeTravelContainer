// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ttcchunk

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var (
	leaseMagic       = [8]byte{'T', 'T', 'C', 'M', 'L', 'E', 'A', 1}
	transactionMagic = [8]byte{'T', 'T', 'C', 'M', 'T', 'X', 'N', 1}
)

const (
	// DefaultLeaseTTL is deliberately much longer than the intended daemon
	// heartbeat. GC also quarantines and rescans before unlinking, so renewal
	// concurrent with collection remains safe.
	DefaultLeaseTTL = 5 * time.Minute
	// DefaultTransactionTTL bounds debris from a publisher that crashes before
	// installing a branch root. Live publishers refresh their transaction.
	DefaultTransactionTTL = time.Hour
)

func randomName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func quarantineRootName(repositoryRoot, path string) (string, error) {
	relative, err := filepath.Rel(repositoryRoot, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("TTC repository path %q is outside %q", path, repositoryRoot)
	}
	return "root-" + hex.EncodeToString([]byte(relative)), nil
}

func recoverRepositoryTrash(root string, now time.Time, staleAfter time.Duration) error {
	trashRoot := filepath.Join(root, "trash")
	sweeps, err := os.ReadDir(trashRoot)
	if err != nil {
		return err
	}
	for _, sweep := range sweeps {
		if !sweep.IsDir() {
			return fmt.Errorf("unexpected TTC memory trash entry %q", sweep.Name())
		}
		info, err := sweep.Info()
		if err != nil {
			return err
		}
		// A fresh sweep may belong to another ttcd using this shared
		// repository. Only recover a quarantine after its collector has
		// demonstrably stopped making progress for the crash-recovery window.
		if now.Sub(info.ModTime()) <= staleAfter {
			continue
		}
		sweepPath := filepath.Join(trashRoot, sweep.Name())
		entries, err := os.ReadDir(sweepPath)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				return fmt.Errorf("unexpected TTC memory quarantine directory %q", entry.Name())
			}
			var destination string
			switch {
			case strings.HasPrefix(entry.Name(), "manifest-"):
				id, err := ParseManifestID(strings.TrimPrefix(entry.Name(), "manifest-"))
				if err != nil {
					return err
				}
				name := id.String()
				destination = filepath.Join(root, "manifests", name[:4], name+".manifest")
			case strings.HasPrefix(entry.Name(), "chunk-"):
				id, err := ParseID(strings.TrimPrefix(entry.Name(), "chunk-"))
				if err != nil {
					return err
				}
				name := id.String()
				destination = filepath.Join(root, "chunks", name[:4], name+".chunk")
			case strings.HasPrefix(entry.Name(), "root-"):
				encoded := strings.TrimPrefix(entry.Name(), "root-")
				relative, err := hex.DecodeString(encoded)
				if err != nil {
					return fmt.Errorf("invalid TTC quarantined root %q", entry.Name())
				}
				destination = filepath.Join(root, string(relative))
				if verified, err := quarantineRootName(root, destination); err != nil || verified != entry.Name() {
					return fmt.Errorf("invalid TTC quarantined root path %q", entry.Name())
				}
			default:
				return fmt.Errorf("unexpected TTC memory quarantine entry %q", entry.Name())
			}
			source := filepath.Join(sweepPath, entry.Name())
			if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
				return err
			}
			if err := os.Rename(source, destination); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return err
				}
				if err := os.Remove(source); err != nil {
					return err
				}
			}
			if err := syncDirectory(filepath.Dir(destination)); err != nil {
				return err
			}
		}
		if err := os.Remove(sweepPath); err != nil {
			return err
		}
	}
	return syncDirectory(trashRoot)
}

func atomicReplace(dir, prefix, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func encodeLease(id ManifestID, ttl time.Duration) ([]byte, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("invalid TTC memory lease TTL %s", ttl)
	}
	data := append([]byte(nil), leaseMagic[:]...)
	data = append(data, id[:]...)
	var duration [8]byte
	binary.LittleEndian.PutUint64(duration[:], uint64(ttl))
	data = append(data, duration[:]...)
	checksum := sha256.Sum256(data)
	return append(data, checksum[:]...), nil
}

func decodeLease(data []byte) (ManifestID, time.Duration, error) {
	var id ManifestID
	wantLen := len(leaseMagic) + len(id) + 8 + sha256.Size
	if len(data) != wantLen || !bytes.Equal(data[:len(leaseMagic)], leaseMagic[:]) {
		return id, 0, errors.New("invalid TTC memory lease")
	}
	want := sha256.Sum256(data[:wantLen-sha256.Size])
	if !bytes.Equal(data[wantLen-sha256.Size:], want[:]) {
		return id, 0, errors.New("TTC memory lease checksum mismatch")
	}
	copy(id[:], data[len(leaseMagic):len(leaseMagic)+len(id)])
	ttl := time.Duration(binary.LittleEndian.Uint64(data[len(leaseMagic)+len(id) : wantLen-sha256.Size]))
	if ttl <= 0 {
		return id, 0, fmt.Errorf("invalid TTC memory lease TTL %s", ttl)
	}
	return id, ttl, nil
}

// Lease is a renewable persistent root for a manifest used by an active
// runtime. Callers must renew substantially before its TTL and release it when
// the runtime no longer references the manifest.
type Lease struct {
	mu       sync.Mutex
	repo     *MemoryRepository
	path     string
	manifest ManifestID
	ttl      time.Duration
	released bool
}

// AcquireLease publishes a persistent root and then revalidates the complete
// manifest. Revalidation, combined with GC's post-quarantine root rescan,
// closes acquisition/collection races across processes and hosts.
func (r *MemoryRepository) AcquireLease(id ManifestID, ttl time.Duration) (*Lease, error) {
	if ttl == 0 {
		ttl = DefaultLeaseTTL
	}
	data, err := encodeLease(id, ttl)
	if err != nil {
		return nil, err
	}
	if err := r.validateManifestObjects(id); err != nil {
		return nil, fmt.Errorf("acquiring lease for unavailable manifest %s: %w", id, err)
	}
	name, err := randomName()
	if err != nil {
		return nil, err
	}
	leaseName := name + ".lease"
	leasePath := filepath.Join(r.leaseRoot, leaseName)
	if r.rootFD != nil {
		leasePath = "leases/" + leaseName
	}
	lease := &Lease{repo: r, path: leasePath, manifest: id, ttl: ttl}
	r.mu.Lock()
	if r.rootFD != nil {
		err = atomicReplaceRelative(int(r.rootFD.Fd()), "leases", leaseName, data)
	} else {
		err = atomicReplace(r.leaseRoot, ".lease-", lease.path, data)
	}
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := r.validateManifestObjects(id); err != nil {
		_ = lease.Release()
		return nil, fmt.Errorf("manifest %s was reclaimed while acquiring its lease: %w", id, err)
	}
	return lease, nil
}

// Renew atomically refreshes the lease's modification time and contents.
func (l *Lease) Renew() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return errors.New("TTC memory lease is released")
	}
	data, err := encodeLease(l.manifest, l.ttl)
	if err != nil {
		return err
	}
	l.repo.mu.Lock()
	if l.repo.rootFD != nil {
		err = atomicReplaceRelative(int(l.repo.rootFD.Fd()), "leases", filepath.Base(l.path), data)
	} else {
		err = atomicReplace(l.repo.leaseRoot, ".lease-", l.path, data)
	}
	l.repo.mu.Unlock()
	if err != nil {
		return err
	}
	if err := l.repo.validateManifestObjects(l.manifest); err != nil {
		if l.repo.rootFD != nil {
			_ = unlinkRelative(int(l.repo.rootFD.Fd()), l.path)
		} else {
			_ = os.Remove(l.path)
		}
		l.released = true
		return fmt.Errorf("manifest %s was reclaimed while renewing its lease: %w", l.manifest, err)
	}
	return nil
}

// Release durably removes the lease root. It is idempotent.
func (l *Lease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.repo.mu.Lock()
	var err error
	if l.repo.rootFD != nil {
		err = unlinkRelative(int(l.repo.rootFD.Fd()), l.path)
		if err == nil {
			err = syncDirRelative(int(l.repo.rootFD.Fd()), "leases")
		}
	} else {
		err = os.Remove(l.path)
		if err == nil {
			err = syncDirectory(l.repo.leaseRoot)
		}
	}
	l.repo.mu.Unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l.released = true
	return nil
}

type publicationTransaction struct {
	Manifest ManifestID
	Objects  []ID
}

func encodeTransaction(txn publicationTransaction) ([]byte, error) {
	objects := append([]ID(nil), txn.Objects...)
	sort.Slice(objects, func(i, j int) bool { return objects[i].String() < objects[j].String() })
	data := append([]byte(nil), transactionMagic[:]...)
	data = append(data, txn.Manifest[:]...)
	var count [4]byte
	binary.LittleEndian.PutUint32(count[:], uint32(len(objects)))
	data = append(data, count[:]...)
	var previous ID
	for i, object := range objects {
		if object == (ID{}) || i != 0 && object == previous {
			return nil, fmt.Errorf("invalid repeated TTC transaction object %s", object)
		}
		data = append(data, object[:]...)
		previous = object
	}
	checksum := sha256.Sum256(data)
	return append(data, checksum[:]...), nil
}

func decodeTransaction(data []byte) (publicationTransaction, error) {
	var txn publicationTransaction
	header := len(transactionMagic) + len(txn.Manifest) + 4
	if len(data) < header+sha256.Size || !bytes.Equal(data[:len(transactionMagic)], transactionMagic[:]) {
		return txn, errors.New("invalid TTC memory publication transaction")
	}
	count := binary.LittleEndian.Uint32(data[len(transactionMagic)+len(txn.Manifest) : header])
	wantLen := header + int(count)*sha256.Size + sha256.Size
	if len(data) != wantLen {
		return txn, errors.New("invalid TTC memory publication transaction length")
	}
	want := sha256.Sum256(data[:wantLen-sha256.Size])
	if !bytes.Equal(data[wantLen-sha256.Size:], want[:]) {
		return txn, errors.New("TTC memory publication transaction checksum mismatch")
	}
	copy(txn.Manifest[:], data[len(transactionMagic):len(transactionMagic)+len(txn.Manifest)])
	txn.Objects = make([]ID, count)
	offset := header
	for i := range txn.Objects {
		copy(txn.Objects[i][:], data[offset:offset+sha256.Size])
		if txn.Objects[i] == (ID{}) || i != 0 && txn.Objects[i-1].String() >= txn.Objects[i].String() {
			return publicationTransaction{}, errors.New("invalid TTC memory publication transaction object order")
		}
		offset += sha256.Size
	}
	return txn, nil
}

func (r *MemoryRepository) beginTransaction(manifest ManifestID, objects []ID) (string, error) {
	data, err := encodeTransaction(publicationTransaction{Manifest: manifest, Objects: objects})
	if err != nil {
		return "", err
	}
	name, err := randomName()
	if err != nil {
		return "", err
	}
	path := filepath.Join(r.txnRoot, name+".txn")
	if err := atomicReplace(r.txnRoot, ".txn-", path, data); err != nil {
		return "", err
	}
	return path, nil
}

func (r *MemoryRepository) removeTransactionsFor(manifest ManifestID) error {
	entries, err := os.ReadDir(r.txnRoot)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txn") {
			continue
		}
		path := filepath.Join(r.txnRoot, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		txn, err := decodeTransaction(data)
		if err != nil {
			return fmt.Errorf("reading publication transaction %q: %w", entry.Name(), err)
		}
		if txn.Manifest == manifest {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			changed = true
		}
	}
	if changed {
		return syncDirectory(r.txnRoot)
	}
	return nil
}

func (r *MemoryRepository) validateManifestObjects(id ManifestID) error {
	manifest, err := r.LoadManifest(id)
	if err != nil {
		return err
	}
	seen := make(map[ID]struct{})
	for _, run := range manifest.Runs {
		if _, ok := seen[run.Object]; ok {
			continue
		}
		if err := r.chunks.Contains(run.Object); err != nil {
			return fmt.Errorf("loading memory object %s: %w", run.Object, err)
		}
		seen[run.Object] = struct{}{}
	}
	return nil
}

// GCOptions controls repository garbage collection.
type GCOptions struct {
	Now            time.Time
	TransactionTTL time.Duration
}

// GCResult reports immutable objects reclaimed by CollectGarbage.
type GCResult struct {
	Chunks              int
	Manifests           int
	ExpiredLeases       int
	ExpiredTransactions int
}

type repositoryRoots struct {
	manifests         map[ManifestID]struct{}
	requiredManifests map[ManifestID]struct{}
	pendingManifests  map[ManifestID]struct{}
	fullManifests     map[ManifestID]struct{}
	branchConsumption map[ManifestID][]map[ID]struct{}
	objects           map[ID]struct{}
}

func newRepositoryRoots() repositoryRoots {
	return repositoryRoots{
		manifests: make(map[ManifestID]struct{}), requiredManifests: make(map[ManifestID]struct{}),
		pendingManifests: make(map[ManifestID]struct{}),
		fullManifests:    make(map[ManifestID]struct{}), branchConsumption: make(map[ManifestID][]map[ID]struct{}),
		objects: make(map[ID]struct{}),
	}
}

func staleTemporary(entry os.DirEntry, prefix string, now time.Time, ttl time.Duration) (bool, error) {
	if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
		return false, nil
	}
	info, err := entry.Info()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return now.Sub(info.ModTime()) > ttl, nil
}

func (r *MemoryRepository) scanRoots(now time.Time, transactionTTL time.Duration) (repositoryRoots, []string, error) {
	roots := newRepositoryRoots()
	var expired []string
	activeConsumption := make(map[string]struct{})
	branches, err := os.ReadDir(r.branchRoot)
	if err != nil {
		return roots, nil, err
	}
	for _, entry := range branches {
		if stale, staleErr := staleTemporary(entry, ".branch-", now, transactionTTL); staleErr != nil {
			return roots, nil, staleErr
		} else if stale {
			expired = append(expired, filepath.Join(r.branchRoot, entry.Name()))
			continue
		} else if strings.HasPrefix(entry.Name(), ".branch-") {
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".root") {
			return roots, nil, fmt.Errorf("unexpected TTC memory branch entry %q", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(r.branchRoot, entry.Name()))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return roots, nil, err
		}
		id, err := decodeBranchPointer(data)
		if err != nil {
			return roots, nil, fmt.Errorf("reading branch root %q: %w", entry.Name(), err)
		}
		roots.manifests[id] = struct{}{}
		roots.requiredManifests[id] = struct{}{}
		branchNameBytes, err := hex.DecodeString(strings.TrimSuffix(entry.Name(), ".root"))
		if err != nil || len(branchNameBytes) == 0 {
			return roots, nil, fmt.Errorf("invalid TTC memory branch entry %q", entry.Name())
		}
		consumed, err := r.readConsumption(string(branchNameBytes))
		if err != nil {
			return roots, nil, fmt.Errorf("reading branch consumption %q: %w", branchNameBytes, err)
		}
		if consumed.Manifest != (ManifestID{}) && consumed.Manifest != id {
			return roots, nil, fmt.Errorf("branch %q consumption references manifest %s, want %s", branchNameBytes, consumed.Manifest, id)
		}
		roots.branchConsumption[id] = append(roots.branchConsumption[id], consumed.Objects)
		activeConsumption[hex.EncodeToString(branchNameBytes)+".consumed"] = struct{}{}
	}
	consumptions, err := os.ReadDir(r.consumedRoot)
	if err != nil {
		return roots, nil, err
	}
	for _, entry := range consumptions {
		path := filepath.Join(r.consumedRoot, entry.Name())
		if strings.HasPrefix(entry.Name(), ".consume-") || strings.HasPrefix(entry.Name(), ".txn-") {
			if stale, staleErr := staleTemporary(entry, strings.SplitN(entry.Name(), "-", 2)[0]+"-", now, transactionTTL); staleErr != nil {
				return roots, nil, staleErr
			} else if stale {
				expired = append(expired, path)
			}
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".consumed") {
			return roots, nil, fmt.Errorf("unexpected TTC memory consumption entry %q", entry.Name())
		}
		encoded := strings.TrimSuffix(entry.Name(), ".consumed")
		decoded, decodeErr := hex.DecodeString(encoded)
		if decodeErr != nil || len(decoded) == 0 || hex.EncodeToString(decoded) != encoded {
			return roots, nil, fmt.Errorf("invalid TTC memory consumption entry %q", entry.Name())
		}
		if _, ok := activeConsumption[entry.Name()]; !ok {
			expired = append(expired, path)
		}
	}
	leases, err := os.ReadDir(r.leaseRoot)
	if err != nil {
		return roots, nil, err
	}
	for _, entry := range leases {
		if stale, staleErr := staleTemporary(entry, ".lease-", now, transactionTTL); staleErr != nil {
			return roots, nil, staleErr
		} else if stale {
			expired = append(expired, filepath.Join(r.leaseRoot, entry.Name()))
			continue
		} else if strings.HasPrefix(entry.Name(), ".lease-") {
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lease") {
			return roots, nil, fmt.Errorf("unexpected TTC memory lease entry %q", entry.Name())
		}
		path := filepath.Join(r.leaseRoot, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return roots, nil, err
		}
		id, ttl, err := decodeLease(data)
		if err != nil {
			return roots, nil, fmt.Errorf("reading lease %q: %w", entry.Name(), err)
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return roots, nil, err
		}
		if now.Sub(info.ModTime()) > ttl {
			expired = append(expired, path)
			continue
		}
		roots.manifests[id] = struct{}{}
		roots.requiredManifests[id] = struct{}{}
		roots.fullManifests[id] = struct{}{}
	}
	transactions, err := os.ReadDir(r.txnRoot)
	if err != nil {
		return roots, nil, err
	}
	for _, entry := range transactions {
		if stale, staleErr := staleTemporary(entry, ".txn-", now, transactionTTL); staleErr != nil {
			return roots, nil, staleErr
		} else if stale {
			expired = append(expired, filepath.Join(r.txnRoot, entry.Name()))
			continue
		} else if strings.HasPrefix(entry.Name(), ".txn-") {
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txn") {
			return roots, nil, fmt.Errorf("unexpected TTC memory transaction entry %q", entry.Name())
		}
		path := filepath.Join(r.txnRoot, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return roots, nil, err
		}
		txn, err := decodeTransaction(data)
		if err != nil {
			return roots, nil, fmt.Errorf("reading transaction %q: %w", entry.Name(), err)
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return roots, nil, err
		}
		if now.Sub(info.ModTime()) > transactionTTL {
			expired = append(expired, path)
			continue
		}
		roots.manifests[txn.Manifest] = struct{}{}
		roots.pendingManifests[txn.Manifest] = struct{}{}
		roots.fullManifests[txn.Manifest] = struct{}{}
		for _, object := range txn.Objects {
			roots.objects[object] = struct{}{}
		}
	}
	return roots, expired, nil
}

func (r *MemoryRepository) traceRoots(roots repositoryRoots) error {
	for id := range roots.manifests {
		manifest, err := r.LoadManifest(id)
		if err != nil {
			// An in-flight transaction may publish its manifest after this scan;
			// its explicit object list is sufficient to protect partial output.
			if _, required := roots.requiredManifests[id]; !required {
				if _, pending := roots.pendingManifests[id]; pending {
					continue
				}
			}
			return fmt.Errorf("rooted memory manifest %s is unavailable: %w", id, err)
		}
		if _, full := roots.fullManifests[id]; full {
			for _, run := range manifest.Runs {
				roots.objects[run.Object] = struct{}{}
			}
			continue
		}
		manifestObjects := make(map[ID]struct{}, len(manifest.Runs))
		for _, run := range manifest.Runs {
			manifestObjects[run.Object] = struct{}{}
		}
		for _, consumed := range roots.branchConsumption[id] {
			for object := range consumed {
				if _, ok := manifestObjects[object]; !ok {
					return fmt.Errorf("memory consumption for manifest %s names unrelated object %s", id, object)
				}
			}
			for _, run := range manifest.Runs {
				if _, done := consumed[run.Object]; !done {
					roots.objects[run.Object] = struct{}{}
				}
			}
		}
	}
	return nil
}

type garbageFile struct {
	kind       string
	id         string
	original   string
	quarantine string
}

func immutableFiles(root, suffix, kind string, now time.Time, ttl time.Duration) ([]garbageFile, []string, error) {
	var files []garbageFile
	var stale []string
	shards, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	for _, shard := range shards {
		if !shard.IsDir() || len(shard.Name()) != 4 {
			return nil, nil, fmt.Errorf("unexpected TTC %s shard %q", kind, shard.Name())
		}
		entries, err := os.ReadDir(filepath.Join(root, shard.Name()))
		if err != nil {
			return nil, nil, err
		}
		for _, entry := range entries {
			if temporary, tempErr := staleTemporary(entry, ".publish-", now, ttl); tempErr != nil {
				return nil, nil, tempErr
			} else if temporary {
				stale = append(stale, filepath.Join(root, shard.Name(), entry.Name()))
				continue
			} else if strings.HasPrefix(entry.Name(), ".publish-") {
				continue
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
				return nil, nil, fmt.Errorf("unexpected TTC %s entry %q", kind, entry.Name())
			}
			id := strings.TrimSuffix(entry.Name(), suffix)
			if len(id) != sha256.Size*2 || !strings.HasPrefix(id, shard.Name()) {
				return nil, nil, fmt.Errorf("invalid TTC %s name %q", kind, entry.Name())
			}
			files = append(files, garbageFile{kind: kind, id: id, original: filepath.Join(root, shard.Name(), entry.Name())})
		}
	}
	return files, stale, nil
}

// CollectGarbage removes immutable chunks and manifests unreachable from any
// branch, unexpired lease, or in-flight publication transaction. Candidates
// are first atomically moved to a private quarantine and roots are rescanned;
// this ensures a lease or branch created concurrently either protects and
// restores its data or fails validation before it can become active.
func (r *MemoryRepository) CollectGarbage(opts GCOptions) (GCResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gcLock, err := r.lockGarbageCollection(unix.LOCK_EX)
	if err != nil {
		return GCResult{}, fmt.Errorf("locking TTC memory repository for garbage collection: %w", err)
	}
	defer unlockGarbageCollection(gcLock)
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	transactionTTL := opts.TransactionTTL
	if transactionTTL == 0 {
		transactionTTL = DefaultTransactionTTL
	}
	if transactionTTL < 0 {
		return GCResult{}, fmt.Errorf("invalid transaction TTL %s", transactionTTL)
	}
	roots, expired, err := r.scanRoots(now, transactionTTL)
	if err != nil {
		return GCResult{}, err
	}
	if err := r.traceRoots(roots); err != nil {
		return GCResult{}, err
	}
	manifestFiles, manifestTemps, err := immutableFiles(r.manifestRoot, ".manifest", "manifest", now, transactionTTL)
	if err != nil {
		return GCResult{}, err
	}
	chunkFiles, chunkTemps, err := immutableFiles(r.chunks.root, ".chunk", "chunk", now, transactionTTL)
	if err != nil {
		return GCResult{}, err
	}
	// A lazy branch may name objects that are authoritative in a cold pack or
	// remote peer but have never been cached in this repository. Post-GC
	// validation must prove that GC retained every *local* object that was
	// rooted; requiring all logical objects to exist locally turns a healthy
	// sparse cache into an error after the first consumption update.
	initialLocalChunks := make(map[ID]struct{}, len(chunkFiles))
	for _, file := range chunkFiles {
		id, parseErr := ParseID(file.id)
		if parseErr != nil {
			return GCResult{}, parseErr
		}
		initialLocalChunks[id] = struct{}{}
	}
	expired = append(expired, manifestTemps...)
	expired = append(expired, chunkTemps...)
	all := append(manifestFiles, chunkFiles...)
	nonce, err := randomName()
	if err != nil {
		return GCResult{}, err
	}
	quarantineRoot := filepath.Join(r.trashRoot, nonce)
	if err := os.Mkdir(quarantineRoot, 0700); err != nil {
		return GCResult{}, err
	}
	var expiredQuarantined []garbageFile
	var candidates []garbageFile
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, files := range [][]garbageFile{candidates, expiredQuarantined} {
			for _, file := range files {
				if file.quarantine != "" {
					_ = os.Rename(file.quarantine, file.original)
				}
			}
		}
	}()
	for _, path := range expired {
		name, err := quarantineRootName(r.root, path)
		if err != nil {
			return GCResult{}, err
		}
		quarantine := filepath.Join(quarantineRoot, name)
		if err := os.Rename(path, quarantine); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return GCResult{}, err
		}
		expiredQuarantined = append(expiredQuarantined, garbageFile{original: path, quarantine: quarantine})
		info, err := os.Stat(quarantine)
		if err != nil {
			return GCResult{}, err
		}
		ttl := transactionTTL
		if strings.HasSuffix(path, ".lease") {
			data, err := os.ReadFile(quarantine)
			if err != nil {
				return GCResult{}, err
			}
			_, leaseTTL, err := decodeLease(data)
			if err != nil {
				return GCResult{}, err
			}
			ttl = leaseTTL
		}
		if now.Sub(info.ModTime()) <= ttl {
			if err := os.Rename(quarantine, path); err != nil {
				return GCResult{}, err
			}
			expiredQuarantined[len(expiredQuarantined)-1].quarantine = ""
			continue
		}
	}
	for _, file := range all {
		marked := false
		if file.kind == "manifest" {
			id, parseErr := ParseManifestID(file.id)
			if parseErr != nil {
				return GCResult{}, parseErr
			}
			_, marked = roots.manifests[id]
		} else {
			id, parseErr := ParseID(file.id)
			if parseErr != nil {
				return GCResult{}, parseErr
			}
			_, marked = roots.objects[id]
		}
		if marked {
			continue
		}
		file.quarantine = filepath.Join(quarantineRoot, file.kind+"-"+file.id)
		if err := os.Rename(file.original, file.quarantine); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return GCResult{}, err
		}
		candidates = append(candidates, file)
	}

	// Any root published while candidates were being moved gets the final say.
	latest, _, err := r.scanRoots(now, transactionTTL)
	if err != nil {
		return GCResult{}, err
	}
	for id := range latest.manifests {
		for i := range candidates {
			if candidates[i].kind == "manifest" && candidates[i].id == id.String() {
				if err := os.Rename(candidates[i].quarantine, candidates[i].original); err != nil && !errors.Is(err, os.ErrNotExist) {
					return GCResult{}, err
				}
				candidates[i].quarantine = ""
			}
		}
	}
	if err := r.traceRoots(latest); err != nil {
		return GCResult{}, err
	}
	for id := range latest.objects {
		for i := range candidates {
			if candidates[i].kind == "chunk" && candidates[i].id == id.String() {
				if err := os.Rename(candidates[i].quarantine, candidates[i].original); err != nil && !errors.Is(err, os.ErrNotExist) {
					return GCResult{}, err
				}
				candidates[i].quarantine = ""
			}
		}
	}
	for id := range latest.manifests {
		if _, required := latest.requiredManifests[id]; !required {
			if _, pending := latest.pendingManifests[id]; pending {
				continue
			}
		}
		manifest, err := r.LoadManifest(id)
		if err != nil {
			return GCResult{}, fmt.Errorf("validating rooted memory manifest %s after collection: %w", id, err)
		}
		if _, full := latest.fullManifests[id]; full {
			for _, run := range manifest.Runs {
				if _, wasLocal := initialLocalChunks[run.Object]; !wasLocal {
					continue
				}
				if err := r.chunks.Contains(run.Object); err != nil {
					return GCResult{}, fmt.Errorf("validating locally retained memory object %s for manifest %s after collection: %w", run.Object, id, err)
				}
			}
			continue
		}
		for _, consumed := range latest.branchConsumption[id] {
			for _, run := range manifest.Runs {
				if _, done := consumed[run.Object]; done {
					continue
				}
				if _, wasLocal := initialLocalChunks[run.Object]; !wasLocal {
					continue
				}
				if err := r.chunks.Contains(run.Object); err != nil {
					return GCResult{}, fmt.Errorf("validating retained memory object %s for manifest %s after collection: %w", run.Object, id, err)
				}
			}
		}
	}
	result := GCResult{}
	for _, file := range candidates {
		if file.quarantine == "" {
			continue
		}
		if file.kind == "manifest" {
			result.Manifests++
		} else {
			result.Chunks++
		}
	}
	mutationDirs := make(map[string]struct{})
	for _, files := range [][]garbageFile{candidates, expiredQuarantined} {
		for _, file := range files {
			mutationDirs[filepath.Dir(file.original)] = struct{}{}
		}
	}
	for dir := range mutationDirs {
		if err := syncDirectory(dir); err != nil {
			return GCResult{}, err
		}
	}
	if err := removeGarbageQuarantine(quarantineRoot); err != nil {
		return GCResult{}, err
	}
	committed = true
	for _, file := range expiredQuarantined {
		if file.quarantine == "" {
			continue
		}
		if strings.HasPrefix(file.original, r.leaseRoot+string(os.PathSeparator)) {
			result.ExpiredLeases++
		} else {
			result.ExpiredTransactions++
		}
	}
	if err := syncDirectory(r.trashRoot); err != nil {
		return GCResult{}, err
	}
	return result, nil
}

// removeGarbageQuarantine tolerates only the short directory-coherency window
// that may follow the last close/unlink on NFS. Demand readers and GC are
// serialized by gc.lock, and live readers additionally hold manifest leases,
// so a persistent ENOTEMPTY remains a correctness failure. In particular, we
// do not recognize or ignore any filesystem-generated placeholder filename.
func removeGarbageQuarantine(path string) error {
	deadline := time.Now().Add(time.Second)
	for {
		err := os.RemoveAll(path)
		transientDirectoryState := errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EBUSY)
		if err == nil || !transientDirectoryState || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}
