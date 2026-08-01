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

package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"gvisor.dev/gvisor/runsc/boot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

type distributedSourceRuntime struct {
	capabilities  cluster.Capabilities
	peers         map[string]*cluster.Client
	pageListen    string
	pageAdvertise string
}

type distributedForkBackend interface {
	forkRemote(source selector, targetNode, targetContainer string, paused bool) (selector, error)
}

type distributedPageRelay struct {
	listener net.Listener
	backend  *runtimeBackend
	source   string
	session  string
	close    sync.Once
	mu       sync.Mutex
	closed   bool
	commands map[*exec.Cmd]struct{}
}

func newDistributedPageRelay(listener net.Listener, backend *runtimeBackend, source, session string) *distributedPageRelay {
	relay := &distributedPageRelay{
		listener: listener, backend: backend, source: source, session: session,
		commands: make(map[*exec.Cmd]struct{}),
	}
	go relay.serve()
	return relay
}

func (r *distributedPageRelay) serve() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			return
		}
		tcp, ok := conn.(*net.TCPConn)
		if !ok {
			conn.Close()
			continue
		}
		file, err := tcp.File()
		conn.Close()
		if err != nil {
			continue
		}
		go r.serveFile(file)
	}
}

func (r *distributedPageRelay) serveFile(file *os.File) {
	defer file.Close()
	command := runscCommand(r.backend.runsc,
		"--root", r.backend.root, "timetravel", "--distributed-connection-fd=3",
		r.source, "distributed-serve", r.session)
	command.ExtraFiles = []*os.File{file}
	command.Stdin = strings.NewReader("")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if err := command.Start(); err != nil {
		r.mu.Unlock()
		return
	}
	r.commands[command] = struct{}{}
	r.mu.Unlock()
	_ = command.Wait()
	r.mu.Lock()
	delete(r.commands, command)
	r.mu.Unlock()
}

func (r *distributedPageRelay) Close() {
	if r != nil {
		r.close.Do(func() {
			_ = r.listener.Close()
			r.mu.Lock()
			r.closed = true
			commands := make([]*exec.Cmd, 0, len(r.commands))
			for command := range r.commands {
				commands = append(commands, command)
			}
			r.mu.Unlock()
			for _, command := range commands {
				if command.Process != nil {
					_ = command.Process.Kill()
				}
			}
		})
	}
}

func resolveDistributedAdvertiseAddress(configured, bound string) (string, error) {
	host, port, err := net.SplitHostPort(configured)
	if err != nil || host == "" {
		return "", fmt.Errorf("invalid cluster page advertise address %q", configured)
	}
	if port == "0" {
		_, port, err = net.SplitHostPort(bound)
		if err != nil {
			return "", err
		}
	}
	return net.JoinHostPort(host, port), nil
}

func newDistributedTransactionID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "fork-" + hex.EncodeToString(random[:]), nil
}

func distributedRootfsDigest(info ttcbundle.ContainerInfo, sourceBranch, repositoryID string) (string, error) {
	identity := struct {
		ContainerID  string                    `json:"container_id"`
		ImageDigest  string                    `json:"image_digest"`
		RootfsUpper  string                    `json:"rootfs_upper"`
		Volumes      []ttcbundle.ManagedVolume `json:"volumes"`
		SourceBranch string                    `json:"source_branch"`
		RepositoryID string                    `json:"repository_id"`
	}{info.ID, info.Digest, info.RootfsUpperID, info.Volumes, sourceBranch, repositoryID}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (b *runtimeBackend) forkRemote(source selector, targetNode, targetContainer string, paused bool) (selector, error) {
	if b.distributedSource == nil {
		return selector{}, errors.New("ttcd is not configured for distributed forks")
	}
	peer := b.distributedSource.peers[targetNode]
	if peer == nil {
		return selector{}, fmt.Errorf("cluster target node %q is not configured", targetNode)
	}
	targetSelector, err := parseSelector(targetContainer)
	if err != nil {
		return selector{}, fmt.Errorf("target container: %w", err)
	}
	if targetSelector.container != targetContainer || targetSelector.branch != defaultBranchID {
		return selector{}, fmt.Errorf("target container %q must not include a branch", targetContainer)
	}
	resolved, sourceInfo, err := b.resolveSelector(source)
	if err != nil {
		return selector{}, err
	}
	source = resolved
	transactionID, err := newDistributedTransactionID()
	if err != nil {
		return selector{}, fmt.Errorf("generating distributed transaction ID: %w", err)
	}
	rootfsDigest, err := distributedRootfsDigest(sourceInfo, source.branch, b.distributedSource.capabilities.RepositoryID)
	if err != nil {
		return selector{}, err
	}
	identity := cluster.ForkIdentity{
		TransactionID: transactionID, SourceNodeID: b.distributedSource.capabilities.NodeID,
		TargetNodeID: targetNode, SourceContainer: source.container, SourceBranch: source.branch,
		TargetContainer: targetContainer, TargetBranch: transactionID,
		ImageDigest: sourceInfo.Digest, RootfsDigest: rootfsDigest,
		RepositoryID: b.distributedSource.capabilities.RepositoryID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tx, err := peer.Reserve(ctx, cluster.ReserveRequest{
		SourceCapabilities: b.distributedSource.capabilities,
		Identity:           identity,
	})
	if err != nil {
		return selector{}, fmt.Errorf("reserving target node %s: %w", targetNode, err)
	}
	var relay *distributedPageRelay
	captured := false
	committed := false
	defer func() {
		if committed {
			return
		}
		relay.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		status, statusErr := peer.Status(cleanupCtx, transactionID)
		if statusErr == nil {
			_, _ = peer.Cancel(cleanupCtx, transactionID, status.Revision)
		} else if tx != nil {
			_, _ = peer.Cancel(cleanupCtx, transactionID, tx.Revision)
		}
		if captured {
			_, _ = b.command("timetravel", source.container, "distributed-cancel", transactionID)
		}
	}()
	listener, err := net.Listen("tcp", b.distributedSource.pageListen)
	if err != nil {
		return selector{}, fmt.Errorf("opening distributed page listener: %w", err)
	}
	advertise, err := resolveDistributedAdvertiseAddress(b.distributedSource.pageAdvertise, listener.Addr().String())
	if err != nil {
		listener.Close()
		return selector{}, err
	}
	relayBackend := *b
	relayBackend.stdin = strings.NewReader("")
	relayBackend.stdout = io.Discard
	relayBackend.stderr = io.Discard
	relay = newDistributedPageRelay(listener, &relayBackend, source.container, transactionID)

	args := []string{
		"timetravel", "--source", source.branch,
		"--distributed-session", transactionID,
		"--distributed-family", source.container,
		"--distributed-external-listener",
		"--distributed-advertise", advertise,
		"--distributed-max-transfer", fmt.Sprint(tx.Negotiated.MaxTransferSize),
	}
	args = append(args, source.container, "distributed-capture", transactionID)
	output, err := b.command(args...)
	if err != nil {
		return selector{}, fmt.Errorf("capturing source: %w", err)
	}
	captured = true
	var session boot.DistributedForkSessionInfo
	if err := json.Unmarshal(output, &session); err != nil {
		return selector{}, fmt.Errorf("decoding source capture: %w", err)
	}
	if session.SessionID != transactionID || session.MetadataBytes == 0 || session.StreamBytes == 0 {
		return selector{}, fmt.Errorf("source returned an incomplete distributed session")
	}
	descriptor := cluster.RemoteSessionDescriptor{
		Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
		MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
		MaxTransferBytes: tx.Negotiated.MaxTransferSize,
	}
	tx, err = peer.Prepare(ctx, transactionID, tx.Revision, descriptor)
	if err != nil {
		return selector{}, fmt.Errorf("restoring target node %s: %w", targetNode, err)
	}
	tx, err = peer.Publish(ctx, transactionID, tx.Revision, paused)
	if err != nil {
		// A response may be lost after publication. Status makes this operation
		// safely retryable without accidentally deleting a visible target.
		status, statusErr := peer.Status(ctx, transactionID)
		if statusErr != nil || (status.State != cluster.TransactionPublished && status.State != cluster.TransactionIndependent) {
			return selector{}, fmt.Errorf("publishing target node %s: %w", targetNode, err)
		}
		tx = status
	}
	committed = true
	monitorBackend := *b
	monitorBackend.stdin = strings.NewReader("")
	monitorBackend.stdout = io.Discard
	monitorBackend.stderr = io.Discard
	go monitorBackend.monitorDistributedFork(peer, source.container, transactionID, tx.Revision, relay)
	return selector{container: targetContainer, branch: defaultBranchID}, nil
}

func (b *runtimeBackend) monitorDistributedFork(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay) {
	defer relay.Close()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		peerStatus, statusErr := peer.Status(ctx, transactionID)
		cancel()
		if statusErr == nil {
			revision = peerStatus.Revision
			switch peerStatus.State {
			case cluster.TransactionIndependent:
				_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
				return
			case cluster.TransactionCanceled, cluster.TransactionFailed:
				_, _ = b.command("timetravel", sourceContainer, "distributed-cancel", transactionID)
				return
			}
		}
		output, err := b.command("timetravel", sourceContainer, "distributed-status", transactionID)
		if err != nil {
			return
		}
		var status boot.DistributedForkSessionStatus
		if err := json.Unmarshal(output, &status); err != nil {
			return
		}
		if status.Snapshot.PagesOutstanding != 0 {
			continue
		}
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		tx, err := peer.MarkIndependent(ctx, transactionID, revision)
		if err != nil {
			if current, statusErr := peer.Status(ctx, transactionID); statusErr == nil && current.State == cluster.TransactionIndependent {
				tx = current
				err = nil
			}
		}
		cancel()
		if err != nil || tx == nil {
			continue
		}
		_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
		return
	}
}

func resumeDistributedForkMonitors(targetRuntime *daemonTargetRuntime, backend *runtimeBackend, runtime *daemonClusterRuntime) {
	if targetRuntime == nil || backend == nil || runtime == nil || runtime.controlPlane == nil || runtime.controlPlane.Store == nil {
		return
	}
	transactions, err := runtime.controlPlane.Store.List()
	if err != nil {
		return
	}
	localNode := runtime.controlPlane.Capabilities.NodeID
	for i := range transactions {
		tx := transactions[i]
		if tx.Terminal() {
			continue
		}
		if tx.Identity.TargetNodeID == localNode && (tx.State == cluster.TransactionPublished || tx.State == cluster.TransactionDraining) {
			if info, err := backend.containerStore().LoadDistributedForkTarget(tx.Identity.TransactionID); err == nil {
				targetRuntime.monitorDependency(tx.Identity.TransactionID, info.ID)
			}
		}
		if tx.Identity.SourceNodeID != localNode || !tx.SourceNeeded ||
			(tx.State != cluster.TransactionPublished && tx.State != cluster.TransactionDraining) {
			continue
		}
		peer := runtime.source.peers[tx.Identity.TargetNodeID]
		if peer == nil {
			continue
		}
		monitorBackend := *backend
		monitorBackend.stdin = strings.NewReader("")
		monitorBackend.stdout = io.Discard
		monitorBackend.stderr = io.Discard
		go monitorBackend.monitorDistributedFork(peer, tx.Identity.SourceContainer,
			tx.Identity.TransactionID, tx.Revision, nil)
	}
}
