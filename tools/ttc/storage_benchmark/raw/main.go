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

// Binary raw is a lower-bound comparison for the Pebble TTC-shaped benchmark.
// It deliberately provides no branching or immutable-object semantics: pages
// are offsets in one host file and writes replace them in place.
package main

import (
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

	"golang.org/x/sys/unix"
)

const pageSize = 4096

type latency struct {
	Count int     `json:"count"`
	P50us float64 `json:"p50_us"`
	P95us float64 `json:"p95_us"`
	P99us float64 `json:"p99_us"`
	Maxus float64 `json:"max_us"`
}

type report struct {
	Pages                 int      `json:"pages"`
	LogicalBytes          int64    `json:"logical_bytes"`
	BuildSeconds          float64  `json:"build_seconds"`
	BuildMiBPerSec        float64  `json:"build_mib_per_sec"`
	RootSyncLatency       latency  `json:"root_sync_latency"`
	Updates               int      `json:"updates"`
	SyncUpdates           bool     `json:"sync_updates"`
	UpdateSeconds         float64  `json:"update_seconds"`
	UpdatesPerSec         float64  `json:"updates_per_sec"`
	UpdateCommitLatency   latency  `json:"update_commit_latency"`
	ConcurrentReadOps     uint64   `json:"concurrent_read_ops"`
	ConcurrentReadErrors  uint64   `json:"concurrent_read_errors"`
	WarmReadLatency       latency  `json:"warm_read_latency"`
	WarmReadOpsPerSec     float64  `json:"warm_read_ops_per_sec"`
	ColdReadLatency       latency  `json:"cold_read_latency"`
	ColdReadOpsPerSec     float64  `json:"cold_read_ops_per_sec"`
	AllocatedBytes        int64    `json:"allocated_bytes"`
	ComparisonLimitations []string `json:"comparison_limitations"`
}

func main() {
	dir := flag.String("dir", filepath.Join(os.TempDir(), "ttc-raw-baseline"), "benchmark directory (removed before use)")
	pages := flag.Int("pages", 131072, "base 4 KiB pages")
	updates := flag.Int("updates", 4*4096, "random in-place updates")
	batch := flag.Int("batch", 128, "updates per fdatasync")
	syncUpdates := flag.Bool("sync-updates", true, "fdatasync every update batch")
	roots := flag.Int("roots", 1000, "8-byte root writes, each fdatasync'd")
	warmReads := flag.Int("warm-reads", 30000, "warm random reads")
	coldReads := flag.Int("cold-reads", 10000, "cache-dropped random reads")
	flag.Parse()
	if *pages <= 0 || *batch <= 0 {
		fatalf("pages and batch must be positive")
	}
	if err := os.RemoveAll(*dir); err != nil {
		fatalf("remove: %v", err)
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		fatalf("mkdir: %v", err)
	}
	r, err := run(*dir, *pages, *updates, *batch, *roots, *warmReads, *coldReads, *syncUpdates)
	if err != nil {
		fatalf("benchmark: %v", err)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
}

func run(dir string, pages, updates, batch, roots, warmReads, coldReads int, syncUpdates bool) (*report, error) {
	r := &report{Pages: pages, LogicalBytes: int64(pages * pageSize), Updates: updates, SyncUpdates: syncUpdates}
	dataPath := filepath.Join(dir, "pages.data")
	f, err := os.OpenFile(dataPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}

	// Use the same 4,096-page grouping as the Pebble base builder, while the
	// raw file turns each group into one contiguous host write.
	t0 := time.Now()
	const groupPages = 4096
	for start := 0; start < pages; start += groupPages {
		n := min(groupPages, pages-start)
		buf := make([]byte, n*pageSize)
		for i := 0; i < n; i++ {
			fillPage(buf[i*pageSize:(i+1)*pageSize], uint64(start+i+1))
		}
		if _, err := f.Write(buf); err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := unix.Fdatasync(int(f.Fd())); err != nil {
		f.Close()
		return nil, err
	}
	r.BuildSeconds = time.Since(t0).Seconds()
	r.BuildMiBPerSec = float64(r.LogicalBytes) / (1 << 20) / r.BuildSeconds

	rootFile, err := os.OpenFile(filepath.Join(dir, "root"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		f.Close()
		return nil, err
	}
	// Pebble preallocates its WAL. Do the equivalent here so this measures data
	// synchronization rather than extending-file metadata on every root.
	if err := rootFile.Truncate(int64(roots * 8)); err != nil {
		return nil, err
	}
	rootLats := make([]time.Duration, 0, roots)
	rootBuf := make([]byte, 8)
	for i := 0; i < roots; i++ {
		binary.LittleEndian.PutUint64(rootBuf, uint64(i+1))
		start := time.Now()
		if _, err := rootFile.Write(rootBuf); err != nil {
			return nil, err
		}
		if err := unix.Fdatasync(int(rootFile.Fd())); err != nil {
			return nil, err
		}
		rootLats = append(rootLats, time.Since(start))
	}
	r.RootSyncLatency = summarize(rootLats)
	if err := rootFile.Close(); err != nil {
		return nil, err
	}

	var stop atomic.Bool
	var readOps, readErrs atomic.Uint64
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
			buf := make([]byte, pageSize)
			for !stop.Load() {
				_, err := f.ReadAt(buf, int64(rng.IntN(pages)*pageSize))
				if err != nil {
					readErrs.Add(1)
				}
				readOps.Add(1)
			}
		}(uint64(g + 1))
	}

	rng := rand.New(rand.NewPCG(100, 900))
	page := make([]byte, pageSize)
	commitLats := make([]time.Duration, 0, (updates+batch-1)/batch)
	t0 = time.Now()
	for done := 0; done < updates; {
		n := min(batch, updates-done)
		for j := 0; j < n; j++ {
			index := rng.IntN(pages)
			fillPage(page, uint64(pages+done+j+1))
			if _, err := f.WriteAt(page, int64(index*pageSize)); err != nil {
				return nil, err
			}
		}
		if syncUpdates {
			start := time.Now()
			if err := unix.Fdatasync(int(f.Fd())); err != nil {
				return nil, err
			}
			commitLats = append(commitLats, time.Since(start))
		}
		done += n
	}
	r.UpdateSeconds = time.Since(t0).Seconds()
	r.UpdatesPerSec = float64(updates) / r.UpdateSeconds
	r.UpdateCommitLatency = summarize(commitLats)
	stop.Store(true)
	wg.Wait()
	r.ConcurrentReadOps = readOps.Load()
	r.ConcurrentReadErrors = readErrs.Load()

	r.WarmReadLatency, r.WarmReadOpsPerSec, err = measureReads(f, pages, warmReads, 500)
	if err != nil {
		return nil, err
	}
	if err := unix.Fdatasync(int(f.Fd())); err != nil {
		return nil, err
	}
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	f, err = os.Open(dataPath)
	if err != nil {
		return nil, err
	}
	r.ColdReadLatency, r.ColdReadOpsPerSec, err = measureReads(f, pages, coldReads, 700)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	st, err := os.Stat(dataPath)
	if err != nil {
		return nil, err
	}
	if sys, ok := st.Sys().(*unix.Stat_t); ok {
		r.AllocatedBytes = sys.Blocks * 512
	} else {
		r.AllocatedBytes = st.Size()
	}
	r.ComparisonLimitations = []string{
		"Raw pages are direct offsets in one file; no immutable objects, CoW branches, reachability, checksums, or recovery metadata are provided.",
		"A raw read is one pread; a TTC-shaped Pebble read performs three metadata Gets and one data Get.",
		"Raw writes overwrite pages in place; Pebble preserves inherited versions and publishes new roots.",
	}
	return r, nil
}

func measureReads(f *os.File, pages, count int, seed uint64) (latency, float64, error) {
	rng := rand.New(rand.NewPCG(seed, seed^0x517cc1b727220a95))
	buf := make([]byte, pageSize)
	lats := make([]time.Duration, 0, count)
	t0 := time.Now()
	for i := 0; i < count; i++ {
		start := time.Now()
		n, err := f.ReadAt(buf, int64(rng.IntN(pages)*pageSize))
		lats = append(lats, time.Since(start))
		if err != nil && err != io.EOF {
			return latency{}, 0, err
		}
		if n != pageSize {
			return latency{}, 0, io.ErrUnexpectedEOF
		}
	}
	seconds := time.Since(t0).Seconds()
	return summarize(lats), float64(count) / seconds, nil
}

func fillPage(b []byte, id uint64) {
	x := id*0x9e3779b97f4a7c15 + 0xd1b54a32d192ed03
	for i := 0; i < len(b); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(b[i:], x)
	}
}

func summarize(v []time.Duration) latency {
	if len(v) == 0 {
		return latency{}
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	q := func(p float64) float64 {
		return float64(v[int(float64(len(v)-1)*p)].Nanoseconds()) / 1000
	}
	return latency{Count: len(v), P50us: q(.5), P95us: q(.95), P99us: q(.99), Maxus: q(1)}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "raw benchmark: "+format+"\n", args...)
	os.Exit(1)
}
