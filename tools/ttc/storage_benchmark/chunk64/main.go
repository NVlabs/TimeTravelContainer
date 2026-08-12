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

// Binary chunk64 compares a raw host file with two Pebble 64 KiB layouts:
// 64 KiB values (extent CoW) and sixteen 4 KiB values per 64 KiB SST block.
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
	"strings"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"golang.org/x/sys/unix"
)

const (
	pageSize  = 4 << 10
	chunkSize = 64 << 10
	perChunk  = chunkSize / pageSize
)

type latency struct {
	Count int     `json:"count"`
	P50us float64 `json:"p50_us"`
	P95us float64 `json:"p95_us"`
	P99us float64 `json:"p99_us"`
	Maxus float64 `json:"max_us"`
}

type measurement struct {
	Seconds   float64 `json:"seconds"`
	MiBPerSec float64 `json:"mib_per_sec,omitempty"`
	OpsPerSec float64 `json:"ops_per_sec,omitempty"`
	Latency   latency `json:"latency,omitempty"`
}

type result struct {
	Layout                string      `json:"layout"`
	SequentialWrite       measurement `json:"sequential_write_64k"`
	InitialCompaction     measurement `json:"initial_compaction"`
	SequentialReadCold    measurement `json:"sequential_read_64k_cold"`
	RandomRead4KWarm      measurement `json:"random_read_4k_warm"`
	RandomRead4KCold      measurement `json:"random_read_4k_cold"`
	RandomWrite4KNoSync   measurement `json:"random_write_4k_nosync"`
	RandomWrite4KDurable  measurement `json:"random_write_4k_durable"`
	DurableCommitLatency  latency     `json:"durable_batch_commit_latency"`
	AllocatedAfterBuild   int64       `json:"allocated_after_build"`
	AllocatedAfterUpdates int64       `json:"allocated_after_updates"`
	LogicalBytes          int64       `json:"logical_bytes"`
	PhysicalReadNote      string      `json:"physical_read_note"`
}

type report struct {
	Chunks       int      `json:"chunks"`
	LogicalBytes int64    `json:"logical_bytes"`
	RandomOps    int      `json:"random_ops_per_write_phase"`
	ColdReads    int      `json:"cold_read_ops"`
	Batch        int      `json:"write_batch"`
	Results      []result `json:"results"`
}

func main() {
	dir := flag.String("dir", filepath.Join(os.TempDir(), "ttc-chunk64"), "benchmark directory (removed before use)")
	chunks := flag.Int("chunks", 8192, "64 KiB chunks (8192 = 512 MiB)")
	randomOps := flag.Int("random-ops", 8192, "random 4 KiB writes per sync mode")
	warmReads := flag.Int("warm-reads", 30000, "warm random 4 KiB reads")
	coldReads := flag.Int("cold-reads", 4096, "cache-dropped random 4 KiB reads")
	batch := flag.Int("batch", 128, "writes per batch/durability barrier")
	layoutsFlag := flag.String("layouts", "raw,pebble-64k-value,pebble-4k-values-64k-block,pebble-4k-values-4k-block", "comma-separated layouts")
	flag.Parse()
	if *chunks <= 0 || *randomOps < 0 || *batch <= 0 {
		fatalf("invalid arguments")
	}
	if err := os.RemoveAll(*dir); err != nil {
		fatalf("remove: %v", err)
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		fatalf("mkdir: %v", err)
	}
	r := report{Chunks: *chunks, LogicalBytes: int64(*chunks * chunkSize), RandomOps: *randomOps, ColdReads: *coldReads, Batch: *batch}
	var err error
	for _, layout := range strings.Split(*layoutsFlag, ",") {
		if layout != "raw" && layout != "pebble-64k-value" && layout != "pebble-4k-values-64k-block" && layout != "pebble-4k-values-4k-block" {
			fatalf("unknown layout %q", layout)
		}
		var one result
		if layout == "raw" {
			one, err = runRaw(filepath.Join(*dir, layout), *chunks, *randomOps, *warmReads, *coldReads, *batch)
		} else {
			one, err = runPebble(filepath.Join(*dir, layout), layout, *chunks, *randomOps, *warmReads, *coldReads, *batch)
		}
		if err != nil {
			fatalf("%s: %v", layout, err)
		}
		r.Results = append(r.Results, one)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
}

func runRaw(dir string, chunks, randomOps, warmReads, coldReads, batch int) (result, error) {
	r := result{Layout: "raw", LogicalBytes: int64(chunks * chunkSize), PhysicalReadNote: "pread requests exactly 4 KiB for random reads"}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return r, err
	}
	path := filepath.Join(dir, "data")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return r, err
	}
	buf := make([]byte, chunkSize)
	t0 := time.Now()
	for i := 0; i < chunks; i++ {
		fill(buf, uint64(i+1))
		if _, err := f.Write(buf); err != nil {
			return r, err
		}
	}
	if err := unix.Fdatasync(int(f.Fd())); err != nil {
		return r, err
	}
	r.SequentialWrite = throughput(time.Since(t0), int64(chunks*chunkSize), chunks)
	r.AllocatedAfterBuild = allocated(dir)
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		return r, err
	}
	if err := f.Close(); err != nil {
		return r, err
	}
	f, err = os.Open(path)
	if err != nil {
		return r, err
	}
	r.SequentialReadCold, err = rawSequentialRead(f, chunks)
	if err != nil {
		return r, err
	}
	r.RandomRead4KWarm, err = rawRandomRead(f, chunks, warmReads, 100)
	if err != nil {
		return r, err
	}
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		return r, err
	}
	if err := f.Close(); err != nil {
		return r, err
	}
	f, err = os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return r, err
	}
	r.RandomRead4KCold, err = rawRandomRead(f, chunks, coldReads, 200)
	if err != nil {
		return r, err
	}
	r.RandomWrite4KNoSync, _, err = rawRandomWrite(f, chunks, randomOps, batch, false, 300)
	if err != nil {
		return r, err
	}
	if err := unix.Fdatasync(int(f.Fd())); err != nil {
		return r, err
	}
	r.RandomWrite4KDurable, r.DurableCommitLatency, err = rawRandomWrite(f, chunks, randomOps, batch, true, 400)
	if err != nil {
		return r, err
	}
	if err := f.Close(); err != nil {
		return r, err
	}
	r.AllocatedAfterUpdates = allocated(dir)
	return r, nil
}

func runPebble(dir, layout string, chunks, randomOps, warmReads, coldReads, batch int) (result, error) {
	r := result{Layout: layout, LogicalBytes: int64(chunks * chunkSize)}
	if layout == "pebble-64k-value" {
		r.PhysicalReadNote = "a random 4 KiB request fetches and materializes its complete 64 KiB value/block"
	} else if layout == "pebble-4k-values-64k-block" {
		r.PhysicalReadNote = "a random 4 KiB request returns one value but a cache miss reads its containing 64 KiB SST block"
	} else {
		r.PhysicalReadNote = "a random 4 KiB request returns one value from a 4 KiB target SST block"
	}
	blockSize := chunkSize
	if layout == "pebble-4k-values-4k-block" {
		blockSize = pageSize
	}
	db, cache, err := openPebble(dir, blockSize)
	if err != nil {
		return r, err
	}
	t0 := time.Now()
	b := db.NewBatch()
	values := chunks
	valueSize := chunkSize
	if layout != "pebble-64k-value" {
		values = chunks * perChunk
		valueSize = pageSize
	}
	buf := make([]byte, valueSize)
	for i := 0; i < values; i++ {
		fill(buf, uint64(i+1))
		if err := b.Set(dataKey(i), buf, nil); err != nil {
			return r, err
		}
		if (i+1)%4096 == 0 {
			if err := b.Commit(pebble.NoSync); err != nil {
				return r, err
			}
			b.Close()
			b = db.NewBatch()
		}
	}
	if b.Count() != 0 {
		if err := b.Commit(pebble.Sync); err != nil {
			return r, err
		}
	}
	b.Close()
	if err := db.Flush(); err != nil {
		return r, err
	}
	r.SequentialWrite = throughput(time.Since(t0), int64(chunks*chunkSize), chunks)
	t0 = time.Now()
	if err := db.Compact(context.Background(), []byte{0}, []byte{0xff}, true); err != nil {
		return r, err
	}
	r.InitialCompaction = measurement{Seconds: time.Since(t0).Seconds()}
	r.AllocatedAfterBuild = allocated(dir)
	if err := closePebble(db, cache); err != nil {
		return r, err
	}
	dropCaches(dir)
	db, cache, err = openPebble(dir, blockSize)
	if err != nil {
		return r, err
	}
	r.SequentialReadCold, err = pebbleSequentialRead(db, layout, chunks)
	if err != nil {
		return r, err
	}
	r.RandomRead4KWarm, err = pebbleRandomRead(db, layout, chunks, warmReads, 100)
	if err != nil {
		return r, err
	}
	if err := closePebble(db, cache); err != nil {
		return r, err
	}
	dropCaches(dir)
	db, cache, err = openPebble(dir, blockSize)
	if err != nil {
		return r, err
	}
	r.RandomRead4KCold, err = pebbleRandomRead(db, layout, chunks, coldReads, 200)
	if err != nil {
		return r, err
	}
	r.RandomWrite4KNoSync, _, err = pebbleRandomWrite(db, layout, chunks, randomOps, batch, false, 300)
	if err != nil {
		return r, err
	}
	if err := db.Flush(); err != nil {
		return r, err
	}
	r.RandomWrite4KDurable, r.DurableCommitLatency, err = pebbleRandomWrite(db, layout, chunks, randomOps, batch, true, 400)
	if err != nil {
		return r, err
	}
	if err := db.Flush(); err != nil {
		return r, err
	}
	r.AllocatedAfterUpdates = allocated(dir)
	if err := closePebble(db, cache); err != nil {
		return r, err
	}
	return r, nil
}

func openPebble(dir string, blockSize int) (*pebble.DB, *pebble.Cache, error) {
	cache := pebble.NewCache(128 << 20)
	o := &pebble.Options{Cache: cache, MemTableSize: 32 << 20, MemTableStopWritesThreshold: 4, L0CompactionThreshold: 2, L0StopWritesThreshold: 12, BytesPerSync: 1 << 20, WALBytesPerSync: 1 << 20}
	for i := range o.Levels {
		o.Levels[i].BlockSize = blockSize
		o.Levels[i].IndexBlockSize = blockSize
	}
	db, err := pebble.Open(dir, o)
	if err != nil {
		cache.Unref()
		return nil, nil, err
	}
	return db, cache, nil
}

func closePebble(db *pebble.DB, cache *pebble.Cache) error {
	err := db.Close()
	cache.Unref()
	return err
}

func rawSequentialRead(f *os.File, chunks int) (measurement, error) {
	buf := make([]byte, chunkSize)
	t0 := time.Now()
	for i := 0; i < chunks; i++ {
		if _, err := f.ReadAt(buf, int64(i*chunkSize)); err != nil {
			return measurement{}, err
		}
	}
	return throughput(time.Since(t0), int64(chunks*chunkSize), chunks), nil
}

func rawRandomRead(f *os.File, chunks, ops int, seed uint64) (measurement, error) {
	rng := rand.New(rand.NewPCG(seed, seed^99))
	buf := make([]byte, pageSize)
	lats := make([]time.Duration, 0, ops)
	t0 := time.Now()
	for i := 0; i < ops; i++ {
		page := rng.IntN(chunks * perChunk)
		start := time.Now()
		if _, err := f.ReadAt(buf, int64(page*pageSize)); err != nil {
			return measurement{}, err
		}
		lats = append(lats, time.Since(start))
	}
	return opMeasurement(time.Since(t0), ops, lats), nil
}

func rawRandomWrite(f *os.File, chunks, ops, batch int, sync bool, seed uint64) (measurement, latency, error) {
	rng := rand.New(rand.NewPCG(seed, seed^99))
	buf := make([]byte, pageSize)
	commits := make([]time.Duration, 0, (ops+batch-1)/batch)
	t0 := time.Now()
	for done := 0; done < ops; {
		n := min(batch, ops-done)
		for i := 0; i < n; i++ {
			page := rng.IntN(chunks * perChunk)
			fill(buf, uint64(done+i+1)^seed)
			if _, err := f.WriteAt(buf, int64(page*pageSize)); err != nil {
				return measurement{}, latency{}, err
			}
		}
		if sync {
			start := time.Now()
			if err := unix.Fdatasync(int(f.Fd())); err != nil {
				return measurement{}, latency{}, err
			}
			commits = append(commits, time.Since(start))
		}
		done += n
	}
	d := time.Since(t0)
	return opMeasurement(d, ops, nil), summarize(commits), nil
}

func pebbleSequentialRead(db *pebble.DB, layout string, chunks int) (measurement, error) {
	it, err := db.NewIter(nil)
	if err != nil {
		return measurement{}, err
	}
	defer it.Close()
	t0 := time.Now()
	var bytes int64
	for ok := it.First(); ok; ok = it.Next() {
		v := it.Value()
		bytes += int64(len(v))
		if len(v) == 0 {
			return measurement{}, io.ErrUnexpectedEOF
		}
	}
	if err := it.Error(); err != nil {
		return measurement{}, err
	}
	if bytes != int64(chunks*chunkSize) {
		return measurement{}, fmt.Errorf("read %d bytes, want %d", bytes, chunks*chunkSize)
	}
	return throughput(time.Since(t0), bytes, chunks), nil
}

func pebbleRandomRead(db *pebble.DB, layout string, chunks, ops int, seed uint64) (measurement, error) {
	rng := rand.New(rand.NewPCG(seed, seed^99))
	lats := make([]time.Duration, 0, ops)
	var sink byte
	t0 := time.Now()
	for i := 0; i < ops; i++ {
		page := rng.IntN(chunks * perChunk)
		key := page
		offset := 0
		want := pageSize
		if layout == "pebble-64k-value" {
			key = page / perChunk
			offset = (page % perChunk) * pageSize
			want = chunkSize
		}
		start := time.Now()
		v, closer, err := db.Get(dataKey(key))
		if err != nil {
			return measurement{}, err
		}
		if len(v) != want {
			closer.Close()
			return measurement{}, io.ErrUnexpectedEOF
		}
		sink ^= v[offset]
		closer.Close()
		lats = append(lats, time.Since(start))
	}
	if sink == 0xff {
		fmt.Fprint(io.Discard, sink)
	}
	return opMeasurement(time.Since(t0), ops, lats), nil
}

func pebbleRandomWrite(db *pebble.DB, layout string, chunks, ops, batchN int, sync bool, seed uint64) (measurement, latency, error) {
	rng := rand.New(rand.NewPCG(seed, seed^99))
	commits := make([]time.Duration, 0, (ops+batchN-1)/batchN)
	pageBuf := make([]byte, pageSize)
	t0 := time.Now()
	for done := 0; done < ops; {
		n := min(batchN, ops-done)
		b := db.NewIndexedBatch()
		for i := 0; i < n; i++ {
			page := rng.IntN(chunks * perChunk)
			if layout == "pebble-64k-value" {
				key := dataKey(page / perChunk)
				v, closer, err := b.Get(key)
				if err != nil {
					b.Close()
					return measurement{}, latency{}, err
				}
				copyBuf := append([]byte(nil), v...)
				closer.Close()
				fill(copyBuf[(page%perChunk)*pageSize:(page%perChunk+1)*pageSize], uint64(done+i+1)^seed)
				if err := b.Set(key, copyBuf, nil); err != nil {
					b.Close()
					return measurement{}, latency{}, err
				}
			} else {
				fill(pageBuf, uint64(done+i+1)^seed)
				if err := b.Set(dataKey(page), pageBuf, nil); err != nil {
					b.Close()
					return measurement{}, latency{}, err
				}
			}
		}
		wo := pebble.NoSync
		if sync {
			wo = pebble.Sync
		}
		start := time.Now()
		if err := b.Commit(wo); err != nil {
			b.Close()
			return measurement{}, latency{}, err
		}
		if sync {
			commits = append(commits, time.Since(start))
		}
		b.Close()
		done += n
	}
	return opMeasurement(time.Since(t0), ops, nil), summarize(commits), nil
}

func dataKey(i int) []byte {
	b := make([]byte, 9)
	b[0] = 'd'
	binary.BigEndian.PutUint64(b[1:], uint64(i))
	return b
}

func fill(b []byte, seed uint64) {
	x := seed*0x9e3779b97f4a7c15 + 0xd1b54a32d192ed03
	for i := 0; i < len(b); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(b[i:], x)
	}
}

func throughput(d time.Duration, bytes int64, ops int) measurement {
	seconds := d.Seconds()
	return measurement{Seconds: seconds, MiBPerSec: float64(bytes) / (1 << 20) / seconds, OpsPerSec: float64(ops) / seconds}
}

func opMeasurement(d time.Duration, ops int, lats []time.Duration) measurement {
	return measurement{Seconds: d.Seconds(), OpsPerSec: float64(ops) / d.Seconds(), Latency: summarize(lats)}
}

func summarize(v []time.Duration) latency {
	if len(v) == 0 {
		return latency{}
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	q := func(p float64) float64 { return float64(v[int(float64(len(v)-1)*p)].Nanoseconds()) / 1000 }
	return latency{Count: len(v), P50us: q(.5), P95us: q(.95), P99us: q(.99), Maxus: q(1)}
}

func allocated(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			var st unix.Stat_t
			if e := unix.Stat(path, &st); e == nil {
				n += st.Blocks * 512
			}
		}
		return nil
	})
	return n
}

func dropCaches(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if f, e := os.Open(path); e == nil {
				_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
				f.Close()
			}
		}
		return nil
	})
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "chunk64 benchmark: "+format+"\n", args...)
	os.Exit(1)
}
