// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

// distributedCopyRoute describes the local half of a cp command that must
// remain on the ingress daemon. A host pathname always names the machine on
// which the client-connected daemon runs, not whichever node owns the branch.
type distributedCopyRoute struct {
	opts            copyCommandOptions
	source          string
	destination     string
	sourceContainer bool
	destContainer   bool
}

func parseDistributedCopyRoute(args []string) (distributedCopyRoute, bool, error) {
	if len(args) == 0 || args[0] != "cp" {
		return distributedCopyRoute{}, false, nil
	}
	opts, source, destination, err := parseCopyCommandArgs(args[1:])
	if err != nil {
		return distributedCopyRoute{}, true, err
	}
	_, sourceContainer, err := splitCopyContainerEndpoint(source)
	if err != nil {
		return distributedCopyRoute{}, true, err
	}
	_, destinationContainer, err := splitCopyContainerEndpoint(destination)
	if err != nil {
		return distributedCopyRoute{}, true, err
	}
	if sourceContainer == destinationContainer {
		return distributedCopyRoute{}, true, errors.New("exactly one of SRC_PATH and DEST_PATH must name a container")
	}
	return distributedCopyRoute{
		opts: opts, source: source, destination: destination,
		sourceContainer: sourceContainer, destContainer: destinationContainer,
	}, true, nil
}

func distributedCopyArgs(opts copyCommandOptions, source, destination string) []string {
	args := []string{"cp"}
	if opts.preparedInput {
		args = append(args, copyPreparedInputArg)
	}
	if opts.archive {
		args = append(args, "--archive")
	}
	if opts.followLink {
		args = append(args, "--follow-link")
	}
	return append(args, source, destination)
}

// daemonStdinFrameReader turns an ordinary byte stream into TTC daemon stdin
// frames. The request header is carried separately by io.MultiReader.
type daemonStdinFrameReader struct {
	raw       io.Reader
	flow      *daemonStdinFlow
	buffer    []byte
	terminal  bool
	readError error
}

func (r *daemonStdinFrameReader) Read(destination []byte) (int, error) {
	for len(r.buffer) == 0 {
		if r.terminal {
			if r.readError != nil && !errors.Is(r.readError, io.EOF) {
				return 0, r.readError
			}
			return 0, io.EOF
		}
		payload := make([]byte, 32<<10)
		n, err := r.raw.Read(payload)
		if n != 0 {
			if r.flow != nil {
				if flowErr := r.flow.reserve(n); flowErr != nil {
					return 0, flowErr
				}
			}
			payload = payload[:n]
			header := [5]byte{daemonFrameStdin}
			putDaemonFrameLength(header[1:], uint32(n))
			r.buffer = append(header[:], payload...)
			if err != nil {
				r.readError = err
			}
			break
		}
		if err == nil {
			continue
		}
		r.readError = err
		header := [5]byte{daemonFrameEOF}
		putDaemonFrameLength(header[1:], 0)
		r.buffer = append(r.buffer, header[:]...)
		r.terminal = true
	}
	n := copy(destination, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}

// putDaemonFrameLength is kept here so the copy routing path does not expose
// the wire encoding outside the daemon protocol implementation.
func putDaemonFrameLength(destination []byte, length uint32) {
	destination[0] = byte(length >> 24)
	destination[1] = byte(length >> 16)
	destination[2] = byte(length >> 8)
	destination[3] = byte(length)
}

// prepareDistributedCopyUpload captures a host source at the ingress daemon
// before routing the command. Spooling first prevents a local read failure from
// leaving a partially imported tree on the remote owner. The spool lives on
// TTC_LOCAL_ROOT and is removed as soon as the routed command completes.
func prepareDistributedCopyUpload(localRoot string, request daemonRequest, route distributedCopyRoute) (daemonRequest, io.Reader, *daemonStdinFlow, func(), error) {
	temporaryRoot := filepath.Join(localRoot, "tmp")
	if err := os.MkdirAll(temporaryRoot, 0700); err != nil {
		return daemonRequest{}, nil, nil, nil, fmt.Errorf("creating distributed cp staging directory: %w", err)
	}
	archive, err := os.CreateTemp(temporaryRoot, "cp-upload-*.tar")
	if err != nil {
		return daemonRequest{}, nil, nil, nil, fmt.Errorf("creating distributed cp archive: %w", err)
	}
	cleanup := func() {
		name := archive.Name()
		_ = archive.Close()
		_ = os.Remove(name)
	}
	if err := archive.Chmod(0600); err != nil {
		cleanup()
		return daemonRequest{}, nil, nil, nil, err
	}
	if err := writeCopyArchive(route.source, route.opts.followLink, archive); err != nil {
		cleanup()
		return daemonRequest{}, nil, nil, nil, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return daemonRequest{}, nil, nil, nil, err
	}
	route.opts.preparedInput = true
	request.Args = distributedCopyArgs(route.opts, "-", route.destination)
	request.Stdin = true
	header, err := json.Marshal(&request)
	if err != nil {
		cleanup()
		return daemonRequest{}, nil, nil, nil, err
	}
	header = append(header, '\n')
	flow := newDaemonStdinFlow()
	input := io.MultiReader(bytes.NewReader(header), &daemonStdinFrameReader{raw: archive, flow: flow})
	return request, input, flow, cleanup, nil
}

// proxyDistributedCopyUpload forwards a prepared archive while terminating
// stdin flow control at the ingress daemon. The original client supplied a
// host pathname rather than a byte stream, so it has no outstanding stdin
// window and must never see acknowledgements generated by the remote owner.
// Consuming acknowledgements here also prevents a fast NFS/local read from
// overrunning the remote daemon's bounded stdin queue.
func proxyDistributedCopyUpload(ctx context.Context, peer *cluster.Client, input io.Reader, flow *daemonStdinFlow, output io.Writer, outputMu *sync.Mutex) error {
	responseReader, responseWriter := io.Pipe()
	responseDone := make(chan error, 1)
	go func() {
		defer responseReader.Close()
		reader := bufio.NewReader(responseReader)
		for {
			typ, payload, err := readDaemonFrame(reader)
			if err != nil {
				if errors.Is(err, io.EOF) {
					responseDone <- nil
				} else {
					responseDone <- err
				}
				return
			}
			if typ == daemonFrameStdinAck {
				acknowledged, err := decodeDaemonStdinAcknowledgement(payload)
				if err == nil {
					err = flow.acknowledge(acknowledged)
				}
				if err != nil {
					responseDone <- err
					return
				}
				continue
			}
			if err := writeDaemonFrame(output, outputMu, typ, payload); err != nil {
				responseDone <- err
				return
			}
		}
	}()
	proxyErr := peer.ProxyDaemon(ctx, input, responseWriter)
	_ = responseWriter.CloseWithError(proxyErr)
	flow.close()
	responseErr := <-responseDone
	return errors.Join(proxyErr, responseErr)
}

// daemonCopyArchiveReader exposes only stdout frame payloads as one tar stream
// while retaining diagnostics and the terminal status for the ingress daemon.
type daemonCopyArchiveReader struct {
	reader     *bufio.Reader
	current    []byte
	diagnostic bytes.Buffer
	status     int
	statusSeen bool
}

func newDaemonCopyArchiveReader(reader io.Reader) *daemonCopyArchiveReader {
	return &daemonCopyArchiveReader{reader: bufio.NewReader(reader), status: -1}
}

func (r *daemonCopyArchiveReader) Read(destination []byte) (int, error) {
	for len(r.current) == 0 {
		if r.statusSeen {
			return 0, io.EOF
		}
		typ, payload, err := readDaemonFrame(r.reader)
		if err != nil {
			return 0, err
		}
		switch typ {
		case daemonFrameStdout:
			r.current = payload
		case daemonFrameStderr:
			r.diagnostic.Write(payload)
		case daemonFrameStatus:
			if len(payload) != 4 {
				return 0, errors.New("remote daemon returned an invalid status frame")
			}
			r.status = int(payload[0])<<24 | int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
			r.statusSeen = true
		default:
			return 0, fmt.Errorf("unexpected daemon frame %q during distributed cp", typ)
		}
	}
	n := copy(destination, r.current)
	r.current = r.current[n:]
	return n, nil
}

// proxyDistributedCopyDownload keeps the host destination on the ingress
// node. The remote owner exports a tar stream to stdout; only after the remote
// response is complete do we update the local destination and reply to the
// client. This preserves Docker cp's local-path meaning across cluster routing.
func proxyDistributedCopyDownload(ctx context.Context, peer *cluster.Client, localRoot string, request daemonRequest, route distributedCopyRoute, conn io.Writer, writeMu *sync.Mutex) error {
	request.Args = distributedCopyArgs(route.opts, route.source, "-")
	header, err := json.Marshal(&request)
	if err != nil {
		return err
	}
	temporaryRoot := filepath.Join(localRoot, "tmp")
	if err := os.MkdirAll(temporaryRoot, 0700); err != nil {
		return fmt.Errorf("creating distributed cp staging directory: %w", err)
	}
	response, err := os.CreateTemp(temporaryRoot, "cp-download-*.frames")
	if err != nil {
		return fmt.Errorf("creating distributed cp response: %w", err)
	}
	responseName := response.Name()
	defer func() {
		_ = response.Close()
		_ = os.Remove(responseName)
	}()
	if err := peer.ProxyDaemon(ctx, bytes.NewReader(append(header, '\n')), response); err != nil {
		return err
	}
	if _, err := response.Seek(0, io.SeekStart); err != nil {
		return err
	}
	archive := newDaemonCopyArchiveReader(response)
	extractErr := extractCopyArchive(archive, route.destination, route.opts.archive, os.Geteuid(), os.Getegid(), false)
	// archive/tar may recognize its end marker without requesting the following
	// status frame. Drain the frame reader so the remote outcome is authoritative.
	_, drainErr := io.Copy(io.Discard, archive)
	if archive.diagnostic.Len() != 0 {
		_ = writeDaemonFrame(conn, writeMu, daemonFrameStderr, archive.diagnostic.Bytes())
	}
	status := archive.status
	if status < 0 {
		status = 1
	}
	if extractErr != nil || drainErr != nil {
		if extractErr == nil {
			extractErr = drainErr
		}
		_ = writeDaemonFrame(conn, writeMu, daemonFrameStderr, []byte("ttcd: extracting distributed cp result: "+extractErr.Error()+"\n"))
		status = 1
	}
	return writeDaemonStatus(conn, writeMu, status)
}
