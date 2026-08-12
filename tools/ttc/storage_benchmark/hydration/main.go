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

// Binary hydration compares 64 KiB read-then-discard snapshots in Pebble with
// a packed host file whose consumed ranges are reclaimed by hole punching and
// a directory of 64 KiB files that are unlinked after loading.
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
	"github.com/cockroachdb/pebble/v2/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/cockroachdb/pebble/v2/vfs"
	"golang.org/x/sys/unix"
)

const chunkSize = 64 << 10

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

type reclaimPoint struct {
	ConsumedPercent int   `json:"consumed_percent"`
	AllocatedBytes  int64 `json:"allocated_bytes"`
}

type result struct {
	Layout                     string         `json:"layout"`
	Build                      measurement    `json:"build"`
	InitialCompaction          measurement    `json:"initial_compaction,omitempty"`
	AllocatedAfterBuild        int64          `json:"allocated_after_build"`
	RegularFilesAfterBuild     int            `json:"regular_files_after_build"`
	DirectoriesAfterBuild      int            `json:"directories_after_build"`
	SequentialColdRead         measurement    `json:"sequential_64k_cold_read"`
	WarmRandomRead             measurement    `json:"warm_random_64k_read"`
	ColdRandomRead             measurement    `json:"cold_random_64k_read"`
	ColdConcurrentRead         measurement    `json:"cold_random_64k_read_16_workers"`
	RandomConsumeTotal         measurement    `json:"random_read_then_discard_total"`
	RandomConsumeReadLatency   latency        `json:"random_consume_read_latency"`
	DiscardBatchLatency        latency        `json:"discard_batch_sync_latency"`
	Reclamation                []reclaimPoint `json:"reclamation"`
	AllocatedBeforeCompaction  int64          `json:"allocated_before_final_compaction"`
	FinalCompaction            measurement    `json:"final_compaction,omitempty"`
	AllocatedAfterCompaction   int64          `json:"allocated_after_final_compaction"`
	WholeStoreRemoval          measurement    `json:"whole_store_removal"`
	AllocatedAfterStoreRemoval int64          `json:"allocated_after_store_removal"`
}

type report struct {
	Chunks       int      `json:"chunks"`
	LogicalBytes int64    `json:"logical_bytes"`
	ReadOps      int      `json:"random_read_ops"`
	Batch        int      `json:"discard_sync_batch"`
	Results      []result `json:"results"`
}

func main() {
	dir := flag.String("dir", filepath.Join(os.TempDir(), "ttc-hydration"), "benchmark directory (removed before use)")
	chunks := flag.Int("chunks", 4096, "64 KiB chunks (4096 = 256 MiB)")
	readOps := flag.Int("read-ops", 4096, "warm and cold random read operations")
	batch := flag.Int("batch", 128, "discard operations per durability barrier")
	pebbleBuild := flag.String("pebble-build", "ingest", "Pebble snapshot construction: ingest or nowal")
	pebbleDiscard := flag.String("pebble-discard", "whole", "Pebble consumption: whole or tombstone")
	flag.Parse()
	if *chunks <= 0 || *readOps < 0 || *batch <= 0 {
		fatalf("invalid arguments")
	}
	if err := os.RemoveAll(*dir); err != nil {
		fatalf("removing output: %v", err)
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		fatalf("creating output: %v", err)
	}
	r := report{Chunks: *chunks, LogicalBytes: int64(*chunks * chunkSize), ReadOps: *readOps, Batch: *batch}
	fileResult, err := runFile(filepath.Join(*dir, "file"), *chunks, *readOps, *batch)
	if err != nil {
		fatalf("packed file: %v", err)
	}
	r.Results = append(r.Results, fileResult)
	looseResult, err := runLooseFiles(filepath.Join(*dir, "loose"), *chunks, *readOps, *batch)
	if err != nil {
		fatalf("loose files: %v", err)
	}
	r.Results = append(r.Results, looseResult)
	if *pebbleBuild != "ingest" && *pebbleBuild != "nowal" {
		fatalf("invalid --pebble-build %q", *pebbleBuild)
	}
	if *pebbleDiscard != "whole" && *pebbleDiscard != "tombstone" {
		fatalf("invalid --pebble-discard %q", *pebbleDiscard)
	}
	pebbleResult, err := runPebble(filepath.Join(*dir, "pebble"), *chunks, *readOps, *batch, *pebbleBuild, *pebbleDiscard)
	if err != nil {
		fatalf("Pebble: %v", err)
	}
	r.Results = append(r.Results, pebbleResult)
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
}

func runFile(dir string, chunks, readOps, batch int) (result, error) {
	r := result{Layout: "packed-file-punch-hole"}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return r, err
	}
	path := filepath.Join(dir, "snapshot")
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
	r.Build = throughput(time.Since(t0), chunks)
	r.AllocatedAfterBuild = allocated(dir)
	r.RegularFilesAfterBuild, r.DirectoriesAfterBuild = entryCounts(dir)
	if err := dropFileCache(f); err != nil {
		return r, err
	}
	if err := f.Close(); err != nil {
		return r, err
	}
	f, err = os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return r, err
	}
	r.SequentialColdRead, err = fileSequentialRead(f, chunks)
	if err != nil {
		return r, err
	}
	r.WarmRandomRead, err = fileRandomRead(f, chunks, readOps, 101)
	if err != nil {
		return r, err
	}
	if err := dropFileCache(f); err != nil {
		return r, err
	}
	if err := f.Close(); err != nil {
		return r, err
	}
	f, err = os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return r, err
	}
	r.ColdRandomRead, err = fileRandomRead(f, chunks, readOps, 202)
	if err != nil {
		return r, err
	}
	if err := dropFileCache(f); err != nil {
		return r, err
	}
	if err := f.Close(); err != nil {
		return r, err
	}
	f, err = os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return r, err
	}
	r.ColdConcurrentRead, err = fileConcurrentRead(f, chunks, readOps, 16, 252)
	if err != nil {
		return r, err
	}
	if err := dropFileCache(f); err != nil {
		return r, err
	}
	r.RandomConsumeTotal, r.RandomConsumeReadLatency, r.DiscardBatchLatency, r.Reclamation, err = fileConsume(f, dir, chunks, batch)
	if err != nil {
		return r, err
	}
	r.AllocatedBeforeCompaction = allocated(dir)
	r.AllocatedAfterCompaction = r.AllocatedBeforeCompaction
	if err := f.Close(); err != nil {
		return r, err
	}
	t0 = time.Now()
	if err := os.RemoveAll(dir); err != nil {
		return r, err
	}
	r.WholeStoreRemoval = measurement{Seconds: time.Since(t0).Seconds()}
	r.AllocatedAfterStoreRemoval = allocated(dir)
	return r, nil
}

// runLooseFiles models the simplest object-like snapshot: one ordinary host
// file for every 64 KiB transfer unit. Directories contain at most 1024 files
// so large snapshots do not create one unbounded host directory. Snapshot
// construction uses one syncfs after all files are closed; this is durable but
// may flush unrelated dirty data on the same host filesystem.
func runLooseFiles(dir string, chunks, readOps, batch int) (result, error) {
	r := result{Layout: "loose-64k-files"}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return r, err
	}
	for shard := 0; shard <= (chunks-1)/1024; shard++ {
		if err := os.Mkdir(filepath.Join(dir, looseShardName(shard)), 0700); err != nil {
			return r, err
		}
	}
	store, err := openLooseStore(dir, chunks)
	if err != nil {
		return r, err
	}
	defer func() {
		if store != nil {
			store.close()
		}
	}()
	buf := make([]byte, chunkSize)
	t0 := time.Now()
	for i := 0; i < chunks; i++ {
		fill(buf, uint64(i+1))
		if err := store.create(i, buf); err != nil {
			return r, err
		}
	}
	if err := unix.Syncfs(int(store.shards[0].Fd())); err != nil {
		return r, err
	}
	r.Build = throughput(time.Since(t0), chunks)
	r.AllocatedAfterBuild = allocated(dir)
	r.RegularFilesAfterBuild, r.DirectoriesAfterBuild = entryCounts(dir)
	if err := store.dropCaches(chunks); err != nil {
		return r, err
	}
	r.SequentialColdRead, err = looseSequentialRead(store, chunks)
	if err != nil {
		return r, err
	}
	r.WarmRandomRead, err = looseRandomRead(store, chunks, readOps, 101)
	if err != nil {
		return r, err
	}
	if err := store.dropCaches(chunks); err != nil {
		return r, err
	}
	r.ColdRandomRead, err = looseRandomRead(store, chunks, readOps, 202)
	if err != nil {
		return r, err
	}
	if err := store.dropCaches(chunks); err != nil {
		return r, err
	}
	r.ColdConcurrentRead, err = looseConcurrentRead(store, chunks, readOps, 16, 252)
	if err != nil {
		return r, err
	}
	if err := store.dropCaches(chunks); err != nil {
		return r, err
	}
	r.RandomConsumeTotal, r.RandomConsumeReadLatency, r.DiscardBatchLatency, r.Reclamation, err = looseConsume(store, dir, chunks, batch)
	if err != nil {
		return r, err
	}
	r.AllocatedBeforeCompaction = allocated(dir)
	r.AllocatedAfterCompaction = r.AllocatedBeforeCompaction
	if err := store.close(); err != nil {
		return r, err
	}
	store = nil
	t0 = time.Now()
	if err := os.RemoveAll(dir); err != nil {
		return r, err
	}
	r.WholeStoreRemoval = measurement{Seconds: time.Since(t0).Seconds()}
	r.AllocatedAfterStoreRemoval = allocated(dir)
	return r, nil
}

func runPebble(dir string, chunks, readOps, batchN int, buildMode, discardMode string) (result, error) {
	r := result{Layout: "pebble-64k-values-" + buildMode + "-" + discardMode}
	if buildMode == "ingest" {
		db, cache, err := openPebble(dir, false)
		if err != nil {
			return r, err
		}
		t0 := time.Now()
		externalPath := dir + ".snapshot.sst"
		file, err := vfs.Default.Create(externalPath, vfs.WriteCategoryUnspecified)
		if err != nil {
			return r, err
		}
		writer := sstable.NewWriter(objstorageprovider.NewFileWritable(file), sstable.WriterOptions{BlockSize: chunkSize, IndexBlockSize: chunkSize})
		buf := make([]byte, chunkSize)
		for i := 0; i < chunks; i++ {
			fill(buf, uint64(i+1))
			if err := writer.Set(key(i), buf); err != nil {
				writer.Close()
				return r, err
			}
		}
		if err := writer.Close(); err != nil {
			return r, err
		}
		if err := db.Ingest(context.Background(), []string{externalPath}); err != nil {
			return r, err
		}
		r.Build = throughput(time.Since(t0), chunks)
		r.AllocatedAfterBuild = allocated(dir)
		r.RegularFilesAfterBuild, r.DirectoriesAfterBuild = entryCounts(dir)
		if err := closePebble(db, cache); err != nil {
			return r, err
		}
	} else {
		var err error
		// This database is not published until Flush and the surrounding TTC
		// metadata commit succeed. Avoid writing every snapshot byte twice through
		// the WAL; an interrupted unpublished build is discarded as a unit.
		db, cache, err := openPebble(dir, true)
		if err != nil {
			return r, err
		}
		buf := make([]byte, chunkSize)
		b := db.NewBatch()
		t0 := time.Now()
		for i := 0; i < chunks; i++ {
			fill(buf, uint64(i+1))
			if err := b.Set(key(i), buf, nil); err != nil {
				return r, err
			}
			if (i+1)%batchN == 0 {
				if err := b.Commit(pebble.NoSync); err != nil {
					return r, err
				}
				b.Close()
				b = db.NewBatch()
			}
		}
		if b.Count() != 0 {
			if err := b.Commit(pebble.NoSync); err != nil {
				return r, err
			}
		}
		b.Close()
		if err := db.Flush(); err != nil {
			return r, err
		}
		r.Build = throughput(time.Since(t0), chunks)
		t0 = time.Now()
		if err := db.Compact(context.Background(), []byte{0}, []byte{0xff}, true); err != nil {
			return r, err
		}
		r.InitialCompaction = measurement{Seconds: time.Since(t0).Seconds()}
		r.AllocatedAfterBuild = allocated(dir)
		r.RegularFilesAfterBuild, r.DirectoriesAfterBuild = entryCounts(dir)
		if err := closePebble(db, cache); err != nil {
			return r, err
		}
	}
	dropCaches(dir)
	db, cache, err := openPebble(dir, false)
	if err != nil {
		return r, err
	}
	r.SequentialColdRead, err = pebbleSequentialRead(db, chunks)
	if err != nil {
		return r, err
	}
	r.WarmRandomRead, err = pebbleRandomRead(db, chunks, readOps, 101)
	if err != nil {
		return r, err
	}
	if err := closePebble(db, cache); err != nil {
		return r, err
	}
	dropCaches(dir)
	db, cache, err = openPebble(dir, false)
	if err != nil {
		return r, err
	}
	r.ColdRandomRead, err = pebbleRandomRead(db, chunks, readOps, 202)
	if err != nil {
		return r, err
	}
	if err := closePebble(db, cache); err != nil {
		return r, err
	}
	dropCaches(dir)
	db, cache, err = openPebble(dir, false)
	if err != nil {
		return r, err
	}
	r.ColdConcurrentRead, err = pebbleConcurrentRead(db, chunks, readOps, 16, 252)
	if err != nil {
		return r, err
	}
	if err := closePebble(db, cache); err != nil {
		return r, err
	}
	dropCaches(dir)
	db, cache, err = openPebble(dir, false)
	if err != nil {
		return r, err
	}
	if discardMode == "whole" {
		r.RandomConsumeTotal, r.RandomConsumeReadLatency, r.Reclamation, err = pebbleConsumeWhole(db, dir, chunks)
	} else {
		r.RandomConsumeTotal, r.RandomConsumeReadLatency, r.DiscardBatchLatency, r.Reclamation, err = pebbleConsume(db, dir, chunks, batchN)
	}
	if err != nil {
		return r, err
	}
	r.AllocatedBeforeCompaction = allocated(dir)
	if discardMode == "tombstone" {
		if err := db.Flush(); err != nil {
			return r, err
		}
		t0 := time.Now()
		if err := db.Compact(context.Background(), []byte{0}, []byte{0xff}, true); err != nil {
			return r, err
		}
		r.FinalCompaction = measurement{Seconds: time.Since(t0).Seconds()}
		r.AllocatedAfterCompaction = allocated(dir)
	} else {
		r.AllocatedAfterCompaction = r.AllocatedBeforeCompaction
	}
	if err := closePebble(db, cache); err != nil {
		return r, err
	}
	t0 := time.Now()
	if err := os.RemoveAll(dir); err != nil {
		return r, err
	}
	r.WholeStoreRemoval = measurement{Seconds: time.Since(t0).Seconds()}
	r.AllocatedAfterStoreRemoval = allocated(dir)
	return r, nil
}

func openPebble(dir string, disableWAL bool) (*pebble.DB, *pebble.Cache, error) {
	cache := pebble.NewCache(128 << 20)
	o := &pebble.Options{Cache: cache, DisableWAL: disableWAL, MemTableSize: 32 << 20, MemTableStopWritesThreshold: 4, L0CompactionThreshold: 2, L0StopWritesThreshold: 12, BytesPerSync: 1 << 20, WALBytesPerSync: 1 << 20}
	for i := range o.Levels {
		o.Levels[i].BlockSize = chunkSize
		o.Levels[i].IndexBlockSize = chunkSize
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

func fileSequentialRead(f *os.File, chunks int) (measurement, error) {
	buf := make([]byte, chunkSize)
	t0 := time.Now()
	for i := 0; i < chunks; i++ {
		if _, err := f.ReadAt(buf, int64(i*chunkSize)); err != nil {
			return measurement{}, err
		}
	}
	return throughput(time.Since(t0), chunks), nil
}

func pebbleSequentialRead(db *pebble.DB, chunks int) (measurement, error) {
	it, err := db.NewIter(nil)
	if err != nil {
		return measurement{}, err
	}
	defer it.Close()
	t0 := time.Now()
	count := 0
	for ok := it.First(); ok; ok = it.Next() {
		if len(it.Value()) != chunkSize {
			return measurement{}, io.ErrUnexpectedEOF
		}
		count++
	}
	if err := it.Error(); err != nil {
		return measurement{}, err
	}
	if count != chunks {
		return measurement{}, fmt.Errorf("read %d chunks, want %d", count, chunks)
	}
	return throughput(time.Since(t0), chunks), nil
}

func looseSequentialRead(store *looseStore, chunks int) (measurement, error) {
	buf := make([]byte, chunkSize)
	t0 := time.Now()
	for i := 0; i < chunks; i++ {
		if err := store.read(i, buf); err != nil {
			return measurement{}, err
		}
	}
	return throughput(time.Since(t0), chunks), nil
}

func fileRandomRead(f *os.File, chunks, ops int, seed uint64) (measurement, error) {
	rng := rand.New(rand.NewPCG(seed, seed^99))
	buf := make([]byte, chunkSize)
	lats := make([]time.Duration, 0, ops)
	t0 := time.Now()
	for i := 0; i < ops; i++ {
		start := time.Now()
		if _, err := f.ReadAt(buf, int64(rng.IntN(chunks)*chunkSize)); err != nil {
			return measurement{}, err
		}
		lats = append(lats, time.Since(start))
	}
	return opMeasurement(time.Since(t0), ops, lats), nil
}

func pebbleRandomRead(db *pebble.DB, chunks, ops int, seed uint64) (measurement, error) {
	rng := rand.New(rand.NewPCG(seed, seed^99))
	dst := make([]byte, chunkSize)
	lats := make([]time.Duration, 0, ops)
	t0 := time.Now()
	for i := 0; i < ops; i++ {
		start := time.Now()
		v, closer, err := db.Get(key(rng.IntN(chunks)))
		if err != nil {
			return measurement{}, err
		}
		if copy(dst, v) != chunkSize {
			closer.Close()
			return measurement{}, io.ErrUnexpectedEOF
		}
		closer.Close()
		lats = append(lats, time.Since(start))
	}
	return opMeasurement(time.Since(t0), ops, lats), nil
}

func looseRandomRead(store *looseStore, chunks, ops int, seed uint64) (measurement, error) {
	rng := rand.New(rand.NewPCG(seed, seed^99))
	dst := make([]byte, chunkSize)
	lats := make([]time.Duration, 0, ops)
	t0 := time.Now()
	for i := 0; i < ops; i++ {
		start := time.Now()
		if err := store.read(rng.IntN(chunks), dst); err != nil {
			return measurement{}, err
		}
		lats = append(lats, time.Since(start))
	}
	return opMeasurement(time.Since(t0), ops, lats), nil
}

func fileConcurrentRead(f *os.File, chunks, ops, workers int, seed uint64) (measurement, error) {
	return concurrentRead(chunks, ops, workers, seed, func(chunk int, dst []byte) error {
		_, err := f.ReadAt(dst, int64(chunk*chunkSize))
		return err
	})
}

func pebbleConcurrentRead(db *pebble.DB, chunks, ops, workers int, seed uint64) (measurement, error) {
	return concurrentRead(chunks, ops, workers, seed, func(chunk int, dst []byte) error {
		v, closer, err := db.Get(key(chunk))
		if err != nil {
			return err
		}
		if copy(dst, v) != chunkSize {
			closer.Close()
			return io.ErrUnexpectedEOF
		}
		return closer.Close()
	})
}

func looseConcurrentRead(store *looseStore, chunks, ops, workers int, seed uint64) (measurement, error) {
	return concurrentRead(chunks, ops, workers, seed, func(chunk int, dst []byte) error {
		return store.read(chunk, dst)
	})
}

func concurrentRead(chunks, ops, workers int, seed uint64, read func(int, []byte) error) (measurement, error) {
	order := make([]int, 0, ops)
	rng := rand.New(rand.NewPCG(seed, seed^99))
	for len(order) < ops {
		p := rng.Perm(chunks)
		order = append(order, p[:min(len(p), ops-len(order))]...)
	}
	var next atomic.Int64
	lats := make([]time.Duration, ops)
	var firstErr error
	var errMu sync.Mutex
	var wg sync.WaitGroup
	t0 := time.Now()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := make([]byte, chunkSize)
			for {
				i := int(next.Add(1) - 1)
				if i >= ops {
					return
				}
				start := time.Now()
				err := read(order[i], dst)
				lats[i] = time.Since(start)
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return measurement{}, firstErr
	}
	return opMeasurement(time.Since(t0), ops, lats), nil
}

func fileConsume(f *os.File, dir string, chunks, batch int) (measurement, latency, latency, []reclaimPoint, error) {
	order := rand.New(rand.NewPCG(303, 404)).Perm(chunks)
	buf := make([]byte, chunkSize)
	reads := make([]time.Duration, 0, chunks)
	syncs := make([]time.Duration, 0, (chunks+batch-1)/batch)
	var points []reclaimPoint
	nextPoint := 25
	t0 := time.Now()
	for i, chunk := range order {
		start := time.Now()
		if _, err := f.ReadAt(buf, int64(chunk*chunkSize)); err != nil {
			return measurement{}, latency{}, latency{}, nil, err
		}
		reads = append(reads, time.Since(start))
		if err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, int64(chunk*chunkSize), chunkSize); err != nil {
			return measurement{}, latency{}, latency{}, nil, err
		}
		if (i+1)%batch == 0 || i+1 == chunks {
			start = time.Now()
			if err := unix.Fdatasync(int(f.Fd())); err != nil {
				return measurement{}, latency{}, latency{}, nil, err
			}
			syncs = append(syncs, time.Since(start))
		}
		percent := (i + 1) * 100 / chunks
		if percent >= nextPoint {
			points = append(points, reclaimPoint{ConsumedPercent: nextPoint, AllocatedBytes: allocated(dir)})
			nextPoint += 25
		}
	}
	return opMeasurement(time.Since(t0), chunks, nil), summarize(reads), summarize(syncs), points, nil
}

func pebbleConsume(db *pebble.DB, dir string, chunks, batchN int) (measurement, latency, latency, []reclaimPoint, error) {
	order := rand.New(rand.NewPCG(303, 404)).Perm(chunks)
	dst := make([]byte, chunkSize)
	reads := make([]time.Duration, 0, chunks)
	commits := make([]time.Duration, 0, (chunks+batchN-1)/batchN)
	var points []reclaimPoint
	nextPoint := 25
	t0 := time.Now()
	for done := 0; done < chunks; {
		n := min(batchN, chunks-done)
		b := db.NewBatch()
		for i := 0; i < n; i++ {
			chunk := order[done+i]
			start := time.Now()
			v, closer, err := db.Get(key(chunk))
			if err != nil {
				return measurement{}, latency{}, latency{}, nil, err
			}
			if copy(dst, v) != chunkSize {
				closer.Close()
				return measurement{}, latency{}, latency{}, nil, io.ErrUnexpectedEOF
			}
			closer.Close()
			reads = append(reads, time.Since(start))
			if err := b.Delete(key(chunk), nil); err != nil {
				return measurement{}, latency{}, latency{}, nil, err
			}
		}
		start := time.Now()
		if err := b.Commit(pebble.Sync); err != nil {
			return measurement{}, latency{}, latency{}, nil, err
		}
		commits = append(commits, time.Since(start))
		b.Close()
		done += n
		percent := done * 100 / chunks
		if percent >= nextPoint {
			points = append(points, reclaimPoint{ConsumedPercent: nextPoint, AllocatedBytes: allocated(dir)})
			nextPoint += 25
		}
	}
	return opMeasurement(time.Since(t0), chunks, nil), summarize(reads), summarize(commits), points, nil
}

func pebbleConsumeWhole(db *pebble.DB, dir string, chunks int) (measurement, latency, []reclaimPoint, error) {
	order := rand.New(rand.NewPCG(303, 404)).Perm(chunks)
	dst := make([]byte, chunkSize)
	reads := make([]time.Duration, 0, chunks)
	var points []reclaimPoint
	nextPoint := 25
	t0 := time.Now()
	for i, chunk := range order {
		start := time.Now()
		v, closer, err := db.Get(key(chunk))
		if err != nil {
			return measurement{}, latency{}, nil, err
		}
		if copy(dst, v) != chunkSize {
			closer.Close()
			return measurement{}, latency{}, nil, io.ErrUnexpectedEOF
		}
		closer.Close()
		reads = append(reads, time.Since(start))
		percent := (i + 1) * 100 / chunks
		if percent >= nextPoint {
			points = append(points, reclaimPoint{ConsumedPercent: nextPoint, AllocatedBytes: allocated(dir)})
			nextPoint += 25
		}
	}
	return opMeasurement(time.Since(t0), chunks, nil), summarize(reads), points, nil
}

func looseConsume(store *looseStore, dir string, chunks, batch int) (measurement, latency, latency, []reclaimPoint, error) {
	order := rand.New(rand.NewPCG(303, 404)).Perm(chunks)
	dst := make([]byte, chunkSize)
	reads := make([]time.Duration, 0, chunks)
	syncs := make([]time.Duration, 0, (chunks+batch-1)/batch)
	dirtyShards := make(map[int]struct{})
	var points []reclaimPoint
	nextPoint := 25
	t0 := time.Now()
	for i, chunk := range order {
		start := time.Now()
		if err := store.read(chunk, dst); err != nil {
			return measurement{}, latency{}, latency{}, nil, err
		}
		reads = append(reads, time.Since(start))
		if err := store.unlink(chunk); err != nil {
			return measurement{}, latency{}, latency{}, nil, err
		}
		dirtyShards[chunk/1024] = struct{}{}
		if (i+1)%batch == 0 || i+1 == chunks {
			start = time.Now()
			for shard := range dirtyShards {
				if err := store.shards[shard].Sync(); err != nil {
					return measurement{}, latency{}, latency{}, nil, err
				}
			}
			syncs = append(syncs, time.Since(start))
			clear(dirtyShards)
		}
		percent := (i + 1) * 100 / chunks
		if percent >= nextPoint {
			points = append(points, reclaimPoint{ConsumedPercent: nextPoint, AllocatedBytes: allocated(dir)})
			nextPoint += 25
		}
	}
	return opMeasurement(time.Since(t0), chunks, nil), summarize(reads), summarize(syncs), points, nil
}

type looseStore struct {
	shards []*os.File
}

func openLooseStore(dir string, chunks int) (*looseStore, error) {
	s := &looseStore{}
	for shard := 0; shard <= (chunks-1)/1024; shard++ {
		f, err := os.Open(filepath.Join(dir, looseShardName(shard)))
		if err != nil {
			s.close()
			return nil, err
		}
		s.shards = append(s.shards, f)
	}
	return s, nil
}

func (s *looseStore) close() error {
	var firstErr error
	for _, f := range s.shards {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *looseStore) create(chunk int, src []byte) error {
	fd, err := unix.Openat(int(s.shards[chunk/1024].Fd()), looseFileName(chunk), unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), looseFileName(chunk))
	_, writeErr := f.Write(src)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func (s *looseStore) read(chunk int, dst []byte) error {
	fd, err := unix.Openat(int(s.shards[chunk/1024].Fd()), looseFileName(chunk), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), looseFileName(chunk))
	_, readErr := io.ReadFull(f, dst)
	closeErr := f.Close()
	if readErr != nil {
		return readErr
	}
	return closeErr
}

func (s *looseStore) unlink(chunk int) error {
	return unix.Unlinkat(int(s.shards[chunk/1024].Fd()), looseFileName(chunk), 0)
}

func (s *looseStore) dropCaches(chunks int) error {
	for i := 0; i < chunks; i++ {
		fd, err := unix.Openat(int(s.shards[i/1024].Fd()), looseFileName(i), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		if err := unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED); err != nil {
			unix.Close(fd)
			return err
		}
		if err := unix.Close(fd); err != nil {
			return err
		}
	}
	return nil
}

func looseShardName(shard int) string {
	return fmt.Sprintf("%08x", shard)
}

func looseFileName(chunk int) string {
	return fmt.Sprintf("%016x.page", chunk)
}

func key(i int) []byte {
	b := make([]byte, 9)
	b[0] = 'p'
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

func throughput(d time.Duration, chunks int) measurement {
	seconds := d.Seconds()
	return measurement{Seconds: seconds, MiBPerSec: float64(chunks*chunkSize) / (1 << 20) / seconds, OpsPerSec: float64(chunks) / seconds}
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
	var total int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			var st unix.Stat_t
			if e := unix.Stat(path, &st); e == nil {
				total += st.Blocks * 512
			}
		}
		return nil
	})
	return total
}

func entryCounts(dir string) (files, directories int) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			directories++
		} else {
			files++
		}
		return nil
	})
	return files, directories
}

func dropFileCache(f *os.File) error {
	if err := unix.Fdatasync(int(f.Fd())); err != nil {
		return err
	}
	return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
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
	fmt.Fprintf(os.Stderr, "hydration benchmark: "+format+"\n", args...)
	os.Exit(1)
}
