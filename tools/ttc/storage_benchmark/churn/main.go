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
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"golang.org/x/sys/unix"
)

const pageSize = 4096

type latency struct {
	Count              int `json:"count"`
	P50, P95, P99, Max float64
}
type cycle struct {
	Cycle             int     `json:"cycle"`
	WriteSeconds      float64 `json:"write_seconds"`
	WritesPerSec      float64 `json:"writes_per_sec"`
	SyncLatency       latency `json:"sync_latency_us"`
	DiskBeforeCompact int64   `json:"disk_before_compact"`
	CompactSeconds    float64 `json:"compact_seconds"`
	DiskAfterCompact  int64   `json:"disk_after_compact"`
	ReadOps           uint64  `json:"concurrent_read_ops"`
	ReadErrors        uint64  `json:"concurrent_read_errors"`
	ReadLatency       latency `json:"concurrent_read_latency_us"`
	BlobLive          uint64  `json:"blob_live"`
	BlobReferenced    uint64  `json:"blob_referenced"`
	BlobRewrites      int64   `json:"blob_rewrites"`
}
type report struct {
	Mode                 string `json:"mode"`
	Pages, Cycles, Batch int
	LogicalBytes         int64
	InitialSeconds       float64
	InitialDisk          int64
	Results              []cycle
	EmptyDisk            int64
	EmptyAfterReopen     int64
}

func main() {
	mode := flag.String("mode", "sst", "sst or blob")
	dir := flag.String("dir", "", "database directory")
	pages := flag.Int("pages", 65536, "4 KiB keys in working set")
	cycles := flag.Int("cycles", 4, "full overwrite cycles")
	batchN := flag.Int("batch", 256, "updates per synced batch")
	flag.Parse()
	if *dir == "" {
		*dir = filepath.Join(os.TempDir(), "ttc-pebble-churn-"+*mode)
	}
	_ = os.RemoveAll(*dir)
	cache := pebble.NewCache(128 << 20)
	opts := &pebble.Options{Cache: cache, FormatMajorVersion: pebble.FormatValueSeparation, MemTableSize: 32 << 20, MemTableStopWritesThreshold: 4, L0CompactionThreshold: 2, L0StopWritesThreshold: 12, BytesPerSync: 1 << 20, WALBytesPerSync: 1 << 20}
	if *mode == "blob" {
		opts.Experimental.EnableColumnarBlocks = func() bool { return true }
		opts.Experimental.ValueSeparationPolicy = func() pebble.ValueSeparationPolicy {
			return pebble.ValueSeparationPolicy{Enabled: true, MinimumSize: 1024, MaxBlobReferenceDepth: 8, RewriteMinimumAge: 0, TargetGarbageRatio: .20}
		}
	}
	db, err := pebble.Open(*dir, opts)
	must(err)
	r := report{Mode: *mode, Pages: *pages, Cycles: *cycles, Batch: *batchN, LogicalBytes: int64(*pages * pageSize)}
	t0 := time.Now()
	must(writeCycle(db, *pages, *batchN, 0, nil))
	must(db.Flush())
	must(db.Compact(context.Background(), []byte{0}, []byte{0xff}, true))
	r.InitialSeconds = time.Since(t0).Seconds()
	r.InitialDisk = disk(*dir)
	for c := 1; c <= *cycles; c++ {
		r.Results = append(r.Results, runCycle(db, *dir, *pages, *batchN, c))
	}
	must(db.DeleteRange([]byte{0}, []byte{0xff}, pebble.Sync))
	must(db.Flush())
	must(db.Compact(context.Background(), []byte{0}, []byte{0xff}, true))
	time.Sleep(time.Second)
	must(db.Close())
	cache.Unref()
	r.EmptyDisk = disk(*dir)
	cache = pebble.NewCache(8 << 20)
	db, err = pebble.Open(*dir, &pebble.Options{Cache: cache, FormatMajorVersion: pebble.FormatValueSeparation})
	must(err)
	must(db.Close())
	cache.Unref()
	r.EmptyAfterReopen = disk(*dir)
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
}

func runCycle(db *pebble.DB, dir string, pages, batchN, gen int) cycle {
	var stop atomic.Bool
	var ops, errs atomic.Uint64
	var mu sync.Mutex
	reads := make([]time.Duration, 0, 100000)
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, seed^99))
			for !stop.Load() {
				start := time.Now()
				v, cl, e := db.Get(key(rng.IntN(pages)))
				if e == nil {
					if len(v) != pageSize {
						e = io.ErrUnexpectedEOF
					}
					cl.Close()
				}
				if e != nil {
					errs.Add(1)
				}
				d := time.Since(start)
				mu.Lock()
				if len(reads) < cap(reads) {
					reads = append(reads, d)
				}
				mu.Unlock()
				ops.Add(1)
			}
		}(uint64(gen*10 + g + 1))
	}
	var syncs []time.Duration
	t0 := time.Now()
	must(writeCycle(db, pages, batchN, gen, &syncs))
	writeSec := time.Since(t0).Seconds()
	before := disk(dir)
	t0 = time.Now()
	must(db.Flush())
	must(db.Compact(context.Background(), []byte{0}, []byte{0xff}, true))
	time.Sleep(time.Second)
	compactSec := time.Since(t0).Seconds()
	stop.Store(true)
	wg.Wait()
	m := db.Metrics()
	return cycle{Cycle: gen, WriteSeconds: writeSec, WritesPerSec: float64(pages) / writeSec, SyncLatency: summarize(syncs), DiskBeforeCompact: before, CompactSeconds: compactSec, DiskAfterCompact: disk(dir), ReadOps: ops.Load(), ReadErrors: errs.Load(), ReadLatency: summarize(reads), BlobLive: m.BlobFiles.LiveSize, BlobReferenced: m.BlobFiles.ReferencedValueSize, BlobRewrites: m.Compact.BlobFileRewriteCount}
}

func writeCycle(db *pebble.DB, pages, batchN, gen int, syncs *[]time.Duration) error {
	order := rand.New(rand.NewPCG(uint64(gen+1), uint64(gen+200))).Perm(pages)
	for start := 0; start < pages; start += batchN {
		b := db.NewBatch()
		for _, i := range order[start:min(start+batchN, pages)] {
			if err := b.Set(key(i), page(i, gen), nil); err != nil {
				return err
			}
		}
		t := time.Now()
		if err := b.Commit(pebble.Sync); err != nil {
			return err
		}
		if syncs != nil {
			*syncs = append(*syncs, time.Since(t))
		}
		b.Close()
	}
	return nil
}
func key(i int) []byte {
	b := make([]byte, 9)
	b[0] = 'p'
	binary.BigEndian.PutUint64(b[1:], uint64(i))
	return b
}
func page(i, gen int) []byte {
	b := make([]byte, pageSize)
	x := uint64(i) ^ uint64(gen)*0x9e3779b97f4a7c15
	for j := 0; j < len(b); j += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(b[j:], x)
	}
	return b
}
func summarize(v []time.Duration) latency {
	if len(v) == 0 {
		return latency{}
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	q := func(p float64) float64 { return float64(v[int(float64(len(v)-1)*p)].Nanoseconds()) / 1000 }
	return latency{len(v), q(.5), q(.95), q(.99), q(1)}
}
func disk(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			if s, x := d.Info(); x == nil {
				if sys, ok := s.Sys().(*unix.Stat_t); ok {
					n += sys.Blocks * 512
				} else {
					n += s.Size()
				}
			}
		}
		return nil
	})
	return n
}
func must(e error) {
	if e != nil {
		panic(e)
	}
}
