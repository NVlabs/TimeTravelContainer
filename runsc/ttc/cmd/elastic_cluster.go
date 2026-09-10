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
	"syscall"
	"time"

	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcvolume"
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
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		http.Error(w, "cluster transport does not support full-duplex streams: "+err.Error(), http.StatusInternalServerError)
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

func daemonCommandOwner(dataRoot string, args []string) (string, error) {
	owner, _, err := daemonCommandPlacement(dataRoot, args)
	return owner, err
}

func daemonCommandPlacement(dataRoot string, args []string) (string, []daemonPlacementClaim, error) {
	placements, placementErr := cluster.OpenPlacementStore(filepath.Join(dataRoot, "cluster", "placements"))
	if placementErr != nil {
		return "", nil, placementErr
	}
	return daemonCommandPlacementWithCatalog(dataRoot, placements, args)
}

func daemonCommandPlacementWithCatalog(dataRoot string, placements cluster.PlacementCatalog, args []string) (string, []daemonPlacementClaim, error) {
	if dataRoot == "" {
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
	allowMaterializing := len(args) != 0 && (args[0] == "start" || args[0] == "rehydrate")
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
			continue
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
			// Legacy families acquire a placement record lazily at their first
			// migration. Until then persistent branch ownership remains the
			// compatibility routing source.
			branchOwner = info.BranchNodeID(selected.branch)
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

func validateDaemonPlacementClaims(dataRoot, localNode string, claims []daemonPlacementClaim) error {
	placements, err := cluster.OpenPlacementStore(filepath.Join(dataRoot, "cluster", "placements"))
	if err != nil {
		return err
	}
	return validateDaemonPlacementClaimsWithCatalog(dataRoot, localNode, claims, placements)
}

func validateDaemonPlacementClaimsWithCatalog(dataRoot, localNode string, claims []daemonPlacementClaim, placements cluster.PlacementCatalog) error {
	if len(claims) == 0 {
		return nil
	}
	if placements == nil {
		return errors.New("placement authority is unavailable")
	}
	for _, claim := range claims {
		record, err := placements.Lookup(claim.FamilyID, claim.BranchID)
		if err != nil {
			return fmt.Errorf("validating placement %s@%s: %w", claim.FamilyID, claim.BranchID, err)
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
// pause/resume. Cold branches retain their last owner for compatibility until
// an explicit drain or rehydration transaction claims them. Records for
// deleted locally owned branches are reclaimed.
func (b *runtimeBackend) reconcilePlacementCatalog() error {
	if b == nil || b.dataRoot == "" || b.nodeID == "" || b.distributedSource == nil || b.distributedSource.directory == nil {
		return nil
	}
	current, err := b.distributedSource.directory.Lookup(b.nodeID)
	if err != nil {
		return err
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	containers, err := b.containerStore().ListContainers()
	if err != nil {
		return err
	}
	known := make(map[string]struct{})
	var result error
	now := time.Now()
	for _, info := range containers {
		for _, branch := range info.Branches {
			key := info.ID + "\x00" + branch.ID
			known[key] = struct{}{}
			owner := info.BranchNodeID(branch.ID)
			state := cluster.PlacementState(normalizeLifecycleState(branch.State))
			if owner != b.nodeID || (state != cluster.PlacementRunning && state != cluster.PlacementPaused) {
				continue
			}
			record, lookupErr := placements.Lookup(info.ID, branch.ID)
			if errors.Is(lookupErr, os.ErrNotExist) {
				// During restart recovery, first adopt all existing placements
				// while the node is still unschedulable. Previously untracked
				// local branches are enrolled after the node becomes ready.
				if current.State == cluster.NodeJoining {
					continue
				}
				_, createErr := placements.Create(cluster.PlacementRecord{
					FamilyID: info.ID, BranchID: branch.ID, PhysicalContainer: info.ID, PhysicalBranch: branch.ID,
					OwnerNode: b.nodeID, OwnerIncarnation: current.Incarnation, State: state,
				}, now)
				if createErr != nil && !errors.Is(createErr, os.ErrExist) {
					result = errors.Join(result, fmt.Errorf("creating placement for %s@%s: %w", info.ID, branch.ID, createErr))
				} else if createErr == nil && b.distributedSource.authority != nil {
					if _, inheritErr := b.distributedSource.authority.InheritBranchDependencies(info.ID, branch.ID, branch.ParentID); inheritErr != nil {
						result = errors.Join(result, fmt.Errorf("recording dependencies for %s@%s: %w", info.ID, branch.ID, inheritErr))
					}
				}
				continue
			}
			if lookupErr != nil {
				result = errors.Join(result, lookupErr)
				continue
			}
			if record.State == cluster.PlacementMigrating || record.OwnerNode != b.nodeID {
				continue
			}
			if record.OwnerIncarnation != current.Incarnation {
				record, err = placements.AdoptOwnerIncarnation(info.ID, branch.ID, b.nodeID, record.OwnerIncarnation, current.Incarnation, record.Generation, now)
				if err != nil {
					result = errors.Join(result, fmt.Errorf("adopting placement for %s@%s: %w", info.ID, branch.ID, err))
					continue
				}
			}
			if record.State != state {
				if _, err := placements.UpdateStableState(info.ID, branch.ID, b.nodeID, current.Incarnation, record.Generation, state, now); err != nil {
					result = errors.Join(result, fmt.Errorf("updating placement state for %s@%s: %w", info.ID, branch.ID, err))
				}
			}
			if b.distributedSource.authority != nil {
				if _, inheritErr := b.distributedSource.authority.InheritBranchDependencies(info.ID, branch.ID, branch.ParentID); inheritErr != nil {
					result = errors.Join(result, fmt.Errorf("recording dependencies for %s@%s: %w", info.ID, branch.ID, inheritErr))
				}
			}
		}
	}
	records, err := placements.List()
	if err != nil {
		return errors.Join(result, err)
	}
	for _, record := range records {
		if _, ok := known[record.FamilyID+"\x00"+record.BranchID]; ok || record.State == cluster.PlacementMigrating {
			continue
		}
		if record.OwnerNode != "" && record.OwnerNode != b.nodeID {
			continue
		}
		if err := placements.Remove(record.FamilyID, record.BranchID, record.Generation); err != nil {
			result = errors.Join(result, fmt.Errorf("removing stale placement %s@%s: %w", record.FamilyID, record.BranchID, err))
		}
	}
	return result
}

func daemonCommandNeedsPlacementAdmission(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "run", "create", "start", "fork", "rehydrate", "restore":
		return true
	case "container":
		return len(args) > 1 && (args[1] == "run" || args[1] == "create")
	default:
		return false
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
	if s == nil || backend == nil || s.clusterDirectory == nil || s.clusterCapabilities == nil || len(args) < 2 || (args[0] != "rehydrate" && args[0] != "start") {
		return nil
	}
	node, err := s.clusterDirectory.Lookup(s.clusterCapabilities.NodeID)
	if err != nil {
		return fmt.Errorf("loading cold-branch placement node: %w", err)
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
		info, err := store.LoadContainer(target.container)
		if err != nil {
			return err
		}
		branch, err := persistentBranchInfo(info, target.branch)
		if err != nil {
			return err
		}
		record, err := placements.Lookup(info.ID, branch.ID)
		if errors.Is(err, os.ErrNotExist) {
			if !branch.Unplaced {
				// Legacy placed branches are enrolled by ordinary post-command
				// reconciliation and require no cold-placement claim.
				continue
			}
			record, err = placements.Create(cluster.PlacementRecord{
				FamilyID: info.ID, BranchID: branch.ID, State: cluster.PlacementUnplaced,
			}, time.Now())
		}
		if err != nil {
			return fmt.Errorf("loading placement for %s: %w", target, err)
		}
		if record.State != cluster.PlacementUnplaced {
			if record.OwnerNode != node.NodeID || record.OwnerIncarnation != node.Incarnation {
				return fmt.Errorf("cold-branch placement for %s belongs to %s/%s", target, record.OwnerNode, record.OwnerIncarnation)
			}
			if branch.Unplaced {
				if err := store.CompareAndSwapBranchNodeID(info.ID, branch.ID, "", node.NodeID); err != nil {
					return fmt.Errorf("repairing claimed cold-branch owner for %s: %w", target, err)
				}
			}
			continue
		}
		claimed, err := placements.ClaimUnplaced(info.ID, branch.ID, node.NodeID, node.Incarnation, info.ID, branch.ID, record.Generation, cluster.PlacementMaterializing, time.Now())
		if err != nil {
			return fmt.Errorf("claiming cold-branch placement for %s: %w", target, err)
		}
		if err := store.CompareAndSwapBranchNodeID(info.ID, branch.ID, "", node.NodeID); err != nil {
			_, releaseErr := placements.Unplace(info.ID, branch.ID, node.NodeID, node.Incarnation, claimed.Generation, time.Now())
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
	prefix  []string
	targets []string
}

// parseDistributedFamilyMutation recognizes the deliberately small set of
// commands whose unqualified operands mean "all branches in this family".
// It follows flag.FlagSet's ordering rule: options precede positional targets.
func parseDistributedFamilyMutation(args []string) (distributedFamilyMutation, bool) {
	if len(args) < 2 || (args[0] != "rm" && args[0] != "stop" && args[0] != "dehydrate") {
		return distributedFamilyMutation{}, false
	}
	i := 1
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" {
		arg := args[i]
		switch args[0] {
		case "rm":
			if arg != "-f" && arg != "--force" && arg != "-v" && arg != "--volumes" && arg != "-fv" && arg != "-vf" {
				return distributedFamilyMutation{}, false
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
	hasFamily := false
	for _, target := range args[i:] {
		if !strings.Contains(target, "@") {
			hasFamily = true
		}
	}
	if !hasFamily {
		return distributedFamilyMutation{}, false
	}
	return distributedFamilyMutation{prefix: append([]string(nil), args[:i]...), targets: append([]string(nil), args[i:]...)}, true
}

func distributedChildRequestID(parent string, ordinal int, args []string) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", parent, ordinal, strings.Join(args, "\x00"))))
	return hex.EncodeToString(hash[:16])
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
	info, err := store.LoadContainer(target.container)
	if err != nil {
		return true, 1, nil, nil, err
	}
	if _, err := persistentBranchInfo(info, target.branch); err != nil {
		return true, 1, nil, nil, err
	}
	owners := make(map[string]string)
	merged := FamilyResourceStats{Container: info.Name}
	if merged.Container == "" {
		merged.Container = target.container
	}
	for _, branch := range info.Branches {
		record, lookupErr := s.clusterPlacements.Lookup(info.ID, branch.ID)
		if lookupErr == nil && record.OwnerNode != "" && (record.State == cluster.PlacementRunning || record.State == cluster.PlacementPaused) {
			if _, ok := owners[record.OwnerNode]; !ok {
				owners[record.OwnerNode] = branch.ID
			}
			continue
		}
		if lookupErr != nil && !errors.Is(lookupErr, os.ErrNotExist) {
			return true, 1, nil, nil, fmt.Errorf("resolving stats placement for %s@%s: %w", info.Name, branch.ID, lookupErr)
		}
		// Process-free and corrupted branches have no node-local resident graph.
		// Keep them visible in the cluster snapshot. Immutable cold packs are
		// cluster storage rather than capacity attributable to a runtime owner.
		state := normalizeLifecycleState(branch.State)
		if lookupErr == nil {
			state = string(record.State)
		}
		merged.Branches = append(merged.Branches, BranchResourceStats{
			Target: merged.Container + "@" + branch.ID, State: state,
			StorageState: branch.StorageState, StorageGeneration: branch.StorageGeneration,
			SourceNode: branch.SourceNode, DependentMemoryBytes: branch.DependentMemoryBytes,
			DependentFilesystemBytes: branch.DependentFilesystemBytes,
		})
		merged.DependentMemoryBytes += branch.DependentMemoryBytes
		merged.DependentFilesystemBytes += branch.DependentFilesystemBytes
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
			Options: request.Options, Args: args, DistributedLeaf: true,
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

// executeDistributedFamilyMutation coordinates family-wide lifecycle changes
// before any daemon takes its process-local lifecycle lock. This is necessary
// once one logical family can have physical runtimes on multiple nodes.
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
	var children []child
	for _, operand := range mutation.targets {
		if strings.Contains(operand, "@") {
			selected, err := parseSelector(operand)
			if err != nil {
				return true, 1, nil, nil, fmt.Errorf("invalid target %q: %w", operand, err)
			}
			info, err := store.LoadContainer(selected.container)
			if err != nil {
				return true, 1, nil, nil, err
			}
			node := info.BranchNodeID(selected.branch)
			if node == "" {
				node = s.clusterCapabilities.NodeID
			}
			children = append(children, child{node: node, args: append(append([]string(nil), mutation.prefix...), operand)})
			continue
		}
		info, err := store.LoadContainer(operand)
		if err != nil {
			return true, 1, nil, nil, err
		}
		branches := append([]ttcbundle.BranchInfo(nil), info.Branches...)
		sort.Slice(branches, func(i, j int) bool { return branches[i].ID < branches[j].ID })
		if len(branches) == 0 {
			return true, 1, nil, nil, fmt.Errorf("container %q has no branches", operand)
		}
		finalNode := info.BranchNodeID(branches[len(branches)-1].ID)
		if finalNode == "" {
			finalNode = s.clusterCapabilities.NodeID
		}
		formerOwners := make(map[string]struct{})
		for i, branch := range branches {
			node := info.BranchNodeID(branch.ID)
			if node == "" {
				node = s.clusterCapabilities.NodeID
			}
			target := operand + "@" + branch.ID
			// The final rm must run as a family removal so it can reclaim the
			// family record and anonymous volumes after every peer runtime is gone.
			if request.Args[0] == "rm" && i == len(branches)-1 {
				target = operand
			} else if request.Args[0] == "rm" && node != finalNode {
				formerOwners[node] = struct{}{}
			}
			children = append(children, child{node: node, args: append(append([]string(nil), mutation.prefix...), target)})
		}
		if request.Args[0] == "rm" && len(formerOwners) != 0 {
			// Run owner-local branch deletes first, then purge physical shells on
			// former owners, and only then let the final owner delete metadata.
			final := children[len(children)-1]
			children = children[:len(children)-1]
			owners := make([]string, 0, len(formerOwners))
			for node := range formerOwners {
				owners = append(owners, node)
			}
			sort.Strings(owners)
			for _, node := range owners {
				children = append(children, child{node: node, args: []string{"_purge-runtime", info.ID}})
			}
			children = append(children, final)
		}
	}

	var diagnostic bytes.Buffer
	for i, child := range children {
		if owner, claims, err := daemonCommandPlacement(s.dataRoot, child.args); err != nil {
			return true, 1, nil, diagnostic.Bytes(), err
		} else {
			if owner != "" {
				child.node = owner
			}
			child.claims = claims
		}
		childRequest := daemonRequest{
			Version: daemonAPIVersion, Action: "command", RequestID: distributedChildRequestID(request.RequestID, i, child.args),
			Options: request.Options, Args: child.args, ClientTERM: request.ClientTERM,
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

func (s *daemonServer) executeLocalDistributedCommand(request daemonRequest) (int, string, string, error) {
	opts := s.optionsFromRequest(request.Options)
	var stdout, stderr bytes.Buffer
	backend := directBackend(opts, strings.NewReader(""), &stdout, &stderr)
	s.configureVolumeBackend(backend)
	backend.consoleSidecars = s
	backend.distributedSource = s.distributedSource
	backend.nodeID = s.clusterCapabilities.NodeID
	s.reconcile(backend)
	unlock := s.lockMutation(opts.dataRoot, request.Args)
	defer unlock()
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
		record, replay, err := beginDaemonMutation(opts.dataRoot, request)
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
		record, err = completeDaemonMutation(opts.dataRoot, record, status, stdout.Bytes(), stderr.Bytes())
		if err != nil {
			return 1, stdout.String(), stderr.String(), err
		}
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
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	target = resolved
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
	sourceRecord, err := b.distributedSource.directory.Lookup(b.nodeID)
	if err != nil {
		return fmt.Errorf("source node lease is unavailable: %w", err)
	}
	if !sourceRecord.LeaseActive(time.Now()) {
		return fmt.Errorf("source node %q lease is expired or offline", b.nodeID)
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
	placement, err := placements.Lookup(info.ID, target.branch)
	if errors.Is(err, os.ErrNotExist) {
		placement, err = placements.Create(cluster.PlacementRecord{
			FamilyID: info.ID, BranchID: target.branch, PhysicalContainer: info.ID, PhysicalBranch: target.branch,
			OwnerNode: b.nodeID, OwnerIncarnation: sourceRecord.Incarnation, State: cluster.PlacementState(state),
		}, time.Now())
	}
	if err != nil {
		return err
	}
	if placement.OwnerNode == b.nodeID && placement.OwnerIncarnation != sourceRecord.Incarnation {
		placement, err = placements.AdoptOwnerIncarnation(info.ID, target.branch, b.nodeID, placement.OwnerIncarnation, sourceRecord.Incarnation, placement.Generation, time.Now())
		if err != nil {
			return fmt.Errorf("adopting placement after node restart: %w", err)
		}
	}
	requestID, err := newDistributedTransactionID()
	if err != nil {
		return err
	}
	if _, err := placements.BeginMigration(info.ID, target.branch, b.nodeID, sourceRecord.Incarnation, placement.Generation, requestID, targetNode, time.Now()); err != nil {
		return err
	}
	rootfsDigest, err := distributedRootfsDigest(info, target.branch, b.distributedSource.capabilities.RepositoryID)
	if err != nil {
		_, _ = placements.AbortMigration(info.ID, target.branch, requestID, time.Now())
		return err
	}
	identity := cluster.ForkIdentity{
		TransactionID: requestID, SourceNodeID: b.nodeID, TargetNodeID: targetNode,
		SourceContainer: info.ID, SourceBranch: target.branch,
		TargetContainer: info.ID, TargetBranch: requestID, TargetVisibleBranch: target.branch,
		ImageDigest: info.Digest, RootfsDigest: rootfsDigest,
		RepositoryID: b.distributedSource.capabilities.RepositoryID, Migration: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tx, err := peer.Reserve(ctx, cluster.ReserveRequest{SourceCapabilities: b.distributedSource.capabilities, Identity: identity})
	if err != nil {
		_, _ = placements.AbortMigration(info.ID, target.branch, requestID, time.Now())
		return fmt.Errorf("reserving sparse migration target %q: %w", targetNode, err)
	}

	execSessions, sidecarErr := b.stopMigrationBranchSidecars(target, info)
	if sidecarErr != nil {
		_, _ = peer.Cancel(ctx, requestID, tx.Revision)
		_, _ = placements.AbortMigration(info.ID, target.branch, requestID, time.Now())
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
		if _, abortErr := placements.AbortMigration(info.ID, target.branch, requestID, time.Now()); abortErr != nil {
			return fmt.Errorf("%w; restoring sparse migration source: %v; aborting placement: %v", cause, restoreErr, abortErr)
		}
		if restoreErr != nil {
			return fmt.Errorf("%w; restoring sparse migration source: %v", cause, restoreErr)
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
	output, err := b.command(
		"timetravel", "--source", target.branch,
		"--distributed-session", requestID,
		"--distributed-family", info.ID,
		"--distributed-external-listener", "--distributed-advertise", advertise,
		"--distributed-max-transfer", fmt.Sprint(dataPlaneMax),
		"--distributed-push-policy", string(tx.Negotiated.PagingPolicy.Push),
		"--distributed-push-high-water", fmt.Sprint(tx.Negotiated.PagingPolicy.MaxOutstandingBytes),
		"--distributed-leave-source-paused",
		info.ID, "distributed-capture", requestID,
	)
	if err != nil {
		return abort(fmt.Errorf("capturing sparse migration source: %w", err))
	}
	captured = true
	var session boot.DistributedForkSessionInfo
	if err := json.Unmarshal(output, &session); err != nil {
		return abort(fmt.Errorf("decoding sparse migration capture: %w", err))
	}
	if session.SessionID != requestID || session.MetadataBytes == 0 || session.StreamBytes == 0 {
		return abort(errors.New("sparse migration capture returned incomplete session metadata"))
	}
	var filesystemToken ttcvolume.RemoteToken
	copy(filesystemToken[:], session.Token[:])
	filesystemPack, filesystemBytes, err := b.prepareDistributedFilesystem(info, session.FilesystemBranch, requestID, filesystemToken, relay)
	if err != nil {
		return abort(fmt.Errorf("sealing sparse migration filesystem generation: %w", err))
	}
	descriptor := cluster.RemoteSessionDescriptor{
		Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
		MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
		MaxTransferBytes: dataPlaneMax, FilesystemMetadataPack: filesystemPack,
		FilesystemBytes: filesystemBytes,
	}
	tx, err = peer.Prepare(ctx, requestID, tx.Revision, descriptor)
	if err != nil {
		return abort(fmt.Errorf("preparing sparse migration target: %w", err))
	}
	tx, err = peer.Publish(ctx, requestID, tx.Revision, state == "paused")
	if err != nil {
		status, statusErr := peer.Status(ctx, requestID)
		if statusErr != nil || (status.State != cluster.TransactionPublished && status.State != cluster.TransactionDraining && status.State != cluster.TransactionIndependent) {
			return abort(fmt.Errorf("publishing sparse migration target: %w", err))
		}
		tx = status
	}
	if b.distributedSource == nil || b.distributedSource.authority == nil {
		return abort(errors.New("Raft migration authority is unavailable"))
	}
	cutover, err := b.distributedSource.authority.CutoverMigration(requestID, tx.Revision, info.ID, target.branch, requestID, targetNode, targetRecord.Incarnation, info.ID, target.branch)
	if err != nil {
		return abort(fmt.Errorf("publishing migration placement: %w", err))
	}
	tx = &cutover.Transaction
	published = true
	tx, err = peer.Activate(ctx, requestID, tx.Revision)
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
	go monitorBackend.handoffDistributedFork(peer, info.ID, requestID, tx.Revision, relay)
	go monitorBackend.monitorSparseMigrationCleanup(peer, info.ID, target.branch, requestID, relay)
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
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tx, err := peer.Status(ctx, transactionID)
		cancel()
		if err != nil {
			continue
		}
		switch tx.State {
		case cluster.TransactionIndependent:
			_, _ = b.command("timetravel", containerID, "distributed-complete", transactionID)
			relay.Close()
			if err := b.releaseMigratedSourceRuntime(containerID, branchID); err != nil && b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: retaining source runtime after sparse migration %s@%s: %v\n", containerID, branchID, err)
			}
			return
		case cluster.TransactionCanceled, cluster.TransactionFailed:
			// A published migration never rolls ownership backward. Retain the
			// source snapshot for diagnostics or an explicit recovery attempt.
			return
		}
	}
}

func (b *runtimeBackend) releaseMigratedSourceRuntime(containerID, branchID string) error {
	info, err := b.containerStore().LoadContainer(containerID)
	if err != nil {
		return err
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
		return nil
	}
	if physicalPeers == 0 {
		return b.purgeRuntime(info.ID)
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
		return nil
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
		info, err := b.containerStore().LoadContainer(runtimeFamily.ID)
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
// physical target on nodeID. The logical branch remains source-owned until the
// placement compare-and-swap, so BranchNodeID alone cannot identify it.
func pendingDistributedTarget(info ttcbundle.ContainerInfo, branchID, nodeID string) bool {
	for _, pending := range info.DistributedForkImports {
		if pending.Migration && pending.BranchID == branchID && pending.TargetNode == nodeID {
			return true
		}
	}
	return false
}

func (b *runtimeBackend) activeSparseMigrationSource(containerID, branchID string) bool {
	if b.dataRoot == "" || b.nodeID == "" {
		return false
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
		if !tx.Identity.Migration || tx.Identity.SourceNodeID != b.nodeID || tx.Identity.SourceContainer != containerID || tx.Identity.SourceBranch != branchID ||
			tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed {
			continue
		}
		// The live session itself is authoritative. In particular, protect the
		// narrow interval after the target records independence but before the
		// source releases its last page and filesystem references.
		if _, err := b.command("timetravel", containerID, "distributed-status", tx.Identity.TransactionID); err == nil {
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
		info, loadErr := b.containerStore().LoadContainer(placement.FamilyID)
		if loadErr != nil {
			result = errors.Join(result, fmt.Errorf("loading interrupted migration %s@%s: %w", placement.FamilyID, placement.BranchID, loadErr))
			continue
		}
		branch, branchErr := persistentBranchInfo(info, placement.BranchID)
		if branchErr != nil {
			result = errors.Join(result, fmt.Errorf("loading interrupted migration branch %s@%s: %w", placement.FamilyID, placement.BranchID, branchErr))
			continue
		}
		name := info.Name
		if name == "" {
			name = info.ID
		}
		target := selector{container: name, branch: placement.BranchID}
		actualOwner := info.BranchNodeID(placement.BranchID)
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
			runtimeState, runtimeErr := b.runtimeBranchLifecycle(info.ID, placement.BranchID)
			if placement.ResumeState == cluster.PlacementRunning && runtimeErr == nil && runtimeState == "paused" {
				if _, err := b.command("timetravel", info.ID, "resume", placement.BranchID); err != nil {
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
				branch, branchErr = persistentBranchInfo(info, placement.BranchID)
				if branchErr != nil {
					result = errors.Join(result, branchErr)
					continue
				}
				state = normalizeLifecycleState(branch.State)
			}
			runtimeState, runtimeErr = b.runtimeBranchLifecycle(info.ID, placement.BranchID)
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
			runtimeState, runtimeErr := b.runtimeBranchLifecycle(info.ID, placement.BranchID)
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
				branch, branchErr = persistentBranchInfo(info, placement.BranchID)
				if branchErr != nil {
					result = errors.Join(result, branchErr)
					continue
				}
				state = normalizeLifecycleState(branch.State)
			}
			runtimeState, runtimeErr = b.runtimeBranchLifecycle(info.ID, placement.BranchID)
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
				current.Incarnation, info.ID, placement.BranchID); err != nil {
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
	containers, err := b.containerStore().ListContainers()
	if err != nil {
		return snapshot, err
	}
	for _, info := range containers {
		name := info.Name
		if name == "" {
			name = info.ID
		}
		var local []ttcbundle.BranchInfo
		for _, branch := range info.Branches {
			state := normalizeLifecycleState(branch.State)
			if info.BranchNodeID(branch.ID) == b.nodeID && (state == "running" || state == "paused") {
				local = append(local, branch)
			}
		}
		if len(local) == 0 {
			continue
		}
		output, err := b.command("timetravel", info.ID, "stats", local[0].ID)
		if err != nil {
			return snapshot, fmt.Errorf("accounting resident memory for %q: %w", name, err)
		}
		var resident boot.FamilyResidentMemory
		if err := json.Unmarshal(output, &resident); err != nil {
			return snapshot, fmt.Errorf("decoding resident memory for %q: %w", name, err)
		}
		byID := make(map[string]boot.BranchResidentMemory, len(resident.Branches))
		for _, memory := range resident.Branches {
			byID[memory.ID] = memory
		}
		localIDs := make(map[string]string, len(local))
		for _, branch := range local {
			memory, ok := byID[branch.ID]
			if !ok {
				return snapshot, fmt.Errorf("runtime omitted resident accounting for %s@%s", name, branch.ID)
			}
			id := selector{container: name, branch: branch.ID}.String()
			localIDs[branch.ID] = id
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
	var executionErr error
	for _, move := range plan.Moves {
		target, err := parseSelector(move.Branch)
		if err != nil {
			executionErr = err
			break
		}
		if err := b.migrate(target, move.ToNode); err != nil {
			executionErr = fmt.Errorf("executing placement move %s to %s: %w", target, move.ToNode, err)
			break
		}
		if reservation := byNode[move.ToNode]; reservation != nil {
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
	if !source.LeaseActive(time.Now()) {
		return nil, fmt.Errorf("source node %q lease is expired or offline", sourceNode)
	}
	if source.State != cluster.NodeReady && source.State != cluster.NodePressured && source.State != cluster.NodeDraining {
		return nil, fmt.Errorf("node %q cannot stop from state %s", sourceNode, source.State)
	}
	if source.State != cluster.NodeDraining {
		lease := source.LeaseExpiresAt.Sub(source.UpdatedAt)
		if _, err := b.distributedSource.directory.Transition(source.NodeID, source.Incarnation, cluster.NodeDraining, time.Now(), lease); err != nil {
			return nil, fmt.Errorf("cordoning node %q: %w", sourceNode, err)
		}
	}

	switch mode {
	case "":
		if err := rejectActiveDaemonContainers(b); err != nil {
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
	if b.publicationQueue != nil && mode != "migrate" {
		if err := b.publicationQueue.Drain(context.Background()); err != nil {
			return nil, fmt.Errorf("waiting for durable branch publication: %w", err)
		}
	}
	moved, err := b.drainNode(sourceNode, "")
	if err != nil {
		return moved, err
	}
	b.daemonStopRequested = true
	return moved, nil
}

// drainNode implements the owner-local, restart-safe evacuation used by node
// stop and crash recovery. It rejects new
// placements first, then migrates active branches and transfers cold branch
// ownership. A failed step leaves the node draining so retry cannot race new
// work onto it.
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
		if err := finishElasticDrainAttempt(b.dataRoot, b.nodeID, intent.OperationID, retErr); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("recording drain outcome: %w", err))
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
	containers, err := b.containerStore().ListContainers()
	if err != nil {
		return nil, err
	}
	ownsBranches := false
	for _, info := range containers {
		for _, branch := range info.Branches {
			if info.BranchNodeID(branch.ID) == b.nodeID {
				ownsBranches = true
				break
			}
		}
		if ownsBranches {
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
	containers, err = b.containerStore().ListContainers()
	if err != nil {
		return nil, err
	}
	type work struct {
		target selector
		state  string
	}
	var pending []work
	for _, info := range containers {
		name := info.Name
		if name == "" {
			name = info.ID
		}
		for _, branch := range info.Branches {
			if info.BranchNodeID(branch.ID) == b.nodeID {
				pending = append(pending, work{target: selector{container: name, branch: branch.ID}, state: normalizeLifecycleState(branch.State)})
			}
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].target.String() < pending[j].target.String() })
	moved = append(make([]selector, 0, len(moved)+len(pending)), moved...)
	for index, item := range pending {
		destination := targets[index%len(targets)].NodeID
		switch item.state {
		case "running", "paused":
			return moved, fmt.Errorf("active branch %s remained on drained node after placement execution", item.target)
		case "created", "dehydrated", "exited", "failed", "dead":
			if err := b.containerStore().CompareAndSwapBranchNodeID(item.target.container, item.target.branch, b.nodeID, destination); err != nil {
				return moved, fmt.Errorf("transferring cold branch %s to %q: %w", item.target, destination, err)
			}
		default:
			return moved, fmt.Errorf("branch %s is transiently %s; retry the drain after it settles", item.target, item.state)
		}
		moved = append(moved, item.target)
	}
	transactionStore, err := b.transactionCatalog()
	if err != nil {
		return moved, err
	}
	if err := drainSourceDependencies(b, b.distributedSource, transactionStore, b.nodeID); err != nil {
		return moved, fmt.Errorf("draining remote page dependencies: %w", err)
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

// fenceElasticRuntimes uses SIGSTOP as the final physical lease fence. Unlike
// a logical branch pause, this freezes every branch and every in-flight Sentry
// operation in one host-kernel action. The durable PID/start-time record lets
// a later daemon resume only the exact processes it has re-authorized.
func fenceElasticRuntimes(backend *runtimeBackend, nodeID, incarnation string) error {
	families, err := backend.listRuntimeFamilies()
	if err != nil {
		return err
	}
	record := elasticFenceRecord{Version: 1, NodeID: nodeID, Incarnation: incarnation, FencedAt: time.Now().UTC()}
	for _, family := range families {
		if family.SandboxPID <= 0 {
			continue
		}
		start, err := processStartTime(family.SandboxPID)
		if err != nil {
			continue
		}
		process := elasticFencedProcess{PID: family.SandboxPID, StartTime: start, ContainerID: family.ID}
		for _, branch := range family.Branches {
			process.Branches = append(process.Branches, branch.ID)
		}
		sort.Strings(process.Branches)
		record.Processes = append(record.Processes, process)
	}
	path := elasticFencePath(backend.activeDataRoot())
	var result error
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		result = errors.Join(result, err)
	} else if err := writeDurableJSON(path, record); err != nil {
		// Failure to record resumable identity must not prevent the physical
		// fence. These Sentries will stay stopped (or be killed) for manual
		// recovery rather than execute outside their lease.
		result = errors.Join(result, fmt.Errorf("recording execution fence: %w", err))
	}
	for _, process := range record.Processes {
		if err := syscall.Kill(process.PID, syscall.SIGSTOP); err != nil && !errors.Is(err, syscall.ESRCH) {
			// Failure to stop before lease expiry must fail closed.
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
		var revoked []Branch
		for _, branch := range family.Branches {
			placement, lookupErr := backend.distributedSource.authority.LookupPlacement(family.ID, branch.ID)
			if lookupErr == nil && placement.OwnerNode == nodeID && placement.OwnerIncarnation == incarnation &&
				(placement.State == cluster.PlacementRunning || placement.State == cluster.PlacementPaused || placement.State == cluster.PlacementMigrating) {
				continue
			}
			revoked = append(revoked, branch)
		}
		if len(revoked) == 0 {
			continue
		}
		if len(revoked) == len(family.Branches) {
			if _, err := backend.command("delete", "--force", family.ID); err != nil {
				return fmt.Errorf("destroying wholly unauthorized runtime %q: %w", family.ID, err)
			}
			continue
		}
		for _, branch := range revoked {
			if branch.State == "running" || branch.State == "restarting" {
				if _, err := backend.command("timetravel", family.ID, "prepare-stop", branch.ID); err != nil {
					return fmt.Errorf("quiescing revoked branch %s@%s: %w", family.ID, branch.ID, err)
				}
			}
			if _, err := backend.command("timetravel", family.ID, "destroy", branch.ID); err != nil {
				return fmt.Errorf("destroying revoked branch %s@%s: %w", family.ID, branch.ID, err)
			}
			if placement, err := backend.distributedSource.authority.LookupPlacement(family.ID, branch.ID); err == nil && placement.State == cluster.PlacementCorrupted {
				_ = backend.containerStore().UpdateBranchState(family.ID, branch.ID, "corrupted")
			}
		}
	}
	return nil
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
			family := Family{ID: containerID}
			for _, branchID := range process.Branches {
				family.Branches = append(family.Branches, Branch{ID: branchID})
			}
			if len(family.Branches) == 0 && containerID != "" {
				// Guardian-created records lack an in-Sentry branch snapshot. Shared
				// metadata is safe to inspect without waking the process. Restrict
				// the fallback to branches whose last physical owner was this node;
				// ambiguity fails closed below.
				if info, loadErr := backend.containerStore().LoadContainer(containerID); loadErr == nil {
					for _, branch := range info.Branches {
						if info.BranchNodeID(branch.ID) == nodeID && (normalizeLifecycleState(branch.State) == "running" || normalizeLifecycleState(branch.State) == "paused") {
							family.Branches = append(family.Branches, Branch{ID: branch.ID})
						}
					}
				}
			}
			authorized := containerID != "" && fencedRuntimeAuthorized(family, nodeID, incarnation, backend.distributedSource.authority.LookupPlacement)
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

func maintainElasticNode(directory cluster.NodeCatalog, initial cluster.NodeRecord, lease time.Duration, stderr io.Writer, done <-chan struct{}, renewed func() error, fence func() error, fatal chan<- error) {
	interval := lease / 3
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	record := initial
	// This is deliberately monotonic-local, not the leader's absolute wall
	// clock deadline. Starting it before each renewal request makes network RTT
	// conservative and avoids assuming synchronized clocks for physical fencing.
	localDeadline := time.Now().Add(lease)
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			renewalStarted := time.Now()
			current, err := currentNodeMemoryBytes()
			if err == nil {
				record, err = heartbeatElasticNode(directory, record, current, now, lease)
				if err == nil && renewed != nil {
					err = renewed()
				}
				if err == nil {
					localDeadline = renewalStarted.Add(lease)
					continue
				}
			}
			if stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: elastic cluster heartbeat failed: %v\n", err)
			}
			margin := lease / 3
			if margin < 500*time.Millisecond {
				margin = 500 * time.Millisecond
			}
			if !time.Now().Before(localDeadline.Add(-margin)) {
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
		}
	}
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

func maintainElasticDrain(backend *runtimeBackend, nodeID string, lease time.Duration, stderr io.Writer, done <-chan struct{}, serialize func(func() error) error) {
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
		if intent.State == elasticDrainComplete {
			return
		}
		var moved []selector
		err = serialize(func() error {
			if _, recoverErr := backend.recoverInterruptedMigrations(); recoverErr != nil {
				return recoverErr
			}
			var drainErr error
			moved, drainErr = backend.drainNode(nodeID, intent.TargetNode)
			return drainErr
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
