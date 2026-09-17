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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/bits"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/log"
	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/runsc/boot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

type daemonClusterProxy struct {
	socket string
}

type elasticCapacityRequest struct {
	Version         int        `json:"version"`
	RequestID       string     `json:"request_id"`
	Operation       string     `json:"operation"`
	Nodes           int        `json:"nodes"`
	MemoryBytesEach uint64     `json:"memory_bytes_each"`
	Architecture    string     `json:"architecture"`
	RepositoryID    string     `json:"repository_id"`
	SourceNode      string     `json:"source_node"`
	Reason          string     `json:"reason"`
	CreatedAt       time.Time  `json:"created_at"`
	Deadline        time.Time  `json:"deadline"`
	Attempts        uint64     `json:"attempts,omitempty"`
	LastAttemptAt   *time.Time `json:"last_attempt_at,omitempty"`
	NextAttemptAt   *time.Time `json:"next_attempt_at,omitempty"`
	AcceptedAt      *time.Time `json:"accepted_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
}

const (
	elasticDrainIntentVersion = 1
	elasticDrainActive        = "draining"
	elasticDrainComplete      = "complete"
)

type elasticDrainIntent struct {
	Version     int        `json:"version"`
	OperationID string     `json:"operation_id"`
	NodeID      string     `json:"node_id"`
	TargetNode  string     `json:"target_node,omitempty"`
	State       string     `json:"state"`
	Attempts    uint64     `json:"attempts"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func elasticDrainIntentPath(dataRoot, nodeID string) (string, error) {
	if nodeID == "" || filepath.Base(nodeID) != nodeID || nodeID == "." {
		return "", fmt.Errorf("invalid drain node ID %q", nodeID)
	}
	dir := filepath.Join(dataRoot, "cluster", "drains")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, nodeID+".json"), nil
}

func withElasticDrainIntent(dataRoot, nodeID string, fn func(string, *elasticDrainIntent) error) error {
	path, err := elasticDrainIntentPath(dataRoot, nodeID)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(strings.TrimSuffix(path, ".json")+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var intent elasticDrainIntent
	contents, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(contents, &intent); err != nil {
			return fmt.Errorf("decoding drain intent %q: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fn(path, &intent)
}

func validateElasticDrainIntent(intent elasticDrainIntent, nodeID string) error {
	if intent.Version != elasticDrainIntentVersion || intent.OperationID == "" || intent.NodeID != nodeID ||
		(intent.State != elasticDrainActive && intent.State != elasticDrainComplete) || intent.CreatedAt.IsZero() || intent.UpdatedAt.Before(intent.CreatedAt) {
		return fmt.Errorf("invalid durable drain intent for node %q", nodeID)
	}
	return nil
}

func loadElasticDrainIntent(dataRoot, nodeID string) (*elasticDrainIntent, error) {
	var result *elasticDrainIntent
	err := withElasticDrainIntent(dataRoot, nodeID, func(_ string, intent *elasticDrainIntent) error {
		if intent.Version == 0 {
			return nil
		}
		if err := validateElasticDrainIntent(*intent, nodeID); err != nil {
			return err
		}
		copy := *intent
		result = &copy
		return nil
	})
	return result, err
}

func beginElasticDrainIntent(dataRoot, nodeID, targetNode string) (*elasticDrainIntent, error) {
	var result elasticDrainIntent
	err := withElasticDrainIntent(dataRoot, nodeID, func(path string, intent *elasticDrainIntent) error {
		now := time.Now().UTC()
		if intent.Version != 0 {
			if err := validateElasticDrainIntent(*intent, nodeID); err != nil {
				return err
			}
			if intent.State == elasticDrainComplete {
				if intent.TargetNode != targetNode {
					return fmt.Errorf("completed drain %q for node %q targeted %q", intent.OperationID, nodeID, intent.TargetNode)
				}
				result = *intent
				return nil
			}
			intent.TargetNode = targetNode
			intent.Attempts++
			intent.UpdatedAt = now
			intent.LastError = ""
		} else {
			operationID, err := newDaemonRequestID()
			if err != nil {
				return err
			}
			*intent = elasticDrainIntent{
				Version: elasticDrainIntentVersion, OperationID: "drain-" + operationID,
				NodeID: nodeID, TargetNode: targetNode, State: elasticDrainActive,
				Attempts: 1, CreatedAt: now, UpdatedAt: now,
			}
		}
		if err := writeDurableJSON(path, intent); err != nil {
			return err
		}
		result = *intent
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func finishElasticDrainAttempt(dataRoot, nodeID, operationID string, attemptErr error) error {
	return withElasticDrainIntent(dataRoot, nodeID, func(path string, intent *elasticDrainIntent) error {
		if err := validateElasticDrainIntent(*intent, nodeID); err != nil {
			return err
		}
		if intent.OperationID != operationID {
			return fmt.Errorf("drain operation for node %q changed from %q to %q", nodeID, operationID, intent.OperationID)
		}
		now := time.Now().UTC()
		intent.UpdatedAt = now
		if attemptErr != nil {
			intent.State = elasticDrainActive
			intent.LastError = attemptErr.Error()
			intent.CompletedAt = nil
		} else {
			intent.State = elasticDrainComplete
			intent.LastError = ""
			intent.CompletedAt = &now
		}
		return writeDurableJSON(path, intent)
	})
}

func removeCompletedElasticDrainIntent(dataRoot, nodeID string) error {
	return withElasticDrainIntent(dataRoot, nodeID, func(path string, intent *elasticDrainIntent) error {
		if intent.Version == 0 {
			return nil
		}
		if err := validateElasticDrainIntent(*intent, nodeID); err != nil {
			return err
		}
		if intent.State != elasticDrainComplete {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// emitCapacityRequest publishes one outstanding request per node. The daemon's
// capacity worker delivers it to the configured provisioner and removes it only
// after compatible capacity has appeared in the cluster directory. The lock
// makes concurrent manual and automatic rebalance attempts coalesce into the
// same durable event.
func (b *runtimeBackend) emitCapacityRequest(source cluster.NodeRecord, reason string) (string, error) {
	var result string
	err := withElasticCapacityRequest(b.dataRoot, source.NodeID, func(path string, request *elasticCapacityRequest) error {
		result = path
		if request.Version != 0 {
			return validateElasticCapacityRequest(*request, source.NodeID)
		}
		requestID, err := newDaemonRequestID()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		memory := source.MemoryTargetBytes
		if memory == 0 {
			memory = source.MemoryCurrentBytes
		}
		*request = elasticCapacityRequest{
			Version: 1, RequestID: "capacity-" + requestID, Operation: "acquire", Nodes: 1,
			MemoryBytesEach: memory, Architecture: b.distributedSource.capabilities.Architecture,
			RepositoryID: b.distributedSource.capabilities.RepositoryID, SourceNode: source.NodeID,
			Reason: reason, CreatedAt: now, Deadline: now.Add(elasticCapacityAcceptanceLease),
		}
		return writeDurableJSON(path, request)
	})
	return result, err
}

func (p daemonClusterProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/vnd.ttc.daemon-stream" {
		http.Error(w, "invalid daemon stream content type", http.StatusUnsupportedMediaType)
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.EnableFullDuplex(); err != nil {
		http.Error(w, "cluster transport does not support full-duplex streams: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The HTTP server's ordinary request deadlines are appropriate for bounded
	// control RPCs, but a daemon stream may contain an interactive exec or a
	// node retirement whose evacuation legitimately takes longer than the
	// server WriteTimeout. That deadline starts when the request is accepted,
	// not when its first response byte is written; leaving it armed can discard
	// the final successful status frame after a slow migration. The caller's
	// context remains the lifetime and cancellation bound for this stream.
	if err := controller.SetReadDeadline(time.Time{}); err != nil {
		http.Error(w, "cluster transport cannot clear daemon-stream read deadline: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		http.Error(w, "cluster transport cannot clear daemon-stream write deadline: "+err.Error(), http.StatusInternalServerError)
		return
	}
	conn, err := net.Dial("unix", p.socket)
	if err != nil {
		http.Error(w, "connecting to local ttcd: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer conn.Close()
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(conn, r.Body)
		if unix, ok := conn.(*net.UnixConn); ok {
			_ = unix.CloseWrite()
		}
		close(copyDone)
	}()
	w.Header().Set("Content-Type", "application/vnd.ttc.daemon-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := conn.Read(buffer)
		if n != 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
	select {
	case <-copyDone:
	default:
	}
}

func daemonCommandPlacementWithCatalog(dataRoot string, placements cluster.PlacementCatalog, args []string) (string, []daemonPlacementClaim, error) {
	if dataRoot == "" {
		return "", nil, nil
	}
	// Published ports are ingress-node resources. Keep the command on the
	// daemon contacted by the client; each accepted connection resolves and
	// tunnels to the branch's current placement.
	if len(args) != 0 && args[0] == "port" {
		return "", nil, nil
	}
	// Durable wait observes Raft publication state and shared metadata; it does
	// not operate on a runtime. Routing it with the branch's old running
	// placement creates a race with the publication commit that deliberately
	// unplaces the branch before declaring it durable.
	if len(args) > 0 && args[0] == "wait" {
		for i := 1; i < len(args); i++ {
			if args[i] == "--condition=durable" || (args[i] == "--condition" && i+1 < len(args) && args[i+1] == "durable") {
				return "", nil, nil
			}
		}
	}
	// Route node-wide mutations before taking the receiving daemon's local
	// lifecycle lock. Otherwise an arbitrary entry node can hold its lock while
	// the source calls back into it as a migration target, deadlocking cutover.
	if len(args) >= 3 && args[0] == "node" && (args[1] == "stop" || args[1] == "rebalance" || args[1] == "_planner-resources") {
		var positional []string
		for i := 2; i < len(args); i++ {
			switch {
			case args[i] == "--_local" || args[i] == "--stop-all" || args[i] == "--dehydrate-all" || args[i] == "--migrate-all":
			case strings.HasPrefix(args[i], "-"):
				// Let runNode diagnose unknown flags on the selected daemon.
			default:
				positional = append(positional, args[i])
			}
		}
		if len(positional) == 1 {
			return positional[0], nil, nil
		}
	}
	store := ttcbundle.Store{Root: dataRoot}
	if placements == nil {
		return "", nil, errors.New("placement authority is unavailable")
	}
	owner := ""
	allowMaterializing := len(args) != 0 && (args[0] == "start" || args[0] == "rehydrate" || args[0] == "restart")
	var claims []daemonPlacementClaim
	seen := make(map[string]struct{})
	for _, arg := range args[1:] {
		candidate := arg
		if colon := strings.IndexByte(candidate, ':'); colon >= 0 {
			candidate = candidate[:colon]
		}
		at := strings.LastIndexByte(candidate, '@')
		if at <= 0 || at == len(candidate)-1 {
			continue
		}
		selected, err := parseSelector(candidate)
		if err != nil {
			continue
		}
		info, err := store.LoadContainer(selected.container)
		if err != nil {
			familyID, resolveErr := resolvePlacementFamily(placements, selected.container)
			if resolveErr != nil {
				continue
			}
			info.ID = familyID
		}
		branchOwner := ""
		placement, err := placements.Lookup(info.ID, selected.branch)
		switch {
		case err == nil:
			if placement.State == cluster.PlacementCorrupted {
				if args[0] == "rm" {
					continue
				}
				return "", nil, fmt.Errorf("branch %s is corrupted; only rm is permitted", selected)
			}
			if placement.State == cluster.PlacementMigrating {
				return "", nil, fmt.Errorf("branch %s is migrating from %q to %q; retry after cutover", selected, placement.OwnerNode, placement.TargetNode)
			}
			if placement.State == cluster.PlacementMaterializing && !allowMaterializing {
				return "", nil, fmt.Errorf("branch %s is materializing on %q; only start or rehydrate may retry it", selected, placement.OwnerNode)
			}
			if placement.State != cluster.PlacementUnplaced {
				branchOwner = placement.OwnerNode
				key := placement.FamilyID + "\x00" + placement.BranchID
				if _, ok := seen[key]; !ok {
					claims = append(claims, daemonPlacementClaim{
						FamilyID: placement.FamilyID, BranchID: placement.BranchID,
						OwnerNode: placement.OwnerNode, OwnerIncarnation: placement.OwnerIncarnation,
						Generation: placement.Generation, AllowMaterializing: placement.State == cluster.PlacementMaterializing && allowMaterializing,
					})
					seen[key] = struct{}{}
				}
			}
		case errors.Is(err, os.ErrNotExist):
			return "", nil, fmt.Errorf("resolving placement for %s: current-format clustered branch has no Raft placement record", selected)
		default:
			return "", nil, fmt.Errorf("resolving placement for %s: %w", selected, err)
		}
		if branchOwner == "" {
			continue
		}
		if owner != "" && owner != branchOwner {
			return "", nil, fmt.Errorf("command spans cluster nodes %q and %q", owner, branchOwner)
		}
		owner = branchOwner
	}
	return owner, claims, nil
}

// resolvePlacementFamily resolves the same useful subset of container naming
// as Store.LoadContainer without depending on shared-filesystem cache
// convergence. Placement display metadata is committed in the same Raft entry
// that first makes a branch routable.
func resolvePlacementFamily(placements cluster.PlacementCatalog, identifier string) (string, error) {
	if placements == nil {
		return "", os.ErrNotExist
	}
	records, err := placements.List()
	if err != nil {
		return "", err
	}
	matched := make(map[string]struct{})
	for _, record := range records {
		if record.FamilyID == identifier || record.FamilyName == identifier || strings.HasPrefix(record.FamilyID, identifier) {
			matched[record.FamilyID] = struct{}{}
		}
	}
	if len(matched) != 1 {
		return "", os.ErrNotExist
	}
	for id := range matched {
		return id, nil
	}
	panic("unreachable")
}

// loadDistributedFamily uses Raft display identity to bypass transient NFS
// directory-cache lag after a remote create. Placement is already committed
// before a distributed branch is routable, so its exact family ID is the
// stable lookup key even when Store.LoadContainer cannot yet discover the new
// directory by listing or by name.
func loadDistributedFamily(store ttcbundle.Store, placements cluster.PlacementCatalog, identifier string) (ttcbundle.ContainerInfo, error) {
	info, err := store.LoadContainer(identifier)
	if err == nil {
		return info, nil
	}
	familyID, resolveErr := resolvePlacementFamily(placements, identifier)
	if resolveErr != nil {
		return ttcbundle.ContainerInfo{}, err
	}
	// Raft makes the exact immutable identity authoritative before NFS is
	// required to expose the corresponding directory to every client. Common
	// NFS attribute/negative-cache intervals are tens of seconds, so use the
	// same bounded visibility window as shared snapshot artifacts.
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, exactErr := store.LoadRuntimeContainer(familyID)
		if exactErr == nil {
			return info, nil
		}
		if !errors.Is(exactErr, os.ErrNotExist) || time.Now().After(deadline) {
			return ttcbundle.ContainerInfo{}, fmt.Errorf("loading Raft-visible container %q (%s): %w", identifier, familyID, exactErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func validateDaemonPlacementClaimsWithCatalog(dataRoot, localNode string, claims []daemonPlacementClaim, placements cluster.PlacementCatalog) error {
	if len(claims) == 0 {
		return nil
	}
	if placements == nil {
		return errors.New("placement authority is unavailable")
	}
	records, err := placements.List()
	if err != nil {
		return fmt.Errorf("loading placement authority: %w", err)
	}
	byBranch := make(map[string]cluster.PlacementRecord, len(records))
	for _, record := range records {
		byBranch[record.FamilyID+"\x00"+record.BranchID] = record
	}
	for _, claim := range claims {
		record, ok := byBranch[claim.FamilyID+"\x00"+claim.BranchID]
		if !ok {
			return fmt.Errorf("validating placement %s@%s: %w", claim.FamilyID, claim.BranchID, os.ErrNotExist)
		}
		if record.State != cluster.PlacementRunning && record.State != cluster.PlacementPaused && !(claim.AllowMaterializing && record.State == cluster.PlacementMaterializing) {
			return fmt.Errorf("stale routed command: branch %s@%s is %s", claim.FamilyID, claim.BranchID, record.State)
		}
		if record.OwnerNode != localNode || record.OwnerNode != claim.OwnerNode || record.OwnerIncarnation != claim.OwnerIncarnation || record.Generation != claim.Generation {
			return fmt.Errorf("stale routed command: branch %s@%s placement changed from %s/%s generation %d to %s/%s generation %d",
				claim.FamilyID, claim.BranchID, claim.OwnerNode, claim.OwnerIncarnation, claim.Generation,
				record.OwnerNode, record.OwnerIncarnation, record.Generation)
		}
	}
	return nil
}

// reconcilePlacementCatalog enrolls every locally owned live branch in the
// generation-fenced catalog and advances lifecycle generations after a local
// pause/resume. Every placed branch has an explicit owner; unplaced branches
// have none. Records for deleted locally owned branches are reclaimed.
func (b *runtimeBackend) reconcilePlacementCatalog() error {
	if b == nil || b.dataRoot == "" || b.nodeID == "" || b.distributedSource == nil || b.distributedSource.directory == nil {
		return nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	// Reconciliation is a whole-catalog operation. Take one linearizable Raft
	// snapshot and index it once; looking up every branch separately copied and
	// revalidated the complete authority state O(branches) times. Under a wide
	// family, concurrent exec requests could consequently spend minutes in
	// post-command reconciliation despite changing no lifecycle state.
	var authorityState *cluster.AuthorityState
	var current cluster.NodeRecord
	placementByBranch := make(map[string]cluster.PlacementRecord)
	if b.distributedSource.authority != nil {
		authorityState, err = b.distributedSource.authority.AuthorityState()
		if err != nil {
			return err
		}
		var ok bool
		current, ok = authorityState.Nodes[b.nodeID]
		if !ok {
			return fmt.Errorf("cluster node %q is absent from placement authority", b.nodeID)
		}
		for key, record := range authorityState.Placements {
			if branch, ok := authorityState.Branches[key]; ok && branch.Corrupted {
				record.State = cluster.PlacementCorrupted
				record.OwnerNode, record.OwnerIncarnation = "", ""
				record.PhysicalContainer, record.PhysicalBranch = "", ""
				record.OperationID, record.TargetNode, record.ResumeState = "", "", ""
			}
			placementByBranch[key] = record
		}
	} else {
		currentRecord, lookupErr := b.distributedSource.directory.Lookup(b.nodeID)
		err = lookupErr
		if err != nil {
			return err
		}
		current = *currentRecord
		records, err := placements.List()
		if err != nil {
			return err
		}
		for _, record := range records {
			placementByBranch[record.FamilyID+"\x00"+record.BranchID] = record
		}
	}
	var result error
	// Raft placement is the ownership authority. Keep the canonical bundle's
	// denormalized owner in sync so cache reclamation and crash diagnostics do
	// not treat a retired migration source as local work. Transaction-scoped
	// physical wrappers may not have a canonical branch entry; those are
	// intentionally ignored.
	for _, record := range placementByBranch {
		if record.OwnerNode == "" || record.State == cluster.PlacementMigrating || record.State == cluster.PlacementCorrupted {
			continue
		}
		if err := b.syncLogicalBranchOwner(record.FamilyID, record.BranchID, record.OwnerNode); err != nil {
			result = errors.Join(result, fmt.Errorf("synchronizing logical owner for %s@%s: %w", record.FamilyID, record.BranchID, err))
		}
	}
	// Placement reconciliation needs the physical runtime behind every remote
	// logical branch. The user-facing inventory deliberately hides those
	// containers, so using ListContainers here made a newly committed fork look
	// absent and allowed the target node to delete its own Raft placement.
	containers, err := b.containerStore().ListContainersForPlacement()
	if err != nil {
		return err
	}
	// Persistent container metadata is not proof that a running placement still
	// has execution state. In particular, a batch scheduler may SIGKILL every
	// process in a job while leaving both the local runsc state directory and the
	// shared Raft placement intact. Adopting that placement into the daemon's new
	// incarnation would make every node continue routing commands to a dead PID.
	//
	// Query only runsc's sandbox inventory here, rather than each Sentry control
	// server. A lease fence deliberately SIGSTOPs Sentries during recovery, so a
	// control RPC cannot be required before the new incarnation re-authorizes and
	// resumes them. The host PID is the physical execution proof at this boundary.
	liveSandboxes, err := b.liveRuntimeSandboxes()
	if err != nil {
		return fmt.Errorf("listing physical sandboxes for placement recovery: %w", err)
	}
	persistentPhysicalBranches := make(map[string]struct{})
	for _, info := range containers {
		for _, branch := range info.Branches {
			persistentPhysicalBranches[info.ID+"\x00"+branch.ID] = struct{}{}
		}
	}
	if b.distributedSource.authority != nil {
		for key, record := range placementByBranch {
			if record.OwnerNode != b.nodeID || (record.State != cluster.PlacementRunning && record.State != cluster.PlacementPaused) {
				continue
			}
			physicalContainer := record.PhysicalContainer
			if physicalContainer == "" {
				physicalContainer = record.FamilyID
			}
			physicalBranch := record.PhysicalBranch
			if physicalBranch == "" {
				physicalBranch = record.BranchID
			}
			if _, stillRecorded := persistentPhysicalBranches[physicalContainer+"\x00"+physicalBranch]; !stillRecorded {
				// Branch removal deletes physical persistent metadata before the
				// post-command reconciliation removes its Raft placement. The
				// missing runtime is intentional in this narrow, lifecycle-locked
				// window and must not be reported as state loss.
				continue
			}
			if _, alive := liveSandboxes[physicalContainer]; alive {
				continue
			}
			// The init-console follower durably records normal process exit
			// before the Sentry disappears. During startup the node is still
			// Joining and therefore cannot authorize the asynchronous filesystem
			// publication yet. Preserve this placement through the early recovery
			// pass; reconcileObservedInitExits consumes it immediately after the
			// incarnation becomes Ready.
			if paths, pathErr := b.containerStore().BranchConsolePaths(physicalContainer, physicalBranch); pathErr == nil {
				if _, completed, statusErr := readConsoleWaitStatus(paths.WaitStatus); statusErr == nil && completed {
					continue
				}
			}
			// Stop and dehydration deliberately release the physical runtime
			// before their immutable capture has finished publishing. The Raft
			// placement remains live until publication commit atomically installs
			// the durable roots and unplaces the branch. Do not confuse that
			// generation-fenced process-free interval with unexpected runtime
			// loss. Local lifecycle strings are intentionally insufficient proof:
			// after a daemon crash they may be stale or describe an incomplete
			// capture. Only the exact active Raft publication authorizes it.
			if placementHasProcessFreePublication(authorityState, record) {
				continue
			}
			reason := fmt.Sprintf("physical runtime %q on node %q was lost while branch was %s", physicalContainer, b.nodeID, record.State)
			if _, corruptErr := b.distributedSource.authority.MarkBranchCorrupted(record.FamilyID, record.BranchID, reason); corruptErr != nil {
				result = errors.Join(result, fmt.Errorf("fencing lost placement %s@%s: %w", record.FamilyID, record.BranchID, corruptErr))
				continue
			}
			// Raft is authoritative, but keep the physical materialization record
			// consistent so offline cleanup and a later branch removal do not
			// mistake this dead runtime for reusable execution state.
			_ = b.containerStore().UpdateBranchState(physicalContainer, physicalBranch, "corrupted")
			_, _ = b.command("delete", "--force", physicalContainer)
			if currentPlacement, lookupErr := b.distributedSource.authority.LookupPlacement(record.FamilyID, record.BranchID); lookupErr == nil {
				placementByBranch[key] = *currentPlacement
			}
		}
	}
	known := make(map[string]struct{})
	now := time.Now()
	for _, info := range containers {
		logicalFamily := info.ID
		if info.DistributedFamilyID != "" {
			logicalFamily = info.DistributedFamilyID
		}
		for _, branch := range info.Branches {
			placementFamily, placementBranch := logicalFamily, branch.ID
			transactionID := info.DistributedForkTransactionID
			internalPhysical := info.DistributedFamilyID != "" && transactionID != ""
			mappedPhysical := false
			// A migration target may be imported into an existing physical
			// family. In that case the container itself is ordinary and only the
			// transaction-named branch carries the private identity marker.
			// Reconciliation must bind it back to its Raft logical branch, never
			// enroll the transaction ID as a new user branch.
			if branch.DistributedMigration && branch.DistributedForkTransactionID != "" {
				transactionID = branch.DistributedForkTransactionID
				internalPhysical = true
			}
			if internalPhysical {
				// A sparse fork or migration target exists physically before its
				// Raft publication/cutover. It must never enroll its transaction
				// branch as a new logical branch in that window. Once authority is
				// committed, bind the runtime only to the logical identity named by
				// the transaction. This is also deliberately independent of branch
				// names: migration runtimes use transaction IDs while remote-fork
				// runtimes may use a user-visible destination name.
				placementFamily, placementBranch, mappedPhysical = logicalPlacementForPhysicalRuntime(authorityState, info.ID, branch.ID)
				if !mappedPhysical {
					if placementFamily, placementBranch, mappedPhysical = logicalPlacementForUnmappedTransactionDescendant(authorityState, transactionID, info.ID, branch.ID); mappedPhysical {
						// Unlike the imported transaction branch, this descendant is
						// already a completed user mutation and may be enrolled below.
						internalPhysical = false
					} else {
						placementFamily, placementBranch, mappedPhysical = logicalPlacementForPhysicalTransaction(authorityState, transactionID)
						if !mappedPhysical {
							continue
						}
					}
				}
			}
			key := placementFamily + "\x00" + placementBranch
			known[key] = struct{}{}
			if branch.Unplaced {
				if branch.NodeID != "" {
					result = errors.Join(result, fmt.Errorf("branch %s@%s is unplaced but retains node owner %q", info.ID, branch.ID, branch.NodeID))
				}
				continue
			}
			if branch.NodeID == "" {
				result = errors.Join(result, fmt.Errorf("branch %s@%s has no explicit cluster node owner", info.ID, branch.ID))
				continue
			}
			owner := info.BranchNodeID(branch.ID)
			state := cluster.PlacementState(normalizeLifecycleState(branch.State))
			record, found := placementByBranch[key]
			// A never-started branch has no sandbox to discover, but its local
			// writable repository remains authoritative across daemon restarts.
			// Re-fence that process-free owner to the new daemon incarnation.
			if state == "created" && found && record.State == cluster.PlacementMaterializing && record.OwnerNode == b.nodeID {
				if record.OwnerIncarnation != current.Incarnation {
					if _, err := placements.AdoptOwnerIncarnation(placementFamily, placementBranch, b.nodeID, record.OwnerIncarnation, current.Incarnation, record.Generation, now); err != nil {
						result = errors.Join(result, fmt.Errorf("adopting prepared placement for %s@%s: %w", info.ID, branch.ID, err))
					}
				}
				continue
			}
			if owner != b.nodeID || (state != cluster.PlacementRunning && state != cluster.PlacementPaused) {
				continue
			}
			// A persistent branch record is not execution authority. Migration
			// deliberately leaves the canonical family metadata behind after its
			// source Sentry is reclaimed, and an abnormal host exit leaves the same
			// shape. Only a live physical sandbox may create or adopt a running or
			// paused placement. Reservations cover the pre-Sentry creation window.
			if _, alive := liveSandboxes[info.ID]; !alive {
				continue
			}
			if !found {
				if internalPhysical {
					// The transaction is known but publication has not installed its
					// placement yet. Only the Raft publish/cutover operation may do so.
					continue
				}
				// A missing placement may be enrolled only while this node accepts
				// new ownership. Pressured/draining nodes frequently retain a
				// physical source briefly after migration or removal has deleted its
				// logical placement; recreating that placement would resurrect the
				// branch and is rejected by Raft in any event. During restart
				// recovery, existing placements are adopted while joining and truly
				// untracked local branches are enrolled after the node becomes ready.
				if current.State != cluster.NodeReady {
					continue
				}
				_, createErr := placements.Create(cluster.PlacementRecord{
					FamilyID: placementFamily, BranchID: placementBranch, PhysicalContainer: info.ID, PhysicalBranch: branch.ID,
					OwnerNode: b.nodeID, OwnerIncarnation: current.Incarnation, State: state,
					FamilyName: info.Name, ImageReference: info.Reference, ImageDigest: info.Digest, ImageSize: info.ImageSize,
					LiveForkEligible: info.LiveForkEligible, LiveForkWaiver: info.LiveForkWaiver,
					FamilyCreatedAt: info.CreatedAt, ParentBranch: branch.ParentID, BranchCreatedAt: branch.CreatedAt,
					Quarantined: branch.Quarantined,
				}, now)
				if createErr != nil && !errors.Is(createErr, os.ErrExist) {
					result = errors.Join(result, fmt.Errorf("creating placement for %s@%s: %w", info.ID, branch.ID, createErr))
				} else if createErr == nil && b.distributedSource.authority != nil {
					if _, inheritErr := b.distributedSource.authority.InheritBranchDependencies(placementFamily, placementBranch, branch.ParentID); inheritErr != nil {
						result = errors.Join(result, fmt.Errorf("recording dependencies for %s@%s: %w", placementFamily, placementBranch, inheritErr))
					}
				}
				continue
			}
			if record.State == cluster.PlacementMigrating || record.OwnerNode != b.nodeID {
				continue
			}
			if record.OwnerIncarnation != current.Incarnation {
				adopted, adoptErr := placements.AdoptOwnerIncarnation(placementFamily, placementBranch, b.nodeID, record.OwnerIncarnation, current.Incarnation, record.Generation, now)
				err = adoptErr
				if err != nil {
					result = errors.Join(result, fmt.Errorf("adopting placement for %s@%s: %w", info.ID, branch.ID, err))
					continue
				}
				record = *adopted
			}
			branchAuthority, authorityKnown := cluster.BranchAuthority{}, false
			if authorityState != nil {
				branchAuthority, authorityKnown = authorityState.Branches[key]
			}
			// Publication changes the branch authority lifecycle before its
			// irreversible commit, while the physical placement intentionally
			// remains on the current node. Canceling that publication and starting
			// or rehydrating the branch can therefore leave the placement already
			// in the desired stable state but the branch authority at the terminal
			// publication state. Re-propose the idempotent stable placement update
			// when either view differs; Raft then repairs both atomically.
			if record.State != state || (authorityKnown && branchAuthority.Lifecycle != string(state)) {
				if _, err := placements.UpdateStableState(placementFamily, placementBranch, b.nodeID, current.Incarnation, record.Generation, state, now); err != nil {
					result = errors.Join(result, fmt.Errorf("updating placement state for %s@%s: %w", info.ID, branch.ID, err))
				}
			}
			if b.distributedSource.authority != nil && (!authorityKnown || !branchAuthority.DependenciesInherited) {
				if _, inheritErr := b.distributedSource.authority.InheritBranchDependencies(placementFamily, placementBranch, branch.ParentID); inheritErr != nil {
					result = errors.Join(result, fmt.Errorf("recording dependencies for %s@%s: %w", placementFamily, placementBranch, inheritErr))
				}
			}
		}
	}
	for _, record := range placementByBranch {
		if _, ok := known[record.FamilyID+"\x00"+record.BranchID]; ok || record.State == cluster.PlacementMigrating {
			continue
		}
		// An unplaced or corrupted branch is authoritative Raft state with no
		// physical owner. Its derived shared container record may legitimately
		// lag an atomic publication commit. Reconciliation may clean up only
		// execution state owned by this node; deleting ownerless state here would
		// turn an ordinary dehydrate/restart race into permanent branch loss.
		if record.OwnerNode == "" || record.OwnerNode != b.nodeID {
			continue
		}
		if err := placements.Remove(record.FamilyID, record.BranchID, record.Generation); err != nil {
			result = errors.Join(result, fmt.Errorf("removing stale placement %s@%s: %w", record.FamilyID, record.BranchID, err))
			continue
		}
		if err := b.retireTransactionsForRemovedPlacement(record); err != nil {
			result = errors.Join(result, fmt.Errorf("retiring transactions for removed placement %s@%s: %w", record.FamilyID, record.BranchID, err))
		}
	}
	return result
}

// newPlacementReservation is the Raft ownership record installed before a
// newly prepared container may execute guest code. Placement reconciliation is
// deliberately a recovery mechanism, not an admission protocol: creating the
// record after runsc start leaves a window in which a live runtime is invisible
// to routing and may fail enrollment if the node becomes pressured meanwhile.
type newPlacementReservation struct {
	record *cluster.PlacementRecord
}

// registerCreatedContainerPlacement publishes the logical identity and local
// storage owner of a cluster-created, never-started branch. The writable
// repository is node-local authoritative state, so advertising this branch as
// unplaced would let start run on a node that does not have its filesystem.
func (b *runtimeBackend) registerCreatedContainerPlacement(info ttcbundle.ContainerInfo) error {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	owner, err := b.renewDistributedExecutionAuthority()
	if err != nil {
		return fmt.Errorf("renewing created-container placement authority: %w", err)
	}
	branch, err := persistentBranchInfo(info, defaultBranchID)
	if err != nil {
		return err
	}
	_, err = placements.Create(cluster.PlacementRecord{
		FamilyID: info.ID, BranchID: branch.ID, PhysicalContainer: info.ID, PhysicalBranch: branch.ID,
		OwnerNode: b.nodeID, OwnerIncarnation: owner.Incarnation, State: cluster.PlacementMaterializing,
		FamilyName: info.Name, ImageReference: info.Reference, ImageDigest: info.Digest, ImageSize: info.ImageSize,
		LiveForkEligible: info.LiveForkEligible, LiveForkWaiver: info.LiveForkWaiver,
		FamilyCreatedAt: info.CreatedAt, ParentBranch: branch.ParentID, BranchCreatedAt: branch.CreatedAt,
		Quarantined: branch.Quarantined,
	}, time.Now())
	if err != nil {
		return fmt.Errorf("registering created placement for %s@%s: %w", info.Name, branch.ID, err)
	}
	return nil
}

// reserveNewContainerPlacement fences a newly prepared main branch before its
// Sentry exists. PlacementMaterializing grants this node exclusive construction
// authority but is not executable/routable as a normal branch. Standalone TTC
// has no placement authority and returns a no-op reservation.
func (b *runtimeBackend) reserveNewContainerPlacement(info ttcbundle.ContainerInfo) (newPlacementReservation, error) {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil || b.nodeID == "" {
		return newPlacementReservation{}, nil
	}
	owner, err := b.renewDistributedExecutionAuthority()
	if err != nil {
		return newPlacementReservation{}, fmt.Errorf("renewing new-container placement authority: %w", err)
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return newPlacementReservation{}, err
	}
	branch, err := persistentBranchInfo(info, defaultBranchID)
	if err != nil {
		return newPlacementReservation{}, err
	}
	record, err := placements.Create(cluster.PlacementRecord{
		FamilyID: info.ID, BranchID: branch.ID, PhysicalContainer: info.ID, PhysicalBranch: branch.ID,
		OwnerNode: b.nodeID, OwnerIncarnation: owner.Incarnation, State: cluster.PlacementMaterializing,
		FamilyName: info.Name, ImageReference: info.Reference, ImageDigest: info.Digest, ImageSize: info.ImageSize,
		LiveForkEligible: info.LiveForkEligible, LiveForkWaiver: info.LiveForkWaiver,
		FamilyCreatedAt: info.CreatedAt, ParentBranch: branch.ParentID, BranchCreatedAt: branch.CreatedAt,
		Quarantined: branch.Quarantined,
	}, time.Now())
	if err != nil {
		return newPlacementReservation{}, fmt.Errorf("reserving placement for %s@%s: %w", info.Name, branch.ID, err)
	}
	return newPlacementReservation{record: record}, nil
}

// activateNewContainerPlacement makes a successfully started reservation
// normally routable. A node is allowed to become pressured after accepting the
// reservation; stable-state completion consequently requires execution
// authority, but not continued admission of unrelated new work.
func (b *runtimeBackend) activateNewContainerPlacement(reservation newPlacementReservation) error {
	if reservation.record == nil {
		return nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	record := reservation.record
	_, err = placements.UpdateStableState(record.FamilyID, record.BranchID, record.OwnerNode, record.OwnerIncarnation, record.Generation, cluster.PlacementRunning, time.Now())
	if err != nil {
		return fmt.Errorf("activating placement for %s@%s: %w", record.FamilyID, record.BranchID, err)
	}
	return nil
}

// cancelNewContainerPlacement releases an unactivated construction record.
// The caller separately removes the prepared local container tree.
func (b *runtimeBackend) cancelNewContainerPlacement(reservation newPlacementReservation) error {
	if reservation.record == nil {
		return nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	record := reservation.record
	if err := placements.Remove(record.FamilyID, record.BranchID, record.Generation); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("canceling placement for %s@%s: %w", record.FamilyID, record.BranchID, err)
	}
	return nil
}

// ensureForkPlacementDestinationsAvailable checks the cluster-wide logical
// namespace before runsc mutates the source Sentry. A physical runtime only
// contains its local branches, so checking ContainerInfo alone misses a branch
// of the same family placed on another node. Detecting that conflict after the
// fork is both slower and leaves rollback responsible for reclaiming live CoW
// state.
func (b *runtimeBackend) ensureForkPlacementDestinationsAvailable(info ttcbundle.ContainerInfo, destinations []string) error {
	if b == nil || b.distributedSource == nil || len(destinations) == 0 {
		return nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	family := logicalFamilyID(info)
	for _, destination := range destinations {
		if _, err := placements.Lookup(family, destination); err == nil {
			return fmt.Errorf("branch %q already exists", destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checking placement for %s@%s: %w", family, destination, err)
		}
	}
	return nil
}

// registerLocalForkPlacements publishes branches created inside an existing
// clustered Sentry. Placement reconciliation remains a crash-recovery tool;
// successful fork must not depend on a later inventory scan to make its new
// logical branches routable. This is especially important for a locally
// forked descendant of a remote import, whose physical container is private
// transaction state and is intentionally hidden from ordinary discovery.
func (b *runtimeBackend) registerLocalForkPlacements(info ttcbundle.ContainerInfo, branches []ttcbundle.BranchInfo) error {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil || b.nodeID == "" || len(branches) == 0 {
		return nil
	}
	owner, err := b.renewDistributedExecutionAuthority()
	if err != nil {
		return fmt.Errorf("renewing fork placement authority: %w", err)
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	logicalFamily := logicalFamilyID(info)
	display := info
	if logicalFamily != info.ID {
		if canonical, loadErr := b.containerStore().LoadContainer(logicalFamily); loadErr == nil {
			display = canonical
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return fmt.Errorf("loading logical fork family %q: %w", logicalFamily, loadErr)
		}
	}
	created := make([]cluster.PlacementRecord, 0, len(branches))
	rollback := func(cause error) error {
		var rollbackErr error
		for i := len(created) - 1; i >= 0; i-- {
			record := created[i]
			if err := placements.Remove(record.FamilyID, record.BranchID, record.Generation); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("removing fork placement %s@%s: %w", record.FamilyID, record.BranchID, err))
			}
		}
		return errors.Join(cause, rollbackErr)
	}
	now := time.Now()
	for i := range branches {
		branch := branches[i]
		state := cluster.PlacementState(normalizeLifecycleState(branch.State))
		if state != cluster.PlacementRunning && state != cluster.PlacementPaused {
			return rollback(fmt.Errorf("forked branch %q has non-routable state %q", branch.ID, branch.State))
		}
		record, err := placements.Create(cluster.PlacementRecord{
			FamilyID: logicalFamily, BranchID: branch.ID, PhysicalContainer: info.ID, PhysicalBranch: branch.ID,
			OwnerNode: b.nodeID, OwnerIncarnation: owner.Incarnation, State: state,
			FamilyName: display.Name, ImageReference: display.Reference, ImageDigest: display.Digest, ImageSize: display.ImageSize,
			LiveForkEligible: display.LiveForkEligible, LiveForkWaiver: display.LiveForkWaiver,
			FamilyCreatedAt: display.CreatedAt, ParentBranch: branch.ParentID, BranchCreatedAt: branch.CreatedAt,
			Quarantined: branch.Quarantined,
		}, now)
		if err != nil {
			return rollback(fmt.Errorf("registering fork placement for %s@%s: %w", logicalFamily, branch.ID, err))
		}
		created = append(created, *record)
		if _, err := b.distributedSource.authority.InheritBranchDependencies(logicalFamily, branch.ID, branch.ParentID); err != nil {
			return rollback(fmt.Errorf("recording fork dependencies for %s@%s: %w", logicalFamily, branch.ID, err))
		}
	}
	return nil
}

// logicalPlacementForPhysicalTransaction resolves transaction-scoped runtime
// identity to the stable logical branch whose authority the transaction moves
// or creates. Returning false before Raft knows the transaction is intentional:
// target preparation must remain invisible until its atomic publication.
func logicalPlacementForPhysicalTransaction(state *cluster.AuthorityState, transactionID string) (string, string, bool) {
	if state == nil || transactionID == "" {
		return "", "", false
	}
	tx, ok := state.Transactions[transactionID]
	if !ok {
		return "", "", false
	}
	if tx.Identity.Migration {
		return tx.Identity.SourceContainer, tx.Identity.SourceBranch, true
	}
	return tx.Identity.TargetContainer, tx.Identity.VisibleTargetBranch(), true
}

// logicalPlacementForUnmappedTransactionDescendant recognizes a user-created
// branch in a transaction-private target runtime during crash recovery. The
// original imported branch is named by TargetPhysical* and remains private
// until its transaction publishes it. Any other persistent physical branch in
// that exact runtime can only have been created by a completed local Sentry
// fork and keeps its user-visible physical name.
//
// This helper deliberately requires Raft to have recorded the original target
// runtime. Before that fence, an arbitrary prepared branch must remain hidden:
// only the transaction publication operation may expose it.
func logicalPlacementForUnmappedTransactionDescendant(state *cluster.AuthorityState, transactionID, physicalContainer, physicalBranch string) (string, string, bool) {
	if state == nil || transactionID == "" || physicalContainer == "" || physicalBranch == "" {
		return "", "", false
	}
	tx, ok := state.Transactions[transactionID]
	if !ok {
		return "", "", false
	}
	targetContainer, targetBranch := tx.TargetPhysicalContainer, tx.TargetPhysicalBranch
	if targetContainer == "" || targetBranch == "" {
		family, branch, found := logicalPlacementForPhysicalTransaction(state, transactionID)
		if !found {
			return "", "", false
		}
		placement, found := state.Placements[family+"\x00"+branch]
		if !found {
			return "", "", false
		}
		targetContainer, targetBranch = placement.PhysicalContainer, placement.PhysicalBranch
	}
	if targetContainer != physicalContainer || targetBranch == "" || targetBranch == physicalBranch {
		return "", "", false
	}
	family := tx.Identity.TargetContainer
	if tx.Identity.Migration {
		family = tx.Identity.SourceContainer
	}
	if family == "" {
		return "", "", false
	}
	return family, physicalBranch, true
}

// logicalPlacementForPhysicalRuntime maps a node-local runtime branch back to
// its single stable Raft identity. Physical transaction IDs must never be used
// as branch keys by lifecycle protocols such as durable publication.
func logicalPlacementForPhysicalRuntime(state *cluster.AuthorityState, physicalContainer, physicalBranch string) (string, string, bool) {
	if state == nil || physicalContainer == "" || physicalBranch == "" {
		return "", "", false
	}
	family, branch := "", ""
	for _, placement := range state.Placements {
		if placement.PhysicalContainer != physicalContainer || placement.PhysicalBranch != physicalBranch {
			continue
		}
		if family != "" && (family != placement.FamilyID || branch != placement.BranchID) {
			return "", "", false
		}
		family, branch = placement.FamilyID, placement.BranchID
	}
	return family, branch, family != ""
}

func logicalFamilyForPhysicalRuntime(state *cluster.AuthorityState, physicalContainer string, physicalBranches []string) (Family, bool) {
	logical := Family{}
	if len(physicalBranches) == 0 {
		return logical, false
	}
	seen := make(map[string]struct{}, len(physicalBranches))
	for _, physicalBranch := range physicalBranches {
		family, branch, found := logicalPlacementForPhysicalRuntime(state, physicalContainer, physicalBranch)
		if !found || (logical.ID != "" && logical.ID != family) {
			return Family{}, false
		}
		logical.ID = family
		if _, duplicate := seen[branch]; duplicate {
			continue
		}
		seen[branch] = struct{}{}
		logical.Branches = append(logical.Branches, Branch{ID: branch})
	}
	return logical, logical.ID != "" && len(logical.Branches) != 0
}

// placementHasProcessFreePublication reports whether Raft has authorized the
// exact placement generation to be temporarily process-free while its sealed
// state is published. Publication commit atomically changes the placement to
// unplaced, so committed or terminal publications never need this exception.
func placementHasProcessFreePublication(state *cluster.AuthorityState, placement cluster.PlacementRecord) bool {
	branch, ok := state.Branches[placement.FamilyID+"\x00"+placement.BranchID]
	if !ok || branch.Corrupted || branch.Generation != placement.Generation || branch.PublicationID == "" {
		return false
	}
	if branch.Lifecycle != "dehydrating" && branch.Lifecycle != "exited" {
		return false
	}
	for _, publication := range state.Publications {
		if publication.FamilyID != placement.FamilyID || publication.BranchID != placement.BranchID ||
			publication.BranchGeneration != placement.Generation || publication.PublicationID != branch.PublicationID {
			continue
		}
		return publication.State == cluster.PublicationPreparing || publication.State == cluster.PublicationReady
	}
	return false
}

// liveRuntimeSandboxes returns physical runsc container IDs whose Sentry host
// process still exists. It intentionally does not contact the Sentry: a
// correctly lease-fenced Sentry may be SIGSTOPped until placement recovery is
// complete, but its PID remains valid execution state.
func (b *runtimeBackend) liveRuntimeSandboxes() (map[string]struct{}, error) {
	output, err := b.command("list", "--format=json", "--sandbox")
	if err != nil {
		return nil, err
	}
	var states []struct {
		ID  string `json:"id"`
		PID int    `json:"pid"`
	}
	if err := json.Unmarshal(output, &states); err != nil {
		return nil, fmt.Errorf("decoding runsc sandbox list: %w", err)
	}
	processes, err := sentryProcessesForRuntimeRoot(b.root)
	if err != nil {
		return nil, fmt.Errorf("discovering Sentry processes: %w", err)
	}
	physicalPIDs := make(map[string]int, len(processes))
	for _, process := range processes {
		if process.ContainerID != "" {
			physicalPIDs[process.ContainerID] = process.PID
		}
	}
	result := make(map[string]struct{}, len(states))
	for _, state := range states {
		// Match both the state-directory PID and the live ttc-sentry command
		// line. kill(pid, 0) alone is insufficient because Linux may have
		// reused a dead Sentry's PID for an unrelated process.
		if state.ID != "" && state.PID > 0 && physicalPIDs[state.ID] == state.PID {
			result[state.ID] = struct{}{}
		}
	}
	return result, nil
}

// retireTransactionsForRemovedPlacement bounds Raft history after the target
// branch has disappeared. The replicated state machine repeats all safety
// checks, so a concurrent dependency or placement change fails closed and is
// retried by a later reconciliation pass.
func (b *runtimeBackend) retireTransactionsForRemovedPlacement(removed cluster.PlacementRecord) error {
	transactions, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	records, err := transactions.List()
	if err != nil {
		return err
	}
	var result error
	for i := range records {
		tx := &records[i]
		if !tx.Terminal() || !transactionTargetsRemovedPlacement(*tx, removed) {
			continue
		}
		// An independent migration may still have a paused physical source and
		// transfer roots on the old owner. Keep its terminal Raft record as the
		// durable cleanup intent until the family-mutation coordinator discards
		// that source. Deleting this record here made a crash permanently orphan
		// the old memory graph.
		if tx.Identity.Migration {
			continue
		}
		if err := transactions.Delete(tx.Identity.TransactionID, tx.Revision); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func transactionTargetsRemovedPlacement(tx cluster.Transaction, removed cluster.PlacementRecord) bool {
	if tx.Identity.Migration {
		return removed.FamilyID == tx.Identity.SourceContainer && removed.BranchID == tx.Identity.SourceBranch
	}
	if removed.BranchID != tx.Identity.VisibleTargetBranch() {
		return false
	}
	return removed.FamilyID == tx.Identity.TargetContainer ||
		removed.PhysicalContainer == tx.Identity.TargetContainer ||
		removed.FamilyID == tx.Identity.SourceContainer ||
		removed.PhysicalContainer == tx.Identity.SourceContainer
}

func daemonCommandNeedsPlacementAdmission(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "run", "create", "start", "rehydrate", "restart", "restore":
		return true
	case "container":
		return len(args) > 1 && (args[1] == "run" || args[1] == "create")
	default:
		return false
	}
}

// daemonCommandMayAwaitColdUnplacement reports whether a placement mismatch
// can be the normal publication race between a completed local capture and
// its Raft commit. Running branches must still reject a conflicting --node
// immediately; only process-free lifecycle operations may wait for ownership
// to become unplaced.
func daemonCommandMayAwaitColdUnplacement(dataRoot string, placements cluster.PlacementCatalog, args []string) bool {
	if dataRoot == "" || len(args) < 2 || (args[0] != "rehydrate" && args[0] != "start") {
		return false
	}
	store := ttcbundle.Store{Root: dataRoot}
	authority, authoritative := placements.(interface {
		Branch(family, branch string) (*cluster.BranchAuthority, error)
	})
	found := false
	for _, operand := range args[1:] {
		target, err := parseSelector(operand)
		if err != nil {
			continue
		}
		info, loadErr := store.LoadContainer(target.container)
		familyID := ""
		if loadErr == nil {
			familyID = logicalFamilyID(info)
		} else {
			familyID, err = resolvePlacementFamily(placements, target.container)
			if err != nil {
				return false
			}
		}
		if authoritative {
			branch, err := authority.Branch(familyID, target.branch)
			if err != nil || branch.Corrupted {
				return false
			}
			// An exited branch produced by stopping a durable dehydrated
			// snapshot has no active publication transaction and no memory
			// root. Its immutable filesystem root is sufficient to admit a
			// fresh start while an older placement release finishes.
			if branch.PublicationID == "" && (branch.Lifecycle != "exited" || branch.Roots.FilesystemRoot == "" || branch.Roots.MemoryRoot != "" || branch.Roots.MemorySnapshot != 0) {
				return false
			}
			switch normalizeLifecycleState(branch.Lifecycle) {
			case "dehydrating", "exited":
			default:
				return false
			}
		} else {
			// Non-Raft catalogs are used by standalone compatibility tests. In
			// production, never let eventually updated filesystem metadata
			// override the linearizable branch authority above.
			if loadErr != nil {
				return false
			}
			branch, err := persistentBranchInfo(info, target.branch)
			if err != nil {
				return false
			}
			switch normalizeLifecycleState(branch.State) {
			case "dehydrating", "dehydrated", "exited", "materializing":
			default:
				return false
			}
		}
		found = true
	}
	return found
}

// waitForCommittedColdBranchMetadata closes the deliberately small gap between
// the authoritative Raft publication commit and its derived container.json
// update. CommitPublication first makes the immutable roots authoritative and
// unplaces the branch in one Raft entry; the publishing node then records the
// shared manifest in container.json. A different node must not claim the
// branch in between those steps, because materialization reads the derived
// manifest path.
//
// Waiting is preferable to reconstructing container.json on the claimant: it
// also orders the old publisher's queue completion before rehydration clears
// the publication identity for the new mutable runtime.
func waitForCommittedColdBranchMetadata(ctx context.Context, store ttcbundle.Store, containerID, branchID string, authority cluster.BranchAuthority) (ttcbundle.ContainerInfo, ttcbundle.BranchInfo, error) {
	if authority.Roots.FilesystemRoot == "" {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, errors.New("unplaced branch has no committed publication roots")
	}
	expectedManifest := filepath.Join(store.Root, filepath.FromSlash(authority.Roots.FilesystemRoot))
	if err := validateColdManifestPath(store.Root, expectedManifest); err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, err
	}
	manifest, err := readColdBranchManifest(expectedManifest)
	if err != nil {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("reading committed cold manifest for %s@%s: %w", containerID, branchID, err)
	}
	if authority.PublicationID != "" && manifest.PublicationID != authority.PublicationID {
		return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("committed cold manifest publication is %q, authority has %q", manifest.PublicationID, authority.PublicationID)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	lastPublicationID := ""
	for {
		// The manifest is the authoritative filesystem root and contains the
		// exact physical record that published it. This remains resolvable after
		// stop discards the memory root and terminal publication transaction,
		// and avoids selecting a stale same-named record after migrations.
		info, err := store.LoadRuntimeContainer(manifest.Container)
		if err != nil {
			return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, err
		}
		branch, err := persistentBranchInfo(info, manifest.Branch)
		if err != nil {
			return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, err
		}
		lastPublicationID = branch.PublicationID
		// Raft publication commit is irreversible and precedes this derived
		// metadata update. An empty or older ID therefore means the publisher
		// has not completed its shared-catalog write yet, not that the committed
		// roots are invalid. Poll until the exact authoritative identity appears.
		if branch.PublicationID == manifest.PublicationID && branch.StorageState == "shared" && branch.StorageManifest != "" {
			if filepath.Clean(branch.StorageManifest) != filepath.Clean(expectedManifest) {
				return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("branch %s shared manifest is %q, committed authority has %q", manifest.Branch, branch.StorageManifest, expectedManifest)
			}
			if authority.Roots.MemorySnapshot != 0 && branch.SnapshotID != authority.Roots.MemorySnapshot {
				return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("branch %s memory snapshot is %d, committed authority has %d", manifest.Branch, branch.SnapshotID, authority.Roots.MemorySnapshot)
			}
			if _, err := os.Stat(expectedManifest); err != nil {
				return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("opening committed cold manifest: %w", err)
			}
			return info, branch, nil
		}
		select {
		case <-ctx.Done():
			return ttcbundle.ContainerInfo{}, ttcbundle.BranchInfo{}, fmt.Errorf("waiting for committed cold branch metadata (last publication %q, want %q): %w", lastPublicationID, manifest.PublicationID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// claimUnplacedLifecycleTargets turns admission of a durable cold branch into
// a generation-fenced placement before its runtime is reconstructed. Without
// this claim, two entry daemons may independently start or rehydrate the same
// unplaced branch, and a successful operation may remain routed to its former
// owner.
//
// A claim intentionally remains owned by this node if rehydration itself
// fails. Retrying is then routed back to the node that may hold local staging
// state, while node drain can still move or republish the cold branch.
func (s *daemonServer) claimUnplacedLifecycleTargets(backend *runtimeBackend, args []string) error {
	if s == nil || backend == nil || s.clusterDirectory == nil || s.clusterCapabilities == nil || len(args) < 2 || (args[0] != "rehydrate" && args[0] != "start" && args[0] != "restart") {
		return nil
	}
	// Raft leadership changes invalidate execution grants from the preceding
	// term. Renew synchronously at the admission boundary instead of depending
	// on the periodic heartbeat winning the race with this user request.
	node, err := backend.renewDistributedExecutionAuthority()
	if err != nil {
		return fmt.Errorf("renewing cold-branch placement authority: %w", err)
	}
	if !node.AcceptsPlacement(time.Now()) {
		return fmt.Errorf("cluster node %q is not accepting cold-branch placement (state=%s)", node.NodeID, node.State)
	}
	placements, err := backend.placementCatalog()
	if err != nil {
		return err
	}
	store := backend.containerStore()
	for _, operand := range args[1:] {
		target, err := parseSelector(operand)
		if err != nil {
			return err
		}
		resolved, info, err := backend.resolveSelector(target)
		if err != nil {
			return err
		}
		branch, err := persistentBranchInfo(info, resolved.branch)
		if err != nil {
			return err
		}
		familyID := logicalFamilyID(info)
		record, err := placements.Lookup(familyID, target.branch)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("loading placement for %s: current-format clustered branch has no Raft placement record", target)
		}
		if err != nil {
			return fmt.Errorf("loading placement for %s: %w", target, err)
		}
		// Restarting a dehydrated branch means discarding its serialized process
		// state and starting a fresh init while retaining its filesystem. Do this
		// while the branch is still unplaced, then claim the resulting exited
		// snapshot below. Claiming first would make the memory-discard transition
		// race a materializing owner and leave unused memory roots reachable.
		if args[0] == "restart" && record.State == cluster.PlacementUnplaced {
			if authority, ok := placements.(interface {
				Branch(family, branch string) (*cluster.BranchAuthority, error)
			}); ok {
				committed, authorityErr := authority.Branch(familyID, target.branch)
				if authorityErr != nil {
					return fmt.Errorf("loading restart authority for %s: %w", target, authorityErr)
				}
				if normalizeLifecycleState(committed.Lifecycle) == "dehydrated" {
					if err := backend.discard(target); err != nil {
						return fmt.Errorf("discarding dehydrated process state before restarting %s: %w", target, err)
					}
					handled, err := backend.finalizeColdStopAuthority(target, "dehydrated")
					if err != nil {
						return err
					}
					if !handled {
						return fmt.Errorf("dehydrated restart for %s did not finalize cold authority", target)
					}
					record, err = placements.Lookup(familyID, target.branch)
					if err != nil {
						return fmt.Errorf("reloading exited placement for %s: %w", target, err)
					}
				}
			}
		}
		if record.State != cluster.PlacementUnplaced {
			if record.OwnerNode != node.NodeID || record.OwnerIncarnation != node.Incarnation {
				return fmt.Errorf("cold-branch placement for %s belongs to %s/%s", target, record.OwnerNode, record.OwnerIncarnation)
			}
			if branch.Unplaced {
				if err := store.CompareAndSwapBranchNodeID(info.ID, resolved.branch, "", node.NodeID); err != nil {
					return fmt.Errorf("repairing claimed cold-branch owner for %s: %w", target, err)
				}
			}
			continue
		}
		// Raft is authoritative and intentionally unplaces a publication before
		// the publisher rewrites derived shared metadata. Do not let a claimant
		// clear the publication identity while that queue completion is still in
		// flight. Local test catalogs have no BranchAuthority and therefore keep
		// the legacy direct-claim behavior.
		if authority, ok := placements.(interface {
			Branch(family, branch string) (*cluster.BranchAuthority, error)
		}); ok {
			committed, authorityErr := authority.Branch(familyID, target.branch)
			if authorityErr != nil {
				return fmt.Errorf("loading cold-branch authority for %s: %w", target, authorityErr)
			}
			// A branch created by `ttc create` has durable bundle/filesystem
			// state but has never produced a cold publication. Claim its existing
			// shared record directly. Every other unplaced lifecycle must pass the
			// immutable publication fence before it can execute elsewhere.
			if normalizeLifecycleState(branch.State) != "created" {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				persistent, persistentBranch, waitErr := waitForCommittedColdBranchMetadata(ctx, store, info.ID, resolved.branch, *committed)
				cancel()
				if waitErr != nil {
					return fmt.Errorf("waiting to claim cold branch %s: %w", target, waitErr)
				}
				info, branch = persistent, persistentBranch
				resolved = selector{container: persistent.ID, branch: persistentBranch.ID}
			}
		}
		claimed, err := placements.ClaimUnplaced(familyID, target.branch, node.NodeID, node.Incarnation, info.ID, resolved.branch, record.Generation, cluster.PlacementMaterializing, time.Now())
		if err != nil {
			return fmt.Errorf("claiming cold-branch placement for %s: %w", target, err)
		}
		// Raft publication atomically unplaces the branch before its derived
		// shared catalog is rewritten. Read and replace that exact stale value;
		// requiring an already-empty NodeID introduces a window in which the
		// authoritative claim succeeds but materialization cannot start.
		persistent, loadErr := store.LoadRuntimeContainer(info.ID)
		if loadErr != nil {
			_, _ = placements.Unplace(familyID, target.branch, node.NodeID, node.Incarnation, claimed.Generation, time.Now())
			return fmt.Errorf("loading cold-branch metadata for %s: %w", target, loadErr)
		}
		expectedOwner := persistent.BranchNodeID(resolved.branch)
		if err := store.CompareAndSwapBranchNodeID(info.ID, resolved.branch, expectedOwner, node.NodeID); err != nil {
			_, releaseErr := placements.Unplace(familyID, target.branch, node.NodeID, node.Incarnation, claimed.Generation, time.Now())
			if releaseErr != nil {
				return fmt.Errorf("recording cold-branch owner for %s: %w; releasing placement also failed: %v", target, err, releaseErr)
			}
			return fmt.Errorf("recording cold-branch owner for %s: %w", target, err)
		}
	}
	return nil
}

func executeRemoteDaemonCommand(ctx context.Context, peer *cluster.Client, requestID string, args []string) (int, string, string, error) {
	request := daemonRequest{Version: daemonAPIVersion, Action: "command", RequestID: requestID, Args: append([]string(nil), args...)}
	return executeRemoteDaemonRequest(ctx, peer, request)
}

func executeRemoteDaemonRequest(ctx context.Context, peer *cluster.Client, request daemonRequest) (int, string, string, error) {
	encoded, err := json.Marshal(&request)
	if err != nil {
		return 0, "", "", err
	}
	var response bytes.Buffer
	if err := peer.ProxyDaemon(ctx, bytes.NewReader(append(encoded, '\n')), &response); err != nil {
		return 0, "", "", err
	}
	var stdout strings.Builder
	var diagnostic strings.Builder
	status := -1
	for response.Len() != 0 {
		typ, payload, err := readDaemonFrame(&response)
		if err != nil {
			return 0, stdout.String(), diagnostic.String(), err
		}
		switch typ {
		case daemonFrameStdout:
			stdout.Write(payload)
		case daemonFrameStderr:
			diagnostic.Write(payload)
		case daemonFrameStatus:
			if len(payload) != 4 {
				return 0, stdout.String(), diagnostic.String(), errors.New("remote daemon returned an invalid status frame")
			}
			status = int(binary.BigEndian.Uint32(payload))
		}
	}
	if status < 0 {
		return 0, stdout.String(), diagnostic.String(), errors.New("remote daemon returned no status")
	}
	return status, stdout.String(), diagnostic.String(), nil
}

type distributedFamilyMutation struct {
	prefix        []string
	targets       []string
	removeVolumes bool
}

// branchPrefix returns the command prefix for one physical branch. A
// family-level --volumes flag is deliberately consumed by the final metadata
// cleanup: runRemove rejects it for branch selectors, and anonymous volumes
// must not disappear while another placement in the family is still live.
func (m distributedFamilyMutation) branchPrefix() []string {
	if len(m.prefix) == 0 || m.prefix[0] != "rm" {
		return append([]string(nil), m.prefix...)
	}
	prefix := []string{"rm"}
	for _, arg := range m.prefix[1:] {
		switch arg {
		case "-f", "--force", "-fv", "-vf":
			if len(prefix) == 1 {
				prefix = append(prefix, "-f")
			}
		}
	}
	return prefix
}

func distributedMutationSourceTransactions(transactions []cluster.Transaction, info ttcbundle.ContainerInfo, branchID string) map[string]map[string]struct{} {
	settleByNode := make(map[string]map[string]struct{})
	for _, tx := range transactions {
		if !transactionNeedsSourceSettlement(tx) {
			continue
		}
		if tx.Identity.SourceContainer != info.ID && tx.Identity.TargetContainer != info.ID {
			continue
		}
		if branchID != "" && tx.Identity.SourceBranch != branchID && tx.Identity.VisibleTargetBranch() != branchID {
			continue
		}
		if settleByNode[tx.Identity.SourceNodeID] == nil {
			settleByNode[tx.Identity.SourceNodeID] = make(map[string]struct{})
		}
		settleByNode[tx.Identity.SourceNodeID][tx.Identity.TransactionID] = struct{}{}
	}
	return settleByNode
}

// distributedRemovalSourceTransactions separates dependencies that must be
// materialized before deletion, remote-fork targets that can be canceled, and
// retired migration providers that should be discarded after their logical
// target has gone away. A fork whose target is also being removed has no
// surviving reader: materializing it first makes family rm proportional to the
// deleted family's resident memory and can wedge on a source that has already
// handed its backing to another generation.
func distributedRemovalSourceTransactions(transactions []cluster.Transaction, placements []cluster.PlacementRecord, info ttcbundle.ContainerInfo, branchID string) (settle, cancel, discard map[string]map[string]struct{}) {
	settle = make(map[string]map[string]struct{})
	cancel = make(map[string]map[string]struct{})
	discard = make(map[string]map[string]struct{})
	for _, tx := range transactions {
		if tx.Identity.SourceContainer != info.ID && tx.Identity.TargetContainer != info.ID {
			continue
		}
		if branchID != "" && tx.Identity.SourceBranch != branchID && tx.Identity.VisibleTargetBranch() != branchID {
			continue
		}
		if !transactionNeedsSourceSettlement(tx) {
			continue
		}
		node := tx.Identity.SourceNodeID
		removesTarget := branchID == "" || tx.Identity.VisibleTargetBranch() == branchID
		// A remote target may have been locally forked after import. Those
		// logical descendants share the transaction-private Sentry and its lazy
		// MemoryFile. Canceling the original transaction would attempt to delete
		// that whole physical runtime. Materialize the handoff instead whenever
		// a branch outside this removal still lives in the same physical family;
		// ordinary branch removal can then destroy only the selected branch.
		hasSurvivingPhysicalPeer := remoteForkTargetHasSurvivingPhysicalPeer(tx, placements, branchID)
		if !tx.Identity.Migration && removesTarget && !hasSurvivingPhysicalPeer {
			if cancel[node] == nil {
				cancel[node] = make(map[string]struct{})
			}
			cancel[node][tx.Identity.TransactionID] = struct{}{}
			continue
		}
		if tx.Identity.Migration {
			// Every migration needs a post-delete source discard. A still-live
			// dependency is first made independent, then follows the same path;
			// categorizing only its initial state lost that postflight.
			if discard[node] == nil {
				discard[node] = make(map[string]struct{})
			}
			discard[node][tx.Identity.TransactionID] = struct{}{}
			if tx.State == cluster.TransactionIndependent {
				continue
			}
		}
		if settle[node] == nil {
			settle[node] = make(map[string]struct{})
		}
		settle[node][tx.Identity.TransactionID] = struct{}{}
	}
	return settle, cancel, discard
}

func remoteForkTargetHasSurvivingPhysicalPeer(tx cluster.Transaction, placements []cluster.PlacementRecord, removedBranch string) bool {
	if tx.Identity.Migration || removedBranch == "" {
		return false
	}
	visibleTarget := tx.Identity.VisibleTargetBranch()
	physicalContainer := ""
	for i := range placements {
		record := placements[i]
		if record.BranchID != visibleTarget || record.OwnerNode != tx.Identity.TargetNodeID {
			continue
		}
		physicalContainer = record.PhysicalContainer
		if physicalContainer == "" {
			physicalContainer = record.FamilyID
		}
		break
	}
	if physicalContainer == "" {
		return false
	}
	for i := range placements {
		record := placements[i]
		if record.BranchID == removedBranch || record.OwnerNode != tx.Identity.TargetNodeID {
			continue
		}
		candidate := record.PhysicalContainer
		if candidate == "" {
			candidate = record.FamilyID
		}
		if candidate == physicalContainer {
			return true
		}
	}
	return false
}

// transactionNeedsSourceSettlement covers both an active remote dependency
// and the short post-independence interval before a migration's old physical
// source has been reclaimed. The latter is deliberately synchronous before a
// destructive lifecycle command: deleting target metadata first would leave
// no durable operation from which to publish/release the source's immutable
// graph roots after a daemon crash.
func transactionNeedsSourceSettlement(tx cluster.Transaction) bool {
	if tx.SourceNeeded && (tx.State == cluster.TransactionPublished || tx.State == cluster.TransactionDraining) {
		return true
	}
	return tx.Identity.Migration && tx.State == cluster.TransactionIndependent
}

// parseDistributedFamilyMutation recognizes the deliberately small set of
// destructive lifecycle commands that require cross-node dependency
// settlement. Both family and branch operands pass through the coordinator;
// family operands are additionally expanded to all logical placements. It
// follows flag.FlagSet's ordering rule: options precede positional targets.
func parseDistributedFamilyMutation(args []string) (distributedFamilyMutation, bool) {
	if len(args) < 2 || (args[0] != "rm" && args[0] != "stop" && args[0] != "dehydrate") {
		return distributedFamilyMutation{}, false
	}
	i := 1
	removeVolumes := false
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" {
		arg := args[i]
		switch args[0] {
		case "rm":
			if arg != "-f" && arg != "--force" && arg != "-v" && arg != "--volumes" && arg != "-fv" && arg != "-vf" {
				return distributedFamilyMutation{}, false
			}
			if arg == "-v" || arg == "--volumes" || arg == "-fv" || arg == "-vf" {
				removeVolumes = true
			}
		case "stop":
			if strings.HasPrefix(arg, "--time=") || strings.HasPrefix(arg, "--timeout=") || strings.HasPrefix(arg, "-t=") {
				break
			}
			if arg != "--time" && arg != "--timeout" && arg != "-t" {
				return distributedFamilyMutation{}, false
			}
			i++
			if i >= len(args) {
				return distributedFamilyMutation{}, false
			}
		case "dehydrate":
			return distributedFamilyMutation{}, false
		}
		i++
	}
	if i == len(args) {
		return distributedFamilyMutation{}, false
	}
	return distributedFamilyMutation{
		prefix: append([]string(nil), args[:i]...), targets: append([]string(nil), args[i:]...), removeVolumes: removeVolumes,
	}, true
}

func distributedChildRequestID(parent string, ordinal int, args []string) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", parent, ordinal, strings.Join(args, "\x00"))))
	return hex.EncodeToString(hash[:16])
}

// logicalFamilyPlacements returns the user-visible branch inventory for a
// clustered family. Raft placement records are the authority for logical
// identities; container metadata may additionally contain transaction-scoped
// physical branches used to implement migration. Those private branches must
// never be expanded into child CLI commands.
func logicalFamilyPlacements(catalog cluster.PlacementCatalog, familyID string) ([]cluster.PlacementRecord, error) {
	records, err := catalog.List()
	if err != nil {
		return nil, err
	}
	result := make([]cluster.PlacementRecord, 0)
	seen := make(map[string]struct{})
	for _, record := range records {
		if record.FamilyID != familyID {
			continue
		}
		if _, ok := seen[record.BranchID]; ok {
			return nil, fmt.Errorf("family %q has duplicate placement for branch %q", familyID, record.BranchID)
		}
		seen[record.BranchID] = struct{}{}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].BranchID < result[j].BranchID })
	return result, nil
}

// orderFamilyRemovalPlacements keeps a branch in the canonical physical
// container until last. The final child is a family rm, so it must execute
// where removing the canonical family record can also tear down its runtime.
// Branches materialized in transaction-named physical containers are removed
// individually first.
func orderFamilyRemovalPlacements(records []cluster.PlacementRecord, familyID string) {
	final := -1
	for i := range records {
		if records[i].PhysicalContainer != familyID {
			continue
		}
		if final == -1 || records[i].PhysicalBranch == records[i].BranchID {
			final = i
		}
		if records[i].PhysicalBranch == records[i].BranchID {
			break
		}
	}
	if final >= 0 && final != len(records)-1 {
		selected := records[final]
		copy(records[final:], records[final+1:])
		records[len(records)-1] = selected
	}
}

// executeDistributedStats obtains one accounting snapshot per physical
// runtime owner and reduces them at the entry daemon. A node-local ttcvolume
// repository can only describe branches resident on that node; trying to
// account a split family from any single owner either fails or reports false
// sharing. Raft placements define the authoritative partition.
func (s *daemonServer) executeDistributedStats(request daemonRequest) (bool, int, []byte, []byte, error) {
	if len(request.Args) < 2 || request.Args[0] != "stats" {
		return false, 0, nil, nil, nil
	}
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	var diagnostic bytes.Buffer
	fs.SetOutput(&diagnostic)
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(request.Args[1:]); err != nil {
		return true, 1, nil, diagnostic.Bytes(), nil
	}
	if fs.NArg() != 1 {
		return true, 1, nil, nil, errors.New("usage: ttc stats [--json] CONTAINER@BRANCH")
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return true, 1, nil, nil, err
	}
	store := ttcbundle.Store{Root: s.dataRoot}
	info, err := loadDistributedFamily(store, s.clusterPlacements, target.container)
	if err != nil {
		return true, 1, nil, nil, err
	}
	placements, err := logicalFamilyPlacements(s.clusterPlacements, info.ID)
	if err != nil {
		return true, 1, nil, nil, err
	}
	foundTarget := false
	for _, placement := range placements {
		if placement.BranchID == target.branch {
			foundTarget = true
			break
		}
	}
	if !foundTarget {
		return true, 1, nil, nil, fmt.Errorf("branch %q does not exist", target.branch)
	}
	authorityByBranch := make(map[string]cluster.BranchAuthority)
	if s.distributedSource != nil && s.distributedSource.authority != nil {
		authorities, authorityErr := s.distributedSource.authority.Branches()
		if authorityErr != nil {
			return true, 1, nil, nil, fmt.Errorf("reading branch authority for stats: %w", authorityErr)
		}
		for _, authority := range authorities {
			if authority.FamilyID == info.ID {
				authorityByBranch[authority.BranchID] = authority
			}
		}
	}
	owners := make(map[string]string)
	merged := FamilyResourceStats{Container: info.Name, Branches: []BranchResourceStats{}}
	if merged.Container == "" {
		merged.Container = target.container
	}
	for _, placement := range placements {
		if placement.OwnerNode != "" && (placement.State == cluster.PlacementRunning || placement.State == cluster.PlacementPaused) {
			if _, ok := owners[placement.OwnerNode]; !ok {
				owners[placement.OwnerNode] = placement.BranchID
			}
			continue
		}
		// Process-free and corrupted branches have no node-local resident graph.
		// Keep them visible in the cluster snapshot. Immutable cold packs are
		// cluster storage rather than capacity attributable to a runtime owner.
		authority, authoritative := authorityByBranch[placement.BranchID]
		state := string(placement.State)
		storageState := ""
		publicationID := ""
		filesystemRoot := ""
		sourceNode := ""
		if authoritative {
			state = normalizeLifecycleState(authority.Lifecycle)
			publicationID = authority.PublicationID
			if authority.Roots.FilesystemRoot != "" {
				storageState = "shared"
				filesystemRoot = filepath.Join(s.dataRoot, authority.Roots.FilesystemRoot)
			}
			if len(authority.VolatileDependencies) == 1 {
				sourceNode = authority.VolatileDependencies[0].SourceNode
			}
		}
		merged.Branches = append(merged.Branches, BranchResourceStats{
			Target: merged.Container + "@" + placement.BranchID, State: state,
			StorageState: storageState, PublicationID: publicationID,
			FilesystemRoot: filesystemRoot, SourceNode: sourceNode,
		})
	}
	nodes := make([]string, 0, len(owners))
	for node := range owners {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for ordinal, node := range nodes {
		args := []string{"_stats-local", "--json", info.Name + "@" + owners[node]}
		child := daemonRequest{
			Version: daemonAPIVersion, Action: "command", RequestID: distributedChildRequestID(request.RequestID, ordinal, args),
			Args: args, DistributedLeaf: true,
		}
		var status int
		var stdout, stderr string
		if node == s.clusterCapabilities.NodeID {
			status, stdout, stderr, err = s.executeLocalDistributedCommand(child)
		} else {
			peer, peerErr := s.distributedSource.peer(node, false)
			if peerErr != nil {
				return true, 1, nil, diagnostic.Bytes(), peerErr
			}
			status, stdout, stderr, err = executeRemoteDaemonRequest(context.Background(), peer, child)
		}
		diagnostic.WriteString(stderr)
		if err != nil {
			return true, 1, nil, diagnostic.Bytes(), fmt.Errorf("accounting branches on node %q: %w", node, err)
		}
		if status != 0 {
			return true, status, nil, diagnostic.Bytes(), nil
		}
		var local FamilyResourceStats
		if err := json.Unmarshal([]byte(stdout), &local); err != nil {
			return true, 1, nil, diagnostic.Bytes(), fmt.Errorf("decoding stats from node %q: %w", node, err)
		}
		// Node-local accounting operates on transaction-scoped physical
		// containers. Physical branch IDs are projected to logical IDs by the
		// owner, but the physical container name must never escape the internal
		// response. The coordinator is the single authority for the public family
		// name and normalizes every branch before aggregation.
		for i := range local.Branches {
			_, branchID, ok := strings.Cut(local.Branches[i].Target, "@")
			if !ok || branchID == "" {
				return true, 1, nil, diagnostic.Bytes(), fmt.Errorf("node %q returned invalid resource target %q", node, local.Branches[i].Target)
			}
			local.Branches[i].Target = merged.Container + "@" + branchID
		}
		merged.Branches = append(merged.Branches, local.Branches...)
		merged.ResidentMemoryUniqueBytes += local.ResidentMemoryUniqueBytes
		merged.MemoryBackingUniqueBytes += local.MemoryBackingUniqueBytes
		merged.FilesystemUniqueBytes += local.FilesystemUniqueBytes
		merged.DependentMemoryBytes += local.DependentMemoryBytes
		merged.DependentFilesystemBytes += local.DependentFilesystemBytes
		merged.ImageCacheBytes += local.ImageCacheBytes
		merged.ImageCacheTotalBytes += local.ImageCacheTotalBytes
	}
	sort.Slice(merged.Branches, func(i, j int) bool { return merged.Branches[i].Target < merged.Branches[j].Target })
	var output bytes.Buffer
	if err := writeResourceStats(&output, merged, *jsonOutput); err != nil {
		return true, 1, nil, diagnostic.Bytes(), err
	}
	return true, 0, output.Bytes(), diagnostic.Bytes(), nil
}

// executeDistributedFamilyMutation coordinates destructive family- and
// branch-scoped lifecycle changes before any daemon takes its process-local
// lifecycle lock. This is necessary once one logical family can have physical
// runtimes and immutable backing providers on multiple nodes.
func (s *daemonServer) executeDistributedFamilyMutation(request daemonRequest) (bool, int, []byte, []byte, error) {
	mutation, ok := parseDistributedFamilyMutation(request.Args)
	if !ok {
		return false, 0, nil, nil, nil
	}
	store := ttcbundle.Store{Root: s.dataRoot}
	type child struct {
		node   string
		args   []string
		claims []daemonPlacementClaim
	}
	// Settle every source transaction whose target is about to be mutated
	// before issuing any destructive child command. The family mutation below
	// is sequential but spans independently locked daemons; a target deleted
	// first cannot subsequently acknowledge its source's retained pages.
	var preflight []child
	transactions, err := s.clusterStore.List()
	if err != nil {
		return true, 1, nil, nil, fmt.Errorf("listing distributed transactions before family mutation: %w", err)
	}
	transactionsByID := make(map[string]*cluster.Transaction, len(transactions))
	for i := range transactions {
		transactionsByID[transactions[i].Identity.TransactionID] = &transactions[i]
	}
	settleByNode := make(map[string]map[string]struct{})
	cancelByNode := make(map[string]map[string]struct{})
	discardByNode := make(map[string]map[string]struct{})
	canceledTargets := make(map[string]string)
	for _, operand := range mutation.targets {
		name := operand
		branchID := ""
		if strings.Contains(operand, "@") {
			selected, err := parseSelector(operand)
			if err != nil {
				return true, 1, nil, nil, fmt.Errorf("invalid target %q: %w", operand, err)
			}
			name = selected.container
			branchID = selected.branch
		}
		info, err := loadDistributedFamily(store, s.clusterPlacements, name)
		if err != nil {
			return true, 1, nil, nil, err
		}
		settle := distributedMutationSourceTransactions(transactions, info, branchID)
		cancel := map[string]map[string]struct{}{}
		discard := map[string]map[string]struct{}{}
		if request.Args[0] == "rm" {
			placements, placementErr := logicalFamilyPlacements(s.clusterPlacements, info.ID)
			if placementErr != nil {
				return true, 1, nil, nil, fmt.Errorf("listing logical branches for %q: %w", operand, placementErr)
			}
			settle, cancel, discard = distributedRemovalSourceTransactions(transactions, placements, info, branchID)
		}
		for node, ids := range settle {
			if settleByNode[node] == nil {
				settleByNode[node] = make(map[string]struct{})
			}
			for transactionID := range ids {
				settleByNode[node][transactionID] = struct{}{}
			}
		}
		for node, ids := range cancel {
			if cancelByNode[node] == nil {
				cancelByNode[node] = make(map[string]struct{})
			}
			for transactionID := range ids {
				cancelByNode[node][transactionID] = struct{}{}
				if tx := transactionsByID[transactionID]; tx != nil {
					canceledTargets[tx.Identity.TargetContainer+"\x00"+tx.Identity.VisibleTargetBranch()] = transactionID
				}
			}
		}
		for node, ids := range discard {
			if discardByNode[node] == nil {
				discardByNode[node] = make(map[string]struct{})
			}
			for transactionID := range ids {
				discardByNode[node][transactionID] = struct{}{}
			}
		}
	}
	nodes := make([]string, 0, max(len(cancelByNode), len(settleByNode)))
	for node := range cancelByNode {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		ids := make([]string, 0, len(cancelByNode[node]))
		for transactionID := range cancelByNode[node] {
			ids = append(ids, transactionID)
		}
		sort.Strings(ids)
		preflight = append(preflight, child{node: node, args: append([]string{"_cancel-source-transactions"}, ids...)})
	}
	nodes = nodes[:0]
	for node := range settleByNode {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		ids := make([]string, 0, len(settleByNode[node]))
		for transactionID := range settleByNode[node] {
			ids = append(ids, transactionID)
		}
		sort.Strings(ids)
		preflight = append(preflight, child{node: node, args: append([]string{"_settle-source-transactions"}, ids...)})
	}
	// Once migration targets are independent, their retired physical sources
	// are unnecessary to a removal. Reclaim them while the target placement is
	// still authoritative so the source's persistent family catalog can never
	// be mistaken for an unowned live branch between placement deletion and
	// postflight. The transaction remains in Raft until postflight and is the
	// idempotent crash-recovery intent for this cleanup.
	nodes = nodes[:0]
	for node := range discardByNode {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		ids := make([]string, 0, len(discardByNode[node]))
		for transactionID := range discardByNode[node] {
			ids = append(ids, transactionID)
		}
		sort.Strings(ids)
		cleanupNode, err := s.distributedCleanupNode(node)
		if err != nil {
			return true, 1, nil, nil, err
		}
		preflight = append(preflight, child{node: cleanupNode, args: append([]string{"_prepare-removed-migration-sources"}, ids...)})
	}
	var children []child
	var finalizers []child
	var postflight []child
	for _, operand := range mutation.targets {
		if strings.Contains(operand, "@") {
			selected, err := parseSelector(operand)
			if err != nil {
				return true, 1, nil, nil, fmt.Errorf("invalid target %q: %w", operand, err)
			}
			info, err := loadDistributedFamily(store, s.clusterPlacements, selected.container)
			if err != nil {
				return true, 1, nil, nil, err
			}
			placement, placementErr := s.clusterPlacements.Lookup(info.ID, selected.branch)
			if placementErr != nil {
				return true, 1, nil, nil, fmt.Errorf("resolving family mutation placement for %s: %w", selected, placementErr)
			}
			node := placement.OwnerNode
			if node == "" {
				node = s.clusterCapabilities.NodeID
			}
			if transactionID := canceledTargets[info.ID+"\x00"+selected.branch]; transactionID != "" {
				children = append(children, child{node: node, args: []string{"_remove-canceled-fork-placement", info.ID + "@" + selected.branch, transactionID}})
			} else {
				children = append(children, child{node: node, args: append(append([]string(nil), mutation.prefix...), operand)})
			}
			if request.Args[0] == "rm" {
				familyPlacements, err := logicalFamilyPlacements(s.clusterPlacements, info.ID)
				if err != nil {
					return true, 1, nil, nil, fmt.Errorf("listing logical branches for %q: %w", operand, err)
				}
				if len(familyPlacements) == 1 && familyPlacements[0].BranchID == selected.branch {
					// Branch-qualified removal of the final logical branch has the
					// same empty-family result as standalone TTC. The physical target
					// may be a migration wrapper, so it cannot remove the canonical
					// family catalog on its own.
					finalizers = append(finalizers, child{node: node, args: []string{"_remove-family-record", info.ID}})
				}
			}
			continue
		}
		info, err := loadDistributedFamily(store, s.clusterPlacements, operand)
		if err != nil {
			return true, 1, nil, nil, err
		}
		placements, err := logicalFamilyPlacements(s.clusterPlacements, info.ID)
		if err != nil {
			return true, 1, nil, nil, fmt.Errorf("listing logical branches for %q: %w", operand, err)
		}
		if len(placements) == 0 {
			return true, 1, nil, nil, fmt.Errorf("container %q has no branches", operand)
		}
		if request.Args[0] == "rm" {
			orderFamilyRemovalPlacements(placements, info.ID)
		}
		finalNode := placements[len(placements)-1].OwnerNode
		if finalNode == "" {
			finalNode = s.clusterCapabilities.NodeID
		}
		branchPrefix := mutation.branchPrefix()
		for _, placement := range placements {
			node := placement.OwnerNode
			if node == "" {
				node = s.clusterCapabilities.NodeID
			}
			if transactionID := canceledTargets[info.ID+"\x00"+placement.BranchID]; transactionID != "" {
				children = append(children, child{node: node, args: []string{"_remove-canceled-fork-placement", info.ID + "@" + placement.BranchID, transactionID}})
			} else {
				target := operand + "@" + placement.BranchID
				children = append(children, child{node: node, args: append(append([]string(nil), branchPrefix...), target)})
			}
		}
		if request.Args[0] == "rm" {
			// Every logical branch above is removed through its exact Raft physical
			// identity. Only then may the canonical family record and its optional
			// anonymous volumes disappear. The finalizer is idempotent because a
			// canonical physical branch may already have removed that record.
			args := []string{"_remove-family-record"}
			if mutation.removeVolumes {
				args = append(args, "--volumes")
			}
			args = append(args, info.ID)
			finalizers = append(finalizers, child{node: finalNode, args: args})
		}
	}
	if request.Args[0] == "rm" {
		nodes := make([]string, 0, len(discardByNode))
		for node := range discardByNode {
			nodes = append(nodes, node)
		}
		sort.Strings(nodes)
		for _, node := range nodes {
			ids := make([]string, 0, len(discardByNode[node]))
			for transactionID := range discardByNode[node] {
				ids = append(ids, transactionID)
			}
			sort.Strings(ids)
			cleanupNode, err := s.distributedCleanupNode(node)
			if err != nil {
				return true, 1, nil, nil, err
			}
			postflight = append(postflight, child{node: cleanupNode, args: append([]string{"_discard-removed-migration-sources"}, ids...)})
		}
	}
	children = append(append(append(preflight, children...), finalizers...), postflight...)

	var diagnostic bytes.Buffer
	for i, child := range children {
		// Clustered roots have exactly one placement authority: the Raft
		// catalog installed on the daemon. Do not reopen the retired
		// shared-filesystem catalog here. In particular, a family operation
		// runs several child mutations and re-resolves each one after the
		// preceding child may have changed metadata; consulting the empty
		// legacy catalog at this point made rm/stop/dehydrate fail after an
		// otherwise successful migration.
		if owner, claims, err := daemonCommandPlacementWithCatalog(s.dataRoot, s.clusterPlacements, child.args); err != nil {
			return true, 1, nil, diagnostic.Bytes(), err
		} else {
			if owner != "" {
				child.node = owner
			}
			child.claims = claims
		}
		childRequest := daemonRequest{
			Version: daemonAPIVersion, Action: "command", RequestID: distributedChildRequestID(request.RequestID, i, child.args),
			Args: child.args, ClientTERM: request.ClientTERM,
			DistributedLeaf: true, PlacementClaims: child.claims,
		}
		var status int
		var stdout, stderr string
		var err error
		if child.node == s.clusterCapabilities.NodeID {
			status, stdout, stderr, err = s.executeLocalDistributedCommand(childRequest)
		} else {
			peer, peerErr := s.distributedSource.peer(child.node, false)
			if peerErr != nil {
				return true, 1, nil, diagnostic.Bytes(), peerErr
			}
			status, stdout, stderr, err = executeRemoteDaemonRequest(context.Background(), peer, childRequest)
		}
		_ = stdout // Child commands print branch-local selectors; the coordinator prints original operands.
		diagnostic.WriteString(stderr)
		if err != nil {
			return true, 1, nil, diagnostic.Bytes(), fmt.Errorf("executing family operation on node %q: %w", child.node, err)
		}
		if status != 0 {
			return true, status, nil, diagnostic.Bytes(), nil
		}
	}
	var output strings.Builder
	for _, operand := range mutation.targets {
		fmt.Fprintln(&output, operand)
	}
	return true, 0, []byte(output.String()), diagnostic.Bytes(), nil
}

func (s *daemonServer) distributedCleanupNode(sourceNode string) (string, error) {
	if _, err := s.clusterDirectory.Lookup(sourceNode); errors.Is(err, os.ErrNotExist) {
		// Cleanly retired nodes have already discarded their physical migration
		// sources before membership removal. A live coordinator can retire the
		// remaining transaction after verifying the durable retirement fence.
		return s.clusterCapabilities.NodeID, nil
	} else if err != nil {
		return "", fmt.Errorf("resolving migration cleanup node %q: %w", sourceNode, err)
	}
	return sourceNode, nil
}

func (s *daemonServer) executeLocalDistributedCommand(request daemonRequest) (int, string, string, error) {
	opts := s.optionsFromRequest()
	var stdout, stderr bytes.Buffer
	backend := directBackend(opts, strings.NewReader(""), &stdout, &stderr)
	s.configureVolumeBackend(backend)
	backend.consoleSidecars = s
	backend.distributedSource = s.distributedSource
	backend.nodeID = s.clusterCapabilities.NodeID
	unlock := s.lockMutation(opts.dataRoot, request.Args)
	defer unlock()
	if request.RequestID != "" && daemonCommandNeedsMutationJournal(request.Args) {
		record, replay, err := replayCompletedDaemonMutation(s.mutationRoot(), request)
		if err != nil {
			return 1, stdout.String(), stderr.String(), err
		}
		if replay {
			return record.Status, string(record.Stdout), string(record.Stderr), nil
		}
	}
	s.reconcile(backend)
	placements, err := backend.placementCatalog()
	if err != nil && len(request.PlacementClaims) != 0 {
		return 1, stdout.String(), stderr.String(), err
	}
	if err := validateDaemonPlacementClaimsWithCatalog(opts.dataRoot, backend.nodeID, request.PlacementClaims, placements); err != nil {
		return 1, stdout.String(), stderr.String(), err
	}
	if err := s.claimUnplacedLifecycleTargets(backend, request.Args); err != nil {
		return 1, stdout.String(), stderr.String(), err
	}
	if request.RequestID != "" && daemonCommandNeedsMutationJournal(request.Args) {
		mutationRoot := s.mutationRoot()
		record, replay, err := beginDaemonMutation(mutationRoot, request)
		if err != nil {
			return 1, stdout.String(), stderr.String(), err
		}
		if replay {
			return record.Status, string(record.Stdout), string(record.Stderr), nil
		}
		status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
		s.recordCommandEvents(opts.dataRoot, request.Args, status)
		if status == 0 {
			if err := s.claimUnplacedLifecycleTargets(backend, request.Args); err != nil {
				return 1, stdout.String(), stderr.String(), err
			}
			if err := backend.reconcilePlacementCatalog(); err != nil {
				fmt.Fprintf(&stderr, "ttcd: warning: reconciling placement catalog: %v\n", err)
			}
		}
		record, err = completeDaemonMutation(mutationRoot, record, status, stdout.Bytes(), stderr.Bytes())
		if err != nil {
			return 1, stdout.String(), stderr.String(), err
		}
		s.maybeCompactDaemonMutations(mutationRoot)
		return record.Status, string(record.Stdout), string(record.Stderr), nil
	}
	status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
	s.recordCommandEvents(opts.dataRoot, request.Args, status)
	if status == 0 {
		if err := s.claimUnplacedLifecycleTargets(backend, request.Args); err != nil {
			return 1, stdout.String(), stderr.String(), err
		}
		if err := backend.reconcilePlacementCatalog(); err != nil {
			fmt.Fprintf(&stderr, "ttcd: warning: reconciling placement catalog: %v\n", err)
		}
	}
	return status, stdout.String(), stderr.String(), nil
}

func (b *runtimeBackend) runtimeBranchLifecycle(containerID, branchID string) (string, error) {
	branches, err := b.runtimeBranches(containerID)
	if err != nil {
		return "", err
	}
	for _, branch := range branches {
		if branch.ID == branchID {
			return normalizeLifecycleState(branch.State), nil
		}
	}
	return "", fmt.Errorf("branch %q disappeared from runtime state", branchID)
}

func (b *runtimeBackend) runtimeBranches(containerID string) ([]runscBranchInfo, error) {
	output, err := b.command("timetravel", containerID, "branches")
	if err != nil {
		return nil, err
	}
	var branches []runscBranchInfo
	if err := json.Unmarshal(output, &branches); err != nil {
		return nil, fmt.Errorf("decoding runtime branches: %w", err)
	}
	return branches, nil
}

// migrate performs an identity-preserving sparse cutover. The source is paused
// once, target metadata and page readers are reconstructed while it remains
// paused, and publication atomically transfers the logical branch. The target
// may execute immediately using live network page pulls; a background sparse
// handoff removes that dependency before the source runtime is reclaimed.
func (b *runtimeBackend) migrate(target selector, targetNode string) error {
	if b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		return errors.New("elastic migration requires dynamic cluster discovery")
	}
	logicalTarget := target
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
	logicalFamily := info.DistributedFamilyID
	if logicalFamily == "" {
		logicalFamily = info.ID
	}
	persistentBranch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return err
	}
	state := normalizeLifecycleState(persistentBranch.State)
	if state != "running" && state != "paused" {
		return fmt.Errorf("branch %s is %s, not running or paused", target, state)
	}
	// Older interrupted rollbacks could publish stable persistent state before
	// restoring the Sentry branch. Repair that narrow mismatch before acquiring
	// a new migration transaction; current rollbacks remain fenced until this
	// condition is true.
	if runtimeState, runtimeErr := b.runtimeBranchLifecycle(info.ID, target.branch); runtimeErr == nil && runtimeState == "dehydrated" {
		if err := b.rehydrate(target); err != nil {
			return fmt.Errorf("restoring stale dehydrated source before migration: %w", err)
		}
		if runtimeState, runtimeErr = b.runtimeBranchLifecycle(info.ID, target.branch); runtimeErr != nil || runtimeState != state {
			return fmt.Errorf("restored migration source has runtime state %q, want %q: %v", runtimeState, state, runtimeErr)
		}
	}
	sourceRecord, err := b.renewDistributedExecutionAuthority()
	if err != nil {
		return fmt.Errorf("renewing source node lease: %w", err)
	}
	targetRecord, err := b.distributedSource.directory.Lookup(targetNode)
	if err != nil {
		return fmt.Errorf("looking up migration target: %w", err)
	}
	if !targetRecord.AcceptsPlacement(time.Now()) {
		return fmt.Errorf("cluster target node %q is not accepting placement (state=%s)", targetNode, targetRecord.State)
	}
	peer, err := b.distributedSource.peer(targetNode, true)
	if err != nil {
		return err
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	placement, err := placements.Lookup(logicalFamily, logicalTarget.branch)
	if errors.Is(err, os.ErrNotExist) {
		placement, err = placements.Create(cluster.PlacementRecord{
			FamilyID: logicalFamily, BranchID: logicalTarget.branch, PhysicalContainer: info.ID, PhysicalBranch: target.branch,
			OwnerNode: b.nodeID, OwnerIncarnation: sourceRecord.Incarnation, State: cluster.PlacementState(state),
			FamilyName: info.Name, ImageReference: info.Reference, ImageDigest: info.Digest, ImageSize: info.ImageSize,
			LiveForkEligible: info.LiveForkEligible, LiveForkWaiver: info.LiveForkWaiver,
			FamilyCreatedAt: info.CreatedAt, ParentBranch: persistentBranch.ParentID, BranchCreatedAt: persistentBranch.CreatedAt,
			Quarantined: persistentBranch.Quarantined,
		}, time.Now())
	}
	if err != nil {
		return err
	}
	if placement.OwnerNode == b.nodeID && placement.OwnerIncarnation != sourceRecord.Incarnation {
		placement, err = placements.AdoptOwnerIncarnation(logicalFamily, logicalTarget.branch, b.nodeID, placement.OwnerIncarnation, sourceRecord.Incarnation, placement.Generation, time.Now())
		if err != nil {
			return fmt.Errorf("adopting placement after node restart: %w", err)
		}
	}
	requestID, err := newDistributedTransactionID()
	if err != nil {
		return err
	}
	if _, err := placements.BeginMigration(logicalFamily, logicalTarget.branch, b.nodeID, sourceRecord.Incarnation, placement.Generation, requestID, targetNode, time.Now()); err != nil {
		return err
	}
	rootfsDigest, err := distributedRootfsDigest(info, target.branch, b.distributedSource.capabilities.RepositoryID)
	if err != nil {
		_, _ = placements.AbortMigration(logicalFamily, logicalTarget.branch, requestID, time.Now())
		return err
	}
	identity := cluster.ForkIdentity{
		TransactionID: requestID, SourceNodeID: b.nodeID, TargetNodeID: targetNode,
		SourceContainer: logicalFamily, SourceBranch: logicalTarget.branch,
		TargetContainer: logicalFamily, TargetBranch: requestID, TargetVisibleBranch: logicalTarget.branch,
		ImageDigest: info.Digest, RootfsDigest: rootfsDigest,
		RepositoryID: b.distributedSource.capabilities.RepositoryID, Migration: true,
	}
	family, err := b.portableDistributedFamily(info)
	if err != nil {
		_, _ = placements.AbortMigration(logicalFamily, logicalTarget.branch, requestID, time.Now())
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tx, err := peer.Reserve(ctx, cluster.ReserveRequest{SourceCapabilities: b.distributedSource.capabilities, Identity: identity, Family: family})
	if err != nil {
		_, _ = placements.AbortMigration(logicalFamily, logicalTarget.branch, requestID, time.Now())
		return fmt.Errorf("reserving sparse migration target %q: %w", targetNode, err)
	}
	transferRetained := false
	if len(persistentBranch.MemoryRootObjects) != 0 {
		if err := b.retainDistributedGraphTransfer(requestID, info.ID, target.branch, branchGraphRootObjects(persistentBranch)); err != nil {
			_, _ = peer.Cancel(ctx, requestID, tx.Revision)
			_, _ = placements.AbortMigration(logicalFamily, logicalTarget.branch, requestID, time.Now())
			return fmt.Errorf("retaining sparse migration source graph: %w", err)
		}
		transferRetained = true
	}

	execSessions, sidecarErr := b.stopMigrationBranchSidecars(target, info)
	if sidecarErr != nil {
		_, _ = peer.Cancel(ctx, requestID, tx.Revision)
		_, _ = placements.AbortMigration(logicalFamily, logicalTarget.branch, requestID, time.Now())
		if transferRetained {
			_ = b.releaseDistributedGraphReferences(graphTransferReferenceOperation(requestID))
		}
		return sidecarErr
	}
	var relay *distributedPageRelay
	captured := false
	published := false
	restoreSource := func() error {
		var result error
		if captured {
			result = errors.Join(result, func() error {
				_, err := b.command("timetravel", info.ID, "distributed-cancel", requestID)
				return err
			}())
		}
		if captured && state == "running" {
			if _, err := b.command("timetravel", info.ID, "resume", target.branch); err != nil {
				result = errors.Join(result, fmt.Errorf("resuming migration source: %w", err))
			}
		}
		result = errors.Join(result, b.restoreMigrationBranchSidecars(target, info, execSessions))
		return result
	}
	abort := func(cause error) error {
		if published {
			return cause
		}
		if relay != nil {
			relay.Close()
		}
		// Tear down the source-side capture before canceling the target. Target
		// cancellation closes the dependency connection and may otherwise make
		// the capture session disappear before distributed-cancel can restore
		// its bookkeeping. The target is still physically paused and placement
		// remains fenced, so resuming the authoritative source first is safe.
		restoreErr := restoreSource()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		var cancelErr error
		if current, statusErr := peer.Status(cleanupCtx, requestID); statusErr == nil {
			_, cancelErr = peer.Cancel(cleanupCtx, requestID, current.Revision)
		} else {
			_, cancelErr = peer.Cancel(cleanupCtx, requestID, tx.Revision)
		}
		cleanupCancel()
		if cancelErr != nil {
			// Keep the placement intent fenced. Startup/background recovery can
			// retry this exact transaction; publishing a stable source placement
			// while an unpublished target runtime survived would allow the next
			// attempt to collide with it.
			return fmt.Errorf("%w; restoring sparse migration source: %v; canceling sparse migration target: %v", cause, restoreErr, cancelErr)
		}
		if _, abortErr := placements.AbortMigration(logicalFamily, logicalTarget.branch, requestID, time.Now()); abortErr != nil {
			return fmt.Errorf("%w; restoring sparse migration source: %v; aborting placement: %v", cause, restoreErr, abortErr)
		}
		if restoreErr != nil {
			return fmt.Errorf("%w; restoring sparse migration source: %v", cause, restoreErr)
		}
		if transferRetained {
			if releaseErr := b.releaseDistributedGraphReferences(graphTransferReferenceOperation(requestID)); releaseErr != nil {
				return fmt.Errorf("%w; releasing sparse migration transfer roots: %v", cause, releaseErr)
			}
		}
		return cause
	}

	listener, err := net.Listen("tcp", b.distributedSource.pageListen)
	if err != nil {
		return abort(fmt.Errorf("opening sparse migration page listener: %w", err))
	}
	advertise, err := resolveDistributedAdvertiseAddress(b.distributedSource.pageAdvertise, listener.Addr().String())
	if err != nil {
		listener.Close()
		return abort(err)
	}
	relayBackend := *b
	relayBackend.stdin = strings.NewReader("")
	relayBackend.stdout = io.Discard
	relayBackend.stderr = io.Discard
	relay = newDistributedPageRelay(listener, &relayBackend, info.ID, requestID)
	dataPlaneMax := tx.Negotiated.PagingPolicy.TransferBytes
	if tx.Negotiated.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
		dataPlaneMax = tx.Negotiated.MaxTransferSize
	}
	commandArgs := []string{
		"timetravel", "--source", target.branch,
		"--distributed-session", requestID,
		"--distributed-family", logicalFamily,
		"--distributed-external-listener", "--distributed-advertise", advertise,
		"--distributed-max-transfer", fmt.Sprint(dataPlaneMax),
		"--distributed-push-policy", string(tx.Negotiated.PagingPolicy.Push),
		"--distributed-push-high-water", fmt.Sprint(tx.Negotiated.PagingPolicy.MaxOutstandingBytes),
		"--distributed-leave-source-paused",
		info.ID, "distributed-capture", requestID,
	}
	filesystemOrigins, finishFilesystemOrigins, err := b.allocateFilesystemForkOrigins(info.ID, 1)
	if err != nil {
		return abort(err)
	}
	var output []byte
	if len(filesystemOrigins) == 0 {
		output, err = b.command(commandArgs...)
	} else {
		originsReader, pipeErr := filesystemOriginAssignmentsPipe(filesystemOrigins)
		if pipeErr != nil {
			return abort(errors.Join(pipeErr, finishFilesystemOrigins(false)))
		}
		commandArgs = append([]string{"timetravel", "--filesystem-origins-fd=3"}, commandArgs[1:]...)
		output, err = b.commandWithFiles([]*os.File{originsReader}, commandArgs...)
		closeErr := originsReader.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return abort(errors.Join(fmt.Errorf("capturing sparse migration source: %w", err), finishFilesystemOrigins(false)))
	}
	if err := finishFilesystemOrigins(true); err != nil {
		return abort(err)
	}
	captured = true
	var session boot.DistributedForkSessionInfo
	if err := json.Unmarshal(output, &session); err != nil {
		return abort(fmt.Errorf("decoding sparse migration capture: %w", err))
	}
	if session.SessionID != requestID || session.MetadataBytes == 0 || session.StreamBytes == 0 {
		return abort(errors.New("sparse migration capture returned incomplete session metadata"))
	}
	filesystemRoots, filesystemBytes, err := b.prepareDistributedFilesystem(info, session.FilesystemBranch, relay)
	if err != nil {
		return abort(fmt.Errorf("sealing sparse migration filesystem generation: %w", err))
	}
	internalMounts, err := b.portableDistributedInternalMounts(info.ID)
	if err != nil {
		return abort(fmt.Errorf("capturing portable internal mounts: %w", err))
	}
	execConsoleLogs, execConsoleFiles, err := b.captureRemoteExecConsoleLogs(info.ID, target.branch, session.ExecSessions)
	if err != nil {
		return abort(fmt.Errorf("capturing retained exec output: %w", err))
	}
	if len(execConsoleLogs) != 0 {
		if err := relay.ConfigureConsoleLogs(session.Token, execConsoleFiles); err != nil {
			return abort(fmt.Errorf("configuring retained exec output relay: %w", err))
		}
	}
	descriptor := cluster.RemoteSessionDescriptor{
		Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
		MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
		MaxTransferBytes: dataPlaneMax, FilesystemRoots: filesystemRoots,
		FilesystemBytes: filesystemBytes, InternalMounts: internalMounts,
		ExecSessions: remoteExecSessions(session.ExecSessions), ExecConsoleLogs: execConsoleLogs,
		GraphSnapshot: persistentBranch.SnapshotID, GraphRootObjects: append([]string(nil), persistentBranch.MemoryRootObjects...),
	}
	tx, err = peer.Prepare(ctx, requestID, tx.Revision, descriptor)
	if err != nil {
		return abort(fmt.Errorf("preparing sparse migration target: %w", err))
	}
	tx, err = publishRemoteTarget(ctx, peer, requestID, tx.Revision, state == "paused")
	if err != nil {
		status, statusErr := peer.Status(ctx, requestID)
		if statusErr != nil || (status.State != cluster.TransactionPublished && status.State != cluster.TransactionDraining && status.State != cluster.TransactionIndependent) {
			return abort(fmt.Errorf("publishing sparse migration target: %w", err))
		}
		tx = status
	}
	if b.distributedSource == nil || b.distributedSource.authority == nil {
		return abort(errors.New("raft migration authority is unavailable"))
	}
	if tx.TargetPhysicalContainer == "" || tx.TargetPhysicalBranch == "" || tx.TargetPhysicalIncarnation != targetRecord.Incarnation {
		return abort(errors.New("published migration target omitted or changed its physical runtime identity"))
	}
	cutover, err := b.distributedSource.authority.CutoverMigration(requestID, tx.Revision, logicalFamily, logicalTarget.branch, requestID, targetNode, targetRecord.Incarnation, tx.TargetPhysicalContainer, tx.TargetPhysicalBranch)
	if err != nil {
		return abort(fmt.Errorf("publishing migration placement: %w", err))
	}
	tx = &cutover.Transaction
	published = true
	if err := b.syncLogicalBranchOwner(logicalFamily, logicalTarget.branch, targetNode); err != nil {
		// Raft cutover is already authoritative and must not be rolled back for a
		// denormalized bundle write. Reconciliation retries this repair before
		// using local ownership for reclamation.
		log.Warningf("TTC: migration %s cut over but canonical owner repair is pending: %v", requestID, err)
	}
	tx, err = activateRemoteMigration(ctx, peer, requestID, tx.Revision)
	if err != nil {
		status, statusErr := peer.Status(ctx, requestID)
		if statusErr != nil || !status.Activated {
			return fmt.Errorf("placement moved to %q but target activation is pending recovery: %w", targetNode, err)
		}
		tx = status
	}
	monitorBackend := *b
	monitorBackend.stdin = strings.NewReader("")
	monitorBackend.stdout = io.Discard
	monitorBackend.stderr = io.Discard
	// A cordoned source is being evacuated. drainNode immediately establishes
	// an eager independence barrier after this call returns, so racing that
	// barrier with the ordinary asynchronous graph handoff creates two terminal
	// consumers of the same live filesystem session. In particular, a slow
	// shared filesystem can let the reader switch win while the eager
	// filesystem walk is still using the source stream. Use the direct drain
	// path exclusively for retirement; target execution is already active, and
	// node shutdown must wait for complete materialization in either case.
	if sourceRecord.State != cluster.NodeDraining {
		b.distributedSource.scheduleHandoff(func() {
			monitorBackend.handoffDistributedFork(peer, info.ID, requestID, tx.Revision, relay)
		})
	}
	// monitorSparseMigrationCleanup is deliberately passive until the eager
	// retirement barrier has made the target independent. It keeps the relay
	// alive, then closes it and releases the physical source branch. Starting
	// monitorDistributedFork here as well would create a second active drain
	// driver; after either caller timed out, its still-running HTTP handler and
	// retries could form an O(branches^2) target-lock convoy.
	b.distributedSource.scheduleSourceMonitor(func() {
		monitorBackend.monitorSparseMigrationCleanup(peer, info.ID, target.branch, requestID, relay)
	})
	return nil
}

func (b *runtimeBackend) stopMigrationBranchSidecars(target selector, info ttcbundle.ContainerInfo) ([]runscExecSession, error) {
	sessions, err := b.execSessions(target)
	if err != nil {
		return nil, fmt.Errorf("listing migration exec sessions: %w", err)
	}
	store := b.containerStore()
	paths, err := store.BranchConsolePaths(info.ID, target.branch)
	if err != nil {
		return nil, err
	}
	if err := b.stopConsoleDrain(paths); err != nil {
		return nil, fmt.Errorf("stopping migration init console follower: %w", err)
	}
	for _, session := range sessions {
		paths, pathErr := store.ExecConsolePaths(info.ID, target.branch, session.ID)
		if pathErr != nil {
			return sessions, pathErr
		}
		if err := b.stopConsoleDrain(paths); err != nil {
			_ = b.restoreMigrationBranchSidecars(target, info, sessions)
			return sessions, fmt.Errorf("stopping migration exec console %q: %w", session.ID, err)
		}
	}
	if err := b.stopPublishedPortsForBranch(info, target.branch); err != nil {
		_ = b.restoreMigrationBranchSidecars(target, info, sessions)
		return sessions, fmt.Errorf("stopping migration published ports: %w", err)
	}
	return sessions, nil
}

func (b *runtimeBackend) restoreMigrationBranchSidecars(target selector, info ttcbundle.ContainerInfo, sessions []runscExecSession) error {
	var result error
	if _, err := b.ensureConsoleDrain(target, false); err != nil {
		result = errors.Join(result, fmt.Errorf("restoring init console: %w", err))
	}
	for _, session := range sessions {
		if session.State != "running" {
			continue
		}
		if _, err := b.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false); err != nil {
			result = errors.Join(result, fmt.Errorf("restoring exec console %q: %w", session.ID, err))
		}
	}
	result = errors.Join(result, b.ensurePublishedPortsForBranch(info, target.branch))
	return result
}

func (b *runtimeBackend) monitorSparseMigrationCleanup(peer *cluster.Client, containerID, branchID, transactionID string, relay *distributedPageRelay) {
	for {
		timer := time.NewTimer(b.distributedSource.sourceMonitorInterval())
		<-timer.C
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tx, err := peer.Status(ctx, transactionID)
		cancel()
		if err != nil {
			continue
		}
		switch tx.State {
		case cluster.TransactionIndependent:
			unlockSource := b.distributedSource.lockSourceFamily(containerID)
			_, _ = b.command("timetravel", containerID, "distributed-complete", transactionID)
			relay.Close()
			if err := b.releaseDistributedGraphReferences(graphTransferReferenceOperation(transactionID)); err != nil {
				log.Warningf("TTC: retaining completed migration transfer roots for retry %s@%s: %v", containerID, branchID, err)
			}
			// If rm already committed logical placement deletion, there is no
			// surviving reader for this retired provider. Discard it directly;
			// publishing here would race the deletion postflight and copy an
			// entire dead memory graph to shared storage.
			discard := false
			if placements, placementErr := b.placementCatalog(); placementErr == nil {
				_, placementErr = placements.Lookup(tx.Identity.SourceContainer, tx.Identity.SourceBranch)
				discard = errors.Is(placementErr, os.ErrNotExist)
			}
			var releaseErr error
			if discard {
				releaseErr = b.releaseMigratedSourceRuntimeDiscard(containerID, branchID)
			} else {
				releaseErr = b.releaseMigratedSourceRuntime(containerID, branchID)
			}
			if releaseErr != nil {
				log.Warningf("TTC: retaining source runtime after sparse migration %s@%s: %v", containerID, branchID, releaseErr)
			}
			unlockSource()
			return
		case cluster.TransactionCanceled, cluster.TransactionFailed:
			// A published migration never rolls ownership backward. Retain the
			// source snapshot for diagnostics or an explicit recovery attempt.
			return
		}
	}
}

func (b *runtimeBackend) releaseMigratedSourceRuntime(containerID, branchID string) error {
	return b.releaseMigratedSourceRuntimeWithPolicy(containerID, branchID, true)
}

func (b *runtimeBackend) releaseMigratedSourceRuntimeDiscard(containerID, branchID string) error {
	return b.releaseMigratedSourceRuntimeWithPolicy(containerID, branchID, false)
}

func (b *runtimeBackend) releaseMigratedSourceRuntimeWithPolicy(containerID, branchID string, publishBacking bool) error {
	// containerID is an authority-derived physical runtime ID. Distributed
	// targets are intentionally hidden from the public resolver; using it here
	// turned completed migrations into false no-ops and left their paused
	// Sentry/gofer pairs behind after clean node retirement.
	info, err := b.containerStore().LoadRuntimeContainer(containerID)
	if errors.Is(err, os.ErrNotExist) {
		// Metadata absence is not execution absence. A concurrent cold
		// publication or cleanup retry may have removed the transaction wrapper
		// while runsc still retains its paused source Kernel. Reconcile the exact
		// authority-derived physical identity before declaring cleanup complete.
		return b.removeUnrecordedMigratedSourceRuntime(containerID, branchID, nil)
	}
	if err != nil {
		return err
	}
	branchRecorded := false
	for _, branch := range info.Branches {
		if branch.ID == branchID {
			branchRecorded = true
			break
		}
	}
	if !branchRecorded {
		return b.removeUnrecordedMigratedSourceRuntime(containerID, branchID, &info)
	}
	runtimes, err := b.listRuntimeFamilies()
	if err != nil {
		return err
	}
	physicalPeers := 0
	found := false
	for _, family := range runtimes {
		if family.ID != info.ID {
			continue
		}
		for _, branch := range family.Branches {
			if branch.ID == branchID {
				found = true
			} else {
				physicalPeers++
			}
		}
		break
	}
	if !found {
		// Runtime deletion and backing reclamation are separate crash
		// boundaries. A prior attempt may already have removed the Sentry but
		// failed while publishing or releasing immutable roots. Continue from
		// the durable branch record instead of treating runtime absence as full
		// cleanup; otherwise one transient shared-storage failure leaks the
		// entire source graph permanently.
		return b.removeMigratedSourceMetadataWithPolicy(info, branchID, publishBacking)
	}
	if physicalPeers == 0 {
		if err := b.purgeRuntime(info.ID); err != nil {
			return err
		}
		if info.DistributedFamilyID != "" && info.DistributedForkTransactionID != "" {
			// Removing the transaction wrapper alone reclaims its directories but
			// does not release graph/reference authority. Do that while the exact
			// branch metadata is still available, then let the bundle store drop
			// the now-empty wrapper and image lease.
			if err := b.reclaimMigratedSourceBackingWithPolicy(info, branchID, publishBacking); err != nil {
				return err
			}
			return b.containerStore().RemoveDistributedForkTarget(info.DistributedForkTransactionID)
		}
		return b.removeMigratedSourceMetadataWithPolicy(info, branchID, publishBacking)
	}
	if _, err := b.command("timetravel", info.ID, "destroy", branchID); err != nil {
		families, listErr := b.listRuntimeFamilies()
		if listErr != nil {
			return err
		}
		for _, family := range families {
			if family.ID != info.ID {
				continue
			}
			for _, branch := range family.Branches {
				if branch.ID == branchID {
					return err
				}
			}
		}
		// An idempotent retry may observe that the branch is already absent.
		return b.removeMigratedSourceMetadataWithPolicy(info, branchID, publishBacking)
	}
	return b.removeMigratedSourceMetadataWithPolicy(info, branchID, publishBacking)
}

// removeUnrecordedMigratedSourceRuntime closes the crash/race boundary where
// durable physical metadata has already been reclaimed but runsc still lists
// the old source branch. The transaction supplies the exact physical identity,
// so this never searches by a user-visible name. If the Sentry still hosts
// other branches, remove only this source; otherwise tear down the complete
// sandbox and its disposable local directory.
func (b *runtimeBackend) removeUnrecordedMigratedSourceRuntime(containerID, branchID string, info *ttcbundle.ContainerInfo) error {
	// If no other durable branch requires this physical Sentry, do not ask its
	// control server to describe itself before deleting it. A transiently
	// unavailable control server used to make listRuntimeFamilies synthesize a
	// "main" branch; the requested private branch then appeared absent and a
	// clean retirement leaked the complete sandbox. The transaction has already
	// reached independence, so metadata absence (or metadata containing only
	// process-free/remote branches) makes this exact physical runtime disposable.
	if info == nil || !persistentFamilyRequiredRuntime(*info, b.nodeID) {
		sandboxes, err := b.listRuntimeSandboxes()
		if err != nil {
			return fmt.Errorf("listing unrecorded migrated source %s@%s: %w", containerID, branchID, err)
		}
		for _, sandbox := range sandboxes {
			if sandbox.ID != containerID {
				continue
			}
			if err := b.removeOrphanRuntime(containerID); err != nil {
				return fmt.Errorf("removing unrecorded migrated source %s@%s: %w", containerID, branchID, err)
			}
			return nil
		}
		return os.RemoveAll(filepath.Join(b.activeDataRoot(), "containers", containerID))
	}

	// A physical Sentry may still host a different locally-active branch. In
	// that case branch-aware destruction is mandatory, and an unavailable
	// branch inventory is a retirement failure rather than evidence that the
	// requested source has disappeared.
	runtimes, err := b.listRuntimeFamilies()
	if err != nil {
		return err
	}
	for _, family := range runtimes {
		if family.ID != containerID {
			continue
		}
		found := false
		peers := 0
		for _, branch := range family.Branches {
			if branch.ID == branchID {
				found = true
			} else {
				peers++
			}
		}
		if !found {
			if family.Error != "" {
				return fmt.Errorf("inspecting unrecorded migrated source %s@%s: %s", containerID, branchID, family.Error)
			}
			return nil
		}
		if peers != 0 {
			if _, err := b.command("timetravel", containerID, "destroy", branchID); err != nil {
				return fmt.Errorf("destroying unrecorded migrated source %s@%s: %w", containerID, branchID, err)
			}
			return nil
		}
		if err := b.removeOrphanRuntime(containerID); err != nil {
			return fmt.Errorf("removing unrecorded migrated source %s@%s: %w", containerID, branchID, err)
		}
		return nil
	}
	// A previous attempt may have removed runsc state but lost its response.
	// The remaining active-tier directory is non-authoritative and safe to
	// discard once no physical sandbox bears this exact ID.
	return os.RemoveAll(filepath.Join(b.activeDataRoot(), "containers", containerID))
}

// removeMigratedSourceMetadata drops the retired physical incarnation after
// its sparse consumers have become independent. Logical identity and current
// placement live in Raft; retaining this record would leak one branch, one
// filesystem pointer, and its console artifacts per migration and would make a
// later return to this Sentry collide with an obsolete name.
func (b *runtimeBackend) removeMigratedSourceMetadata(info ttcbundle.ContainerInfo, branchID string) error {
	return b.removeMigratedSourceMetadataWithPolicy(info, branchID, true)
}

func (b *runtimeBackend) removeMigratedSourceMetadataWithPolicy(info ttcbundle.ContainerInfo, branchID string, publishBacking bool) error {
	if err := b.reclaimMigratedSourceBackingWithPolicy(info, branchID, publishBacking); err != nil {
		return err
	}
	if info.DistributedFamilyID != "" {
		return b.containerStore().RemoveBranchRecord(info.ID, branchID)
	}
	// The logical catalog record remains, but must not keep pointing at graph
	// roots owned by the retired physical incarnation. The current placement's
	// physical branch carries the live roots used by all subsequent commands.
	return b.containerStore().UpdateBranchSnapshotGraph(info.ID, branchID, 0, nil, nil, "", "", "")
}

func (b *runtimeBackend) reclaimMigratedSourceBacking(info ttcbundle.ContainerInfo, branchID string) error {
	return b.reclaimMigratedSourceBackingWithPolicy(info, branchID, true)
}

func (b *runtimeBackend) reclaimMigratedSourceBackingWithPolicy(info ttcbundle.ContainerInfo, branchID string, publishBacking bool) error {
	var branch ttcbundle.BranchInfo
	found := false
	for _, candidate := range info.Branches {
		if candidate.ID == branchID {
			branch, found = candidate, true
			break
		}
	}
	if !found {
		return nil
	}
	// A live page-stream migration makes every byte resident on the target, but
	// mapped PMAs may continue to name their immutable graph backing. Move any
	// locally authoritative origins to shared storage before releasing this
	// source branch so later dehydration can preserve those edges without the
	// retired node remaining a provider.
	if publishBacking {
		if err := b.publishMigratedMemoryOrigins(branch); err != nil {
			return fmt.Errorf("publishing migrated memory backing: %w", err)
		}
	}
	if err := b.reclaimBranchBacking(info, branch); err != nil {
		return fmt.Errorf("reclaiming migrated branch backing: %w", err)
	}
	if err := b.containerStore().RemoveBranchArtifacts(info.ID, branch); err != nil {
		return fmt.Errorf("removing migrated branch artifacts: %w", err)
	}
	return nil
}

func (b *runtimeBackend) cleanupNonOwnedRuntimeBranches() error {
	if b.nodeID == "" || b.dataRoot == "" {
		return nil
	}
	runtimes, err := b.listRuntimeFamilies()
	if err != nil {
		return err
	}
	var result error
	for _, runtimeFamily := range runtimes {
		// listRuntimeFamilies returns exact physical sandbox IDs, including
		// hidden distributed targets. Resolve that internal identity so cleanup
		// can distinguish a protected sparse provider from a true orphan.
		info, err := b.containerStore().LoadRuntimeContainer(runtimeFamily.ID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, b.purgeRuntime(runtimeFamily.ID))
			}
			continue
		}
		physicalLocalOwned := false
		protectedMigration := false
		for _, branch := range runtimeFamily.Branches {
			if info.BranchNodeID(branch.ID) == b.nodeID {
				physicalLocalOwned = true
				break
			}
			// A migration target is deliberately hidden until placement cutover,
			// so its persistent branch still names the source node while a target
			// Kernel with the same visible branch ID is already resident here.
			// Treat the pending import as local ownership for cleanup purposes.
			// Otherwise a reconciliation request in this window SIGKILLs the
			// provisional target and leaves successfully published metadata
			// pointing at an exited Kernel.
			if pendingDistributedTarget(info, branch.ID, b.nodeID) {
				protectedMigration = true
			}
			if b.activeSparseMigrationSource(info.ID, branch.ID) {
				protectedMigration = true
			}
		}
		if !physicalLocalOwned && !protectedMigration {
			result = errors.Join(result, b.purgeRuntime(info.ID))
			continue
		}
		for _, runtimeBranch := range runtimeFamily.Branches {
			if info.BranchNodeID(runtimeBranch.ID) == b.nodeID {
				continue
			}
			if pendingDistributedTarget(info, runtimeBranch.ID, b.nodeID) {
				continue
			}
			if b.activeSparseMigrationSource(info.ID, runtimeBranch.ID) {
				continue
			}
			if _, err := b.command("timetravel", info.ID, "destroy", runtimeBranch.ID); err != nil {
				result = errors.Join(result, fmt.Errorf("purging non-owned runtime branch %s@%s: %w", info.Name, runtimeBranch.ID, err))
			}
		}
	}
	return result
}

// pendingDistributedTarget reports whether branchID is a transaction-fenced
// physical target on nodeID. The logical branch is absent (fork) or remains
// source-owned (migration) until publication, so BranchNodeID alone cannot
// identify it.
func pendingDistributedTarget(info ttcbundle.ContainerInfo, branchID, nodeID string) bool {
	if info.DistributedForkTransactionID != "" && info.DistributedForkNodeID == nodeID {
		for _, branch := range info.Branches {
			if branch.ID == branchID {
				return true
			}
		}
	}
	for _, pending := range info.DistributedForkImports {
		physicalBranch := pending.PhysicalBranchID
		if physicalBranch == "" {
			physicalBranch = pending.BranchID
		}
		if physicalBranch == branchID && pending.TargetNode == nodeID {
			return true
		}
	}
	for _, branch := range info.Branches {
		if branch.ID == branchID && branch.NodeID == nodeID && branch.DistributedForkTransactionID != "" {
			return true
		}
	}
	return false
}

func localFamilyStateNeeded(info ttcbundle.ContainerInfo, nodeID string) bool {
	if nodeID == "" {
		return true
	}
	for _, branch := range info.Branches {
		if info.BranchNodeID(branch.ID) == nodeID || pendingDistributedTarget(info, branch.ID, nodeID) {
			return true
		}
	}
	for _, pending := range info.DistributedForkImports {
		if pending.TargetNode == nodeID {
			return true
		}
	}
	return false
}

// syncLogicalBranchOwner updates the bundle's denormalized owner from Raft.
// It is deliberately idempotent and tolerates families whose public branch is
// represented only by a transaction-scoped physical runtime.
func (b *runtimeBackend) syncLogicalBranchOwner(familyID, branchID, owner string) error {
	if b == nil || familyID == "" || branchID == "" || owner == "" {
		return nil
	}
	store := b.containerStore()
	info, err := store.LoadContainer(familyID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	branchFound := false
	for _, branch := range info.Branches {
		if branch.ID == branchID {
			branchFound = true
			break
		}
	}
	if !branchFound {
		return nil
	}
	current := info.BranchNodeID(branchID)
	if current == owner {
		return nil
	}
	if err := store.CompareAndSwapBranchNodeID(info.ID, branchID, current, owner); err == nil {
		return nil
	}
	// A concurrent cutover/reconciler may have won the CAS. Re-read before
	// surfacing an error so the common idempotent race stays silent.
	latest, loadErr := store.LoadContainer(info.ID)
	if loadErr == nil && latest.BranchNodeID(branchID) == owner {
		return nil
	}
	if loadErr != nil {
		return loadErr
	}
	return fmt.Errorf("branch owner is %q, want %q", latest.BranchNodeID(branchID), owner)
}

// removeLocalFamilyStateIfUnowned removes a cluster node's disposable active
// tier after its final branch or provisional import has gone away. Shared
// metadata remains authoritative. Refuse cleanup while a Sentry for the
// family still exists, since that would turn a lifecycle ordering bug into
// live backing-file loss.
func (b *runtimeBackend) removeLocalFamilyStateIfUnowned(containerID string) error {
	if b.localRoot == "" || b.nodeID == "" || containerID == "" {
		return nil
	}
	info, err := b.containerStore().LoadContainer(containerID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && localFamilyStateNeeded(info, b.nodeID) {
		return nil
	}
	runtimes, err := b.listRuntimeFamilies()
	if err != nil {
		return fmt.Errorf("checking runtime ownership: %w", err)
	}
	for _, family := range runtimes {
		if family.ID == containerID {
			return fmt.Errorf("runtime family %q still exists", containerID)
		}
	}
	return os.RemoveAll(filepath.Join(b.localRoot, "containers", containerID))
}

// reclaimUnownedLocalFamilyState completes an evacuation's local-tier fence.
// Moving Raft placement is not enough: the source may still have a paused
// Sentry serving sparse state. First remove every physical branch whose source
// dependency has reached the independent transaction state, then discard the
// family-local filesystem and console cache only after no runtime remains.
//
// This intentionally operates on the bounded set of cluster families, not a
// recursive filesystem discovery. Unknown entries in the local containers
// directory remain visible for the ordinary quarantine/recovery path instead
// of being silently deleted as though they were TTC family IDs.
func (b *runtimeBackend) reclaimUnownedLocalFamilyState() error {
	if b.localRoot == "" || b.nodeID == "" {
		return nil
	}
	if err := b.cleanupNonOwnedRuntimeBranches(); err != nil {
		return err
	}
	containers, err := b.containerStore().ListContainersForGC()
	if err != nil {
		return err
	}
	var result error
	for _, info := range containers {
		if localFamilyStateNeeded(info, b.nodeID) {
			continue
		}
		if err := b.removeLocalFamilyStateIfUnowned(info.ID); err != nil {
			result = errors.Join(result, fmt.Errorf("reclaiming node-local family %q: %w", info.ID, err))
		}
	}
	return result
}

// provisionalRaftTargetAuthorized validates the Raft execution grant for a
// physical target that does not have a public placement yet. Container
// metadata identifies the transaction that created the local Kernel; Raft
// supplies the current, incarnation-fenced authority. Both bindings are
// required so stale shared metadata cannot extend a canceled transaction or
// authorize a different branch.
func provisionalRaftTargetAuthorized(info ttcbundle.ContainerInfo, branchID, nodeID, incarnation string, load func(string) (*cluster.Transaction, error)) bool {
	if load == nil || branchID == "" || nodeID == "" || incarnation == "" {
		return false
	}
	var transactionIDs []string
	if info.DistributedForkTransactionID != "" && info.DistributedForkNodeID == nodeID {
		for _, branch := range info.Branches {
			if branch.ID == branchID {
				transactionIDs = append(transactionIDs, info.DistributedForkTransactionID)
				break
			}
		}
	}
	for transactionID, pending := range info.DistributedForkImports {
		if pending.BranchID == branchID && pending.TargetNode == nodeID {
			transactionIDs = append(transactionIDs, transactionID)
		}
	}
	for _, branch := range info.Branches {
		if branch.ID == branchID && branch.NodeID == nodeID && branch.DistributedForkTransactionID != "" {
			transactionIDs = append(transactionIDs, branch.DistributedForkTransactionID)
		}
	}
	for _, transactionID := range transactionIDs {
		tx, err := load(transactionID)
		if err != nil || tx == nil || tx.Identity.TransactionID != transactionID ||
			tx.Identity.TargetNodeID != nodeID || tx.TargetIncarnation != incarnation ||
			(tx.Identity.VisibleTargetBranch() != branchID && tx.Identity.TargetBranch != branchID) ||
			(tx.Identity.TargetContainer != info.ID && tx.Identity.TargetContainer != info.Name) {
			continue
		}
		switch tx.State {
		case cluster.TransactionTargetReserved, cluster.TransactionSourceCaptured,
			cluster.TransactionTargetRestoring, cluster.TransactionTargetPaused:
			return true
		case cluster.TransactionPublished, cluster.TransactionDraining, cluster.TransactionIndependent:
			// A migration target becomes transaction-published before the source
			// can atomically cut placement over to it. Keep that physically
			// paused target authorized until activation records completion. Once
			// activated, the ordinary Raft placement is the only execution grant.
			return !tx.Activated
		}
	}
	return false
}

func (b *runtimeBackend) activeSparseMigrationSource(containerID, branchID string) bool {
	if b.dataRoot == "" || b.nodeID == "" {
		return false
	}
	// A distributed family may use a transaction-scoped physical container on
	// this node. Transactions and placements name the stable logical family,
	// while Sentry inventory and cleanup name that physical container. Normalize
	// before testing the source lease; otherwise cleanup can mistake a still-
	// authoritative sparse provider for an unrelated runtime.
	logicalFamily := containerID
	if info, err := b.containerStore().LoadRuntimeContainer(containerID); err == nil && info.DistributedFamilyID != "" {
		logicalFamily = info.DistributedFamilyID
	}
	transactions, err := b.transactionCatalog()
	if err != nil {
		return false
	}
	records, err := transactions.List()
	if err != nil {
		return false
	}
	for _, tx := range records {
		sourcePhysicalBranch := tx.SourcePhysicalBranch
		if sourcePhysicalBranch == "" {
			sourcePhysicalBranch = tx.Identity.SourceBranch
		}
		if !tx.Identity.Migration || tx.Identity.SourceNodeID != b.nodeID || tx.Identity.SourceContainer != logicalFamily ||
			(tx.SourcePhysicalContainer != "" && tx.SourcePhysicalContainer != containerID) || sourcePhysicalBranch != branchID ||
			tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed {
			continue
		}
		// SourceNeeded is the Raft-committed lifetime fence for the paused source
		// provider. Do not query the Sentry from the lease-renewal path: its
		// control server may be occupied by the migration itself, and blocking a
		// short lease here would self-fence the node that owns the only remaining
		// volatile data. A stale transaction is deliberately conservative; its
		// recovery path must resolve source authority before the runtime is reaped.
		if tx.SourceNeeded {
			return true
		}
	}
	return false
}

// recoverInterruptedMigrations reconciles durable migration intent after the
// daemon that owned the cutover disappeared. It only acts after the exact
// owner incarnation recorded by the transaction is no longer leased. The
// bundle's BranchNodeID is the cutover fact: source ownership means rollback;
// target ownership means roll forward.
func (b *runtimeBackend) recoverInterruptedMigrations() ([]selector, error) {
	if b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		return nil, nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return nil, err
	}
	records, err := placements.List()
	if err != nil {
		return nil, err
	}
	transactions, err := b.transactionCatalog()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var recovered []selector
	var result error
	for _, placement := range records {
		if placement.State != cluster.PlacementMigrating {
			continue
		}
		oldOwner, lookupErr := b.distributedSource.directory.Lookup(placement.OwnerNode)
		if lookupErr != nil && !errors.Is(lookupErr, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("checking migration owner %s for %s@%s: %w", placement.OwnerNode, placement.FamilyID, placement.BranchID, lookupErr))
			continue
		}
		oldOwnerActive := lookupErr == nil && oldOwner.Incarnation == placement.OwnerIncarnation && oldOwner.LeaseActive(now)
		// A healthy source owns the in-flight operation. Other nodes must not
		// interpret its transaction-scoped physical branch as interrupted state.
		if oldOwnerActive && b.nodeID != placement.OwnerNode {
			continue
		}
		logicalInfo, loadErr := b.containerStore().LoadContainer(placement.FamilyID)
		if loadErr != nil {
			result = errors.Join(result, fmt.Errorf("loading interrupted migration %s@%s: %w", placement.FamilyID, placement.BranchID, loadErr))
			continue
		}
		info := logicalInfo
		physicalBranch := placement.BranchID
		branch, branchErr := persistentBranchInfo(info, physicalBranch)
		if branchErr != nil && placement.PhysicalContainer != "" && placement.PhysicalBranch != "" {
			// A remote fork followed by migration has no logical branch record in
			// the canonical container. Raft points at the exact transaction-scoped
			// runtime; recovery must inspect that physical branch instead of
			// treating the intentionally private identity as missing state.
			physical, physicalErr := b.containerStore().LoadRuntimeContainer(placement.PhysicalContainer)
			if physicalErr == nil {
				info = physical
				physicalBranch = placement.PhysicalBranch
				branch, branchErr = persistentBranchInfo(info, physicalBranch)
			}
		}
		if branchErr != nil {
			result = errors.Join(result, fmt.Errorf("loading interrupted migration branch %s@%s: %w", placement.FamilyID, placement.BranchID, branchErr))
			continue
		}
		name := logicalInfo.Name
		if name == "" {
			name = logicalInfo.ID
		}
		target := selector{container: name, branch: placement.BranchID}
		actualOwner := info.BranchNodeID(physicalBranch)
		switch actualOwner {
		case placement.OwnerNode:
			// Only the source daemon may restore and roll back a source-owned
			// transaction. The daemon serializes this reconciler with foreground
			// lifecycle mutations, so the current source incarnation may also
			// finish an abort whose target-cancel RPC timed out. Previously that
			// case remained fenced forever because recovery waited for its own
			// healthy lease to expire.
			if b.nodeID != placement.OwnerNode {
				continue
			}
			current, lookupErr := b.distributedSource.directory.Lookup(b.nodeID)
			if lookupErr != nil || !current.LeaseActive(time.Now()) {
				result = errors.Join(result, fmt.Errorf("source node %q cannot recover %s: lease unavailable", b.nodeID, target))
				continue
			}
			sameIncarnation := oldOwnerActive && current.Incarnation == placement.OwnerIncarnation
			tx, txErr := transactions.Load(placement.OperationID)
			if txErr != nil {
				result = errors.Join(result, fmt.Errorf("loading interrupted migration transaction for %s: %w", target, txErr))
				continue
			}
			if !tx.Identity.Migration {
				result = errors.Join(result, fmt.Errorf("transaction %q for %s is not a migration", placement.OperationID, target))
				continue
			}
			peer, peerErr := b.distributedSource.peer(placement.TargetNode, false)
			if peerErr == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if status, statusErr := peer.Status(ctx, tx.Identity.TransactionID); statusErr == nil {
					if status.Activated {
						cancel()
						// Activation is ordered after placement cutover. Let the shared
						// owner update become visible before deciding whether to roll
						// forward; canceling an activated target could remove the owner.
						continue
					}
					// A migration Publish deliberately leaves the target paused and
					// hidden. It remains safe to cancel even when the publish response
					// was lost, provided placement never cut over and Activate did not
					// run.
					if _, cancelErr := peer.Cancel(ctx, tx.Identity.TransactionID, status.Revision); cancelErr != nil {
						cancel()
						result = errors.Join(result, fmt.Errorf("canceling interrupted migration target %s: %w", target, cancelErr))
						continue
					}
				} else {
					cancel()
					result = errors.Join(result, fmt.Errorf("checking interrupted migration target %s: %w", target, statusErr))
					continue
				}
				cancel()
			} else {
				result = errors.Join(result, fmt.Errorf("connecting to interrupted migration target for %s: %w", target, peerErr))
				continue
			}
			_, _ = b.command("timetravel", info.ID, "distributed-cancel", tx.Identity.TransactionID)
			runtimeState, runtimeErr := b.runtimeBranchLifecycle(info.ID, physicalBranch)
			if placement.ResumeState == cluster.PlacementRunning && runtimeErr == nil && runtimeState == "paused" {
				if _, err := b.command("timetravel", info.ID, "resume", physicalBranch); err != nil {
					result = errors.Join(result, fmt.Errorf("resuming interrupted sparse migration source %s: %w", target, err))
					continue
				}
			}
			if refreshed, refreshErr := b.containerStore().LoadContainer(info.ID); refreshErr == nil {
				info = refreshed
			}
			sessions, sessionsErr := b.execSessions(target)
			if sessionsErr == nil {
				if sidecarErr := b.restoreMigrationBranchSidecars(target, info, sessions); sidecarErr != nil {
					result = errors.Join(result, fmt.Errorf("restoring sparse migration source sidecars %s: %w", target, sidecarErr))
					continue
				}
			}
			state := normalizeLifecycleState(branch.State)
			runtimeState, runtimeErr = b.runtimeBranchLifecycle(info.ID, placement.BranchID)
			if state == "dehydrated" || runtimeErr == nil && runtimeState == "dehydrated" {
				if err := b.rehydrate(target); err != nil {
					result = errors.Join(result, fmt.Errorf("restoring interrupted migration source %s: %w", target, err))
					continue
				}
				info, loadErr = b.containerStore().LoadContainer(info.ID)
				if loadErr != nil {
					result = errors.Join(result, fmt.Errorf("reloading restored migration source %s: %w", target, loadErr))
					continue
				}
				branch, branchErr = persistentBranchInfo(info, physicalBranch)
				if branchErr != nil {
					result = errors.Join(result, branchErr)
					continue
				}
				state = normalizeLifecycleState(branch.State)
			}
			runtimeState, runtimeErr = b.runtimeBranchLifecycle(info.ID, physicalBranch)
			if state != string(placement.ResumeState) || runtimeErr != nil || runtimeState != string(placement.ResumeState) {
				result = errors.Join(result, fmt.Errorf("restored migration source %s has persistent state %q and runtime state %q, want %q: %v", target, state, runtimeState, placement.ResumeState, runtimeErr))
				continue
			}
			var recoveryErr error
			if sameIncarnation {
				_, recoveryErr = placements.AbortMigration(placement.FamilyID, placement.BranchID, placement.OperationID, time.Now())
			} else {
				_, recoveryErr = placements.RecoverSourceMigration(placement.FamilyID, placement.BranchID, placement.OperationID,
					placement.OwnerNode, placement.OwnerIncarnation, current.Incarnation, placement.Generation, time.Now())
			}
			if recoveryErr != nil {
				result = errors.Join(result, fmt.Errorf("publishing recovered migration source %s: %w", target, recoveryErr))
				continue
			}
			recovered = append(recovered, target)
		case placement.TargetNode:
			// After the ownership CAS, only the target daemon rolls forward.
			// Persistent target metadata is installed immediately before the
			// target resumes, but placement ownership is not cut over until the
			// target publish RPC returns. While the old owner lease is active this
			// can therefore be an ordinary in-flight publish, not recovery work.
			// The source remains authoritative and will either commit or abort it.
			if oldOwnerActive {
				continue
			}
			if b.nodeID != placement.TargetNode {
				continue
			}
			current, lookupErr := b.distributedSource.directory.Lookup(b.nodeID)
			if lookupErr != nil || !current.LeaseActive(time.Now()) {
				result = errors.Join(result, fmt.Errorf("target node %q cannot recover %s: lease unavailable", b.nodeID, target))
				continue
			}
			state := normalizeLifecycleState(branch.State)
			runtimeState, runtimeErr := b.runtimeBranchLifecycle(info.ID, physicalBranch)
			if state == "dehydrated" || runtimeErr == nil && runtimeState == "dehydrated" {
				if err := b.rehydrate(target); err != nil {
					result = errors.Join(result, fmt.Errorf("restoring interrupted migration target %s: %w", target, err))
					continue
				}
				info, loadErr = b.containerStore().LoadContainer(info.ID)
				if loadErr != nil {
					result = errors.Join(result, fmt.Errorf("reloading restored migration target %s: %w", target, loadErr))
					continue
				}
				branch, branchErr = persistentBranchInfo(info, physicalBranch)
				if branchErr != nil {
					result = errors.Join(result, branchErr)
					continue
				}
				state = normalizeLifecycleState(branch.State)
			}
			runtimeState, runtimeErr = b.runtimeBranchLifecycle(info.ID, physicalBranch)
			if state != string(placement.ResumeState) || runtimeErr != nil || runtimeState != string(placement.ResumeState) {
				result = errors.Join(result, fmt.Errorf("restored migration target %s has persistent state %q and runtime state %q, want %q: %v", target, state, runtimeState, placement.ResumeState, runtimeErr))
				continue
			}
			if b.distributedSource.authority == nil {
				result = errors.Join(result, fmt.Errorf("publishing recovered migration target %s: Raft authority is unavailable", target))
				continue
			}
			tx, txErr := b.distributedSource.transactions.Load(placement.OperationID)
			if txErr != nil {
				result = errors.Join(result, fmt.Errorf("loading recovered migration transaction %s: %w", target, txErr))
				continue
			}
			if _, err := b.distributedSource.authority.CutoverMigration(placement.OperationID, tx.Revision,
				placement.FamilyID, placement.BranchID, placement.OperationID, placement.TargetNode,
				current.Incarnation, info.ID, physicalBranch); err != nil {
				result = errors.Join(result, fmt.Errorf("publishing recovered migration target %s: %w", target, err))
				continue
			}
			recovered = append(recovered, target)
		default:
			result = errors.Join(result, fmt.Errorf("interrupted migration %s has branch owner %q, want source %q or target %q", target, actualOwner, placement.OwnerNode, placement.TargetNode))
		}
	}
	return recovered, result
}

func (b *runtimeBackend) clusterNodes() ([]cluster.NodeRecord, error) {
	if b.distributedSource == nil || b.distributedSource.directory == nil {
		return nil, errors.New("elastic node discovery is not configured")
	}
	return b.distributedSource.directory.List()
}

func (b *runtimeBackend) removeNode(nodeID string) error {
	if b.distributedSource == nil || b.distributedSource.authority == nil {
		return errors.New("cluster authority is not configured")
	}
	record, err := b.distributedSource.directory.Lookup(nodeID)
	if err != nil {
		return err
	}
	if record.State != cluster.NodeOffline || record.LeaseActive(time.Now()) {
		return fmt.Errorf("node %q is not offline with an expired execution lease", nodeID)
	}
	_, err = b.distributedSource.authority.RemoveNode(record.NodeID, record.Incarnation, record.Revision)
	return err
}

func nodeLoadLess(left, right cluster.NodeRecord) bool {
	leftBytes := left.MemoryCurrentBytes + left.IncomingReservedBytes
	rightBytes := right.MemoryCurrentBytes + right.IncomingReservedBytes
	if left.MemoryTargetBytes != 0 && right.MemoryTargetBytes != 0 {
		leftHi, leftLo := bits.Mul64(leftBytes, right.MemoryTargetBytes)
		rightHi, rightLo := bits.Mul64(rightBytes, left.MemoryTargetBytes)
		if leftHi != rightHi || leftLo != rightLo {
			return leftHi < rightHi || leftHi == rightHi && leftLo < rightLo
		}
	} else if leftBytes != rightBytes {
		return leftBytes < rightBytes
	}
	return left.NodeID < right.NodeID
}

func selectAdmissionNode(records []cluster.NodeRecord, local cluster.Capabilities, now time.Time) (cluster.NodeRecord, error) {
	var candidates []cluster.NodeRecord
	var incompatibilities error
	for _, record := range records {
		if !record.AcceptsPlacement(now) {
			continue
		}
		if record.MemoryTargetBytes != 0 {
			if record.IncomingReservedBytes >= record.MemoryTargetBytes || record.MemoryCurrentBytes >= record.MemoryTargetBytes-record.IncomingReservedBytes {
				continue
			}
		}
		if record.NodeID != local.NodeID {
			if _, err := cluster.Negotiate(local, record.Capabilities); err != nil {
				incompatibilities = errors.Join(incompatibilities, fmt.Errorf("node %s: %w", record.NodeID, err))
				continue
			}
		}
		candidates = append(candidates, record)
	}
	if len(candidates) == 0 {
		if incompatibilities != nil {
			return cluster.NodeRecord{}, fmt.Errorf("no compatible ready cluster node accepts new work: %w", incompatibilities)
		}
		return cluster.NodeRecord{}, errors.New("no ready cluster node has capacity for new work")
	}
	sort.Slice(candidates, func(i, j int) bool { return nodeLoadLess(candidates[i], candidates[j]) })
	return candidates[0], nil
}

// selectAdmissionNodeExcluding repeats admission after a peer connection was
// refused before any request bytes could be delivered. Exclusion is local to
// this command attempt: Raft membership and leases remain authoritative, while
// an unplaced operation is free to choose another accepting node immediately.
func selectAdmissionNodeExcluding(records []cluster.NodeRecord, local cluster.Capabilities, now time.Time, excluded map[string]struct{}) (cluster.NodeRecord, error) {
	if len(excluded) == 0 {
		return selectAdmissionNode(records, local, now)
	}
	eligible := make([]cluster.NodeRecord, 0, len(records))
	for _, record := range records {
		if _, skip := excluded[record.NodeID]; !skip {
			eligible = append(eligible, record)
		}
	}
	return selectAdmissionNode(eligible, local, now)
}

func (b *runtimeBackend) drainTargets(forced string) ([]cluster.NodeRecord, error) {
	records, err := b.clusterNodes()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	targets := make([]cluster.NodeRecord, 0, len(records))
	for _, record := range records {
		if record.NodeID == b.nodeID || !record.AcceptsPlacement(now) {
			continue
		}
		if forced != "" && record.NodeID != forced {
			continue
		}
		targets = append(targets, record)
	}
	if forced != "" && len(targets) == 0 {
		return nil, fmt.Errorf("cluster target node %q is unavailable or not accepting placement", forced)
	}
	sort.Slice(targets, func(i, j int) bool { return nodeLoadLess(targets[i], targets[j]) })
	if len(targets) == 0 {
		return nil, errors.New("no ready cluster node has capacity for drain; provision a node and retry")
	}
	return targets, nil
}

func (b *runtimeBackend) localPlannerSnapshot() (cluster.PlannerSnapshot, error) {
	snapshot := cluster.PlannerSnapshot{NodeID: b.nodeID}
	placements, err := b.placementCatalog()
	if err != nil {
		return snapshot, err
	}
	records, err := placements.List()
	if err != nil {
		return snapshot, err
	}
	// The shared family bundle is display metadata and can retain an earlier
	// node after migration. Raft placement identifies both the logical branch
	// and the physical Sentry that currently owns its resident pages.
	byPhysical := make(map[string][]cluster.PlacementRecord)
	for _, record := range records {
		if record.OwnerNode != b.nodeID || (record.State != cluster.PlacementRunning && record.State != cluster.PlacementPaused) {
			continue
		}
		physical := record.PhysicalContainer
		if physical == "" {
			physical = record.FamilyID
		}
		byPhysical[physical] = append(byPhysical[physical], record)
	}
	physicalIDs := make([]string, 0, len(byPhysical))
	for physical := range byPhysical {
		physicalIDs = append(physicalIDs, physical)
	}
	sort.Strings(physicalIDs)
	for _, physical := range physicalIDs {
		local := byPhysical[physical]
		sort.Slice(local, func(i, j int) bool { return local[i].BranchID < local[j].BranchID })
		firstPhysicalBranch := local[0].PhysicalBranch
		if firstPhysicalBranch == "" {
			firstPhysicalBranch = local[0].BranchID
		}
		output, err := b.command("timetravel", physical, "stats", firstPhysicalBranch)
		if err != nil {
			return snapshot, fmt.Errorf("accounting resident memory for physical runtime %q: %w", physical, err)
		}
		var resident boot.FamilyResidentMemory
		if err := json.Unmarshal(output, &resident); err != nil {
			return snapshot, fmt.Errorf("decoding resident memory for physical runtime %q: %w", physical, err)
		}
		byID := make(map[string]boot.BranchResidentMemory, len(resident.Branches))
		for _, memory := range resident.Branches {
			byID[memory.ID] = memory
		}
		localIDs := make(map[string]string, len(local))
		for _, record := range local {
			physicalBranch := record.PhysicalBranch
			if physicalBranch == "" {
				physicalBranch = record.BranchID
			}
			memory, ok := byID[physicalBranch]
			if !ok {
				return snapshot, fmt.Errorf("physical runtime %q omitted resident accounting for branch %q", physical, physicalBranch)
			}
			name := record.FamilyName
			if name == "" {
				name = record.FamilyID
			}
			id := selector{container: name, branch: record.BranchID}.String()
			localIDs[physicalBranch] = id
			snapshot.Branches = append(snapshot.Branches, cluster.PlannerBranchSnapshot{ID: id, TotalBytes: memory.TotalBytes})
		}
		for _, run := range resident.VersionRuns {
			id, ok := localIDs[run.Branch]
			if !ok {
				continue
			}
			snapshot.VersionRuns = append(snapshot.VersionRuns, cluster.PlannerVersionRun{
				Branch: id, Allocator: run.Allocator, FirstSequence: run.FirstSequence, Pages: run.Pages,
			})
		}
		for _, chunk := range resident.VersionChunks {
			id, ok := localIDs[chunk.Branch]
			if !ok {
				continue
			}
			snapshot.VersionChunks = append(snapshot.VersionChunks, cluster.PlannerVersionChunk{
				Branch: id, Allocator: chunk.Allocator, Chunk: chunk.Chunk, Bitmap: chunk.Bitmap,
			})
		}
	}
	sort.Slice(snapshot.Branches, func(i, j int) bool { return snapshot.Branches[i].ID < snapshot.Branches[j].ID })
	sort.Slice(snapshot.VersionRuns, func(i, j int) bool {
		left, right := snapshot.VersionRuns[i], snapshot.VersionRuns[j]
		if left.Allocator != right.Allocator {
			return left.Allocator < right.Allocator
		}
		if left.FirstSequence != right.FirstSequence {
			return left.FirstSequence < right.FirstSequence
		}
		return left.Branch < right.Branch
	})
	sort.Slice(snapshot.VersionChunks, func(i, j int) bool {
		left, right := snapshot.VersionChunks[i], snapshot.VersionChunks[j]
		if left.Allocator != right.Allocator {
			return left.Allocator < right.Allocator
		}
		if left.Chunk != right.Chunk {
			return left.Chunk < right.Chunk
		}
		return left.Branch < right.Branch
	})
	return snapshot, nil
}

func (b *runtimeBackend) plannerSnapshot(node string, local bool) (cluster.PlannerSnapshot, error) {
	if node == b.nodeID {
		return b.localPlannerSnapshot()
	}
	if local {
		return cluster.PlannerSnapshot{}, fmt.Errorf("planner resource request reached node %q, want %q", b.nodeID, node)
	}
	if b.distributedSource == nil {
		return cluster.PlannerSnapshot{}, errors.New("planner resource request requires cluster discovery")
	}
	peer, err := b.distributedSource.peer(node, false)
	if err != nil {
		return cluster.PlannerSnapshot{}, err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return cluster.PlannerSnapshot{}, err
	}
	status, output, diagnostic, err := executeRemoteDaemonCommand(context.Background(), peer, requestID, []string{"node", "_planner-resources", "--_local", node})
	if err != nil {
		return cluster.PlannerSnapshot{}, err
	}
	if status != 0 {
		return cluster.PlannerSnapshot{}, errors.New(strings.TrimSpace(diagnostic))
	}
	var snapshot cluster.PlannerSnapshot
	if err := json.Unmarshal([]byte(output), &snapshot); err != nil {
		return snapshot, fmt.Errorf("decoding planner resources from node %q: %w", node, err)
	}
	if snapshot.NodeID != node {
		return snapshot, fmt.Errorf("planner resource response identifies node %q, want %q", snapshot.NodeID, node)
	}
	return snapshot, nil
}

func (b *runtimeBackend) placementWindow(source cluster.NodeRecord, targets []cluster.NodeRecord, mode cluster.PlacementMode) (cluster.FixedTerminalPlan, map[string]cluster.NodeRecord, error) {
	all, err := b.clusterNodes()
	if err != nil {
		return cluster.FixedTerminalPlan{}, nil, err
	}
	records := make(map[string]cluster.NodeRecord, len(all))
	allowed := make([]string, 0, len(targets))
	allowedSet := make(map[string]bool, len(targets))
	for _, target := range targets {
		allowed = append(allowed, target.NodeID)
		allowedSet[target.NodeID] = true
	}
	sort.Strings(allowed)
	now := time.Now()
	byID := make(map[string]cluster.NodeRecord, len(all)+1)
	byID[source.NodeID] = source
	for _, record := range all {
		if record.NodeID == source.NodeID || !record.LeaseActive(now) {
			continue
		}
		byID[record.NodeID] = record
	}
	nodeIDs := make([]string, 0, len(byID))
	for node := range byID {
		nodeIDs = append(nodeIDs, node)
	}
	sort.Strings(nodeIDs)
	nodes := make([]cluster.NodeResource, 0, len(nodeIDs))
	for _, node := range nodeIDs {
		record := byID[node]
		targetBytes := record.MemoryTargetBytes
		if targetBytes == 0 {
			targetBytes = ^uint64(0)
		}
		records[node] = record
		nodes = append(nodes, cluster.NodeResource{
			ID: node, CurrentBytes: record.MemoryCurrentBytes, TargetBytes: targetBytes,
			ReservedBytes: record.IncomingReservedBytes,
		})
	}
	for target := range allowedSet {
		if _, ok := records[target]; !ok {
			return cluster.FixedTerminalPlan{}, records, fmt.Errorf("placement target %q lost its active lease", target)
		}
	}
	snapshots := make([]cluster.PlannerSnapshot, 0, len(nodes))
	for _, node := range nodes {
		snapshot, err := b.plannerSnapshot(node.ID, node.ID == b.nodeID)
		if err != nil {
			return cluster.FixedTerminalPlan{}, records, fmt.Errorf("collecting planner resources from node %q: %w", node.ID, err)
		}
		snapshots = append(snapshots, snapshot)
	}
	problem, err := cluster.BuildFixedTerminalProblem(mode, source.NodeID, nodes, snapshots, allowed)
	if err != nil {
		return cluster.FixedTerminalPlan{}, records, err
	}
	policy, err := cluster.LoadPlacementPolicy(filepath.Join(b.dataRoot, "cluster", "placement-policy.json"))
	if err != nil {
		return cluster.FixedTerminalPlan{}, records, fmt.Errorf("loading placement policy: %w", err)
	}
	if err := cluster.ApplyPlacementPolicy(&problem, snapshots, policy); err != nil {
		return cluster.FixedTerminalPlan{}, records, fmt.Errorf("applying placement policy: %w", err)
	}
	plan, err := cluster.PlanFixedTerminals(problem)
	return plan, records, err
}

func (b *runtimeBackend) executePlacementPlan(plan cluster.FixedTerminalPlan, records map[string]cluster.NodeRecord) error {
	if plan.SourceNode == "" || plan.SourceNode != b.nodeID {
		return fmt.Errorf("placement plan for source node %q reached executor on %q", plan.SourceNode, b.nodeID)
	}
	type reservation struct {
		node        string
		incarnation string
		operation   string
		moved       int
	}
	var reservations []reservation
	for node, after := range plan.NodeAfterBytes {
		if node == plan.SourceNode {
			continue
		}
		record, ok := records[node]
		if !ok {
			return fmt.Errorf("placement plan references unknown node %q", node)
		}
		baseline := record.MemoryCurrentBytes + record.IncomingReservedBytes
		if baseline < record.MemoryCurrentBytes {
			baseline = ^uint64(0)
		}
		if after <= baseline {
			continue
		}
		// A Raft election invalidates every previous-term capacity grant. Ask
		// the target daemon to renew its own authority synchronously rather than
		// racing its periodic heartbeat or fabricating a heartbeat on its behalf.
		peer, err := b.distributedSource.peer(node, true)
		if err != nil {
			return fmt.Errorf("connecting to placement target %q before capacity reservation: %w", node, err)
		}
		renewCtx, cancelRenew := context.WithTimeout(context.Background(), 5*time.Second)
		err = peer.RenewExecutionAuthority(renewCtx)
		cancelRenew()
		if err != nil {
			for _, held := range reservations {
				_, _ = b.distributedSource.directory.ReleaseIncoming(held.node, held.incarnation, held.operation, time.Now())
			}
			return fmt.Errorf("renewing placement target %q before capacity reservation: %w", node, err)
		}
		operation, err := newDaemonRequestID()
		if err != nil {
			return err
		}
		bytes := after - baseline
		if _, err := b.distributedSource.directory.ReserveIncoming(node, record.Incarnation, operation, b.nodeID, bytes, time.Now(), 10*time.Minute); err != nil {
			for _, held := range reservations {
				_, _ = b.distributedSource.directory.ReleaseIncoming(held.node, held.incarnation, held.operation, time.Now())
			}
			return fmt.Errorf("reserving %d bytes on placement target %q: %w", bytes, node, err)
		}
		reservations = append(reservations, reservation{node: node, incarnation: record.Incarnation, operation: operation})
	}
	byNode := make(map[string]*reservation, len(reservations))
	for index := range reservations {
		byNode[reservations[index].node] = &reservations[index]
	}
	// A plan contains independent branch cutovers, but migrations of sibling
	// branches to the same node are not independent while the destination
	// physical Sentry is being selected. The first branch establishes that
	// Sentry's placement; a concurrently reserved sibling would not observe the
	// placement yet and would create a second Sentry for the same family. Keep
	// each (logical family, destination node) group ordered while executing
	// unrelated groups in parallel. Each migration still has its own Raft
	// transaction and placement CAS; failures leave the node cordoned and
	// retryable, while successful siblings remain valid completed moves.
	const migrationConcurrency = 4
	moveErrors := make([]error, len(plan.Moves))
	moved := make([]bool, len(plan.Moves))
	type migrationGroup struct {
		indices []int
	}
	groupsByKey := make(map[string]*migrationGroup)
	var groups []*migrationGroup
	for i, move := range plan.Moves {
		target, err := parseSelector(move.Branch)
		if err != nil {
			moveErrors[i] = err
			continue
		}
		key := move.ToNode + "\x00" + target.container
		group := groupsByKey[key]
		if group == nil {
			group = &migrationGroup{}
			groupsByKey[key] = group
			groups = append(groups, group)
		}
		group.indices = append(group.indices, i)
	}
	runBoundedBatch(len(groups), migrationConcurrency, func(groupIndex int) {
		for _, i := range groups[groupIndex].indices {
			move := plan.Moves[i]
			target, err := parseSelector(move.Branch)
			if err != nil {
				moveErrors[i] = err
				continue
			}
			if err := b.migrate(target, move.ToNode); err != nil {
				moveErrors[i] = fmt.Errorf("executing placement move %s to %s: %w", target, move.ToNode, err)
				continue
			}
			moved[i] = true
		}
	})
	var executionErr error
	for i, moveErr := range moveErrors {
		executionErr = errors.Join(executionErr, moveErr)
		if !moved[i] {
			continue
		}
		if reservation := byNode[plan.Moves[i].ToNode]; reservation != nil {
			reservation.moved++
		}
	}
	for _, held := range reservations {
		var err error
		if held.moved == 0 {
			_, err = b.distributedSource.directory.ReleaseIncoming(held.node, held.incarnation, held.operation, time.Now())
		} else {
			_, err = b.distributedSource.directory.CompleteIncoming(held.node, held.incarnation, held.operation, time.Now())
		}
		if err != nil {
			executionErr = errors.Join(executionErr, fmt.Errorf("finalizing placement reservation %s on %s: %w", held.operation, held.node, err))
		}
	}
	return executionErr
}

func placementDestinationCount(moves []cluster.PlacementMove) int {
	nodes := make(map[string]struct{})
	for _, move := range moves {
		nodes[move.ToNode] = struct{}{}
	}
	return len(nodes)
}

func (b *runtimeBackend) rebalanceNode(sourceNode, targetNode string, local bool) (cluster.FixedTerminalPlan, error) {
	if b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		return cluster.FixedTerminalPlan{}, errors.New("elastic rebalance requires dynamic cluster discovery")
	}
	if sourceNode != b.nodeID {
		if local {
			return cluster.FixedTerminalPlan{}, fmt.Errorf("node rebalance reached %q, want %q", b.nodeID, sourceNode)
		}
		peer, err := b.distributedSource.peer(sourceNode, false)
		if err != nil {
			return cluster.FixedTerminalPlan{}, err
		}
		requestID, err := newDaemonRequestID()
		if err != nil {
			return cluster.FixedTerminalPlan{}, err
		}
		args := []string{"node", "rebalance", "--_local"}
		if targetNode != "" {
			args = append(args, "--node", targetNode)
		}
		args = append(args, sourceNode)
		status, output, diagnostic, err := executeRemoteDaemonCommand(context.Background(), peer, requestID, args)
		if err != nil {
			return cluster.FixedTerminalPlan{}, err
		}
		if status != 0 {
			return cluster.FixedTerminalPlan{}, errors.New(strings.TrimSpace(diagnostic))
		}
		var plan cluster.FixedTerminalPlan
		if err := json.Unmarshal([]byte(output), &plan); err != nil {
			return cluster.FixedTerminalPlan{}, fmt.Errorf("decoding remote placement plan: %w", err)
		}
		return plan, nil
	}
	source, err := b.distributedSource.directory.Lookup(sourceNode)
	if err != nil {
		return cluster.FixedTerminalPlan{}, err
	}
	if !source.LeaseActive(time.Now()) || source.MemoryTargetBytes == 0 {
		return cluster.FixedTerminalPlan{}, fmt.Errorf("source node %q has no active lease or memory target", sourceNode)
	}
	if source.MemoryCurrentBytes <= source.MemoryTargetBytes {
		return cluster.FixedTerminalPlan{SourceNode: sourceNode, NodeAfterBytes: map[string]uint64{sourceNode: source.MemoryCurrentBytes}}, nil
	}
	targets, err := b.drainTargets(targetNode)
	if err != nil {
		request, requestErr := b.emitCapacityRequest(*source, "memory-pressure")
		if requestErr != nil {
			return cluster.FixedTerminalPlan{}, fmt.Errorf("%w; publishing capacity request: %v", err, requestErr)
		}
		return cluster.FixedTerminalPlan{}, fmt.Errorf("%w; capacity request published at %s", err, request)
	}
	plan, records, planErr := b.placementWindow(*source, targets, cluster.PlacementRelievePressure)
	if planErr != nil || len(plan.Moves) == 0 {
		request, requestErr := b.emitCapacityRequest(*source, "memory-pressure")
		if requestErr != nil {
			planErr = errors.Join(planErr, fmt.Errorf("publishing capacity request: %w", requestErr))
		}
		if planErr != nil {
			return plan, fmt.Errorf("no feasible pressure-relief placement; capacity request %s: %w", request, planErr)
		}
		return plan, fmt.Errorf("no movable branch can relieve memory pressure; capacity request published at %s", request)
	}
	if err := b.executePlacementPlan(plan, records); err != nil {
		return plan, err
	}
	return plan, nil
}

// stopNode applies an explicit shutdown policy and requests daemon termination
// only after the node has ceased to own authoritative branch state.
func (b *runtimeBackend) stopNode(sourceNode, mode string, local bool) ([]selector, error) {
	if b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		return nil, errors.New("node stop requires distributed mode")
	}
	if sourceNode != b.nodeID {
		if local {
			return nil, fmt.Errorf("node stop reached %q, want %q", b.nodeID, sourceNode)
		}
		peer, err := b.distributedSource.peer(sourceNode, false)
		if err != nil {
			return nil, err
		}
		requestID, err := newDaemonRequestID()
		if err != nil {
			return nil, err
		}
		args := []string{"node", "stop", "--_local"}
		switch mode {
		case "":
		case "stop":
			args = append(args, "--stop-all")
		case "dehydrate":
			args = append(args, "--dehydrate-all")
		case "migrate":
			args = append(args, "--migrate-all")
		default:
			return nil, fmt.Errorf("unknown node stop mode %q", mode)
		}
		args = append(args, sourceNode)
		status, output, diagnostic, err := executeRemoteDaemonCommand(context.Background(), peer, requestID, args)
		if err != nil {
			return nil, err
		}
		if status != 0 {
			return nil, errors.New(strings.TrimSpace(diagnostic))
		}
		var moved []selector
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			if line == "" {
				continue
			}
			selected, err := parseSelector(line)
			if err != nil {
				return nil, fmt.Errorf("decoding node stop result %q: %w", line, err)
			}
			moved = append(moved, selected)
		}
		return moved, nil
	}
	// Cordon before applying the requested policy. Otherwise another daemon
	// could admit work here while stop-all or dehydrate-all is walking the
	// local inventory.
	source, err := b.distributedSource.directory.Lookup(sourceNode)
	if err != nil {
		return nil, err
	}
	// A leadership change invalidates the old term's execution renewal even
	// though its wall-clock lease may not have expired. Node stop is itself an
	// owner-local, authority-changing operation, so renew synchronously before
	// cordoning instead of racing the periodic heartbeat's next tick.
	lease := source.LeaseDuration
	if lease <= 0 {
		lease = source.LeaseExpiresAt.Sub(source.UpdatedAt)
	}
	current := source.MemoryCurrentBytes
	if measured, measureErr := currentNodeMemoryBytes(); measureErr == nil {
		current = measured
	}
	source, err = b.distributedSource.directory.Heartbeat(source.NodeID, source.Incarnation, current, time.Now(), lease)
	if err != nil {
		return nil, fmt.Errorf("renewing node %q before stop: %w", sourceNode, err)
	}
	if !source.LeaseActive(time.Now()) {
		return nil, fmt.Errorf("source node %q lease is expired or offline", sourceNode)
	}
	if source.State != cluster.NodeReady && source.State != cluster.NodePressured && source.State != cluster.NodeDraining {
		return nil, fmt.Errorf("node %q cannot stop from state %s", sourceNode, source.State)
	}
	cordonedForStop := source.State != cluster.NodeDraining
	previousState := source.State
	if cordonedForStop {
		if _, err := b.distributedSource.directory.Transition(source.NodeID, source.Incarnation, cluster.NodeDraining, time.Now(), lease); err != nil {
			return nil, fmt.Errorf("cordoning node %q: %w", sourceNode, err)
		}
	}
	createdPublished, err := publishCreatedDaemonContainers(b)
	if err != nil {
		return nil, err
	}

	switch mode {
	case "":
		if err := rejectActiveDaemonContainers(b); err != nil {
			if cordonedForStop {
				_, rollbackErr := b.distributedSource.directory.Transition(source.NodeID, source.Incarnation, previousState, time.Now(), lease)
				if rollbackErr != nil {
					return nil, errors.Join(err, fmt.Errorf("restoring node %q after rejected stop: %w", sourceNode, rollbackErr))
				}
			}
			return nil, err
		}
	case "stop":
		if err := stopDaemonContainers(b); err != nil {
			return nil, err
		}
	case "dehydrate":
		if err := dehydrateDaemonContainers(b); err != nil {
			return nil, err
		}
	case "migrate":
	default:
		return nil, fmt.Errorf("unknown node stop mode %q", mode)
	}
	if b.publicationQueue != nil && (mode != "migrate" || createdPublished) {
		if err := b.publicationQueue.Drain(context.Background()); err != nil {
			return nil, fmt.Errorf("waiting for durable branch publication: %w", err)
		}
	}
	moved, err := b.drainNode(sourceNode, "")
	if err != nil {
		return moved, err
	}
	terminalCtx, terminalCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer terminalCancel()
	if err := b.finalizeNodeRetirement(terminalCtx); err != nil {
		return moved, err
	}
	b.daemonStopRequested = true
	return moved, nil
}

// finalizeNodeRetirement is the common terminal barrier for foreground node
// stop and crash-recovered drains. Evacuating visible branches is insufficient:
// immutable origins, publication work, and shared reference ownership must all
// cease naming this node before its scratch storage is disposable. The drain
// intent becomes complete only after every one of these conditions holds.
func (b *runtimeBackend) finalizeNodeRetirement(ctx context.Context) error {
	if b.publicationQueue != nil {
		if err := b.publicationQueue.Drain(ctx); err != nil {
			return fmt.Errorf("waiting for durable branch publication: %w", err)
		}
	}
	if err := b.distributedSource.waitForBackgroundTasks(ctx); err != nil {
		return fmt.Errorf("waiting for distributed source cleanup: %w", err)
	}
	if err := b.publishRemainingVolatileOrigins(); err != nil {
		return fmt.Errorf("publishing retiring node snapshot origins: %w", err)
	}
	last, err := waitForTerminalClusterOwnership(ctx, b.distributedSource.authority, b.distributedSource.directory, b.nodeID)
	if err != nil {
		return fmt.Errorf("waiting for terminal cluster storage ownership: %w", err)
	}
	transactions, err := b.transactionCatalog()
	if err != nil {
		return fmt.Errorf("opening transaction catalog for final storage collection: %w", err)
	}
	if err := collectColdStorage(b.dataRoot, b.activeDataRoot(), b.publicationQueue, transactions); err != nil {
		return fmt.Errorf("collecting terminal shared storage: %w", err)
	}
	if b.sharedReferences != nil {
		if err := b.sharedReferences.drainBeforeShutdown(ctx, last); err != nil {
			return fmt.Errorf("collecting terminal shared snapshot graph: %w", err)
		}
	}
	if last {
		// No maintenance iteration is guaranteed after the final daemon exits.
		// All publishers are fenced here, so the last node can remove newly
		// unreachable cold data without the ordinary recovery grace interval.
		if err := collectColdStorageOlderThan(b.dataRoot, b.activeDataRoot(), b.publicationQueue, transactions, time.Now().Add(time.Hour)); err != nil {
			return fmt.Errorf("collecting final cluster cold storage: %w", err)
		}
	}
	intent, err := loadElasticDrainIntent(b.dataRoot, b.nodeID)
	if err != nil {
		return fmt.Errorf("loading terminal drain intent: %w", err)
	}
	if intent == nil {
		return fmt.Errorf("node %q has no durable drain intent", b.nodeID)
	}
	if intent.State != elasticDrainComplete {
		if err := finishElasticDrainAttempt(b.dataRoot, b.nodeID, intent.OperationID, nil); err != nil {
			return fmt.Errorf("committing terminal drain outcome: %w", err)
		}
	}
	return nil
}

// waitForTerminalClusterOwnership distinguishes a live peer, which can inherit
// ordinary maintenance immediately, from an expired but not-yet-fenced peer.
// The latter still contributes reference claims to the Raft state machine, so
// the final daemon must remain online until leader-safe expiry reconciliation
// commits. Comparing leases uses the leader's replicated observation time,
// never this node's potentially skewed wall clock.
func waitForTerminalClusterOwnership(ctx context.Context, authority *cluster.RaftCatalog, directory cluster.NodeCatalog, nodeID string) (bool, error) {
	if authority == nil {
		return lastOnlineClusterNode(directory, nodeID)
	}
	for {
		state, err := authority.AuthorityState()
		if err != nil {
			return false, err
		}
		observed := state.LeaderObservedAt
		unfenced := false
		for _, record := range state.Nodes {
			if record.NodeID == nodeID || record.State == cluster.NodeOffline {
				continue
			}
			if record.LeaseActive(observed) {
				return false, nil
			}
			unfenced = true
		}
		if !unfenced {
			return true, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func lastOnlineClusterNode(directory cluster.NodeCatalog, nodeID string) (bool, error) {
	if directory == nil || nodeID == "" {
		return false, errors.New("cluster node directory is unavailable")
	}
	records, err := directory.List()
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.NodeID != nodeID && record.State != cluster.NodeOffline {
			return false, nil
		}
	}
	return true, nil
}

// drainNode implements the owner-local, restart-safe evacuation used by node
// stop and crash recovery. It rejects new
// placements first, then migrates active branches. Process-free durable
// branches are already unplaced in Raft and require no node-to-node transfer.
// A failed step leaves the node draining so retry cannot race new work onto it.
func (b *runtimeBackend) drainNode(sourceNode, targetNode string) (moved []selector, retErr error) {
	if b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		return nil, errors.New("elastic node drain requires dynamic cluster discovery")
	}
	if sourceNode != b.nodeID {
		return nil, fmt.Errorf("internal evacuation reached %q, want %q", b.nodeID, sourceNode)
	}

	source, err := b.distributedSource.directory.Lookup(b.nodeID)
	if err != nil {
		return nil, err
	}
	if !source.LeaseActive(time.Now()) {
		return nil, fmt.Errorf("source node %q lease is expired or offline", b.nodeID)
	}
	if source.State != cluster.NodeReady && source.State != cluster.NodePressured && source.State != cluster.NodeDraining {
		return nil, fmt.Errorf("node %q cannot begin drain from state %s", b.nodeID, source.State)
	}
	intent, err := beginElasticDrainIntent(b.dataRoot, b.nodeID, targetNode)
	if err != nil {
		return nil, fmt.Errorf("recording durable drain intent: %w", err)
	}
	defer func() {
		// Success means only that visible branches and live dependencies were
		// evacuated. finalizeNodeRetirement owns the later durability barrier and
		// is the sole operation allowed to mark the drain complete.
		if retErr != nil {
			if err := finishElasticDrainAttempt(b.dataRoot, b.nodeID, intent.OperationID, retErr); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("recording drain outcome: %w", err))
			}
		}
	}()
	lease := source.LeaseExpiresAt.Sub(source.UpdatedAt)
	if source.State != cluster.NodeDraining {
		if _, err := b.distributedSource.directory.Transition(source.NodeID, source.Incarnation, cluster.NodeDraining, time.Now(), lease); err != nil {
			return nil, fmt.Errorf("cordoning node %q: %w", b.nodeID, err)
		}
	}
	// stop-all and dehydrate-all publish their process-free state and release
	// placement ownership before they reach this point. An empty node can stop
	// without requiring a second node merely to accept no work.
	placements, err := b.placementCatalog()
	if err != nil {
		return nil, err
	}
	placementRecords, err := placements.List()
	if err != nil {
		return nil, err
	}
	ownsBranches := false
	for _, record := range placementRecords {
		if record.OwnerNode == b.nodeID {
			ownsBranches = true
			break
		}
	}
	if !ownsBranches {
		transactionStore, err := b.transactionCatalog()
		if err != nil {
			return nil, err
		}
		if err := drainSourceDependencies(b, b.distributedSource, transactionStore, b.nodeID); err != nil {
			return nil, fmt.Errorf("draining remote page dependencies: %w", err)
		}
		if err := b.flushGraphReferenceReleases(2 * lease); err != nil {
			return nil, fmt.Errorf("flushing graph-reference releases: %w", err)
		}
		if err := b.reclaimUnownedLocalFamilyState(); err != nil {
			return nil, fmt.Errorf("reclaiming evacuated node-local families: %w", err)
		}
		return nil, nil
	}
	targets, err := b.drainTargets(targetNode)
	if err != nil {
		request, requestErr := b.emitCapacityRequest(*source, "node-drain")
		if requestErr != nil {
			return nil, fmt.Errorf("%w; publishing capacity request: %v", err, requestErr)
		}
		return nil, fmt.Errorf("%w; capacity request published at %s", err, request)
	}
	plan, records, err := b.placementWindow(*source, targets, cluster.PlacementEvacuate)
	if err != nil {
		request, requestErr := b.emitCapacityRequest(*source, "node-drain")
		if requestErr != nil {
			return nil, fmt.Errorf("planning active branch evacuation: %w; publishing capacity request: %v", err, requestErr)
		}
		return nil, fmt.Errorf("planning active branch evacuation: %w; capacity request published at %s", err, request)
	}
	if err := b.executePlacementPlan(plan, records); err != nil {
		return nil, err
	}
	for _, move := range plan.Moves {
		selected, err := parseSelector(move.Branch)
		if err != nil {
			return moved, err
		}
		moved = append(moved, selected)
	}
	placementRecords, err = placements.List()
	if err != nil {
		return nil, err
	}
	type work struct {
		target selector
		state  string
	}
	var pending []work
	for _, record := range placementRecords {
		if record.OwnerNode == b.nodeID {
			name := record.FamilyName
			if name == "" {
				name = record.FamilyID
			}
			pending = append(pending, work{target: selector{container: name, branch: record.BranchID}, state: string(record.State)})
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].target.String() < pending[j].target.String() })
	moved = append(make([]selector, 0, len(moved)+len(pending)), moved...)
	for _, item := range pending {
		switch item.state {
		case "running", "paused":
			return moved, fmt.Errorf("active branch %s remained on drained node after placement execution", item.target)
		case "materializing", "migrating":
			return moved, fmt.Errorf("branch %s remains transiently %s on drained node", item.target, item.state)
		default:
			return moved, fmt.Errorf("branch %s retains unexpected placement state %s on drained node", item.target, item.state)
		}
	}
	transactionStore, err := b.transactionCatalog()
	if err != nil {
		return moved, err
	}
	if err := drainSourceDependencies(b, b.distributedSource, transactionStore, b.nodeID); err != nil {
		return moved, fmt.Errorf("draining remote page dependencies: %w", err)
	}
	if err := b.flushGraphReferenceReleases(2 * lease); err != nil {
		return moved, fmt.Errorf("flushing graph-reference releases: %w", err)
	}
	if err := b.reclaimUnownedLocalFamilyState(); err != nil {
		return moved, fmt.Errorf("reclaiming evacuated node-local families: %w", err)
	}
	return moved, nil
}

func parseClusterAdvertise(value string) (baseURL, serverName string, err error) {
	if strings.ContainsAny(value, "/?#") {
		return "", "", fmt.Errorf("invalid --cluster-advertise %q; want HOST:PORT", value)
	}
	if _, _, err := net.SplitHostPort(value); err != nil {
		return "", "", fmt.Errorf("invalid --cluster-advertise %q; want HOST:PORT", value)
	}
	baseURL = "http://" + value
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", "", fmt.Errorf("parsing --cluster-advertise %q: %w", value, err)
	}
	if parsed.Port() == "" {
		return "", "", fmt.Errorf("--cluster-advertise URL %q must include a port", baseURL)
	}
	if host := parsed.Hostname(); host == "" || host == "0.0.0.0" || host == "::" {
		return "", "", fmt.Errorf("--cluster-advertise URL %q must name a reachable host", baseURL)
	}
	return baseURL, "", nil
}

func resolveClusterAdvertise(baseURL, listenerAddress string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Port() != "0" {
		return baseURL, nil
	}
	_, port, err := net.SplitHostPort(listenerAddress)
	if err != nil || port == "" || port == "0" {
		return "", fmt.Errorf("resolving advertised cluster port from %q: %w", listenerAddress, err)
	}
	parsed.Host = net.JoinHostPort(parsed.Hostname(), port)
	return parsed.String(), nil
}

func advertisedPageAddress(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid cluster advertise URL %q", baseURL)
	}
	return net.JoinHostPort(parsed.Hostname(), "0"), nil
}

// currentNodeMemoryBytes prefers the containing cgroup because Sentries are
// daemon siblings rather than children whose RSS can be summed reliably.
func currentNodeMemoryBytes() (uint64, error) {
	if cgroup, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(cgroup), "\n") {
			parts := strings.SplitN(line, ":", 3)
			if len(parts) != 3 || parts[0] != "0" || parts[1] != "" {
				continue
			}
			path := "/sys/fs/cgroup/" + strings.TrimPrefix(parts[2], "/")
			if value, err := readUintFile(path + "/memory.current"); err == nil {
				return value, nil
			}
		}
	}
	statm, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(statm))
	if len(fields) < 2 {
		return 0, errors.New("/proc/self/statm has no resident page count")
	}
	residentPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return residentPages * uint64(os.Getpagesize()), nil
}

func readUintFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

type elasticFencedProcess struct {
	PID         int    `json:"pid"`
	StartTime   string `json:"start_time"`
	ContainerID string `json:"container_id,omitempty"`
	// Branches is the complete physical branch set in this Sentry at the
	// fencing instant. The out-of-process guardian cannot obtain it after
	// SIGSTOP, so crash fencing may leave this empty and restart recovery then
	// derives a conservative set from shared container metadata.
	Branches []string `json:"branches,omitempty"`
}

type elasticFenceRecord struct {
	Version     int                    `json:"version"`
	NodeID      string                 `json:"node_id"`
	Incarnation string                 `json:"incarnation"`
	FencedAt    time.Time              `json:"fenced_at"`
	Processes   []elasticFencedProcess `json:"processes"`
}

func elasticFencePath(localRoot string) string {
	return filepath.Join(localRoot, "cluster", "execution-fence.json")
}

func processStartTime(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndex(string(data), ") ")
	if end < 0 {
		return "", errors.New("malformed process stat")
	}
	fields := strings.Fields(string(data)[end+2:])
	if len(fields) <= 19 {
		return "", errors.New("process stat omits start time")
	}
	return fields[19], nil
}

// RunLeaseGuardian is the private ttcd child entry point. It intentionally
// lives outside the daemon process: a SIGKILL, panic, or deadlock in ttcd
// closes/stalls the heartbeat pipe and the guardian still physically stops
// every Sentry using this daemon's runtime root.
func RunLeaseGuardian(args []string, stderr io.Writer) int {
	if len(args) != 5 {
		fmt.Fprintln(stderr, "ttcd lease guardian: invalid invocation")
		return 2
	}
	localRoot, runtimeRoot, nodeID, incarnation := args[0], args[1], args[2], args[3]
	leaseNS, err := strconv.ParseInt(args[4], 10, 64)
	if err != nil || leaseNS <= 0 || localRoot == "" || runtimeRoot == "" || nodeID == "" || incarnation == "" {
		fmt.Fprintln(stderr, "ttcd lease guardian: invalid lease configuration")
		return 2
	}
	heartbeats := os.NewFile(3, "ttcd-lease-heartbeats")
	if heartbeats == nil {
		fmt.Fprintln(stderr, "ttcd lease guardian: heartbeat pipe is unavailable")
		return 2
	}
	defer heartbeats.Close()
	events := make(chan bool, 1)
	go func() {
		var buffer [1]byte
		for {
			if _, err := heartbeats.Read(buffer[:]); err != nil {
				events <- false
				return
			}
			select {
			case events <- true:
			default:
			}
		}
	}()
	timer := time.NewTimer(time.Duration(leaseNS))
	defer timer.Stop()
	for {
		select {
		case alive := <-events:
			if !alive {
				if err := fenceElasticRuntimeRoot(localRoot, runtimeRoot, nodeID, incarnation); err != nil {
					fmt.Fprintf(stderr, "ttcd lease guardian: fencing after daemon exit: %v\n", err)
					return 1
				}
				return 0
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Duration(leaseNS))
		case <-timer.C:
			if err := fenceElasticRuntimeRoot(localRoot, runtimeRoot, nodeID, incarnation); err != nil {
				fmt.Fprintf(stderr, "ttcd lease guardian: fencing after heartbeat timeout: %v\n", err)
				return 1
			}
			return 0
		}
	}
}

func startLeaseGuardian(localRoot, runtimeRoot, nodeID, incarnation string, lease time.Duration) (*os.File, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, "_lease-guardian", localRoot, runtimeRoot, nodeID, incarnation, strconv.FormatInt(int64(lease), 10))
	command.ExtraFiles = []*os.File{reader}
	command.Stdin, command.Stdout, command.Stderr = nil, nil, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}
	reader.Close()
	_ = command.Process.Release()
	if _, err := writer.Write([]byte{1}); err != nil {
		writer.Close()
		return nil, err
	}
	return writer, nil
}

func sentryProcessesForRuntimeRoot(runtimeRoot string) ([]elasticFencedProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	rootArgument := "--root=" + runtimeRoot
	var processes []elasticFencedProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(data) == 0 {
			continue
		}
		arguments := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		if len(arguments) == 0 || !strings.HasPrefix(filepath.Base(arguments[0]), "ttc-sentry[") {
			continue
		}
		matchesRoot := false
		for _, argument := range arguments[1:] {
			if argument == rootArgument {
				matchesRoot = true
				break
			}
		}
		if !matchesRoot {
			continue
		}
		start, err := processStartTime(pid)
		if err == nil {
			containerID := arguments[len(arguments)-1]
			if containerID == "" || strings.ContainsAny(containerID, `/\\`) {
				containerID = ""
			}
			processes = append(processes, elasticFencedProcess{PID: pid, StartTime: start, ContainerID: containerID})
		}
	}
	return processes, nil
}

func fenceElasticRuntimeRoot(localRoot, runtimeRoot, nodeID, incarnation string) error {
	processes, err := sentryProcessesForRuntimeRoot(runtimeRoot)
	if err != nil {
		return err
	}
	if len(processes) == 0 {
		return nil
	}
	record := elasticFenceRecord{Version: 1, NodeID: nodeID, Incarnation: incarnation, FencedAt: time.Now().UTC(), Processes: processes}
	path := elasticFencePath(localRoot)
	var result error
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		result = errors.Join(result, err)
	} else if err := writeDurableJSON(path, record); err != nil {
		result = errors.Join(result, fmt.Errorf("recording execution fence: %w", err))
	}
	for _, process := range processes {
		if err := syscall.Kill(process.PID, syscall.SIGSTOP); err != nil && !errors.Is(err, syscall.ESRCH) {
			killErr := syscall.Kill(process.PID, syscall.SIGKILL)
			result = errors.Join(result, fmt.Errorf("stopping Sentry PID %d: %w (kill fallback: %v)", process.PID, err, killErr))
		}
	}
	return result
}

func fencedRuntimeAuthorized(family Family, nodeID, incarnation string, lookup func(string, string) (*cluster.PlacementRecord, error)) bool {
	if family.ID == "" || len(family.Branches) == 0 {
		return false
	}
	for _, branch := range family.Branches {
		placement, err := lookup(family.ID, branch.ID)
		if err != nil || placement.OwnerNode != nodeID || placement.OwnerIncarnation != incarnation ||
			(placement.State != cluster.PlacementRunning && placement.State != cluster.PlacementPaused) {
			return false
		}
	}
	return true
}

// enforceLocalRaftPlacements projects committed branch authority into the
// running Sentry. Node leases are renewed only after this succeeds. Therefore
// a branch revoked since the preceding lease grant can run only through that
// grant's bounded interval; failure to evict it fences the whole node before
// the guardian is allowed to extend execution.
func enforceLocalRaftPlacements(backend *runtimeBackend, nodeID, incarnation string) error {
	if backend == nil || backend.distributedSource == nil || backend.distributedSource.authority == nil {
		return nil
	}
	families, err := backend.listRuntimeFamilies()
	if err != nil {
		return err
	}
	for _, family := range families {
		markCorrupted := func(branchID string) error {
			placement, err := backend.distributedSource.authority.LookupPlacement(family.ID, branchID)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if placement.State != cluster.PlacementCorrupted {
				return nil
			}
			return backend.containerStore().UpdateBranchState(family.ID, branchID, "corrupted")
		}
		var revoked []Branch
		var info ttcbundle.ContainerInfo
		var infoErr error
		infoLoaded := false
		for _, branch := range family.Branches {
			placement, lookupErr := backend.distributedSource.authority.LookupPlacement(family.ID, branch.ID)
			if lookupErr == nil && placement.OwnerNode == nodeID && placement.OwnerIncarnation == incarnation &&
				(placement.State == cluster.PlacementRunning || placement.State == cluster.PlacementPaused || placement.State == cluster.PlacementMigrating) {
				continue
			}
			// Migration transfers guest execution authority before the target has
			// necessarily materialized every sparse page. The old branch remains
			// paused as a transaction-fenced data-plane provider until the target
			// records independence. Do not let the periodic placement enforcer reap
			// that provider merely because ordinary branch placement has moved.
			if backend.activeSparseMigrationSource(family.ID, branch.ID) {
				continue
			}
			// A remote target is restored before its public placement can be
			// committed: the placement contains the physical identity that the
			// target reports only after restore. The Raft transaction is the
			// execution grant during this deliberately narrow interval. Without
			// recognizing it, the heartbeat enforcer can destroy a healthy target
			// between Prepare and Publish.
			if !infoLoaded {
				info, infoErr = backend.containerStore().LoadContainer(family.ID)
				infoLoaded = true
			}
			if infoErr == nil && provisionalRaftTargetAuthorized(info, branch.ID, nodeID, incarnation, backend.distributedSource.authority.LoadTransaction) {
				continue
			}
			revoked = append(revoked, branch)
		}
		if len(revoked) == 0 {
			continue
		}
		if len(revoked) == len(family.Branches) {
			if _, err := backend.command("delete", "--force", family.ID); err != nil {
				present, checkErr := runtimeBranchPresent(backend, family.ID, "")
				if checkErr != nil || present {
					return fmt.Errorf("destroying wholly unauthorized runtime %q: %w", family.ID, err)
				}
			}
			for _, branch := range revoked {
				if err := markCorrupted(branch.ID); err != nil {
					return fmt.Errorf("recording revoked branch %s@%s: %w", family.ID, branch.ID, err)
				}
			}
			continue
		}
		for _, branch := range revoked {
			if branch.State == "running" || branch.State == "restarting" {
				if _, err := backend.command("timetravel", family.ID, "prepare-stop", branch.ID); err != nil {
					present, checkErr := runtimeBranchPresent(backend, family.ID, branch.ID)
					if checkErr == nil && !present {
						continue
					}
					return fmt.Errorf("quiescing revoked branch %s@%s: %w", family.ID, branch.ID, err)
				}
			}
			if _, err := backend.command("timetravel", family.ID, "destroy", branch.ID); err != nil {
				present, checkErr := runtimeBranchPresent(backend, family.ID, branch.ID)
				if checkErr == nil && !present {
					continue
				}
				return fmt.Errorf("destroying revoked branch %s@%s: %w", family.ID, branch.ID, err)
			}
			if err := markCorrupted(branch.ID); err != nil {
				return fmt.Errorf("recording revoked branch %s@%s: %w", family.ID, branch.ID, err)
			}
		}
	}
	return nil
}

// nodeExecutionGrants returns the bounded set of logical runtimes that Raft
// currently permits this exact node incarnation to execute. Node heartbeats
// and transfer accounting deliberately do not affect the set.
func nodeExecutionGrants(state *cluster.AuthorityState, nodeID, incarnation string) map[string]struct{} {
	grants := make(map[string]struct{})
	if state == nil || nodeID == "" || incarnation == "" {
		return grants
	}
	key := func(family, branch string) string { return family + "\x00" + branch }
	for _, placement := range state.Placements {
		if placement.OwnerNode != nodeID || placement.OwnerIncarnation != incarnation {
			continue
		}
		switch placement.State {
		case cluster.PlacementRunning, cluster.PlacementPaused, cluster.PlacementMigrating:
			grants[key(placement.FamilyID, placement.BranchID)] = struct{}{}
		}
	}
	for _, tx := range state.Transactions {
		if tx.Identity.TargetNodeID != nodeID || tx.TargetIncarnation != incarnation {
			continue
		}
		authorized := false
		switch tx.State {
		case cluster.TransactionTargetReserved, cluster.TransactionSourceCaptured,
			cluster.TransactionTargetRestoring, cluster.TransactionTargetPaused:
			authorized = true
		case cluster.TransactionPublished, cluster.TransactionDraining, cluster.TransactionIndependent:
			authorized = !tx.Activated
		}
		if authorized {
			// A distributed fork restores another physical branch in the
			// source family. TargetContainer is the user-facing destination
			// name and may differ from the immutable physical family ID used
			// by the placement committed at publication. Key both provisional
			// and published grants by SourceContainer so the handoff cannot be
			// mistaken for an execution-authority revocation.
			grants[key(tx.Identity.SourceContainer, tx.Identity.VisibleTargetBranch())] = struct{}{}
		}
	}
	return grants
}

func executionGrantRevoked(previous, current map[string]struct{}) bool {
	for grant := range previous {
		if _, ok := current[grant]; !ok {
			return true
		}
	}
	return false
}

// executionRevocationsRequireImmediateFence distinguishes an execution grant
// revocation from the expected physical cleanup lag after migration cutover.
// Cutover leaves the source branch paused and transaction-fenced as a sparse
// data provider; destroying that branch is asynchronous and must not make the
// heartbeat perform a potentially blocking Sentry inventory. Every other
// revocation is conservatively fenced at the host-process boundary.
func executionRevocationsRequireImmediateFence(previous, current map[string]struct{}, state *cluster.AuthorityState, nodeID, incarnation string) bool {
	for grant := range previous {
		if _, retained := current[grant]; retained {
			continue
		}
		separator := strings.IndexByte(grant, 0)
		if separator <= 0 || separator == len(grant)-1 || state == nil {
			return true
		}
		family, branch := grant[:separator], grant[separator+1:]
		cleanupOnly := false
		matchingMigration := false
		for _, tx := range state.Transactions {
			if !tx.Identity.Migration || tx.Identity.SourceNodeID != nodeID ||
				tx.Identity.SourceContainer != family || tx.Identity.SourceBranch != branch {
				continue
			}
			matchingMigration = true
			if tx.OwnershipCommitted && tx.SourceIncarnation == incarnation {
				cleanupOnly = true
				break
			}
		}
		// A matching pre-cutover transaction, or one from another daemon
		// incarnation, cannot authorize this disappearance.
		if matchingMigration && !cleanupOnly {
			return true
		}
		// A heartbeat may observe migration only after its terminal
		// transaction has been compacted. In that case the replacement
		// placement is the durable proof that authority moved instead of
		// disappearing. The source was paused and fenced before Raft was
		// allowed to commit this ownership change, so only asynchronous
		// physical cleanup remains.
		placementFound := false
		for _, placement := range state.Placements {
			if placement.FamilyID != family || placement.BranchID != branch {
				continue
			}
			placementFound = true
			if placement.State == cluster.PlacementCorrupted {
				return true
			}
			// A replacement owner proves migration cutover. An ownerless
			// placement proves that stop/dehydration already removed the
			// runtime before publishing its process-free state. A stable
			// non-executing placement owned here is target materialization,
			// which likewise has no guest instructions to fence.
			if placement.OwnerNode != nodeID || placement.OwnerIncarnation != incarnation ||
				placement.State == cluster.PlacementMaterializing {
				cleanupOnly = true
			}
			break
		}
		if cleanupOnly {
			continue
		}
		branchFound := false
		for _, authority := range state.Branches {
			if authority.FamilyID != family || authority.BranchID != branch {
				continue
			}
			branchFound = true
			if authority.Corrupted {
				return true
			}
			break
		}
		// Placement removal is committed only after local runtime deletion.
		// Both records disappearing is therefore an expected completed rm,
		// not an unexplained execution-authority loss.
		if !placementFound && !branchFound {
			continue
		}
		return true
	}
	// Every revoked grant was independently proven safe. Their coexistence in
	// one heartbeat does not create a new authority violation: Raft serializes
	// migration cutover and branch removal, and removal deletes its placement
	// and branch authority only after local runtime destruction succeeds.
	return false
}

// runtimeBranchPresent resolves a failed idempotent eviction without parsing
// human-readable runsc errors. Empty branchID asks whether the family itself
// remains. The placement enforcer races ordinary migration cleanup by design:
// if that cleanup wins, the desired physical state has already been reached
// and lease renewal must not fail closed merely because destroy saw ENOENT.
func runtimeBranchPresent(backend *runtimeBackend, familyID, branchID string) (bool, error) {
	families, err := backend.listRuntimeFamilies()
	if err != nil {
		return false, err
	}
	return runtimeFamiliesContain(families, familyID, branchID), nil
}

func runtimeFamiliesContain(families []Family, familyID, branchID string) bool {
	for _, family := range families {
		if family.ID != familyID {
			continue
		}
		if branchID == "" {
			return true
		}
		for _, branch := range family.Branches {
			if branch.ID == branchID {
				return true
			}
		}
		return false
	}
	return false
}

// corruptFencedRuntimePeers records the unavoidable blast radius when a
// host-stopped Sentry contains a mix of authorized and revoked branches. The
// stopped Go runtime cannot safely delete only one branch. Resuming it would
// execute the revoked branch, so all otherwise-authorized co-residents are
// fenced permanently before the physical Sentry is killed.
func corruptFencedRuntimePeers(backend *runtimeBackend, family Family, nodeID, incarnation, reason string) error {
	if backend == nil || backend.distributedSource == nil || backend.distributedSource.authority == nil {
		return nil
	}
	var result error
	for _, branch := range family.Branches {
		placement, err := backend.distributedSource.authority.LookupPlacement(family.ID, branch.ID)
		if err != nil || placement.OwnerNode != nodeID || placement.OwnerIncarnation != incarnation ||
			(placement.State != cluster.PlacementRunning && placement.State != cluster.PlacementPaused) {
			continue
		}
		if _, err := backend.distributedSource.authority.MarkBranchCorrupted(family.ID, branch.ID, reason); err != nil {
			result = errors.Join(result, fmt.Errorf("marking co-resident branch %s@%s corrupted: %w", family.ID, branch.ID, err))
			continue
		}
		_ = backend.containerStore().UpdateBranchState(family.ID, branch.ID, "corrupted")
	}
	return result
}

func resumeFencedElasticRuntimes(backend *runtimeBackend, nodeID, incarnation string) error {
	path := elasticFencePath(backend.activeDataRoot())
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var record elasticFenceRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Version != 1 || record.NodeID != nodeID {
		return fmt.Errorf("invalid local execution fence for node %q", nodeID)
	}
	var authorityState *cluster.AuthorityState
	if backend.distributedSource != nil && backend.distributedSource.authority != nil {
		authorityState, err = backend.distributedSource.authority.AuthorityState()
		if err != nil {
			return fmt.Errorf("loading execution authority for fence recovery: %w", err)
		}
	}
	var result error
	for _, process := range record.Processes {
		start, err := processStartTime(process.PID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || start != process.StartTime {
			result = errors.Join(result, fmt.Errorf("fenced Sentry PID %d identity changed", process.PID))
			continue
		}
		containerID := process.ContainerID
		if backend.distributedSource != nil && backend.distributedSource.authority != nil {
			physicalBranches := append([]string(nil), process.Branches...)
			if len(physicalBranches) == 0 && containerID != "" {
				// Guardian-created records lack an in-Sentry branch snapshot. Shared
				// metadata is safe to inspect without waking the process. Restrict
				// the fallback to branches whose last physical owner was this node;
				// ambiguity fails closed below.
				if info, loadErr := backend.containerStore().LoadContainer(containerID); loadErr == nil {
					for _, branch := range info.Branches {
						if info.BranchNodeID(branch.ID) == nodeID && (normalizeLifecycleState(branch.State) == "running" || normalizeLifecycleState(branch.State) == "paused") {
							physicalBranches = append(physicalBranches, branch.ID)
						}
					}
				}
			}
			// The guardian records host-visible runtime identity. After migration
			// both the container and branch may be transaction IDs, while Raft owns
			// the stable family@branch. Re-authorize the exact physical-to-logical
			// mapping before applying the ordinary placement checks.
			family, mapped := logicalFamilyForPhysicalRuntime(authorityState, containerID, physicalBranches)
			authorized := mapped && fencedRuntimeAuthorized(family, nodeID, incarnation, backend.distributedSource.authority.LookupPlacement)
			if !authorized {
				if containerID == "" {
					result = errors.Join(result, fmt.Errorf("fenced Sentry PID %d has no runtime identity and cannot be re-authorized", process.PID))
					continue
				}
				result = errors.Join(result, corruptFencedRuntimePeers(backend, family, nodeID, incarnation,
					"co-resident runtime contained a branch without execution authority during fence recovery"))
				// runsc delete normally asks the Sentry to destroy itself, but a
				// host-stopped Sentry cannot answer that RPC. Its PID/start time was
				// verified above and Raft denied execution authority, so kill it
				// without SIGCONT before asking runsc to reap the gofer and files.
				if err := syscall.Kill(process.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					result = errors.Join(result, fmt.Errorf("killing unauthorized fenced Sentry PID %d: %w", process.PID, err))
					continue
				}
				if _, err := backend.command("delete", "--force", containerID); err != nil {
					result = errors.Join(result, fmt.Errorf("destroying unauthorized fenced runtime %q: %w", containerID, err))
				}
				continue
			}
		}
		if err := syscall.Kill(process.PID, syscall.SIGCONT); err != nil && !errors.Is(err, syscall.ESRCH) {
			result = errors.Join(result, err)
		}
	}
	if result != nil {
		return result
	}
	return os.Remove(path)
}

func maintainElasticNode(directory cluster.NodeCatalog, initial cluster.NodeRecord, lease time.Duration, stderr io.Writer, done <-chan struct{}, renewed func(*cluster.AuthorityState) error, fence func() error, fatal chan<- error) {
	// Begin renewing well before the fencing window. Cluster control traffic may
	// share a constrained link with peer paging or a network filesystem, so a
	// healthy renewal must be allowed to absorb a transient multi-second queue
	// without turning an otherwise healthy node into a false failure.
	interval := lease / 4
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	margin := lease / 3
	if margin < 500*time.Millisecond {
		margin = 500 * time.Millisecond
	}
	retryDelay := interval / 10
	if retryDelay < 50*time.Millisecond {
		retryDelay = 50 * time.Millisecond
	}
	if retryDelay > 250*time.Millisecond {
		retryDelay = 250 * time.Millisecond
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	record := initial
	// This is deliberately monotonic-local, not the leader's absolute wall
	// clock deadline. Starting it before each renewal request makes network RTT
	// conservative and avoids assuming synchronized clocks for physical fencing.
	localDeadline := time.Now().Add(lease)
	var lastErr error
	for {
		select {
		case <-done:
			return
		case <-timer.C:
			renewalStarted := time.Now()
			fenceAt := localDeadline.Add(-margin)
			if !renewalStarted.Before(fenceAt) {
				cause := lastErr
				if cause == nil {
					cause = errors.New("lease renewal safety margin elapsed before the heartbeat could run")
				}
				fenceErr := error(nil)
				if fence != nil {
					fenceErr = fence()
				}
				select {
				case fatal <- fmt.Errorf("elastic cluster lease self-fenced before expiry after heartbeat failure: %w", errors.Join(cause, fenceErr)):
				default:
				}
				return
			}
			current, err := currentNodeMemoryBytes()
			var authorityState *cluster.AuthorityState
			if err == nil {
				if catalog, ok := directory.(*cluster.RaftCatalog); ok {
					// A renewal includes leader forwarding, quorum persistence, and
					// waiting until the committed authority state is locally visible.
					// Give that complete operation most of the remaining safe window.
					// Keeping one third in reserve guarantees another attempt can begin
					// before the local guardian's conservative fencing deadline.
					attemptBudget := elasticHeartbeatAttemptBudget(time.Until(fenceAt))
					if attemptBudget <= 0 {
						err = context.DeadlineExceeded
					} else {
						ctx, cancel := context.WithTimeout(context.Background(), attemptBudget)
						var renewedRecord *cluster.NodeRecord
						renewedRecord, authorityState, err = catalog.HeartbeatState(ctx, record.NodeID, record.Incarnation, current, lease)
						cancel()
						if err == nil {
							record = *renewedRecord
						}
					}
				} else {
					record, err = heartbeatElasticNode(directory, record, current, renewalStarted, lease)
				}
				if err == nil && renewed != nil {
					err = renewed(authorityState)
				}
				if err == nil {
					localDeadline = renewalStarted.Add(lease)
					lastErr = nil
					timer.Reset(interval)
					continue
				}
			}
			lastErr = err
			if stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: elastic cluster heartbeat failed: %v\n", err)
			}
			remaining := time.Until(fenceAt)
			if remaining <= 0 {
				fenceErr := error(nil)
				if fence != nil {
					fenceErr = fence()
				}
				select {
				case fatal <- fmt.Errorf("elastic cluster lease self-fenced before expiry after heartbeat failure: %w", errors.Join(err, fenceErr)):
				default:
				}
				return
			}
			// Retry quickly while there is still execution-lease headroom.
			// Limit the sleep to a fraction of the remaining window so a
			// short control-plane stall cannot turn into a single-shot fence.
			delay := retryDelay
			if quarter := remaining / 4; delay > quarter {
				delay = quarter
			}
			if delay <= 0 {
				delay = time.Millisecond
			}
			timer.Reset(delay)
		}
	}
}

// elasticHeartbeatAttemptBudget returns the maximum duration of one Raft
// renewal attempt. The caller has already subtracted the lease safety margin,
// so using two thirds of this duration cannot delay physical fencing beyond the
// authoritative lease expiry and always reserves headroom for a retry.
func elasticHeartbeatAttemptBudget(remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return 0
	}
	return remaining * 2 / 3
}

// maintainAuthorityExpirations is leader-only failure reconciliation. The
// resulting Raft command atomically revokes the expired incarnation, its
// placements, and every branch with an irreplaceable dependency on it.
func maintainAuthorityExpirations(node *cluster.AuthorityRaftNode, authority *cluster.RaftCatalog, lease time.Duration, stderr io.Writer, done <-chan struct{}) {
	if node == nil || authority == nil {
		return
	}
	interval := lease / 4
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		// Membership is reconciled independently of lease expiry. In
		// particular, a newly joined process remains a learner until its ready
		// execution record is committed, so a failed startup cannot enlarge the
		// quorum and strand the existing cluster.
		if node.IsLeader() {
			ctx, cancel := context.WithTimeout(context.Background(), lease)
			err := node.ReconcileVoters(ctx, 3)
			cancel()
			if err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: reconciling Raft voters: %v\n", err)
			}
		}
		if !node.LeadershipSafeFor(lease) {
			continue
		}
		records, err := authority.List()
		if err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: listing execution leases for expiry: %v\n", err)
			}
			continue
		}
		now := time.Now()
		for _, record := range records {
			if record.State == cluster.NodeOffline || record.LeaseActive(now) {
				continue
			}
			if _, err := authority.ExpireNode(record.NodeID, record.Incarnation, record.Revision); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: expiring node %s/%s: %v\n", record.NodeID, record.Incarnation, err)
			}
		}
	}
}

type sharedGCLeaseKeeperResult struct {
	lease cluster.SharedGCLease
	err   error
}

// keepSharedGCLeaseAlive renews a lease independently of ledger rebuilding,
// collection, compaction, and retirement. Those operations scale with the
// live snapshot graph and must never starve the fixed-rate lease heartbeat.
func keepSharedGCLeaseAlive(authority *cluster.RaftCatalog, initial cluster.SharedGCLease, duration time.Duration) func() (cluster.SharedGCLease, error) {
	stop := make(chan struct{})
	result := make(chan sharedGCLeaseKeeperResult, 1)
	go func() {
		lease := initial
		delay := duration / 4
		if delay < 100*time.Millisecond {
			delay = 100 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				result <- sharedGCLeaseKeeperResult{lease: lease}
				return
			case <-timer.C:
				renewed, err := authority.RenewSharedGCLease(lease, duration)
				if err == nil {
					lease = renewed
					delay = duration / 4
				} else {
					remaining := time.Until(lease.ExpiresAt)
					if remaining <= 0 {
						result <- sharedGCLeaseKeeperResult{lease: lease, err: fmt.Errorf("shared GC lease expired after renewal failure: %w", err)}
						return
					}
					delay = remaining / 4
				}
				if delay < 100*time.Millisecond {
					delay = 100 * time.Millisecond
				}
				timer.Reset(delay)
			}
		}
	}()
	var once sync.Once
	return func() (cluster.SharedGCLease, error) {
		once.Do(func() { close(stop) })
		outcome := <-result
		return outcome.lease, outcome.err
	}
}

// maintainSharedGCCoordinator elects exactly one daemon for shared snapshot
// reference accounting and pack mutation. Actual collection additionally
// validates the lease generation at every retirement boundary; merely holding
// this lease never makes a pack reclaimable.
func maintainSharedGCCoordinator(authority *cluster.RaftCatalog, packs *ttcsnapshot.SharedPackStore, references *sharedReferenceService, nodeID, incarnation string, stderr io.Writer, done <-chan struct{}) {
	if authority == nil || packs == nil || nodeID == "" || incarnation == "" {
		return
	}
	const duration = 15 * time.Second
	maintenanceContext, cancelMaintenance := context.WithCancel(context.Background())
	defer cancelMaintenance()
	go func() {
		select {
		case <-done:
			cancelMaintenance()
		case <-maintenanceContext.Done():
		}
	}()
	var held cluster.SharedGCLease
	deactivateReferences := func() {
		if references != nil {
			if err := references.deactivate(); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: closing shared reference ledger: %v\n", err)
			}
		}
	}
	release := func() {
		deactivateReferences()
		if held.Generation == 0 {
			return
		}
		if err := authority.ReleaseSharedGCLease(held); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: releasing shared GC lease generation %d: %v\n", held.Generation, err)
		}
		held = cluster.SharedGCLease{}
	}
	defer release()
	for {
		now := time.Now()
		if held.Generation != 0 {
			// Activation may retry more frequently while an expired execution
			// incarnation is being fenced. Do not turn those retries into Raft
			// lease writes; the ordinary one-third-duration renewal cadence is
			// sufficient to retain ownership throughout the rebuild.
			if time.Until(held.ExpiresAt) <= 2*duration/3 {
				if renewed, err := authority.RenewSharedGCLease(held, duration); err == nil {
					held = renewed
				} else {
					held = cluster.SharedGCLease{}
					deactivateReferences()
				}
			}
		} else if current, err := authority.SharedGCLease(); err == nil {
			switch {
			case current.Generation != 0 && now.Before(current.ExpiresAt) && (current.NodeID != nodeID || current.Incarnation != incarnation):
				// Another healthy coordinator owns the lease.
				deactivateReferences()
			case current.Generation != 0 && now.Before(current.ExpiresAt):
				held = current
			default:
				if acquired, err := authority.AcquireSharedGCLease(nodeID, incarnation, duration); err == nil {
					held = acquired
				}
			}
		}
		retryActivation := false
		if held.Generation != 0 && time.Now().Before(held.ExpiresAt) {
			stopKeeper := keepSharedGCLeaseAlive(authority, held, duration)
			var maintenanceErr error
			if references != nil {
				if err := references.activate(held); err != nil {
					maintenanceErr = fmt.Errorf("activating shared reference ledger: %w", err)
					retryActivation = true
				}
			}
			if references != nil && maintenanceErr == nil {
				// Collection duration is proportional to the amount of newly dead
				// graph state and to shared-filesystem latency. The independently
				// renewed coordinator lease fences every irreversible boundary, so a
				// short wall-clock timeout adds no safety and can starve a large
				// family forever. Bound it by daemon lifetime instead.
				_, err := references.collectAndRetire(maintenanceContext, held, 64<<10)
				if err != nil {
					maintenanceErr = fmt.Errorf("collecting shared snapshot graph: %w", err)
				}
			}
			retirements, err := authority.RetiredSharedSnapshots()
			if err == nil && maintenanceErr == nil {
				for _, retirement := range retirements {
					var err error
					if references != nil {
						err = references.completeRetirement(ttcsnapshot.SnapshotID(retirement.SnapshotID))
					} else {
						_, err = packs.CompleteRetirement(ttcsnapshot.SnapshotID(retirement.SnapshotID))
					}
					if err != nil && stderr != nil {
						fmt.Fprintf(stderr, "ttcd: warning: completing retirement of shared snapshot %d: %v\n", retirement.SnapshotID, err)
					}
				}
			}
			renewed, keeperErr := stopKeeper()
			if keeperErr != nil {
				held = cluster.SharedGCLease{}
				deactivateReferences()
				if stderr != nil {
					fmt.Fprintf(stderr, "ttcd: warning: maintaining shared GC lease: %v\n", keeperErr)
				}
			} else {
				held = renewed
			}
			if maintenanceErr != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: %v\n", maintenanceErr)
			}
		}
		delay := duration / 3
		if retryActivation {
			// An old provider can become fenced just after a failed rebuild.
			// Waiting for the normal five-second maintenance tick unnecessarily
			// extends the dynamic-unavailability window for otherwise valid
			// forks. Retrying reads is cheap; lease renewal above remains bounded.
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-done:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func maintainElasticRebalancer(backend *runtimeBackend, nodeID string, lease time.Duration, stderr io.Writer, done <-chan struct{}, serialize func(func() error) error) {
	interval := lease
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastAttempt := time.Time{}
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			record, err := backend.distributedSource.directory.Lookup(nodeID)
			if err != nil || record.State != cluster.NodePressured || now.Sub(lastAttempt) < 30*time.Second {
				continue
			}
			lastAttempt = now
			var plan cluster.FixedTerminalPlan
			err = serialize(func() error {
				var planErr error
				plan, planErr = backend.rebalanceNode(nodeID, "", true)
				return planErr
			})
			if err != nil {
				if stderr != nil {
					fmt.Fprintf(stderr, "ttcd: elastic rebalance for node %s needs attention: %v\n", nodeID, err)
				}
				continue
			}
			if stderr != nil && len(plan.Moves) != 0 {
				fmt.Fprintf(stderr, "ttcd: elastic rebalance moved %d branches from %s across %d destinations (estimated connectivity %d bytes)\n", len(plan.Moves), plan.SourceNode, placementDestinationCount(plan.Moves), plan.EstimatedConnectivity)
			}
		}
	}
}

func maintainElasticDrain(backend *runtimeBackend, nodeID string, lease time.Duration, stderr io.Writer, done <-chan struct{}, serialize func(func() error) error, completed func()) {
	interval := lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	consecutiveFailures := uint(0)
	retryDelay := func() time.Duration {
		delay := interval
		for i := uint(1); i < consecutiveFailures && delay < 30*time.Second; i++ {
			delay *= 2
		}
		if delay > 30*time.Second {
			return 30 * time.Second
		}
		return delay
	}
	for {
		select {
		case <-done:
			return
		case <-timer.C:
		}
		intent, err := loadElasticDrainIntent(backend.dataRoot, nodeID)
		if err != nil {
			consecutiveFailures++
			delay := retryDelay()
			if stderr != nil {
				fmt.Fprintf(stderr, "ttcd: elastic drain recovery for node %s cannot read its intent; retrying in %s: %v\n", nodeID, delay, err)
			}
			timer.Reset(delay)
			continue
		}
		if intent == nil {
			consecutiveFailures = 0
			timer.Reset(interval)
			continue
		}
		var moved []selector
		err = serialize(func() error {
			if _, recoverErr := backend.recoverInterruptedMigrations(); recoverErr != nil {
				return recoverErr
			}
			var drainErr error
			moved, drainErr = backend.drainNode(nodeID, intent.TargetNode)
			if drainErr != nil {
				return drainErr
			}
			terminalCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			return backend.finalizeNodeRetirement(terminalCtx)
		})
		if err != nil {
			consecutiveFailures++
			delay := retryDelay()
			if stderr != nil {
				fmt.Fprintf(stderr, "ttcd: elastic drain %s for node %s will retry in %s: %v\n", intent.OperationID, nodeID, delay, err)
			}
			timer.Reset(delay)
			continue
		}
		if stderr != nil {
			fmt.Fprintf(stderr, "ttcd: elastic drain %s for node %s completed after moving %d remaining branches\n", intent.OperationID, nodeID, len(moved))
		}
		if completed != nil {
			completed()
		}
		return
	}
}

func maintainElasticMigrationRecovery(backend *runtimeBackend, lease time.Duration, stderr io.Writer, done <-chan struct{}, serialize func(func() error) error) {
	interval := lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-done:
			return
		case <-timer.C:
		}
		var recovered []selector
		err := serialize(func() error {
			var recoverErr error
			recovered, recoverErr = backend.recoverInterruptedMigrations()
			return recoverErr
		})
		if err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "ttcd: elastic migration recovery needs attention: %v\n", err)
			}
		} else if stderr != nil && len(recovered) != 0 {
			fmt.Fprintf(stderr, "ttcd: recovered %d interrupted elastic migrations\n", len(recovered))
		}
		timer.Reset(interval)
	}
}

func heartbeatElasticNode(directory cluster.NodeCatalog, record cluster.NodeRecord, current uint64, now time.Time, lease time.Duration) (cluster.NodeRecord, error) {
	updated, err := directory.Heartbeat(record.NodeID, record.Incarnation, current, now, lease)
	if err != nil {
		return cluster.NodeRecord{}, err
	}
	next := updated.State
	if updated.MemoryTargetBytes != 0 && current > updated.MemoryTargetBytes && updated.State == cluster.NodeReady {
		next = cluster.NodePressured
	} else if updated.State == cluster.NodePressured && (updated.MemoryTargetBytes == 0 || current < updated.MemoryTargetBytes-updated.MemoryTargetBytes/10) {
		next = cluster.NodeReady
	}
	if next == updated.State {
		return *updated, nil
	}
	transitioned, err := directory.Transition(updated.NodeID, updated.Incarnation, next, now, lease)
	if err != nil {
		return cluster.NodeRecord{}, err
	}
	return *transitioned, nil
}
