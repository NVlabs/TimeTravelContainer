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
	"strings"
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
	captured := false
	committed := false
	defer func() {
		if committed {
			return
		}
		status, statusErr := peer.Status(context.Background(), transactionID)
		if statusErr == nil {
			_, _ = peer.Cancel(context.Background(), transactionID, status.Revision)
		} else if tx != nil {
			_, _ = peer.Cancel(context.Background(), transactionID, tx.Revision)
		}
		if captured {
			_, _ = b.command("timetravel", source.container, "distributed-cancel", transactionID)
		}
	}()

	args := []string{
		"timetravel", "--source", source.branch,
		"--distributed-session", transactionID,
		"--distributed-family", source.container,
		"--distributed-listen", b.distributedSource.pageListen,
		"--distributed-max-transfer", fmt.Sprint(tx.Negotiated.MaxTransferSize),
	}
	if b.distributedSource.pageAdvertise != "" {
		args = append(args, "--distributed-advertise", b.distributedSource.pageAdvertise)
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
	go monitorBackend.monitorDistributedFork(peer, source.container, transactionID, tx.Revision)
	return selector{container: targetContainer, branch: defaultBranchID}, nil
}

func (b *runtimeBackend) monitorDistributedFork(peer *cluster.Client, sourceContainer, transactionID string, revision uint64) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
