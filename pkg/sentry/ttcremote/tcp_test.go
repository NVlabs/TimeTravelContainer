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

package ttcremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

type blockingSnapshotReaderAt struct {
	snapshot *pgalloc.RemoteSnapshot
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (r *blockingSnapshotReaderAt) ReadAt(dst []byte, off int64) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	n, err := r.snapshot.CopyStreamAt(dst, off)
	if err == nil {
		err = r.snapshot.AcknowledgeStreamRange(uint64(off), uint64(len(dst)))
	}
	return n, err
}

func (r *blockingSnapshotReaderAt) Close() error { return r.snapshot.Close() }

type dropFirstControlListener struct {
	net.Listener
	message tcpMessageType
	once    sync.Once
}

func (l *dropFirstControlListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &dropFirstControlConn{Conn: conn, listener: l}, nil
}

type dropFirstControlConn struct {
	net.Conn
	listener *dropFirstControlListener
}

func (c *dropFirstControlConn) Write(data []byte) (int, error) {
	if len(data) == tcpHeaderSize && string(data[:4]) == string(tcpMagic[:]) && tcpMessageType(data[5]) == c.listener.message {
		drop := false
		c.listener.once.Do(func() { drop = true })
		if drop {
			// Pretend the response was written, but make the peer observe loss.
			// The retry must replay the same transaction on a new connection.
			_ = c.Conn.Close()
			return len(data), nil
		}
	}
	return c.Conn.Write(data)
}

func newTCPTestMemoryFile(t *testing.T, pages uint64) (*pgalloc.MemoryFile, memmap.FileRange, []byte) {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-tcp-pages-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	fr, err := mf.Allocate(pages*hostarch.PageSize, pgalloc.AllocOpts{
		Kind: usage.Anonymous,
		Mode: pgalloc.AllocateAndWritePopulate,
	})
	if err != nil {
		mf.Destroy()
		t.Fatal(err)
	}
	data := make([]byte, fr.Length())
	for i := range data {
		data[i] = byte((i*29 + i/int(hostarch.PageSize)) & 0xff)
	}
	dst, err := mf.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := safemem.CopySeq(dst, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data))); err != nil || n != fr.Length() {
		t.Fatalf("fill source: n=%d err=%v", n, err)
	}
	t.Cleanup(func() {
		mf.DecRef(fr)
		mf.Destroy()
	})
	return mf, fr, data
}

func newEmptyTCPTestMemoryFile(t *testing.T) *pgalloc.MemoryFile {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-tcp-target-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DelayedEviction: pgalloc.DelayedEvictionDisabled, DisableIMAWorkAround: true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	return mf
}

func newTCPPushPolicyTestSession(t *testing.T, pageCount uint64) (*pgalloc.MemoryFile, memmap.FileRange, *pgalloc.RemoteSnapshot, *pgalloc.AsyncPagesFileLoad) {
	t.Helper()
	source, sourceFR, _ := newTCPTestMemoryFile(t, pageCount)
	allocator, regions, loads, err := pgalloc.ExportMemoryFileMetadataForRemote(contexttest.Context(t), source)
	if err != nil {
		t.Fatal(err)
	}
	target := newEmptyTCPTestMemoryFile(t)
	if err := pgalloc.ImportMemoryFileMetadataForRemote(contexttest.Context(t), target, allocator); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		target.DecRef(sourceFR)
		target.Destroy()
	})
	snapshot, err := pgalloc.NewRemoteSnapshot(regions)
	if err != nil {
		t.Fatal(err)
	}
	reader := pgalloc.NewLoopbackRemoteReader(snapshot, 4*hostarch.PageSize, 1, 2)
	pages, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, loads, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	t.Cleanup(func() {
		pages.Cancel()
		_ = pages.Wait()
	})
	return source, sourceFR, snapshot, pages
}

func startTCPTestServer(t *testing.T, snapshot *pgalloc.RemoteSnapshot, token SnapshotToken, maxRead uint64) (*TCPPageServer, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewTCPPageServer(listener, token, snapshot, maxRead)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	return server, done
}

func testSnapshotToken() SnapshotToken {
	return SnapshotToken{0x54, 0x54, 0x43, 0x01, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
}

func TestTCPRemoteReaderConcurrentPulls(t *testing.T) {
	mf, fr, want := newTCPTestMemoryFile(t, 64)
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	token := testSnapshotToken()
	server, serveDone := startTCPTestServer(t, snapshot, token, 64<<10)
	reader, err := NewTCPRemoteReader(server.Addr().String(), token, 64<<10, 4, 4, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]byte, len(want))
	const transfer = 64 << 10
	for i, off := 0, 0; off < len(got); i, off = i+1, off+transfer {
		reader.AddRead(i, int64(off), nil, memmap.FileRange{}, got[off:off+transfer])
	}
	completions, err := reader.Wait(nil, len(got)/transfer)
	if err != nil {
		t.Fatal(err)
	}
	for _, completion := range completions {
		if completion.Err != nil || completion.N != transfer {
			t.Fatalf("completion %+v", completion)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("TCP reader returned incorrect memory contents")
	}
	if stats := reader.Stats(); stats.SubmittedReads != 4 || stats.CompletedReads != 4 || stats.FailedReads != 0 ||
		stats.BytesInstalled != uint64(len(want)) || stats.Retries != 0 || stats.AckOnlyRetries != 0 {
		t.Fatalf("reader stats: %+v", stats)
	}
	if debug := reader.DebugState(); debug.Address != server.Addr().String() || debug.MaxReadBytes != 64<<10 ||
		debug.MaxParallel != 4 || debug.Stats.CompletedReads != 4 {
		t.Fatalf("reader debug state: %+v", debug)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	if stats := server.Stats(); stats.PullRequests != 4 || stats.AckRequests != 4 || stats.BytesSent != uint64(len(want)) {
		t.Fatalf("server stats: %+v", stats)
	}
	if stats := snapshot.Stats(); stats.PagesOutstanding != 0 || !stats.Closed {
		t.Fatalf("snapshot stats: %+v", stats)
	}
	if !mf.HasUniqueRef(fr) {
		t.Fatal("source pages did not become unique after TCP acknowledgements")
	}
}

func TestTCPRemoteReaderBatchesAcknowledgements(t *testing.T) {
	mf, fr, want := newTCPTestMemoryFile(t, 4)
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	token := testSnapshotToken()
	server, serveDone := startTCPTestServer(t, snapshot, token, hostarch.PageSize)
	reader, err := NewTCPRemoteReaderWithOpts(server.Addr().String(), token, TCPRemoteReaderOpts{
		MaxReadBytes: hostarch.PageSize, MaxRanges: 1, MaxParallel: 4,
		Timeout: 5 * time.Second, RetryLimit: 2, AckBatchPages: 4, AckInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	for page := range 4 {
		off := page * int(hostarch.PageSize)
		reader.AddRead(page, int64(off), nil, memmap.FileRange{}, got[off:off+int(hostarch.PageSize)])
	}
	completions, err := reader.Wait(nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, completion := range completions {
		if completion.Err != nil || completion.N != hostarch.PageSize {
			t.Fatalf("completion %+v", completion)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("ACK-batched pulls returned incorrect data")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	if stats := server.Stats(); stats.PullRequests != 4 || stats.AckRequests != 4 || stats.AckBatches != 1 {
		t.Fatalf("ACK batch server stats: %+v", stats)
	}
	if stats := reader.Stats(); stats.CompletedReads != 4 || stats.BytesInstalled != fr.Length() {
		t.Fatalf("ACK batch reader stats: %+v", stats)
	}
}

func TestTCPRemoteReaderRecoversLostAckBatchOK(t *testing.T) {
	mf, fr, want := newTCPTestMemoryFile(t, 2)
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &dropFirstControlListener{Listener: base, message: tcpAckBatchOK}
	token := testSnapshotToken()
	server, err := NewTCPPageServer(listener, token, snapshot, hostarch.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	reader, err := NewTCPRemoteReaderWithOpts(listener.Addr().String(), token, TCPRemoteReaderOpts{
		MaxReadBytes: hostarch.PageSize, MaxRanges: 1, MaxParallel: 2,
		Timeout: time.Second, RetryLimit: 2, AckBatchPages: 2, AckInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	for page := range 2 {
		off := page * int(hostarch.PageSize)
		reader.AddRead(page, int64(off), nil, memmap.FileRange{}, got[off:off+int(hostarch.PageSize)])
	}
	completions, err := reader.Wait(nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, completion := range completions {
		if completion.Err != nil || completion.N != hostarch.PageSize {
			t.Fatalf("completion %+v", completion)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("retried ACK batch changed installed data")
	}
	if stats := reader.Stats(); stats.Retries != 1 || stats.AckOnlyRetries != 1 || stats.CompletedReads != 2 {
		t.Fatalf("lost ACK batch reader stats: %+v", stats)
	}
	reader.Close()
	server.Close()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	if stats := snapshot.Stats(); stats.PagesOutstanding != 0 {
		t.Fatalf("lost ACK batch retained source pages: %+v", stats)
	}
}

func TestTCPMetadataHandshakeAndChunkStream(t *testing.T) {
	mf, fr, _ := newTCPTestMemoryFile(t, 1)
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	token := testSnapshotToken()
	server, serveDone := startTCPTestServer(t, snapshot, token, 64<<10)
	metadata := make([]byte, 700<<10)
	for i := range metadata {
		metadata[i] = byte(i*37 + 5)
	}
	if err := server.SetMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	got, err := FetchTCPMetadata(context.Background(), server.Addr().String(), token, 64<<10, 1<<20, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, metadata) {
		t.Fatal("fetched Kernel metadata differs")
	}
	if stats := server.Stats(); stats.MetadataBytesSent != uint64(len(metadata)) || stats.PullRequests != 0 {
		t.Fatalf("metadata transport stats: %+v", stats)
	}
	server.Close()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestTCPRemoteTargetSubprocess(t *testing.T) {
	if os.Getenv("TTC_REMOTE_TARGET_HELPER") == "1" {
		address := os.Getenv("TTC_REMOTE_TARGET_ADDRESS")
		encoded, err := FetchTCPMetadata(context.Background(), address, testSnapshotToken(), 64<<10, 4<<20, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		session, err := UnmarshalSessionMetadata(encoded, SessionMetadataLimits{
			MaxTotalBytes: 4 << 20, MaxKernelBytes: 1, MaxAllocatorBytes: 2 << 20, MaxLoadRanges: 1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		target := newEmptyTCPTestMemoryFile(t)
		defer target.Destroy()
		if err := pgalloc.ImportMemoryFileMetadataForRemote(contexttest.Context(t), target, session.AllocatorMetadata); err != nil {
			t.Fatal(err)
		}
		reader, err := NewTCPRemoteReader(address, testSnapshotToken(), 64<<10, 16, 2, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		pages, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := target.AttachRemotePageLoad(pages, session.LoadRanges, nil, nil); err != nil {
			t.Fatal(err)
		}
		pages.MemoryFilesDone()
		first := session.LoadRanges[0].Target
		first.End = first.Start + hostarch.PageSize
		mapped, err := target.MapInternal(first, hostarch.Read)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, hostarch.PageSize)
		if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), mapped); err != nil || n != hostarch.PageSize {
			t.Fatalf("copy target page: n=%d err=%v", n, err)
		}
		for i, value := range got {
			want := byte((i*29 + i/int(hostarch.PageSize)) & 0xff)
			if value != want {
				t.Fatalf("target byte %d=%d, want %d", i, value, want)
			}
		}
		pages.Cancel()
		_ = pages.Wait()
		return
	}

	mf, _, _ := newTCPTestMemoryFile(t, 16)
	allocator, regions, loads, err := pgalloc.ExportMemoryFileMetadataForRemote(contexttest.Context(t), mf)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := pgalloc.NewRemoteSnapshot(regions)
	if err != nil {
		t.Fatal(err)
	}
	session, err := (&SessionMetadata{
		FamilyID: "subprocess", SourceBranchID: "a", TargetBranchID: "b",
		AllocatorMetadata: allocator, LoadRanges: loads,
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	server, serveDone := startTCPTestServer(t, snapshot, testSnapshotToken(), 64<<10)
	if err := server.SetMetadata(session); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTCPRemoteTargetSubprocess$")
	cmd.Env = append(os.Environ(), "TTC_REMOTE_TARGET_HELPER=1", "TTC_REMOTE_TARGET_ADDRESS="+server.Addr().String())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remote target subprocess: %v\n%s", err, output)
	}
	if stats := server.Stats(); stats.BytesSent != hostarch.PageSize {
		t.Fatalf("target fetched %d page bytes before exit, want one page", stats.BytesSent)
	}
	server.Close()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestTCPAckAfterInstallation(t *testing.T) {
	mf, fr, want := newTCPTestMemoryFile(t, 1)
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	token := testSnapshotToken()
	server, serveDone := startTCPTestServer(t, snapshot, token, hostarch.PageSize)
	conn, err := net.DialTimeout("tcp", server.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pull := tcpHeader{type_: tcpPull, token: token, requestID: 7, length: uint32(hostarch.PageSize)}
	if err := writeTCPMessage(conn, pull, nil); err != nil {
		t.Fatal(err)
	}
	response, payload, err := readTCPResponse(conn, token, 7, hostarch.PageSize)
	if err != nil || response.type_ != tcpData {
		t.Fatalf("data response=%+v err=%v", response, err)
	}
	if !bytes.Equal(payload, want) {
		t.Fatal("manual pull returned incorrect data")
	}
	if mf.HasUniqueRef(fr) {
		t.Fatal("source snapshot reference was released before target ACK")
	}
	installed := append([]byte(nil), payload...)
	if !bytes.Equal(installed, want) {
		t.Fatal("target installation failed")
	}
	ack := tcpHeader{type_: tcpAck, token: token, requestID: 7, length: uint32(hostarch.PageSize)}
	if err := writeTCPMessage(conn, ack, nil); err != nil {
		t.Fatal(err)
	}
	ackResponse, _, err := readTCPResponse(conn, token, 7, hostarch.PageSize)
	if err != nil || ackResponse.type_ != tcpAckOK {
		t.Fatalf("ACK response=%+v err=%v", ackResponse, err)
	}
	if !mf.HasUniqueRef(fr) {
		t.Fatal("source page did not become unique after confirmed ACK")
	}
	conn.Close()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestTCPRemoteReaderReadv(t *testing.T) {
	mf, fr, want := newTCPTestMemoryFile(t, 2)
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	token := testSnapshotToken()
	server, serveDone := startTCPTestServer(t, snapshot, token, fr.Length())
	reader, err := NewTCPRemoteReader(server.Addr().String(), token, fr.Length(), 2, 1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, hostarch.PageSize)
	second := make([]byte, hostarch.PageSize)
	iovecs := []unix.Iovec{
		{Base: &first[0], Len: uint64(len(first))},
		{Base: &second[0], Len: uint64(len(second))},
	}
	reader.AddReadv(0, 0, fr.Length(), nil, nil, iovecs)
	completions, err := reader.Wait(nil, 1)
	if err != nil || len(completions) != 1 || completions[0].Err != nil {
		t.Fatalf("completions=%+v err=%v", completions, err)
	}
	if !bytes.Equal(append(first, second...), want) {
		t.Fatal("TCP readv returned incorrect data")
	}
	reader.Close()
	server.Close()
	<-serveDone
}

func TestTCPRemoteReaderRejectsWrongToken(t *testing.T) {
	mf, fr, _ := newTCPTestMemoryFile(t, 1)
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	server, serveDone := startTCPTestServer(t, snapshot, testSnapshotToken(), hostarch.PageSize)
	wrong := testSnapshotToken()
	wrong[0]++
	reader, err := NewTCPRemoteReader(server.Addr().String(), wrong, hostarch.PageSize, 1, 1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, hostarch.PageSize)
	reader.AddRead(0, 0, nil, memmap.FileRange{}, dst)
	completions, err := reader.Wait(nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if completions[0].Err == nil || completions[0].N != 0 {
		t.Fatalf("wrong-token completion: %+v", completions[0])
	}
	if stats := snapshot.Stats(); stats.PagesOutstanding != 1 {
		t.Fatalf("wrong token released source page: %+v", stats)
	}
	reader.Close()
	server.Close()
	<-serveDone
	if !mf.HasUniqueRef(fr) {
		t.Fatal("server cancellation did not release wrong-token snapshot")
	}
}

func TestTCPRemoteReaderRecoversLostAckOK(t *testing.T) {
	mf, fr, want := newTCPTestMemoryFile(t, 1)
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	token := testSnapshotToken()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		pull, err := readTCPHeader(conn)
		if err == nil && pull.type_ != tcpPull {
			err = errors.New("first request is not PULL")
		}
		if err == nil {
			err = writeTCPMessage(conn, tcpHeader{
				type_: tcpData, token: token, requestID: pull.requestID,
				offset: pull.offset, length: pull.length, checksum: crc32.ChecksumIEEE(want),
			}, want)
		}
		var ack tcpHeader
		if err == nil {
			ack, err = readTCPHeader(conn)
		}
		if err == nil && ack.type_ != tcpAck {
			err = errors.New("second request is not ACK")
		}
		if err == nil {
			err = snapshot.AcknowledgeStreamRange(ack.offset, uint64(ack.length))
		}
		// Simulate ACKOK loss after the source has already released the page.
		conn.Close()
		if err != nil {
			serverDone <- err
			return
		}

		conn, err = listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		retry, err := readTCPHeader(conn)
		if err == nil && retry.type_ != tcpAck {
			err = fmt.Errorf("retry type=%d, want ACK", retry.type_)
		}
		if err == nil {
			err = snapshot.AcknowledgeStreamRange(retry.offset, uint64(retry.length))
		}
		if err == nil {
			err = writeTCPMessage(conn, tcpHeader{
				type_: tcpAckOK, token: token, requestID: retry.requestID,
				offset: retry.offset, length: retry.length,
			}, nil)
		}
		serverDone <- err
	}()

	reader, err := NewTCPRemoteReaderWithOpts(listener.Addr().String(), token, TCPRemoteReaderOpts{
		MaxReadBytes: hostarch.PageSize,
		MaxRanges:    1,
		MaxParallel:  1,
		Timeout:      time.Second,
		RetryLimit:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, hostarch.PageSize)
	reader.AddRead(19, 0, nil, memmap.FileRange{}, got)
	completions, err := reader.Wait(nil, 1)
	if err != nil || completions[0].Err != nil || completions[0].N != hostarch.PageSize {
		t.Fatalf("completion=%+v Wait err=%v", completions, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("retry changed installed page data")
	}
	if stats := reader.Stats(); stats.SubmittedReads != 1 || stats.CompletedReads != 1 || stats.FailedReads != 0 ||
		stats.BytesInstalled != hostarch.PageSize || stats.Retries != 1 || stats.AckOnlyRetries != 1 {
		t.Fatalf("lost-ACK reader stats: %+v", stats)
	}
	reader.Close()
	listener.Close()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if !mf.HasUniqueRef(fr) {
		t.Fatal("lost ACKOK recovery retained source snapshot reference")
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTCPRemoteReaderRejectsCorruptData(t *testing.T) {
	token := testSnapshotToken()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		defer close(serverDone)
		for range 2 {
			conn, err := listener.Accept()
			if err != nil {
				serverDone <- err
				return
			}
			header, err := readTCPHeader(conn)
			if err == nil {
				payload := make([]byte, hostarch.PageSize)
				err = writeTCPMessage(conn, tcpHeader{
					type_: tcpData, token: token, requestID: header.requestID,
					offset: header.offset, length: uint32(len(payload)), checksum: 1,
				}, payload)
			}
			conn.Close()
			if err != nil {
				serverDone <- err
				return
			}
		}
	}()
	reader, err := NewTCPRemoteReaderWithOpts(listener.Addr().String(), token, TCPRemoteReaderOpts{
		MaxReadBytes: hostarch.PageSize,
		MaxRanges:    1,
		MaxParallel:  1,
		Timeout:      time.Second,
		RetryLimit:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, hostarch.PageSize)
	reader.AddRead(1, 0, nil, memmap.FileRange{}, dst)
	completions, err := reader.Wait(nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if completions[0].Err == nil || completions[0].N != 0 {
		t.Fatalf("corrupt completion: %+v", completions[0])
	}
	reader.Close()
	listener.Close()
	for err := range serverDone {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestTCPProactivePushInstallAndAcknowledge(t *testing.T) {
	source, sourceFR, want := newTCPTestMemoryFile(t, 4)
	allocator, regions, loads, err := pgalloc.ExportMemoryFileMetadataForRemote(contexttest.Context(t), source)
	if err != nil {
		t.Fatal(err)
	}
	target := newEmptyTCPTestMemoryFile(t)
	if err := pgalloc.ImportMemoryFileMetadataForRemote(contexttest.Context(t), target, allocator); err != nil {
		t.Fatal(err)
	}
	defer func() {
		target.DecRef(sourceFR)
		target.Destroy()
	}()
	snapshot, err := pgalloc.NewRemoteSnapshot(regions)
	if err != nil {
		t.Fatal(err)
	}
	reader := pgalloc.NewLoopbackRemoteReader(snapshot, hostarch.PageSize, 1, 1)
	pages, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, loads, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token := testSnapshotToken()
	receiver, err := NewTCPPushReceiver(listener, token, pages, hostarch.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Serve() }()
	sender, err := StartTCPPushSender(listener.Addr().String(), token, snapshot, hostarch.PageSize, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	source.NotifyRemoteCOW(memmap.FileRange{sourceFR.Start, sourceFR.Start + hostarch.PageSize})
	deadline := time.Now().Add(5 * time.Second)
	for sender.BytesPushed() != hostarch.PageSize && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sender.BytesPushed() != hostarch.PageSize {
		t.Fatalf("sender pushed %d bytes", sender.BytesPushed())
	}
	installed, skipped := receiver.Stats()
	if installed != hostarch.PageSize || skipped != 0 {
		t.Fatalf("receiver installed=%d skipped=%d", installed, skipped)
	}
	first := memmap.FileRange{sourceFR.Start, sourceFR.Start + hostarch.PageSize}
	mapped, err := target.MapInternal(first, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, hostarch.PageSize)
	if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), mapped); err != nil || n != hostarch.PageSize {
		t.Fatalf("copy pushed target: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want[:hostarch.PageSize]) {
		t.Fatal("TCP pushed page differs")
	}
	pages.Cancel()
	_ = pages.Wait()
	if err := sender.Wait(); err != nil {
		t.Fatal(err)
	}
	receiver.Close()
	if err := <-receiverDone; err != nil {
		t.Fatal(err)
	}
}

func TestTCPDonatedReversePushLane(t *testing.T) {
	_, _, snapshot, pages := newTCPPushPolicyTestSession(t, 4)
	defer pages.Cancel()
	token := SnapshotToken{9, 8, 7, 6}
	server, err := NewTCPPageSession(token, snapshot, 4*hostarch.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ConfigurePush(TCPPushSenderOpts{
		MaxPush: 4 * hostarch.PageSize, Timeout: time.Second, RetryLimit: 0,
		Policy: TCPPushEagerDrain,
	}); err != nil {
		t.Fatal(err)
	}
	firstAttempt := true
	receiver, err := NewTCPPushSession(token, func(offset uint64, data []byte) (bool, error) {
		if firstAttempt {
			firstAttempt = false
			return false, nil // Model an in-flight pull winning the first race.
		}
		return pages.TryInstallRemotePages(offset, data)
	}, 4*hostarch.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sourceConn, targetConn := net.Pipe()
	go func() { _ = server.ServeConn(sourceConn) }()
	done := make(chan error, 1)
	go func() { done <- receiver.ServeStream(targetConn) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		installed, _ := receiver.Stats()
		if installed == 4*hostarch.PageSize {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reverse lane installed %d bytes", installed)
		}
		time.Sleep(time.Millisecond)
	}
	if got := server.SnapshotStats().PagesOutstanding; got != 0 {
		t.Fatalf("source retains %d pages after reverse push", got)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if got := server.Stats().PushBytesSent; got != 4*hostarch.PageSize {
		t.Fatalf("source push bytes=%d", got)
	}
	if _, skipped := receiver.Stats(); skipped != 4*hostarch.PageSize {
		t.Fatalf("receiver skipped bytes=%d, want one retried chunk", skipped)
	}
}

func TestCoalesceAdjacentAndOverlappingPushHints(t *testing.T) {
	hints := make(chan pgalloc.RemotePushHint, 4)
	hints <- pgalloc.RemotePushHint{StreamOffset: 3 * hostarch.PageSize, Length: hostarch.PageSize}
	hints <- pgalloc.RemotePushHint{StreamOffset: 2 * hostarch.PageSize, Length: 2 * hostarch.PageSize}
	hints <- pgalloc.RemotePushHint{StreamOffset: 8 * hostarch.PageSize, Length: hostarch.PageSize}
	close(hints)
	merged, consumed, closed := collectCoalescedPushHints(
		pgalloc.RemotePushHint{StreamOffset: hostarch.PageSize, Length: hostarch.PageSize}, hints)
	if consumed != 4 || !closed {
		t.Fatalf("consumed=%d closed=%t", consumed, closed)
	}
	want := []pgalloc.RemotePushHint{
		{StreamOffset: hostarch.PageSize, Length: 3 * hostarch.PageSize},
		{StreamOffset: 8 * hostarch.PageSize, Length: hostarch.PageSize},
	}
	if len(merged) != len(want) {
		t.Fatalf("merged=%+v, want %+v", merged, want)
	}
	for i := range want {
		if merged[i] != want[i] {
			t.Fatalf("merged[%d]=%+v, want %+v", i, merged[i], want[i])
		}
	}
}

func TestTCPProactivePushRecoversLostPushOK(t *testing.T) {
	source, sourceFR, _ := newTCPTestMemoryFile(t, 1)
	allocator, regions, loads, err := pgalloc.ExportMemoryFileMetadataForRemote(contexttest.Context(t), source)
	if err != nil {
		t.Fatal(err)
	}
	target := newEmptyTCPTestMemoryFile(t)
	if err := pgalloc.ImportMemoryFileMetadataForRemote(contexttest.Context(t), target, allocator); err != nil {
		t.Fatal(err)
	}
	defer func() {
		target.DecRef(sourceFR)
		target.Destroy()
	}()
	snapshot, err := pgalloc.NewRemoteSnapshot(regions)
	if err != nil {
		t.Fatal(err)
	}
	reader := pgalloc.NewLoopbackRemoteReader(snapshot, hostarch.PageSize, 1, 1)
	pages, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, loads, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &dropFirstControlListener{Listener: base, message: tcpPushOK}
	token := testSnapshotToken()
	receiver, err := NewTCPPushReceiver(listener, token, pages, hostarch.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Serve() }()
	sender, err := StartTCPPushSenderWithOpts(listener.Addr().String(), token, snapshot, TCPPushSenderOpts{
		MaxPush: hostarch.PageSize, Timeout: time.Second, RetryLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	source.NotifyRemoteCOW(sourceFR)
	deadline := time.Now().Add(5 * time.Second)
	for sender.BytesPushed() != hostarch.PageSize && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sender.BytesPushed() != hostarch.PageSize || sender.Retries() == 0 {
		t.Fatalf("push bytes=%d retries=%d", sender.BytesPushed(), sender.Retries())
	}
	installed, skipped := receiver.Stats()
	if installed != hostarch.PageSize || skipped != 0 {
		t.Fatalf("duplicate push installed=%d skipped=%d", installed, skipped)
	}
	if debug := receiver.DebugState(); debug.PendingReceipts != 0 || debug.BytesInstalled != hostarch.PageSize {
		t.Fatalf("push receiver debug state after commit: %+v", debug)
	}
	if stats := snapshot.Stats(); stats.PagesOutstanding != 0 {
		t.Fatalf("retried push retained source page: %+v", stats)
	}
	pages.Cancel()
	_ = pages.Wait()
	if err := sender.Wait(); err != nil {
		t.Fatal(err)
	}
	receiver.Close()
	if err := <-receiverDone; err != nil {
		t.Fatal(err)
	}
}

func TestTCPPushPoliciesWithoutSourceWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy TCPPushPolicy
		high   uint64
		want   uint64
	}{
		{name: "demand-only", policy: TCPPushDemandOnly, want: 0},
		{name: "eager-drain", policy: TCPPushEagerDrain, want: 4 * hostarch.PageSize},
		{name: "memory-pressure", policy: TCPPushMemoryPressure, high: 4 * hostarch.PageSize, want: 4 * hostarch.PageSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, sourceFR, snapshot, pages := newTCPPushPolicyTestSession(t, 4)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			token := testSnapshotToken()
			receiver, err := NewTCPPushReceiver(listener, token, pages, 4*hostarch.PageSize)
			if err != nil {
				t.Fatal(err)
			}
			receiverDone := make(chan error, 1)
			go func() { receiverDone <- receiver.Serve() }()
			sender, err := StartTCPPushSenderWithOpts(listener.Addr().String(), token, snapshot, TCPPushSenderOpts{
				MaxPush: 4 * hostarch.PageSize, Timeout: time.Second, RetryLimit: 2,
				Policy: tc.policy, RetainedHighWater: tc.high, ScanInterval: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for sender.BytesPushed() != tc.want && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if sender.BytesPushed() != tc.want {
				t.Fatalf("policy %s pushed %d bytes, want %d", tc.policy, sender.BytesPushed(), tc.want)
			}
			if tc.want == 0 {
				time.Sleep(20 * time.Millisecond)
				if stats := snapshot.Stats(); stats.PagesOutstanding != 4 {
					t.Fatalf("demand-only released source pages: %+v", stats)
				}
			} else {
				if stats := snapshot.Stats(); stats.PagesOutstanding != 0 {
					t.Fatalf("policy %s retained source pages: %+v", tc.policy, stats)
				}
				if stats := sender.Stats(); stats.Scans == 0 || stats.Chunks != 1 {
					t.Fatalf("policy %s sender stats: %+v", tc.policy, stats)
				}
				if !source.HasUniqueRef(sourceFR) {
					t.Fatalf("policy %s did not restore the source unique-page fast path", tc.policy)
				}
			}
			pages.Cancel()
			_ = pages.Wait()
			if err := sender.Wait(); err != nil {
				t.Fatal(err)
			}
			receiver.Close()
			if err := <-receiverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTCPPushSkipsRangeOwnedByConcurrentPull(t *testing.T) {
	source, sourceFR, _ := newTCPTestMemoryFile(t, 1)
	allocator, regions, loads, err := pgalloc.ExportMemoryFileMetadataForRemote(contexttest.Context(t), source)
	if err != nil {
		t.Fatal(err)
	}
	target := newEmptyTCPTestMemoryFile(t)
	if err := pgalloc.ImportMemoryFileMetadataForRemote(contexttest.Context(t), target, allocator); err != nil {
		t.Fatal(err)
	}
	defer func() {
		target.DecRef(sourceFR)
		target.Destroy()
	}()
	snapshot, err := pgalloc.NewRemoteSnapshot(regions)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockingSnapshotReaderAt{snapshot: snapshot, started: make(chan struct{}), release: make(chan struct{})}
	reader := stateio.NewIOReader(blocked, hostarch.PageSize, 1, 1)
	pages, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, loads, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	pullDone := make(chan error, 1)
	go func() {
		_, err := target.MapInternal(sourceFR, hostarch.Read)
		pullDone <- err
	}()
	select {
	case <-blocked.started:
	case <-time.After(5 * time.Second):
		t.Fatal("pull did not start")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token := testSnapshotToken()
	receiver, err := NewTCPPushReceiver(listener, token, pages, hostarch.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Serve() }()
	sender, err := StartTCPPushSender(listener.Addr().String(), token, snapshot, hostarch.PageSize, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	source.NotifyRemoteCOW(sourceFR)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, skipped := receiver.Stats()
		if skipped == hostarch.PageSize || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	installed, skipped := receiver.Stats()
	if installed != 0 || skipped != hostarch.PageSize || sender.BytesPushed() != 0 {
		t.Fatalf("pull/push race installed=%d skipped=%d acknowledged=%d", installed, skipped, sender.BytesPushed())
	}
	if stats := snapshot.Stats(); stats.PagesOutstanding != 1 {
		t.Fatalf("PUSH_SKIP released pull-owned page: %+v", stats)
	}
	close(blocked.release)
	select {
	case err := <-pullDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pull did not complete")
	}
	if stats := snapshot.Stats(); stats.PagesOutstanding != 0 {
		t.Fatalf("pull ACK did not release page: %+v", stats)
	}
	pages.Cancel()
	_ = pages.Wait()
	if err := sender.Wait(); err != nil {
		t.Fatal(err)
	}
	receiver.Close()
	if err := <-receiverDone; err != nil {
		t.Fatal(err)
	}
}
