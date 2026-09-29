// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cockroachdb/pebble/v2"
)

func main() {
	mode := flag.String("mode", "sst", "sst or blob")
	dir := flag.String("dir", "", "database directory")
	worker := flag.Bool("worker", false, "internal")
	rounds := flag.Int("rounds", 50, "kill/recover rounds")
	flag.Parse()
	if *dir == "" {
		*dir = filepath.Join(os.TempDir(), "ttc-pebble-crash-"+*mode)
	}
	if *worker {
		runWorker(*dir, *mode)
		return
	}
	_ = os.RemoveAll(*dir)
	exe, e := os.Executable()
	must(e)
	var last uint64
	t0 := time.Now()
	for i := 0; i < *rounds; i++ {
		cmd := exec.Command(exe, "--worker", "--dir", *dir, "--mode", *mode)
		stdout, e := cmd.StdoutPipe()
		must(e)
		cmd.Stderr = io.Discard
		must(cmd.Start())
		// Do not spend most of the rounds killing the process in Pebble.Open.
		// Wait until recovery is complete, then kill at a random write boundary.
		ready, e := bufio.NewReader(stdout).ReadString('\n')
		must(e)
		if ready != "ready\n" {
			panic(fmt.Sprintf("unexpected worker handshake %q", ready))
		}
		time.Sleep(time.Duration(rand.IntN(6)) * time.Millisecond)
		must(cmd.Process.Signal(syscall.SIGKILL))
		_ = cmd.Wait()
		seq, e := recoverDB(*dir, *mode)
		must(e)
		if seq < last {
			panic(fmt.Sprintf("root moved backward: %d -> %d", last, seq))
		}
		last = seq
	}
	b, _ := json.MarshalIndent(map[string]any{"mode": *mode, "rounds": *rounds, "final_committed_root": last, "seconds": time.Since(t0).Seconds()}, "", "  ")
	fmt.Println(string(b))
}

func opts(mode string) *pebble.Options {
	o := &pebble.Options{FormatMajorVersion: pebble.FormatValueSeparation, MemTableSize: 1 << 20, MemTableStopWritesThreshold: 2, L0CompactionThreshold: 2, L0StopWritesThreshold: 12}
	if mode == "blob" {
		o.Experimental.EnableColumnarBlocks = func() bool { return true }
		o.Experimental.ValueSeparationPolicy = func() pebble.ValueSeparationPolicy {
			return pebble.ValueSeparationPolicy{Enabled: true, MinimumSize: 1024, MaxBlobReferenceDepth: 4, RewriteMinimumAge: 0, TargetGarbageRatio: .2}
		}
	}
	return o
}
func runWorker(dir, mode string) {
	db, e := pebble.Open(dir, opts(mode))
	must(e)
	var seq uint64
	if v, c, e := db.Get([]byte("root")); e == nil {
		seq = binary.LittleEndian.Uint64(v)
		c.Close()
	}
	fmt.Println("ready")
	for {
		seq++
		b := db.NewBatch()
		p := payload(seq, 4096)
		n := payload(seq, 512)
		must(b.Set(key('p', seq), p, nil))
		must(b.Set(key('a', seq), n, nil))
		must(b.Set(key('b', seq), n, nil))
		must(b.Set(key('c', seq), n, nil))
		must(b.Set([]byte("root"), u64(seq), nil))
		must(b.Commit(pebble.Sync))
		b.Close()
	}
}
func recoverDB(dir, mode string) (uint64, error) {
	db, e := pebble.Open(dir, opts(mode))
	if e != nil {
		return 0, e
	}
	defer db.Close()
	v, c, e := db.Get([]byte("root"))
	if e == pebble.ErrNotFound {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	seq := binary.LittleEndian.Uint64(v)
	c.Close()
	for _, p := range []byte{'p', 'a', 'b', 'c'} {
		v, c, e = db.Get(key(p, seq))
		if e != nil {
			return 0, e
		}
		if len(v) < 8 || binary.LittleEndian.Uint64(v) != seq {
			c.Close()
			return 0, fmt.Errorf("root %d has corrupt %c", seq, p)
		}
		c.Close()
	}
	return seq, nil
}
func key(p byte, v uint64) []byte {
	b := make([]byte, 9)
	b[0] = p
	binary.BigEndian.PutUint64(b[1:], v)
	return b
}
func u64(v uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); return b }
func payload(v uint64, n int) []byte {
	b := make([]byte, n)
	for i := 0; i < n; i += 8 {
		binary.LittleEndian.PutUint64(b[i:], v)
	}
	return b
}
func must(e error) {
	if e != nil {
		panic(e)
	}
}
