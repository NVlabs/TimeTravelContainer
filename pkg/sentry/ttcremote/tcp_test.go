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
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

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
