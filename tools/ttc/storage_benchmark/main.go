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

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"golang.org/x/sys/unix"
)

const (
	pageSize = 4096
	fanout   = 64
	depth    = 3
)

type config struct {
	Mode             string
	Dir              string
	SyncUpdates      bool
	Pages            int
	Branches         int
	UpdatesPerBranch int
	WarmReads        int
	ColdReads        int
	BatchUpdates     int
	Forks            int
	CacheMiB         int
}

type latency struct {
	Count int     `json:"count"`
	P50us float64 `json:"p50_us"`
	P95us float64 `json:"p95_us"`
	P99us float64 `json:"p99_us"`
	Maxus float64 `json:"max_us"`
}

type diskStats struct {
	AllocatedBytes int64            `json:"allocated_bytes"`
	ApparentBytes  int64            `json:"apparent_bytes"`
	AllocatedByExt map[string]int64 `json:"allocated_by_extension"`
	ApparentByExt  map[string]int64 `json:"apparent_by_extension"`
}

type metricStats struct {
	DiskSpace       uint64 `json:"pebble_disk_space_bytes"`
	WALBytesIn      uint64 `json:"wal_bytes_in"`
	WALBytesWritten uint64 `json:"wal_bytes_written"`
	TableWritten    uint64 `json:"table_bytes_written"`
	BlobWritten     uint64 `json:"blob_bytes_written"`
	BlobLive        uint64 `json:"blob_live_bytes"`
	BlobReferenced  uint64 `json:"blob_referenced_value_bytes"`
	BlobRewrites    int64  `json:"blob_rewrite_count"`
	Compactions     int64  `json:"compaction_count"`
	ReadAmp         int    `json:"read_amplification"`
}

type report struct {
	Config               config      `json:"config"`
	Started              time.Time   `json:"started"`
	BaseLogicalBytes     int64       `json:"base_logical_bytes"`
	BaseBuildSeconds     float64     `json:"base_build_seconds"`
	BaseBuildMiBPerSec   float64     `json:"base_build_mib_per_sec"`
	BaseDisk             diskStats   `json:"base_disk"`
	ForkSyncLatency      latency     `json:"fork_sync_latency"`
	BranchUpdateSeconds  float64     `json:"branch_update_seconds"`
	BranchUpdatesPerSec  float64     `json:"branch_updates_per_sec"`
	BranchCommitLatency  latency     `json:"branch_commit_latency"`
	ConcurrentReadOps    uint64      `json:"concurrent_read_ops"`
	ConcurrentReadErrors uint64      `json:"concurrent_read_errors"`
	DivergedDisk         diskStats   `json:"diverged_disk"`
	WarmReadLatency      latency     `json:"warm_page_lookup_latency"`
	WarmReadOpsPerSec    float64     `json:"warm_page_lookups_per_sec"`
	WarmDirectLatency    latency     `json:"warm_direct_data_get_latency"`
	WarmDirectOpsPerSec  float64     `json:"warm_direct_data_gets_per_sec"`
	RestartSeconds       float64     `json:"restart_seconds"`
	ColdReadLatency      latency     `json:"cold_page_lookup_latency"`
	ColdReadOpsPerSec    float64     `json:"cold_page_lookups_per_sec"`
	ColdDirectLatency    latency     `json:"cold_direct_data_get_latency"`
	ColdDirectOpsPerSec  float64     `json:"cold_direct_data_gets_per_sec"`
	DeleteSeconds        float64     `json:"delete_branch_keys_seconds"`
	AfterDeleteDisk      diskStats   `json:"after_delete_before_compact_disk"`
	CompactSeconds       float64     `json:"compact_seconds"`
	AfterCompactDisk     diskStats   `json:"after_compact_disk"`
	EmptyCompactSeconds  float64     `json:"empty_compact_seconds"`
	EmptyDisk            diskStats   `json:"empty_disk"`
	MetricsBeforeDelete  metricStats `json:"metrics_before_delete"`
	MetricsAfterCompact  metricStats `json:"metrics_after_compact"`
	Notes                []string    `json:"notes"`
}

type getter interface {
	Get([]byte) ([]byte, io.Closer, error)
}

type branchAlloc struct {
	pages []uint64
	nodes []uint64
}

type store struct {
	db       *pebble.DB
	cache    *pebble.Cache
	dir      string
	mode     string
	nextPage atomic.Uint64
	nextNode atomic.Uint64
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.Mode, "mode", "sst", "sst or blob")
	flag.StringVar(&cfg.Dir, "dir", "", "database directory")
	flag.BoolVar(&cfg.SyncUpdates, "sync-updates", true, "sync every branch-update batch")
	flag.IntVar(&cfg.Pages, "pages", 131072, "base 4 KiB pages")
	flag.IntVar(&cfg.Branches, "branches", 4, "diverging branches")
	flag.IntVar(&cfg.UpdatesPerBranch, "updates", 4096, "random updates per branch")
	flag.IntVar(&cfg.WarmReads, "warm-reads", 30000, "warm random page lookups")
	flag.IntVar(&cfg.ColdReads, "cold-reads", 10000, "cold random page lookups")
	flag.IntVar(&cfg.BatchUpdates, "batch-updates", 128, "updates per durable branch commit")
	flag.IntVar(&cfg.Forks, "forks", 40, "durable root-only forks")
	flag.IntVar(&cfg.CacheMiB, "cache-mib", 128, "Pebble block cache MiB")
	flag.Parse()
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(os.TempDir(), "ttc-pebble-"+cfg.Mode)
	}
	if cfg.Mode != "sst" && cfg.Mode != "blob" {
		fatalf("invalid mode %q", cfg.Mode)
	}
	if cfg.Pages <= 0 || cfg.Pages > fanout*fanout*fanout {
		fatalf("pages must be in 1..%d", fanout*fanout*fanout)
	}
	if err := os.RemoveAll(cfg.Dir); err != nil {
		fatalf("remove dir: %v", err)
	}

	r, err := run(cfg)
	if err != nil {
		fatalf("benchmark: %v", err)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
}

func run(cfg config) (*report, error) {
	ctx := context.Background()
	r := &report{Config: cfg, Started: time.Now(), BaseLogicalBytes: int64(cfg.Pages * pageSize)}
	s, err := openStore(cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if s.db != nil {
			_ = s.close()
		}
	}()

	t0 := time.Now()
	baseRoot, err := s.buildBase(ctx, cfg.Pages)
	if err != nil {
		return nil, err
	}
	if err := s.db.Flush(); err != nil {
		return nil, err
	}
	r.BaseBuildSeconds = time.Since(t0).Seconds()
	r.BaseBuildMiBPerSec = float64(r.BaseLogicalBytes) / (1 << 20) / r.BaseBuildSeconds
	r.BaseDisk, _ = diskUsage(cfg.Dir)

	var forkLats []time.Duration
	for i := 0; i < cfg.Forks; i++ {
		name := fmt.Sprintf("probe-%06d", i)
		start := time.Now()
		if err := s.setRoot(name, baseRoot, pebble.Sync); err != nil {
			return nil, err
		}
		forkLats = append(forkLats, time.Since(start))
	}
	r.ForkSyncLatency = summarize(forkLats)
	// Remove probe roots asynchronously; they contain no unique data.
	b := s.db.NewBatch()
	for i := 0; i < cfg.Forks; i++ {
		_ = b.Delete(rootKey(fmt.Sprintf("probe-%06d", i)), nil)
	}
	if err := b.Commit(pebble.NoSync); err != nil {
		b.Close()
		return nil, err
	}
	b.Close()

	branchRoots := make([]uint64, cfg.Branches)
	allocs := make([]branchAlloc, cfg.Branches)
	for i := range branchRoots {
		branchRoots[i] = baseRoot
		if err := s.setRoot(fmt.Sprintf("b%d", i), baseRoot, pebble.Sync); err != nil {
			return nil, err
		}
	}

	var stopReaders atomic.Bool
	var readOps, readErrs atomic.Uint64
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
			for !stopReaders.Load() {
				if err := s.verifyPage(s.db, baseRoot, rng.IntN(cfg.Pages)); err != nil {
					readErrs.Add(1)
				}
				readOps.Add(1)
			}
		}(uint64(g + 1))
	}

	var commitLats []time.Duration
	t0 = time.Now()
	for bi := 0; bi < cfg.Branches; bi++ {
		rng := rand.New(rand.NewPCG(uint64(100+bi), uint64(900+bi)))
		root := branchRoots[bi]
		for done := 0; done < cfg.UpdatesPerBranch; {
			n := min(cfg.BatchUpdates, cfg.UpdatesPerBranch-done)
			ib := s.db.NewIndexedBatch()
			for j := 0; j < n; j++ {
				idx := rng.IntN(cfg.Pages)
				var err error
				root, err = s.updatePage(ib, root, idx, &allocs[bi])
				if err != nil {
					ib.Close()
					return nil, err
				}
			}
			if err := ib.Set(rootKey(fmt.Sprintf("b%d", bi)), u64(root), nil); err != nil {
				ib.Close()
				return nil, err
			}
			writeOptions := pebble.NoSync
			if cfg.SyncUpdates {
				writeOptions = pebble.Sync
			}
			start := time.Now()
			if err := ib.Commit(writeOptions); err != nil {
				ib.Close()
				return nil, err
			}
			commitLats = append(commitLats, time.Since(start))
			ib.Close()
			done += n
		}
		branchRoots[bi] = root
	}
	r.BranchUpdateSeconds = time.Since(t0).Seconds()
	r.BranchUpdatesPerSec = float64(cfg.Branches*cfg.UpdatesPerBranch) / r.BranchUpdateSeconds
	r.BranchCommitLatency = summarize(commitLats)
	stopReaders.Store(true)
	wg.Wait()
	r.ConcurrentReadOps = readOps.Load()
	r.ConcurrentReadErrors = readErrs.Load()
	if err := s.db.Flush(); err != nil {
		return nil, err
	}
	r.DivergedDisk, _ = diskUsage(cfg.Dir)
	r.MetricsBeforeDelete = captureMetrics(s.db.Metrics())

	allRoots := append([]uint64{baseRoot}, branchRoots...)
	r.WarmReadLatency, r.WarmReadOpsPerSec, err = measureReads(s, allRoots, cfg.Pages, cfg.WarmReads, 500)
	if err != nil {
		return nil, err
	}
	r.WarmDirectLatency, r.WarmDirectOpsPerSec, err = measureDirectReads(s.db, cfg.Pages, cfg.WarmReads, 600)
	if err != nil {
		return nil, err
	}

	if err := s.close(); err != nil {
		return nil, err
	}
	dropCaches(cfg.Dir)
	t0 = time.Now()
	s, err = openStore(cfg)
	if err != nil {
		return nil, err
	}
	r.RestartSeconds = time.Since(t0).Seconds()
	r.ColdDirectLatency, r.ColdDirectOpsPerSec, err = measureDirectReads(s.db, cfg.Pages, cfg.ColdReads, 800)
	if err != nil {
		return nil, err
	}
	// Drop caches again so the direct-data probe does not warm the subsequent
	// full-tree lookup measurement.
	if err := s.close(); err != nil {
		return nil, err
	}
	dropCaches(cfg.Dir)
	s, err = openStore(cfg)
	if err != nil {
		return nil, err
	}
	baseRoot, err = s.getRoot("main")
	if err != nil {
		return nil, err
	}
	branchRoots = branchRoots[:0]
	for i := 0; i < cfg.Branches; i++ {
		root, err := s.getRoot(fmt.Sprintf("b%d", i))
		if err != nil {
			return nil, err
		}
		branchRoots = append(branchRoots, root)
	}
	allRoots = append([]uint64{baseRoot}, branchRoots...)
	r.ColdReadLatency, r.ColdReadOpsPerSec, err = measureReads(s, allRoots, cfg.Pages, cfg.ColdReads, 700)
	if err != nil {
		return nil, err
	}

	t0 = time.Now()
	for i := range allocs {
		if err := s.deleteAlloc(fmt.Sprintf("b%d", i), allocs[i]); err != nil {
			return nil, err
		}
	}
	r.DeleteSeconds = time.Since(t0).Seconds()
	if err := s.db.Flush(); err != nil {
		return nil, err
	}
	r.AfterDeleteDisk, _ = diskUsage(cfg.Dir)
	t0 = time.Now()
	if err := s.db.Compact(ctx, []byte{0}, []byte{0xff}, true); err != nil {
		return nil, err
	}
	// Give the blob rewrite scheduler an opportunity, then compact references again.
	time.Sleep(2 * time.Second)
	if err := s.db.Compact(ctx, []byte{0}, []byte{0xff}, true); err != nil {
		return nil, err
	}
	time.Sleep(time.Second)
	r.CompactSeconds = time.Since(t0).Seconds()
	r.AfterCompactDisk, _ = diskUsage(cfg.Dir)
	r.MetricsAfterCompact = captureMetrics(s.db.Metrics())

	// Final shrink-to-empty check.
	t0 = time.Now()
	if err := s.db.DeleteRange([]byte{0}, []byte{0xff}, pebble.Sync); err != nil {
		return nil, err
	}
	if err := s.db.Flush(); err != nil {
		return nil, err
	}
	if err := s.db.Compact(ctx, []byte{0}, []byte{0xff}, true); err != nil {
		return nil, err
	}
	time.Sleep(2 * time.Second)
	if err := s.db.Compact(ctx, []byte{0}, []byte{0xff}, true); err != nil {
		return nil, err
	}
	r.EmptyCompactSeconds = time.Since(t0).Seconds()
	if err := s.close(); err != nil {
		return nil, err
	}
	r.EmptyDisk, _ = diskUsage(cfg.Dir)
	r.Notes = []string{
		"Each page lookup traverses three immutable 512-byte radix nodes and then reads one 4 KiB page.",
		"Direct-data Get measurements isolate Pebble value lookup from the three-level TTC page-map traversal.",
		"Each branch update CoWs one page and three radix nodes.",
		"Fork latency is a synced write of one named root; it is independent of page count.",
		"Branch deletion explicitly deletes all branch-private immutable objects, then forces compaction.",
		fmt.Sprintf("Branch update batches use sync=%t.", cfg.SyncUpdates),
	}
	return r, nil
}

func openStore(cfg config) (*store, error) {
	cache := pebble.NewCache(int64(cfg.CacheMiB) << 20)
	opts := &pebble.Options{
		Cache:                       cache,
		FormatMajorVersion:          pebble.FormatValueSeparation,
		MemTableSize:                32 << 20,
		MemTableStopWritesThreshold: 4,
		L0CompactionThreshold:       2,
		L0StopWritesThreshold:       12,
		BytesPerSync:                1 << 20,
		WALBytesPerSync:             1 << 20,
		MaxOpenFiles:                256,
	}
	if cfg.Mode == "blob" {
		opts.Experimental.EnableColumnarBlocks = func() bool { return true }
		opts.Experimental.ValueSeparationPolicy = func() pebble.ValueSeparationPolicy {
			return pebble.ValueSeparationPolicy{
				Enabled: true, MinimumSize: 1024, MaxBlobReferenceDepth: 8,
				RewriteMinimumAge: 0, TargetGarbageRatio: 0.20,
			}
		}
	}
	db, err := pebble.Open(cfg.Dir, opts)
	if err != nil {
		cache.Unref()
		return nil, err
	}
	s := &store{db: db, cache: cache, dir: cfg.Dir, mode: cfg.Mode}
	// Counters are persisted implicitly by choosing values above this benchmark's fixed base IDs.
	s.nextPage.Store(uint64(cfg.Pages + 1))
	baseNodes := uint64((cfg.Pages+fanout-1)/fanout + (cfg.Pages+fanout*fanout-1)/(fanout*fanout) + 1)
	s.nextNode.Store(baseNodes + 1)
	return s, nil
}

func (s *store) close() error {
	err := s.db.Close()
	s.cache.Unref()
	s.db = nil
	s.cache = nil
	return err
}

func (s *store) buildBase(ctx context.Context, pages int) (uint64, error) {
	_ = ctx
	const pagesPerBatch = 4096
	for start := 0; start < pages; start += pagesPerBatch {
		b := s.db.NewBatch()
		end := min(start+pagesPerBatch, pages)
		for i := start; i < end; i++ {
			id := uint64(i + 1)
			if err := b.Set(pageKey(id), makePage(id), nil); err != nil {
				b.Close()
				return 0, err
			}
		}
		if err := b.Commit(pebble.NoSync); err != nil {
			b.Close()
			return 0, err
		}
		b.Close()
	}

	nextNode := uint64(1)
	leaves := make([]uint64, (pages+fanout-1)/fanout)
	b := s.db.NewBatch()
	for li := range leaves {
		id := nextNode
		nextNode++
		leaves[li] = id
		entries := make([]uint64, fanout)
		for j := 0; j < fanout; j++ {
			idx := li*fanout + j
			if idx < pages {
				entries[j] = uint64(idx + 1)
			}
		}
		if err := b.Set(nodeKey(id), encodeNode(entries), nil); err != nil {
			b.Close()
			return 0, err
		}
	}
	mids := make([]uint64, (len(leaves)+fanout-1)/fanout)
	for mi := range mids {
		id := nextNode
		nextNode++
		mids[mi] = id
		entries := make([]uint64, fanout)
		copy(entries, leaves[mi*fanout:min((mi+1)*fanout, len(leaves))])
		if err := b.Set(nodeKey(id), encodeNode(entries), nil); err != nil {
			b.Close()
			return 0, err
		}
	}
	root := nextNode
	nextNode++
	rootEntries := make([]uint64, fanout)
	copy(rootEntries, mids)
	if err := b.Set(nodeKey(root), encodeNode(rootEntries), nil); err != nil {
		b.Close()
		return 0, err
	}
	if err := b.Set(rootKey("main"), u64(root), nil); err != nil {
		b.Close()
		return 0, err
	}
	if err := b.Commit(pebble.Sync); err != nil {
		b.Close()
		return 0, err
	}
	b.Close()
	s.nextNode.Store(nextNode)
	return root, nil
}

func (s *store) updatePage(b *pebble.Batch, root uint64, index int, a *branchAlloc) (uint64, error) {
	r, err := readNode(b, root)
	if err != nil {
		return 0, err
	}
	ri, mi, li := index/(fanout*fanout), (index/fanout)%fanout, index%fanout
	midID := r[ri]
	m, err := readNode(b, midID)
	if err != nil {
		return 0, err
	}
	leafID := m[mi]
	l, err := readNode(b, leafID)
	if err != nil {
		return 0, err
	}

	pageID := s.nextPage.Add(1) - 1
	newLeaf := s.nextNode.Add(1) - 1
	newMid := s.nextNode.Add(1) - 1
	newRoot := s.nextNode.Add(1) - 1
	l[li] = pageID
	m[mi] = newLeaf
	r[ri] = newMid
	if err := b.Set(pageKey(pageID), makePage(pageID), nil); err != nil {
		return 0, err
	}
	if err := b.Set(nodeKey(newLeaf), encodeNode(l), nil); err != nil {
		return 0, err
	}
	if err := b.Set(nodeKey(newMid), encodeNode(m), nil); err != nil {
		return 0, err
	}
	if err := b.Set(nodeKey(newRoot), encodeNode(r), nil); err != nil {
		return 0, err
	}
	a.pages = append(a.pages, pageID)
	a.nodes = append(a.nodes, newLeaf, newMid, newRoot)
	return newRoot, nil
}

func (s *store) verifyPage(g getter, root uint64, index int) error {
	r, err := readNode(g, root)
	if err != nil {
		return err
	}
	m, err := readNode(g, r[index/(fanout*fanout)])
	if err != nil {
		return err
	}
	l, err := readNode(g, m[(index/fanout)%fanout])
	if err != nil {
		return err
	}
	pid := l[index%fanout]
	v, c, err := g.Get(pageKey(pid))
	if err != nil {
		return err
	}
	if len(v) != pageSize || binary.LittleEndian.Uint64(v[:8]) != pid {
		c.Close()
		return fmt.Errorf("bad page %d", pid)
	}
	return c.Close()
}

func measureReads(s *store, roots []uint64, pages, count int, seed uint64) (latency, float64, error) {
	rng := rand.New(rand.NewPCG(seed, seed^0x517cc1b727220a95))
	lats := make([]time.Duration, 0, count)
	t0 := time.Now()
	for i := 0; i < count; i++ {
		start := time.Now()
		if err := s.verifyPage(s.db, roots[rng.IntN(len(roots))], rng.IntN(pages)); err != nil {
			return latency{}, 0, err
		}
		lats = append(lats, time.Since(start))
	}
	d := time.Since(t0).Seconds()
	return summarize(lats), float64(count) / d, nil
}

func measureDirectReads(g getter, pages, count int, seed uint64) (latency, float64, error) {
	rng := rand.New(rand.NewPCG(seed, seed^0x517cc1b727220a95))
	lats := make([]time.Duration, 0, count)
	t0 := time.Now()
	for i := 0; i < count; i++ {
		start := time.Now()
		v, closer, err := g.Get(pageKey(uint64(rng.IntN(pages) + 1)))
		lats = append(lats, time.Since(start))
		if err != nil {
			return latency{}, 0, err
		}
		if len(v) != pageSize {
			closer.Close()
			return latency{}, 0, io.ErrUnexpectedEOF
		}
		closer.Close()
	}
	seconds := time.Since(t0).Seconds()
	return summarize(lats), float64(count) / seconds, nil
}

func (s *store) deleteAlloc(branch string, a branchAlloc) error {
	const perBatch = 10000
	keys := make([][]byte, 0, len(a.pages)+len(a.nodes)+1)
	keys = append(keys, rootKey(branch))
	for _, id := range a.pages {
		keys = append(keys, pageKey(id))
	}
	for _, id := range a.nodes {
		keys = append(keys, nodeKey(id))
	}
	for start := 0; start < len(keys); start += perBatch {
		b := s.db.NewBatch()
		for _, k := range keys[start:min(start+perBatch, len(keys))] {
			if err := b.Delete(k, nil); err != nil {
				b.Close()
				return err
			}
		}
		opts := pebble.NoSync
		if start+perBatch >= len(keys) {
			opts = pebble.Sync
		}
		if err := b.Commit(opts); err != nil {
			b.Close()
			return err
		}
		b.Close()
	}
	return nil
}

func (s *store) setRoot(name string, root uint64, opts *pebble.WriteOptions) error {
	return s.db.Set(rootKey(name), u64(root), opts)
}
func (s *store) getRoot(name string) (uint64, error) {
	v, c, err := s.db.Get(rootKey(name))
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if len(v) != 8 {
		return 0, errors.New("invalid root")
	}
	return binary.LittleEndian.Uint64(v), nil
}

func readNode(g getter, id uint64) ([]uint64, error) {
	v, c, err := g.Get(nodeKey(id))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if len(v) != fanout*8 {
		return nil, fmt.Errorf("node %d size %d", id, len(v))
	}
	n := make([]uint64, fanout)
	for i := range n {
		n[i] = binary.LittleEndian.Uint64(v[i*8:])
	}
	return n, nil
}

func makePage(id uint64) []byte {
	b := make([]byte, pageSize)
	binary.LittleEndian.PutUint64(b, id)
	x := id ^ 0x9e3779b97f4a7c15
	for i := 8; i < len(b); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(b[i:], x)
	}
	return b
}

func encodeNode(n []uint64) []byte {
	b := make([]byte, fanout*8)
	for i, v := range n {
		binary.LittleEndian.PutUint64(b[i*8:], v)
	}
	return b
}
func u64(v uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); return b }
func pageKey(id uint64) []byte {
	b := make([]byte, 9)
	b[0] = 'p'
	binary.BigEndian.PutUint64(b[1:], id)
	return b
}
func nodeKey(id uint64) []byte {
	b := make([]byte, 9)
	b[0] = 'n'
	binary.BigEndian.PutUint64(b[1:], id)
	return b
}
func rootKey(s string) []byte { return []byte("r/" + s) }

func summarize(v []time.Duration) latency {
	if len(v) == 0 {
		return latency{}
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	q := func(p float64) float64 { i := int(float64(len(v)-1) * p); return float64(v[i].Nanoseconds()) / 1000 }
	return latency{Count: len(v), P50us: q(.50), P95us: q(.95), P99us: q(.99), Maxus: q(1)}
}

func captureMetrics(m *pebble.Metrics) metricStats {
	var table, blob uint64
	for _, l := range m.Levels {
		table += l.TableBytesFlushed + l.TableBytesCompacted
		blob += l.BlobBytesFlushed + l.BlobBytesCompacted
	}
	return metricStats{m.DiskSpaceUsage(), m.WAL.BytesIn, m.WAL.BytesWritten, table, blob, m.BlobFiles.LiveSize, m.BlobFiles.ReferencedValueSize, m.Compact.BlobFileRewriteCount, m.Compact.Count, m.ReadAmp()}
}

func diskUsage(dir string) (diskStats, error) {
	d := diskStats{AllocatedByExt: map[string]int64{}, ApparentByExt: map[string]int64{}}
	err := filepath.WalkDir(dir, func(path string, ent os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ent.IsDir() {
			return nil
		}
		st, err := ent.Info()
		if err != nil {
			return err
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == "" {
			ext = filepath.Base(path)
		}
		allocated := st.Size()
		if sys, ok := st.Sys().(*unix.Stat_t); ok {
			allocated = sys.Blocks * 512
		}
		d.AllocatedBytes += allocated
		d.ApparentBytes += st.Size()
		d.AllocatedByExt[ext] += allocated
		d.ApparentByExt[ext] += st.Size()
		return nil
	})
	return d, err
}

func dropCaches(dir string) {
	_ = filepath.WalkDir(dir, func(path string, ent os.DirEntry, err error) error {
		if err != nil || ent.IsDir() {
			return nil
		}
		f, e := os.Open(path)
		if e == nil {
			_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
			_ = f.Close()
		}
		return nil
	})
}

func fatalf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }
