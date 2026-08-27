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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	sentryremote "gvisor.dev/gvisor/pkg/sentry/ttcremote"
	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

type distributedSourceRuntime struct {
	capabilities  cluster.Capabilities
	peers         map[string]*cluster.Client
	directory     *cluster.NodeDirectory
	certFile      string
	keyFile       string
	caFile        string
	peerMu        sync.Mutex
	dynamicPeers  map[string]dynamicClusterPeer
	pageListen    string
	pageAdvertise string
}

type dynamicClusterPeer struct {
	incarnation string
	controlURL  string
	serverName  string
	client      *cluster.Client
}

func (r *distributedSourceRuntime) peer(nodeID string, forPlacement bool) (*cluster.Client, error) {
	if r == nil {
		return nil, errors.New("distributed source runtime is unavailable")
	}
	if nodeID == r.capabilities.NodeID {
		return nil, fmt.Errorf("cluster target node %q is the local node", nodeID)
	}
	if peer := r.peers[nodeID]; peer != nil {
		return peer, nil
	}
	if r.directory == nil {
		return nil, fmt.Errorf("cluster target node %q is not configured", nodeID)
	}
	record, err := r.directory.Lookup(nodeID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("cluster target node %q is not registered", nodeID)
		}
		return nil, fmt.Errorf("discovering cluster target node %q: %w", nodeID, err)
	}
	now := time.Now()
	if !record.LeaseActive(now) {
		return nil, fmt.Errorf("cluster target node %q is %s or its lease has expired", nodeID, record.State)
	}
	if forPlacement && !record.AcceptsPlacement(now) {
		return nil, fmt.Errorf("cluster target node %q is not accepting placement (state=%s)", nodeID, record.State)
	}
	if record.Capabilities.RepositoryID != r.capabilities.RepositoryID {
		return nil, fmt.Errorf("cluster target node %q uses repository %q, want %q", nodeID, record.Capabilities.RepositoryID, r.capabilities.RepositoryID)
	}
	r.peerMu.Lock()
	defer r.peerMu.Unlock()
	if cached, ok := r.dynamicPeers[nodeID]; ok && cached.incarnation == record.Incarnation &&
		cached.controlURL == record.ControlURL && cached.serverName == record.ControlServerName {
		return cached.client, nil
	}
	clientTLS, err := cluster.LoadClientTLSConfig(r.certFile, r.keyFile, r.caFile, record.ControlServerName)
	if err != nil {
		return nil, fmt.Errorf("configuring discovered cluster peer %q TLS: %w", nodeID, err)
	}
	client, err := cluster.NewClient(record.ControlURL, clientTLS, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("configuring discovered cluster peer %q: %w", nodeID, err)
	}
	r.dynamicPeers[nodeID] = dynamicClusterPeer{
		incarnation: record.Incarnation, controlURL: record.ControlURL,
		serverName: record.ControlServerName, client: client,
	}
	return client, nil
}

type distributedForkBackend interface {
	forkRemote(source selector, targetNode, targetContainer string, paused bool) (selector, error)
	forkRemoteMany(source selector, targetNode string, targetContainers []string, paused bool) ([]selector, error)
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
	timeout := unix.NsecToTimeval((30 * time.Second).Nanoseconds())
	if err := unix.SetsockoptTimeval(int(file.Fd()), unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		return
	}
	if err := unix.SetsockoptTimeval(int(file.Fd()), unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout); err != nil {
		return
	}
	args := append(r.backend.runtimeControlArgs(),
		"timetravel", "--distributed-connection-fd=3",
		r.source, "distributed-serve", r.session)
	command := runscCommand(r.backend.runsc, args...)
	command.ExtraFiles = []*os.File{file}
	command.Stdin = strings.NewReader("")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	setTTCCommandParentDeathSignal(command)
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

func distributedSnapshotRelativePath(transactionID string) string {
	return filepath.Join("cluster", "memory-snapshots", transactionID+".mem")
}

func (b *runtimeBackend) exportDistributedSnapshot(containerID, transactionID string) (string, error) {
	if b.dataRoot == "" {
		return "", errors.New("distributed snapshot export requires a TTC data root")
	}
	relative := distributedSnapshotRelativePath(transactionID)
	directory := filepath.Join(b.dataRoot, filepath.Dir(relative))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", fmt.Errorf("creating distributed snapshot directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".snapshot-*")
	if err != nil {
		return "", fmt.Errorf("creating distributed memory snapshot: %w", err)
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	args := []string{"timetravel", "--distributed-snapshot-fd=3"}
	if b.allowSlowVolumeCopy {
		args = append(args, "--distributed-snapshot-allow-copy")
	}
	args = append(args, containerID, "distributed-export", transactionID)
	if _, err := b.commandWithFiles([]*os.File{temp}, args...); err != nil {
		return "", fmt.Errorf("exporting durable distributed memory snapshot: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("closing distributed memory snapshot: %w", err)
	}
	finalPath := filepath.Join(b.dataRoot, relative)
	if err := os.Rename(tempPath, finalPath); err != nil {
		return "", fmt.Errorf("publishing distributed memory snapshot: %w", err)
	}
	keep = true
	if dir, err := os.Open(directory); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return relative, nil
}

func (b *runtimeBackend) exportDistributedSparse(containerID, transactionID string) (string, boot.DistributedForkExportInfo, error) {
	if b.dataRoot == "" {
		return "", boot.DistributedForkExportInfo{}, errors.New("distributed sparse export requires a TTC data root")
	}
	// Distributed roots must outlive the source container directory: the
	// target may remain partially resident after the source family is removed.
	// A cluster-level content-addressed repository also deduplicates identical
	// chunks across source families and remote siblings.
	relative := filepath.Join("cluster", "memory-chunks")
	path := filepath.Join(b.dataRoot, relative)
	repository, err := ttcchunk.OpenMemoryRepository(path)
	if err != nil {
		return "", boot.DistributedForkExportInfo{}, fmt.Errorf("opening distributed sparse repository: %w", err)
	}
	defer repository.Close()
	dir, err := os.Open(path)
	if err != nil {
		return "", boot.DistributedForkExportInfo{}, err
	}
	output, err := b.commandWithFiles([]*os.File{dir}, "timetravel", "--dehydration-memory-repository-fd=3", containerID, "distributed-export", transactionID)
	_ = dir.Close()
	if err != nil {
		return "", boot.DistributedForkExportInfo{}, fmt.Errorf("exporting distributed sparse memory: %w", err)
	}
	var info boot.DistributedForkExportInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return "", boot.DistributedForkExportInfo{}, fmt.Errorf("decoding distributed sparse manifest: %w", err)
	}
	if info.Manifest == (ttcchunk.ManifestID{}) || info.FilesystemBranch == "" {
		return "", boot.DistributedForkExportInfo{}, errors.New("distributed sparse export returned incomplete identity")
	}
	if err := repository.SetBranch(info.FilesystemBranch, info.Manifest); err != nil {
		return "", boot.DistributedForkExportInfo{}, fmt.Errorf("rooting distributed sparse manifest: %w", err)
	}
	return relative, info, nil
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

func (b *runtimeBackend) forkRemote(source selector, targetNode, targetDestination string, paused bool) (selector, error) {
	if b.distributedSource == nil {
		return selector{}, errors.New("ttcd is not configured for distributed forks")
	}
	peer, err := b.distributedSource.peer(targetNode, true)
	if err != nil {
		return selector{}, err
	}
	target, err := parseSelector(targetDestination)
	if err != nil {
		return selector{}, fmt.Errorf("invalid distributed destination %q: %w", targetDestination, err)
	}
	targetContainer := target.container
	resolved, sourceInfo, err := b.resolveSelector(source)
	if err != nil {
		return selector{}, err
	}
	source = resolved
	if !sourceInfo.LiveForkEligible {
		return selector{}, fmt.Errorf("container family %q is not live fork eligible: %s", sourceInfo.Name, sourceInfo.LiveForkWaiver)
	}
	sourceBranch, err := persistentBranchInfo(sourceInfo, source.branch)
	if err != nil {
		return selector{}, err
	}
	if err := b.containerStore().PromoteContainerRepositories(source.container); err != nil {
		return selector{}, fmt.Errorf("stabilizing source filesystem repositories: %w", err)
	}
	sourceInfo, err = b.containerStore().LoadContainer(source.container)
	if err != nil {
		return selector{}, fmt.Errorf("reloading promoted source metadata: %w", err)
	}
	if normalizeLifecycleState(sourceBranch.State) == "dehydrated" {
		return b.forkRemoteDehydrated(source, sourceInfo, peer, targetNode, target, paused, nil)
	}
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
		TargetVisibleBranch: target.branch,
		ImageDigest:         sourceInfo.Digest, RootfsDigest: rootfsDigest,
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

	dataPlaneMax := tx.Negotiated.PagingPolicy.TransferBytes
	if tx.Negotiated.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
		// TransferBytes is the adaptive controller's initial request/window
		// size. Negotiate the full protocol limit with the transport so runtime
		// window growth can also coalesce I/O into larger network requests.
		dataPlaneMax = tx.Negotiated.MaxTransferSize
	}
	args := []string{
		"timetravel", "--source", source.branch,
		"--distributed-session", transactionID,
		"--distributed-family", source.container,
		"--distributed-external-listener",
		"--distributed-advertise", advertise,
		"--distributed-max-transfer", fmt.Sprint(dataPlaneMax),
		"--distributed-push-policy", string(tx.Negotiated.PagingPolicy.Push),
		"--distributed-push-high-water", fmt.Sprint(tx.Negotiated.PagingPolicy.MaxOutstandingBytes),
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
		MaxTransferBytes: dataPlaneMax,
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
	if tx.Negotiated.PagingPolicy.Push == ttcpolicy.PushDemandOnly {
		go monitorBackend.handoffDistributedFork(peer, source.container, transactionID, tx.Revision, relay)
	} else {
		go monitorBackend.monitorDistributedFork(peer, source.container, transactionID, tx.Revision, relay)
	}
	return target, nil
}

// forkRemoteDehydrated restores a process-free branch directly from its
// durable kernel metadata and sparse memory graph. No source Sentry is
// created, and the target is independent as soon as publication completes.
func (b *runtimeBackend) forkRemoteDehydrated(source selector, sourceInfo ttcbundle.ContainerInfo, peer *cluster.Client, targetNode string, target selector, paused bool, committedTransaction *string) (result selector, retErr error) {
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
		TargetContainer: target.container, TargetBranch: transactionID, TargetVisibleBranch: target.branch,
		ImageDigest: sourceInfo.Digest, RootfsDigest: rootfsDigest,
		RepositoryID: b.distributedSource.capabilities.RepositoryID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tx, err := peer.Reserve(ctx, cluster.ReserveRequest{SourceCapabilities: b.distributedSource.capabilities, Identity: identity})
	if err != nil {
		return selector{}, fmt.Errorf("reserving target node %s: %w", targetNode, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		status, statusErr := peer.Status(cleanupCtx, transactionID)
		if statusErr == nil {
			_, _ = peer.Cancel(cleanupCtx, transactionID, status.Revision)
		} else {
			_, _ = peer.Cancel(cleanupCtx, transactionID, tx.Revision)
		}
	}()

	sourceBranch, err := persistentBranchInfo(sourceInfo, source.branch)
	if err != nil {
		return selector{}, err
	}
	imageDir := existingMemoryBranchDir(b.dataRoot, sourceInfo.ID, "dehydrated", sourceBranch)
	controlData, err := os.ReadFile(filepath.Join(imageDir, "control.json"))
	if err != nil {
		return selector{}, fmt.Errorf("reading dehydrated source control state: %w", err)
	}
	var capture boot.BranchDehydrateInfo
	if err := json.Unmarshal(controlData, &capture); err != nil {
		return selector{}, fmt.Errorf("decoding dehydrated source control state: %w", err)
	}
	if capture.FilesystemBranch == "" || capture.SparseManifest == (ttcchunk.ManifestID{}) {
		return selector{}, errors.New("dehydrated source has no sparse memory root")
	}
	encodedMetadata, err := os.ReadFile(filepath.Join(imageDir, "metadata.state"))
	if err != nil {
		return selector{}, fmt.Errorf("reading dehydrated source kernel metadata: %w", err)
	}
	metadataRelative := filepath.Join("cluster", "memory-metadata", transactionID+".state")
	metadataPath := filepath.Join(b.dataRoot, metadataRelative)
	if err := writeDurableDistributedFile(metadataPath, encodedMetadata); err != nil {
		return selector{}, fmt.Errorf("publishing distributed metadata: %w", err)
	}
	defer os.Remove(metadataPath)

	clusterRepositoryRelative := filepath.Join("cluster", "memory-chunks")
	clusterRepository, err := ttcchunk.OpenMemoryRepository(filepath.Join(b.dataRoot, clusterRepositoryRelative))
	if err != nil {
		return selector{}, fmt.Errorf("opening cluster memory repository: %w", err)
	}
	defer clusterRepository.Close()
	sourceRepository, err := ttcchunk.OpenMemoryRepository(filepath.Join(b.dataRoot, "containers", sourceInfo.ID, "memory", "chunks"))
	if err != nil {
		return selector{}, fmt.Errorf("opening dehydrated source memory repository: %w", err)
	}
	if _, err := clusterRepository.ImportManifest(sourceRepository, capture.SparseManifest); err != nil {
		sourceRepository.Close()
		return selector{}, fmt.Errorf("importing dehydrated source memory: %w", err)
	}
	if err := sourceRepository.Close(); err != nil {
		return selector{}, err
	}
	childFilesystemBranch := ttcvolume.ScopedBranchID(sourceInfo.ID, transactionID)
	if err := clusterRepository.SetBranch(childFilesystemBranch, capture.SparseManifest); err != nil {
		return selector{}, fmt.Errorf("rooting remote sparse memory: %w", err)
	}
	sparseRootCommitted := false
	defer func() {
		if !sparseRootCommitted {
			_ = clusterRepository.DeleteBranch(childFilesystemBranch)
			_, _ = clusterRepository.CollectGarbage(ttcchunk.GCOptions{})
		}
	}()

	filesystemForks, err := b.forkDehydratedDistributedFilesystems(sourceInfo.ID, capture.FilesystemBranch, childFilesystemBranch)
	if err != nil {
		return selector{}, err
	}
	defer func() {
		if !committed {
			for i := len(filesystemForks) - 1; i >= 0; i-- {
				_ = filesystemForks[i].abort()
			}
		}
	}()
	manifest, err := clusterRepository.LoadManifest(capture.SparseManifest)
	if err != nil {
		return selector{}, err
	}
	var streamBytes uint64
	for _, run := range manifest.Runs {
		streamBytes += uint64(run.Pages) * 4096
	}
	descriptor := cluster.RemoteSessionDescriptor{
		MetadataBytes: uint64(len(encodedMetadata)), StreamBytes: streamBytes,
		MaxTransferBytes: tx.Negotiated.PagingPolicy.TransferBytes,
		SharedMetadata:   metadataRelative, SparseRepository: clusterRepositoryRelative,
		SparseManifest: capture.SparseManifest.String(), SparseBranch: childFilesystemBranch,
		DurableDehydration: true,
	}
	tx, err = peer.Prepare(ctx, transactionID, tx.Revision, descriptor)
	if err != nil {
		return selector{}, fmt.Errorf("restoring dehydrated target node %s: %w", targetNode, err)
	}
	tx, err = peer.Publish(ctx, transactionID, tx.Revision, paused)
	if err != nil {
		status, statusErr := peer.Status(ctx, transactionID)
		if statusErr != nil || (status.State != cluster.TransactionPublished && status.State != cluster.TransactionIndependent) {
			return selector{}, fmt.Errorf("publishing dehydrated target node %s: %w", targetNode, err)
		}
		tx = status
	}
	for _, fork := range filesystemForks {
		if err := fork.commit(); err != nil {
			return selector{}, fmt.Errorf("committing distributed filesystem fork: %w", err)
		}
	}
	sparseRootCommitted = true
	committed = true
	if tx.State != cluster.TransactionIndependent {
		if _, err := peer.MarkIndependentWithMetrics(ctx, transactionID, tx.Revision, cluster.TransferMetrics{}); err != nil {
			if status, statusErr := peer.Status(ctx, transactionID); statusErr != nil || status.State != cluster.TransactionIndependent {
				return selector{}, fmt.Errorf("marking dehydrated target independent: %w", err)
			}
		}
	}
	if committedTransaction != nil {
		*committedTransaction = transactionID
	}
	return target, nil
}

func writeDurableDistributedFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".metadata-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type distributedFilesystemFork struct {
	tx         *ttcvolume.ForkTransaction
	repository *ttcvolume.Repository
}

func (f *distributedFilesystemFork) commit() error {
	err := f.tx.Commit()
	_ = f.repository.Close()
	return err
}

func (f *distributedFilesystemFork) abort() error {
	err := f.tx.Abort()
	_ = f.repository.Close()
	return err
}

func (b *runtimeBackend) forkDehydratedDistributedFilesystems(containerID, sourceBranch, childBranch string) ([]*distributedFilesystemFork, error) {
	spec, err := b.containerStore().LoadSpec(containerID)
	if err != nil {
		return nil, err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return nil, err
	}
	transactions := make([]*distributedFilesystemFork, 0, len(configs))
	seen := make(map[string]bool)
	for _, config := range configs {
		if seen[config.Path] {
			continue
		}
		seen[config.Path] = true
		dir, err := os.Open(config.Path)
		if err != nil {
			return transactions, errors.Join(err, abortFilesystemForks(transactions))
		}
		repository, err := ttcvolume.OpenRepositoryFD(dir, ttcvolume.CopyMode(config.CopyMode))
		_ = dir.Close()
		if err != nil {
			return transactions, errors.Join(err, abortFilesystemForks(transactions))
		}
		tx, err := repository.BeginFork(sourceBranch, childBranch)
		if err != nil {
			repository.Close()
			return transactions, errors.Join(err, abortFilesystemForks(transactions))
		}
		// ForkTransaction retains its repository; close after Commit/Abort is not
		// currently exposed, so keep the donated descriptor for transaction life.
		transactions = append(transactions, &distributedFilesystemFork{tx: tx, repository: repository})
	}
	return transactions, nil
}

func abortFilesystemForks(transactions []*distributedFilesystemFork) error {
	var result error
	for i := len(transactions) - 1; i >= 0; i-- {
		result = errors.Join(result, transactions[i].abort())
	}
	return result
}

// forkRemoteMany reserves multiple targets before pausing the source, then
// captures all sibling memory/filesystem branches in one Sentry operation.
// Each child retains an independent transaction and page session after the
// shared capture.
func (b *runtimeBackend) forkRemoteMany(source selector, targetNode string, targetContainers []string, paused bool) ([]selector, error) {
	if len(targetContainers) == 0 {
		return nil, errors.New("distributed fork requires at least one destination")
	}
	if len(targetContainers) == 1 {
		target, err := b.forkRemote(source, targetNode, targetContainers[0], paused)
		if err != nil {
			return nil, err
		}
		return []selector{target}, nil
	}
	if b.distributedSource == nil {
		return nil, errors.New("ttcd is not configured for distributed forks")
	}
	peer, err := b.distributedSource.peer(targetNode, true)
	if err != nil {
		return nil, err
	}
	destinations := make([]selector, len(targetContainers))
	seenTargets := make(map[string]struct{}, len(targetContainers))
	for i, value := range targetContainers {
		target, err := parseSelector(value)
		if err != nil {
			return nil, fmt.Errorf("invalid distributed destination %q: %w", value, err)
		}
		if _, ok := seenTargets[target.String()]; ok {
			return nil, fmt.Errorf("distributed fork repeats target %q", target)
		}
		seenTargets[target.String()] = struct{}{}
		destinations[i] = target
	}
	resolved, sourceInfo, err := b.resolveSelector(source)
	if err != nil {
		return nil, err
	}
	source = resolved
	if !sourceInfo.LiveForkEligible {
		return nil, fmt.Errorf("container family %q is not live fork eligible: %s", sourceInfo.Name, sourceInfo.LiveForkWaiver)
	}
	if err := b.containerStore().PromoteContainerRepositories(source.container); err != nil {
		return nil, fmt.Errorf("stabilizing source filesystem repositories: %w", err)
	}
	sourceInfo, err = b.containerStore().LoadContainer(source.container)
	if err != nil {
		return nil, fmt.Errorf("reloading promoted source metadata: %w", err)
	}
	sourceBranch, err := persistentBranchInfo(sourceInfo, source.branch)
	if err != nil {
		return nil, err
	}
	if normalizeLifecycleState(sourceBranch.State) == "dehydrated" {
		results := make([]selector, 0, len(targetContainers))
		transactions := make([]string, 0, len(targetContainers))
		for _, target := range destinations {
			var transactionID string
			result, err := b.forkRemoteDehydrated(source, sourceInfo, peer, targetNode, target, paused, &transactionID)
			if err == nil {
				results = append(results, result)
				transactions = append(transactions, transactionID)
				continue
			}
			// Cold targets are already source-independent when the helper
			// returns. Remove earlier siblings so the batch remains all-or-none
			// from the caller's perspective.
			var rollbackErr error
			for i := len(transactions) - 1; i >= 0; i-- {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				status, statusErr := peer.Status(cleanupCtx, transactions[i])
				if statusErr == nil {
					_, cancelErr := peer.Cancel(cleanupCtx, transactions[i], status.Revision)
					// Cancel of an independent transaction removes the runtime
					// before the terminal-state transition is rejected.
					if cancelErr != nil {
						if _, verifyErr := b.containerStore().LoadContainer(results[i].container); verifyErr == nil {
							rollbackErr = errors.Join(rollbackErr, cancelErr)
						}
					}
				} else {
					rollbackErr = errors.Join(rollbackErr, statusErr)
				}
				cleanupCancel()
			}
			return nil, errors.Join(fmt.Errorf("forking dehydrated target %s on node %s: %w", target, targetNode, err), rollbackErr)
		}
		return results, nil
	}
	rootfsDigest, err := distributedRootfsDigest(sourceInfo, source.branch, b.distributedSource.capabilities.RepositoryID)
	if err != nil {
		return nil, err
	}
	type childState struct {
		target       selector
		transaction  string
		tx           *cluster.Transaction
		relay        *distributedPageRelay
		advertise    string
		dataPlaneMax uint64
		captured     bool
	}
	children := make([]childState, len(targetContainers))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		for i := range children {
			child := &children[i]
			child.relay.Close()
			if child.transaction == "" {
				continue
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if status, statusErr := peer.Status(cleanupCtx, child.transaction); statusErr == nil {
				_, _ = peer.Cancel(cleanupCtx, child.transaction, status.Revision)
			} else if child.tx != nil {
				_, _ = peer.Cancel(cleanupCtx, child.transaction, child.tx.Revision)
			}
			cleanupCancel()
			if child.captured {
				_, _ = b.command("timetravel", source.container, "distributed-cancel", child.transaction)
			}
		}
	}()

	var commonPolicy *ttcpolicy.Policy
	var commonDataPlaneMax uint64
	for i, target := range destinations {
		transactionID, err := newDistributedTransactionID()
		if err != nil {
			return nil, fmt.Errorf("generating distributed transaction ID: %w", err)
		}
		identity := cluster.ForkIdentity{
			TransactionID: transactionID, SourceNodeID: b.distributedSource.capabilities.NodeID,
			TargetNodeID: targetNode, SourceContainer: source.container, SourceBranch: source.branch,
			TargetContainer: target.container, TargetBranch: transactionID, TargetVisibleBranch: target.branch,
			ImageDigest: sourceInfo.Digest, RootfsDigest: rootfsDigest,
			RepositoryID: b.distributedSource.capabilities.RepositoryID,
		}
		tx, err := peer.Reserve(ctx, cluster.ReserveRequest{SourceCapabilities: b.distributedSource.capabilities, Identity: identity})
		if err != nil {
			return nil, fmt.Errorf("reserving target %s on node %s: %w", target, targetNode, err)
		}
		dataPlaneMax := tx.Negotiated.PagingPolicy.TransferBytes
		if tx.Negotiated.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
			dataPlaneMax = tx.Negotiated.MaxTransferSize
		}
		if commonPolicy == nil {
			policy := tx.Negotiated.PagingPolicy
			commonPolicy = &policy
			commonDataPlaneMax = dataPlaneMax
		} else if *commonPolicy != tx.Negotiated.PagingPolicy || commonDataPlaneMax != dataPlaneMax {
			return nil, fmt.Errorf("target %s negotiated a different paging policy within one sibling batch", target)
		}
		listener, err := net.Listen("tcp", b.distributedSource.pageListen)
		if err != nil {
			return nil, fmt.Errorf("opening distributed page listener for %s: %w", target, err)
		}
		advertise, err := resolveDistributedAdvertiseAddress(b.distributedSource.pageAdvertise, listener.Addr().String())
		if err != nil {
			listener.Close()
			return nil, err
		}
		relayBackend := *b
		relayBackend.stdin = strings.NewReader("")
		relayBackend.stdout = io.Discard
		relayBackend.stderr = io.Discard
		children[i] = childState{
			target: target, transaction: transactionID, tx: tx,
			relay:     newDistributedPageRelay(listener, &relayBackend, source.container, transactionID),
			advertise: advertise, dataPlaneMax: dataPlaneMax,
		}
	}
	captureArgs := boot.DistributedForkCaptureManyArgs{
		Source: source.branch, FamilyID: source.container,
		MaxTransferBytes: commonDataPlaneMax, PushPolicy: sentryremote.TCPPushPolicy(commonPolicy.Push),
		PushHighWater: commonPolicy.MaxOutstandingBytes,
		Children:      make([]boot.DistributedForkCaptureChildArgs, len(children)),
	}
	for i := range children {
		captureArgs.Children[i] = boot.DistributedForkCaptureChildArgs{
			SessionID: children[i].transaction, TargetBranchID: children[i].transaction,
			AdvertiseAddress: children[i].advertise,
		}
	}
	configReader, configWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating distributed batch config pipe: %w", err)
	}
	encodedConfig, err := json.Marshal(&captureArgs)
	if err == nil {
		_, err = configWriter.Write(encodedConfig)
	}
	closeErr := configWriter.Close()
	if err != nil {
		configReader.Close()
		return nil, fmt.Errorf("writing distributed batch config: %w", err)
	}
	if closeErr != nil {
		configReader.Close()
		return nil, fmt.Errorf("closing distributed batch config: %w", closeErr)
	}
	output, err := b.commandWithFiles([]*os.File{configReader}, "timetravel", "--distributed-config-fd=3",
		source.container, "distributed-capture-many", children[0].transaction)
	configReader.Close()
	if err != nil {
		return nil, fmt.Errorf("capturing %d distributed siblings: %w", len(children), err)
	}
	var sessions []boot.DistributedForkSessionInfo
	if err := json.Unmarshal(output, &sessions); err != nil {
		return nil, fmt.Errorf("decoding distributed sibling capture: %w", err)
	}
	if len(sessions) != len(children) {
		return nil, fmt.Errorf("source returned %d sessions for %d siblings", len(sessions), len(children))
	}
	for i := range children {
		children[i].captured = true
	}
	sessionByID := make(map[string]boot.DistributedForkSessionInfo, len(sessions))
	for _, session := range sessions {
		sessionByID[session.SessionID] = session
	}
	descriptors := make([]cluster.RemoteSessionDescriptor, len(children))
	for i := range children {
		child := &children[i]
		session, ok := sessionByID[child.transaction]
		if !ok || session.MetadataBytes == 0 || session.StreamBytes == 0 || session.Address != child.advertise {
			return nil, fmt.Errorf("source returned an incomplete session for target %s", child.target)
		}
		descriptors[i] = cluster.RemoteSessionDescriptor{
			Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
			MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
			MaxTransferBytes: child.dataPlaneMax,
		}
	}
	prepareErrors := make([]error, len(children))
	var prepareWG sync.WaitGroup
	for i := range children {
		i := i
		prepareWG.Add(1)
		go func() {
			defer prepareWG.Done()
			child := &children[i]
			child.tx, prepareErrors[i] = peer.Prepare(ctx, child.transaction, child.tx.Revision, descriptors[i])
		}()
	}
	prepareWG.Wait()
	for i, prepareErr := range prepareErrors {
		if prepareErr != nil {
			return nil, fmt.Errorf("restoring target %s on node %s: %w", children[i].target, targetNode, prepareErr)
		}
	}
	publishErrors := make([]error, len(children))
	var publishWG sync.WaitGroup
	for i := range children {
		i := i
		publishWG.Add(1)
		go func() {
			defer publishWG.Done()
			child := &children[i]
			child.tx, publishErrors[i] = peer.Publish(ctx, child.transaction, child.tx.Revision, paused)
			if publishErrors[i] != nil {
				status, statusErr := peer.Status(ctx, child.transaction)
				if statusErr == nil && (status.State == cluster.TransactionPublished || status.State == cluster.TransactionIndependent) {
					child.tx = status
					publishErrors[i] = nil
				}
			}
		}()
	}
	publishWG.Wait()
	for i, publishErr := range publishErrors {
		if publishErr != nil {
			return nil, fmt.Errorf("publishing target %s on node %s: %w", children[i].target, targetNode, publishErr)
		}
	}
	// Every target is now externally visible and owns its durable snapshot.
	// Later independence bookkeeping failures must not cancel or delete them.
	cleanup = false
	results := make([]selector, len(children))
	for i := range children {
		child := &children[i]
		monitorBackend := *b
		monitorBackend.stdin = strings.NewReader("")
		monitorBackend.stdout = io.Discard
		monitorBackend.stderr = io.Discard
		if commonPolicy.Push == ttcpolicy.PushDemandOnly {
			go monitorBackend.handoffDistributedFork(peer, source.container, child.transaction, child.tx.Revision, child.relay)
		} else {
			go monitorBackend.monitorDistributedFork(peer, source.container, child.transaction, child.tx.Revision, child.relay)
		}
		results[i] = child.target
	}
	return results, nil
}

// handoffDistributedFork publishes fork-time pages after the target is visible,
// then atomically redirects all remaining faults from TCP to shared chunks.
// Failure before the target acknowledges the switch falls back to ordinary
// live paging; it never invalidates an already-published target.
func (b *runtimeBackend) handoffDistributedFork(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay) {
	repositoryPath, export, err := b.exportDistributedSparse(sourceContainer, transactionID)
	if err != nil {
		b.monitorDistributedFork(peer, sourceContainer, transactionID, revision, relay)
		return
	}
	sparse := cluster.SparseHandoffDescriptor{
		Repository: repositoryPath, Manifest: export.Manifest.String(), Branch: export.FilesystemBranch,
	}
	var tx *cluster.Transaction
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tx, err = peer.Handoff(ctx, transactionID, revision, sparse)
		if err != nil {
			if current, statusErr := peer.Status(ctx, transactionID); statusErr == nil {
				tx = current
				revision = current.Revision
				switch current.State {
				case cluster.TransactionDraining, cluster.TransactionIndependent:
					if current.SparseHandoff != nil && *current.SparseHandoff == sparse {
						err = nil
					} else if current.State == cluster.TransactionIndependent {
						// The target faulted every page from TCP before handoff.
						cancel()
						b.cleanupDistributedSparsePublication(repositoryPath, export.FilesystemBranch)
						_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
						relay.Close()
						return
					}
				case cluster.TransactionCanceled, cluster.TransactionFailed:
					cancel()
					b.cleanupDistributedSparsePublication(repositoryPath, export.FilesystemBranch)
					_, _ = b.command("timetravel", sourceContainer, "distributed-cancel", transactionID)
					relay.Close()
					return
				}
			}
		}
		cancel()
		if err == nil && tx != nil {
			break
		}
		// Once publication succeeds, never delete its root merely because an
		// RPC response or reader switch failed. The target may have completed
		// the switch, and both the durable descriptor and Sentry operation are
		// idempotent. Retain live paging and retry until target acknowledgement.
		time.Sleep(500 * time.Millisecond)
	}
	if tx.State == cluster.TransactionIndependent {
		_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
		relay.Close()
		return
	}
	if err := b.completeSharedDistributedFork(peer, sourceContainer, transactionID, tx.Revision, relay); err != nil {
		// Retrying status/independence is safe; the target now has no source
		// reads, while the source retains its snapshot until completion.
		b.monitorHandoffCompletion(peer, sourceContainer, transactionID, tx.Revision, relay)
	}
}

func (b *runtimeBackend) cleanupDistributedSparsePublication(repositoryPath, branch string) {
	if repositoryPath == "" || branch == "" {
		return
	}
	if repository, err := ttcchunk.OpenMemoryRepository(filepath.Join(b.dataRoot, repositoryPath)); err == nil {
		_ = repository.DeleteBranch(branch)
		_, _ = repository.CollectGarbage(ttcchunk.GCOptions{})
		_ = repository.Close()
	}
}

// reconcileClusterSparseMemoryRoots removes branch pointers left behind when
// a daemon dies after publishing a sparse handoff but before target metadata
// records ownership. Container records are authoritative after Prepare;
// nonterminal transaction identities protect the narrow pre-Prepare window.
func (b *runtimeBackend) reconcileClusterSparseMemoryRoots() error {
	if b == nil || b.dataRoot == "" {
		return nil
	}
	const repositoryRelative = "cluster/memory-chunks"
	repositoryPath := filepath.Join(b.dataRoot, repositoryRelative)
	if _, err := os.Stat(repositoryPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	repository, err := ttcchunk.OpenMemoryRepository(repositoryPath)
	if err != nil {
		return err
	}
	defer repository.Close()
	branches, err := repository.ListBranches()
	if err != nil {
		return err
	}
	keep := make(map[string]struct{})
	containers, err := b.containerStore().ListContainers()
	if err != nil {
		return err
	}
	for _, container := range containers {
		if filepath.Clean(container.DistributedSparseRepository) == repositoryRelative && container.DistributedSparseBranch != "" {
			keep[container.DistributedSparseBranch] = struct{}{}
		}
		for _, branch := range container.Branches {
			if filepath.Clean(branch.DistributedSparseRepository) == repositoryRelative && branch.DistributedSparseBranch != "" {
				keep[branch.DistributedSparseBranch] = struct{}{}
			}
		}
		for _, pending := range container.DistributedForkImports {
			if filepath.Clean(pending.SparseRepository) == repositoryRelative && pending.SparseBranch != "" {
				keep[pending.SparseBranch] = struct{}{}
			}
		}
	}
	transactions, err := cluster.OpenStore(filepath.Join(b.dataRoot, "cluster", "transactions"))
	if err != nil {
		return err
	}
	records, err := transactions.List()
	if err != nil {
		return err
	}
	for i := range records {
		if records[i].Terminal() {
			continue
		}
		keep[ttcvolume.ScopedBranchID(records[i].Identity.SourceContainer, records[i].Identity.TransactionID)] = struct{}{}
	}
	for branch := range branches {
		if _, ok := keep[branch]; ok {
			continue
		}
		if err := repository.DeleteBranch(branch); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing orphaned cluster memory root %q: %w", branch, err)
		}
	}
	_, err = repository.CollectGarbage(ttcchunk.GCOptions{})
	return err
}

func (b *runtimeBackend) monitorHandoffCompletion(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay) {
	defer relay.Close()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		status, err := peer.Status(ctx, transactionID)
		cancel()
		if err == nil {
			revision = status.Revision
			switch status.State {
			case cluster.TransactionIndependent:
				_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
				return
			case cluster.TransactionCanceled, cluster.TransactionFailed:
				_, _ = b.command("timetravel", sourceContainer, "distributed-cancel", transactionID)
				return
			}
		}
		if err := b.completeSharedDistributedFork(peer, sourceContainer, transactionID, revision, relay); err == nil {
			return
		}
	}
}

func (b *runtimeBackend) completeSharedDistributedFork(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay) error {
	output, err := b.command("timetravel", sourceContainer, "distributed-status", transactionID)
	if err != nil {
		return fmt.Errorf("reading shared-snapshot session status: %w", err)
	}
	var status boot.DistributedForkSessionStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return fmt.Errorf("decoding shared-snapshot session status: %w", err)
	}
	metrics := cluster.TransferMetrics{
		PullRequests: status.Transport.PullRequests, AckRequests: status.Transport.AckRequests,
		AckBatches: status.Transport.AckBatches, PullBytes: status.Transport.BytesSent,
		MetadataBytes: status.Transport.MetadataBytesSent, PushBytes: status.Transport.PushBytesSent,
		PushRetries: status.Transport.PushRetries, PagesTotal: status.Snapshot.PagesTotal,
		// The durable snapshot replaces every live source-page reference at this
		// transition. CompleteDistributedForkSession releases those references
		// immediately after the control plane records independence.
		PagesReleased: status.Snapshot.PagesTotal, PushHints: status.Snapshot.PushEnqueued,
		PushHintsDropped: status.Snapshot.PushDropped,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = peer.MarkIndependentWithMetrics(ctx, transactionID, revision, metrics)
	cancel()
	if err != nil {
		return fmt.Errorf("recording shared-snapshot independence: %w", err)
	}
	if _, err := b.command("timetravel", sourceContainer, "distributed-complete", transactionID); err != nil {
		return fmt.Errorf("releasing shared-snapshot source references: %w", err)
	}
	relay.Close()
	return nil
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
		metrics := cluster.TransferMetrics{
			PullRequests:     status.Transport.PullRequests,
			AckRequests:      status.Transport.AckRequests,
			AckBatches:       status.Transport.AckBatches,
			PullBytes:        status.Transport.BytesSent,
			MetadataBytes:    status.Transport.MetadataBytesSent,
			PushBytes:        status.Transport.PushBytesSent,
			PushRetries:      status.Transport.PushRetries,
			PagesTotal:       status.Snapshot.PagesTotal,
			PagesReleased:    status.Snapshot.PagesReleased,
			PushHints:        status.Snapshot.PushEnqueued,
			PushHintsDropped: status.Snapshot.PushDropped,
		}
		tx, err := peer.MarkIndependentWithMetrics(ctx, transactionID, revision, metrics)
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
				if tx.Identity.Migration && tx.State == cluster.TransactionPublished && !tx.Activated {
					if placements, openErr := cluster.OpenPlacementStore(filepath.Join(backend.dataRoot, "cluster", "placements")); openErr == nil {
						if placement, lookupErr := placements.Lookup(tx.Identity.SourceContainer, tx.Identity.SourceBranch); lookupErr == nil &&
							placement.OwnerNode == localNode && placement.LastOperationID == tx.Identity.TransactionID && placement.State != cluster.PlacementMigrating {
							txCopy := tx
							go recoverTargetMigrationActivation(targetRuntime, runtime.controlPlane, &txCopy)
						}
					}
				}
				if tx.State == cluster.TransactionPublished && tx.SparseHandoff != nil {
					txCopy := tx
					go recoverTargetSparseHandoff(targetRuntime, runtime.controlPlane, &txCopy)
				}
				targetRuntime.monitorDependency(tx.Identity.TransactionID, info.ID)
			}
		}
		if tx.Identity.SourceNodeID != localNode || !tx.SourceNeeded ||
			(tx.State != cluster.TransactionPublished && tx.State != cluster.TransactionDraining) {
			continue
		}
		peer, err := runtime.source.peer(tx.Identity.TargetNodeID, false)
		if err != nil {
			continue
		}
		if tx.Identity.Migration && tx.State == cluster.TransactionPublished && !tx.Activated {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			activated, activateErr := peer.Activate(ctx, tx.Identity.TransactionID, tx.Revision)
			cancel()
			if activateErr != nil || activated == nil || !activated.Activated {
				continue
			}
			tx = *activated
		}
		monitorBackend := *backend
		monitorBackend.stdin = strings.NewReader("")
		monitorBackend.stdout = io.Discard
		monitorBackend.stderr = io.Discard
		if tx.Identity.Migration {
			if tx.State == cluster.TransactionDraining {
				go monitorBackend.monitorHandoffCompletion(peer, tx.Identity.SourceContainer,
					tx.Identity.TransactionID, tx.Revision, nil)
			} else {
				go monitorBackend.handoffDistributedFork(peer, tx.Identity.SourceContainer,
					tx.Identity.TransactionID, tx.Revision, nil)
			}
		} else if tx.Negotiated.PagingPolicy.Push == ttcpolicy.PushDemandOnly {
			if tx.State == cluster.TransactionDraining {
				go monitorBackend.monitorHandoffCompletion(peer, tx.Identity.SourceContainer,
					tx.Identity.TransactionID, tx.Revision, nil)
			} else {
				go monitorBackend.handoffDistributedFork(peer, tx.Identity.SourceContainer,
					tx.Identity.TransactionID, tx.Revision, nil)
			}
		} else {
			go monitorBackend.monitorDistributedFork(peer, tx.Identity.SourceContainer,
				tx.Identity.TransactionID, tx.Revision, nil)
		}
		if tx.Identity.Migration {
			go monitorBackend.monitorSparseMigrationCleanup(peer, tx.Identity.SourceContainer,
				tx.Identity.SourceBranch, tx.Identity.TransactionID, nil)
		}
	}
}

func recoverTargetMigrationActivation(targetRuntime *daemonTargetRuntime, control *cluster.ControlPlane, tx *cluster.Transaction) {
	if targetRuntime == nil || control == nil || control.Store == nil || tx == nil || tx.Activated || !tx.Identity.Migration {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := targetRuntime.Activate(ctx, tx)
	cancel()
	if err != nil {
		return
	}
	_, _ = control.Store.MarkActivated(tx.Identity.TransactionID, tx.Revision, control.Now())
}

func recoverTargetSparseHandoff(targetRuntime *daemonTargetRuntime, control *cluster.ControlPlane, tx *cluster.Transaction) {
	if targetRuntime == nil || control == nil || control.Store == nil || tx == nil || tx.SparseHandoff == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := targetRuntime.Handoff(ctx, tx, *tx.SparseHandoff)
	cancel()
	if err != nil {
		return
	}
	_, _ = control.Store.Advance(tx.Identity.TransactionID, tx.Revision, cluster.TransactionDraining, "", control.Now())
}
