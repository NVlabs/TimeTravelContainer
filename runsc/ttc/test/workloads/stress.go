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

// stress is a static Linux workload used to exercise TTC while branches are
// being forked, checkpointed, dehydrated, and migrated. It intentionally uses
// only Linux ABI and ordinary application interfaces; it has no knowledge of
// the selected Sentry backend.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Keep individual workload operations on the same deadline used by TTC's
// Linux lifecycle tests. Shorter ad-hoc deadlines turn temporary saturation
// during fork, restore, or migration into false backend failures.
const workloadOperationTimeout = 15 * time.Second

type counters struct {
	syscalls atomic.Uint64
	memory   atomic.Uint64
	files    atomic.Uint64
	fdRights atomic.Uint64
	process  atomic.Uint64
	network  atomic.Uint64
	signals  atomic.Uint64
	sync     atomic.Uint64
}

type result struct {
	DurationMS int64             `json:"duration_ms"`
	Counters   map[string]uint64 `json:"counters"`
	PID        int               `json:"pid"`
	GoVersion  string            `json:"go_version"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		child()
		return
	}
	duration := flag.Duration("duration", 30*time.Second, "stress duration")
	root := flag.String("root", "/tmp/ttc-diverse-stress", "filesystem working directory")
	workers := flag.Int("workers", max(2, runtime.GOMAXPROCS(0)/2), "syscall and synchronization workers")
	memoryMiB := flag.Int("memory-mib", 8, "private anonymous bytes churned by the memory feature, in MiB")
	featuresFlag := flag.String("features", "all", "comma-separated feature groups or all")
	flag.Parse()
	if *duration <= 0 || *workers <= 0 || *memoryMiB <= 0 || uint64(*memoryMiB) > uint64(^uint(0)>>1)/(1<<20) {
		fatalf("duration, workers, and memory-mib must be positive and representable")
	}
	if err := os.RemoveAll(*root); err != nil {
		fatalf("cleaning root: %v", err)
	}
	if err := os.MkdirAll(*root, 0o755); err != nil {
		fatalf("creating root: %v", err)
	}
	featureNames := []string{"syscalls", "memory", "files", "fd_rights", "processes", "network", "signals", "sync"}
	features := make(map[string]bool, len(featureNames))
	if *featuresFlag == "all" {
		for _, name := range featureNames {
			features[name] = true
		}
	} else {
		for _, name := range strings.Split(*featuresFlag, ",") {
			if name = strings.TrimSpace(name); name != "" {
				features[name] = true
			}
		}
		for name := range features {
			known := false
			for _, candidate := range featureNames {
				if name == candidate {
					known = true
					break
				}
			}
			if !known {
				fatalf("unknown feature group %q", name)
			}
		}
		if len(features) == 0 {
			fatalf("at least one feature group is required")
		}
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	var counts counters
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	start := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				select {
				case errCh <- fmt.Errorf("%s: %w", name, err):
					cancel()
				default:
				}
			}
		}()
	}
	for i := 0; i < *workers; i++ {
		if features["syscalls"] {
			start("syscalls", func(ctx context.Context) error { return syscallLoop(ctx, &counts) })
		}
		if features["sync"] {
			start("synchronization", func(ctx context.Context) error { return synchronizationLoop(ctx, &counts) })
		}
	}
	if features["memory"] {
		start("memory", func(ctx context.Context) error { return memoryLoop(ctx, *memoryMiB<<20, &counts) })
	}
	if features["files"] {
		start("files", func(ctx context.Context) error { return fileLoop(ctx, *root, &counts) })
	}
	if features["fd_rights"] {
		start("SCM_RIGHTS", func(ctx context.Context) error { return fdRightsLoop(ctx, &counts) })
	}
	if features["processes"] {
		start("processes", func(ctx context.Context) error { return processLoop(ctx, &counts) })
	}
	if features["network"] {
		start("network", func(ctx context.Context) error { return networkLoop(ctx, &counts) })
	}
	if features["signals"] {
		start("signals", func(ctx context.Context) error { return signalLoop(ctx, &counts) })
	}
	workloadsDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(workloadsDone)
	}()
	select {
	case <-workloadsDone:
	case <-ctx.Done():
		select {
		case <-workloadsDone:
		case <-time.After(workloadOperationTimeout):
			stacks := make([]byte, 1<<20)
			length := runtime.Stack(stacks, true)
			fatalf("workloads did not stop within 15 seconds after cancellation\n%s", stacks[:length])
		}
	}
	select {
	case err := <-errCh:
		fatalf("%v", err)
	default:
	}

	output := result{
		DurationMS: time.Since(started).Milliseconds(),
		PID:        os.Getpid(),
		GoVersion:  runtime.Version(),
		Counters: map[string]uint64{
			"syscalls":  counts.syscalls.Load(),
			"memory":    counts.memory.Load(),
			"files":     counts.files.Load(),
			"fd_rights": counts.fdRights.Load(),
			"processes": counts.process.Load(),
			"network":   counts.network.Load(),
			"signals":   counts.signals.Load(),
			"sync":      counts.sync.Load(),
		},
	}
	for name, value := range output.Counters {
		if features[name] && value == 0 {
			fatalf("workload %s made no progress", name)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		fatalf("encoding result: %v", err)
	}
}

func syscallLoop(ctx context.Context, counts *counters) error {
	wantPID := uintptr(os.Getpid())
	for ctx.Err() == nil {
		pid, _, errno := syscall.RawSyscall(unix.SYS_GETPID, 0, 0, 0)
		if errno != 0 || pid != wantPID {
			return fmt.Errorf("getpid=(%d,%v), want %d", pid, errno, wantPID)
		}
		if _, _, errno := syscall.RawSyscall(unix.SYS_GETTID, 0, 0, 0); errno != 0 {
			return fmt.Errorf("gettid: %v", errno)
		}
		var ts unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
			return fmt.Errorf("clock_gettime: %w", err)
		}
		counts.syscalls.Add(3)
	}
	return ctx.Err()
}

func memoryLoop(ctx context.Context, length int, counts *counters) error {
	const cancellationCheckBytes = 1 << 20
	for ctx.Err() == nil {
		memory, err := unix.Mmap(-1, 0, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
		if err != nil {
			return fmt.Errorf("mmap: %w", err)
		}
		for offset := 0; offset < len(memory); offset += os.Getpagesize() {
			if offset%cancellationCheckBytes == 0 && ctx.Err() != nil {
				return errors.Join(ctx.Err(), unix.Munmap(memory))
			}
			memory[offset] = byte(offset >> 12)
		}
		if err := unix.Mprotect(memory, unix.PROT_READ); err != nil {
			unix.Munmap(memory)
			return fmt.Errorf("mprotect read: %w", err)
		}
		var sum byte
		for offset := 0; offset < len(memory); offset += os.Getpagesize() {
			if offset%cancellationCheckBytes == 0 && ctx.Err() != nil {
				return errors.Join(ctx.Err(), unix.Munmap(memory))
			}
			sum ^= memory[offset]
		}
		if err := unix.Mprotect(memory, unix.PROT_READ|unix.PROT_WRITE); err != nil {
			unix.Munmap(memory)
			return fmt.Errorf("mprotect write: %w", err)
		}
		memory[len(memory)-1] ^= sum + 1
		if err := unix.Madvise(memory, unix.MADV_DONTNEED); err != nil {
			unix.Munmap(memory)
			return fmt.Errorf("madvise: %w", err)
		}
		if memory[0] != 0 || memory[len(memory)-1] != 0 {
			unix.Munmap(memory)
			return errors.New("MADV_DONTNEED retained private anonymous data")
		}
		if err := unix.Munmap(memory); err != nil {
			return fmt.Errorf("munmap: %w", err)
		}
		counts.memory.Add(uint64(length / os.Getpagesize()))
	}
	return ctx.Err()
}

func fileLoop(ctx context.Context, root string, counts *counters) error {
	const length = 256 << 10
	path := filepath.Join(root, "shared")
	renamed := filepath.Join(root, "renamed")
	linked := filepath.Join(root, "linked")
	iteration := uint64(0)
	for ctx.Err() == nil {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
		if err != nil {
			return err
		}
		if err := file.Truncate(length); err != nil {
			file.Close()
			return err
		}
		first, err := unix.Mmap(int(file.Fd()), 0, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			file.Close()
			return fmt.Errorf("first shared mmap: %w", err)
		}
		second, err := unix.Mmap(int(file.Fd()), 0, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			unix.Munmap(first)
			file.Close()
			return fmt.Errorf("second shared mmap: %w", err)
		}
		value := byte(iteration)
		for offset := 0; offset < length; offset += os.Getpagesize() {
			first[offset] = value ^ byte(offset>>12)
			if second[offset] != first[offset] {
				unix.Munmap(second)
				unix.Munmap(first)
				file.Close()
				return fmt.Errorf("shared mapping incoherent at %#x", offset)
			}
		}
		if err := unix.Msync(first, unix.MS_SYNC); err != nil {
			unix.Munmap(second)
			unix.Munmap(first)
			file.Close()
			return fmt.Errorf("msync: %w", err)
		}
		if err := unix.Munmap(second); err != nil {
			unix.Munmap(first)
			file.Close()
			return err
		}
		if err := unix.Munmap(first); err != nil {
			file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := os.Rename(path, renamed); err != nil {
			return err
		}
		_ = os.Remove(linked)
		if err := os.Link(renamed, linked); err != nil {
			return err
		}
		data, err := os.ReadFile(linked)
		if err != nil {
			return err
		}
		if len(data) != length || data[0] != value {
			return fmt.Errorf("linked file content length=%d first=%d want=%d", len(data), data[0], value)
		}
		if err := os.Remove(linked); err != nil {
			return err
		}
		if err := os.Remove(renamed); err != nil {
			return err
		}
		iteration++
		counts.files.Add(length / uint64(os.Getpagesize()))
	}
	return ctx.Err()
}

func fdRightsLoop(ctx context.Context, counts *counters) error {
	for ctx.Err() == nil {
		sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return err
		}
		pipeFDs := make([]int, 2)
		if err := unix.Pipe2(pipeFDs, unix.O_CLOEXEC); err != nil {
			unix.Close(sockets[0])
			unix.Close(sockets[1])
			return err
		}
		readFD, writeFD := pipeFDs[0], pipeFDs[1]
		token := []byte("rights-token")
		if _, err := unix.Write(writeFD, token); err != nil {
			return err
		}
		if err := unix.Sendmsg(sockets[0], []byte{1}, unix.UnixRights(readFD), nil, 0); err != nil {
			return err
		}
		payload := make([]byte, 1)
		control := make([]byte, unix.CmsgSpace(4))
		_, controlLength, _, _, err := unix.Recvmsg(sockets[1], payload, control, 0)
		if err != nil {
			return err
		}
		messages, err := unix.ParseSocketControlMessage(control[:controlLength])
		if err != nil || len(messages) != 1 {
			return fmt.Errorf("control messages=%d err=%v", len(messages), err)
		}
		rights, err := unix.ParseUnixRights(&messages[0])
		if err != nil || len(rights) != 1 {
			return fmt.Errorf("rights=%v err=%v", rights, err)
		}
		got := make([]byte, len(token))
		received := os.NewFile(uintptr(rights[0]), "received-right")
		_, err = io.ReadFull(received, got)
		closeErr := received.Close()
		unix.Close(readFD)
		unix.Close(writeFD)
		unix.Close(sockets[0])
		unix.Close(sockets[1])
		if err != nil || !bytes.Equal(got, token) {
			return fmt.Errorf("received right data=%q err=%v", got, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close received right: %w", closeErr)
		}
		counts.fdRights.Add(1)
	}
	return ctx.Err()
}

func processLoop(ctx context.Context, counts *counters) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	iteration := uint64(0)
	for ctx.Err() == nil {
		readFile, writeFile, err := os.Pipe()
		if err != nil {
			return err
		}
		token := strconv.FormatUint(iteration, 10)
		cmd := exec.CommandContext(ctx, executable, "child", token)
		cmd.Env = append(os.Environ(), "TTC_STRESS_CHILD="+token)
		cmd.ExtraFiles = []*os.File{writeFile}
		if err := cmd.Start(); err != nil {
			readFile.Close()
			writeFile.Close()
			return err
		}
		writeFile.Close()
		readDeadline := time.Now().Add(workloadOperationTimeout)
		if deadline, ok := ctx.Deadline(); ok && deadline.Before(readDeadline) {
			readDeadline = deadline
		}
		if err := readFile.SetReadDeadline(readDeadline); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			readFile.Close()
			return fmt.Errorf("setting child result deadline: %w", err)
		}
		got := make([]byte, len(token))
		_, readErr := io.ReadFull(readFile, got)
		readFile.Close()
		waitErr := cmd.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if readErr != nil || waitErr != nil || string(got) != token {
			return fmt.Errorf("child token=%q read=%q read_err=%v wait_err=%v", token, got, readErr, waitErr)
		}
		iteration++
		counts.process.Add(1)
	}
	return ctx.Err()
}

func child() {
	if len(os.Args) != 3 || os.Getenv("TTC_STRESS_CHILD") != os.Args[2] {
		os.Exit(91)
	}
	file := os.NewFile(3, "result")
	if file == nil {
		os.Exit(92)
	}
	if _, err := io.WriteString(file, os.Args[2]); err != nil {
		os.Exit(93)
	}
	if err := file.Close(); err != nil {
		os.Exit(94)
	}
}

func networkLoop(ctx context.Context, counts *counters) error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					select {
					case serverErr <- nil:
					default:
					}
				} else {
					select {
					case serverErr <- err:
					default:
					}
				}
				return
			}
			go func(connection net.Conn) {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(workloadOperationTimeout))
				_, err := io.Copy(connection, connection)
				if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
					select {
					case serverErr <- err:
					default:
					}
				}
			}(connection)
		}
	}()
	payload := bytes.Repeat([]byte{0x5a}, 16<<10)
	for ctx.Err() == nil {
		connection, err := net.DialTimeout("tcp4", listener.Addr().String(), workloadOperationTimeout)
		if err != nil {
			return err
		}
		_ = connection.SetDeadline(time.Now().Add(workloadOperationTimeout))
		if _, err := connection.Write(payload); err != nil {
			connection.Close()
			return err
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(connection, got); err != nil {
			connection.Close()
			return err
		}
		connection.Close()
		if !bytes.Equal(got, payload) {
			return errors.New("TCP echo payload changed")
		}
		counts.network.Add(1)
		select {
		case err := <-serverErr:
			if err != nil {
				return err
			}
		default:
		}
	}
	listener.Close()
	return ctx.Err()
}

func signalLoop(ctx context.Context, counts *counters) error {
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGUSR1)
	defer signal.Stop(signals)
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := unix.Kill(os.Getpid(), unix.SIGUSR1); err != nil {
				return err
			}
			select {
			case <-signals:
				counts.signals.Add(1)
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(workloadOperationTimeout):
				return errors.New("SIGUSR1 was not delivered")
			}
		}
	}
}

func synchronizationLoop(ctx context.Context, counts *counters) error {
	var mu sync.Mutex
	condition := sync.NewCond(&mu)
	ready := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			mu.Lock()
			for !ready && ctx.Err() == nil {
				condition.Wait()
			}
			ready = false
			mu.Unlock()
			counts.sync.Add(1)
		}
	}()
	for ctx.Err() == nil {
		mu.Lock()
		ready = true
		condition.Signal()
		mu.Unlock()
		runtime.Gosched()
	}
	mu.Lock()
	ready = true
	condition.Broadcast()
	mu.Unlock()
	<-done
	return ctx.Err()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "stress: "+format+"\n", args...)
	os.Exit(1)
}
