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
	"fmt"
	"io"
	"math/bits"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gvisor.dev/gvisor/runsc/boot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

type daemonClusterProxy struct {
	socket string
}

type elasticCapacityRequest struct {
	Version         int       `json:"version"`
	RequestID       string    `json:"request_id"`
	Operation       string    `json:"operation"`
	Nodes           int       `json:"nodes"`
	MemoryBytesEach uint64    `json:"memory_bytes_each"`
	Architecture    string    `json:"architecture"`
	RepositoryID    string    `json:"repository_id"`
	SourceNode      string    `json:"source_node"`
	Reason          string    `json:"reason"`
	CreatedAt       time.Time `json:"created_at"`
	Deadline        time.Time `json:"deadline"`
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

// emitCapacityRequest publishes one outstanding request per node. An external
// provisioner watches this shared directory and removes the node file after it
// has durably accepted the request; a later pressure episode can then publish
// a new ID. The lock makes concurrent manual and automatic rebalance attempts
// coalesce into the same event.
func (b *runtimeBackend) emitCapacityRequest(source cluster.NodeRecord, reason string) (string, error) {
	dir := filepath.Join(b.dataRoot, "cluster", "capacity-requests")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(filepath.Join(dir, source.NodeID+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(dir, source.NodeID+".json")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	memory := source.MemoryTargetBytes
	if memory == 0 {
		memory = source.MemoryCurrentBytes
	}
	request := elasticCapacityRequest{
		Version: 1, RequestID: "capacity-" + requestID, Operation: "acquire", Nodes: 1,
		MemoryBytesEach: memory, Architecture: b.distributedSource.capabilities.Architecture,
		RepositoryID: b.distributedSource.capabilities.RepositoryID, SourceNode: source.NodeID,
		Reason: reason, CreatedAt: now, Deadline: now.Add(5 * time.Minute),
	}
	if err := writeDurableJSON(path, &request); err != nil {
		return "", err
	}
	return path, nil
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
	if dataRoot == "" {
		return "", nil
	}
	// Route node-wide mutations before taking the receiving daemon's local
	// lifecycle lock. Otherwise an arbitrary entry node can hold its lock while
	// the source calls back into it as a migration target, deadlocking cutover.
	if len(args) >= 3 && args[0] == "node" && (args[1] == "drain" || args[1] == "rebalance") {
		var positional []string
		for i := 2; i < len(args); i++ {
			switch {
			case args[i] == "--target":
				i++
			case strings.HasPrefix(args[i], "--target=") || args[i] == "--_local":
			case strings.HasPrefix(args[i], "-"):
				// Let runNode diagnose unknown flags on the selected daemon.
			default:
				positional = append(positional, args[i])
			}
		}
		if len(positional) == 1 {
			return positional[0], nil
		}
	}
	store := ttcbundle.Store{Root: dataRoot}
	owner := ""
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
		branchOwner := info.BranchNodeID(selected.branch)
		if branchOwner == "" {
			continue
		}
		if owner != "" && owner != branchOwner {
			return "", fmt.Errorf("command spans cluster nodes %q and %q", owner, branchOwner)
		}
		owner = branchOwner
	}
	return owner, nil
}

func daemonCommandNeedsPlacementAdmission(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "run", "create", "fork", "rehydrate", "restore":
		return true
	case "container":
		return len(args) > 1 && (args[1] == "run" || args[1] == "create")
	default:
		return false
	}
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
		node string
		args []string
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
		childRequest := daemonRequest{
			Version: daemonAPIVersion, Action: "command", RequestID: distributedChildRequestID(request.RequestID, i, child.args),
			Options: request.Options, Args: child.args, ClientTERM: request.ClientTERM,
			DistributedLeaf: true,
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
		record, err = completeDaemonMutation(opts.dataRoot, record, status, stdout.Bytes(), stderr.Bytes())
		if err != nil {
			return 1, stdout.String(), stderr.String(), err
		}
		return record.Status, string(record.Stdout), string(record.Stderr), nil
	}
	status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
	s.recordCommandEvents(opts.dataRoot, request.Args, status)
	return status, stdout.String(), stderr.String(), nil
}

func (b *runtimeBackend) runtimeBranchLifecycle(containerID, branchID string) (string, error) {
	output, err := b.command("timetravel", containerID, "branches")
	if err != nil {
		return "", err
	}
	var branches []runscBranchInfo
	if err := json.Unmarshal(output, &branches); err != nil {
		return "", fmt.Errorf("decoding runtime branches: %w", err)
	}
	for _, branch := range branches {
		if branch.ID == branchID {
			return normalizeLifecycleState(branch.State), nil
		}
	}
	return "", fmt.Errorf("branch %q disappeared from runtime state", branchID)
}

// migrate implements the first identity-preserving cutover path. It uses the
// durable dehydration image as the handoff, so correctness and recovery land
// before the later live sparse-transfer optimization. Branch-local ownership
// allows siblings in the same logical family to remain on their current node.
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
	placements, err := cluster.OpenPlacementStore(filepath.Join(b.dataRoot, "cluster", "placements"))
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
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	if _, err := placements.BeginMigration(info.ID, target.branch, b.nodeID, sourceRecord.Incarnation, placement.Generation, requestID, targetNode, time.Now()); err != nil {
		return err
	}
	abort := func(cause error, restore bool) error {
		if restore {
			if rehydrateErr := b.rehydrate(target); rehydrateErr != nil {
				// Leave the placement migrating. Once this incarnation loses
				// its lease, recovery can retry restoration without exposing a
				// stable placement backed by a dehydrated runtime.
				return fmt.Errorf("%w; restoring source before rollback: %v", cause, rehydrateErr)
			}
			if runtimeState, stateErr := b.runtimeBranchLifecycle(info.ID, target.branch); stateErr != nil || runtimeState != state {
				return fmt.Errorf("%w; restored source runtime is %q (want %q): %v", cause, runtimeState, state, stateErr)
			}
		}
		_, abortErr := placements.AbortMigration(info.ID, target.branch, requestID, time.Now())
		if abortErr != nil {
			return fmt.Errorf("%w; aborting placement: %v", cause, abortErr)
		}
		return cause
	}
	if err := b.dehydrate(target); err != nil {
		return abort(fmt.Errorf("dehydrating migration source: %w", err), false)
	}
	if err := b.containerStore().CompareAndSwapBranchNodeID(info.ID, target.branch, b.nodeID, targetNode); err != nil {
		return abort(fmt.Errorf("transferring branch ownership: %w", err), true)
	}
	status, _, diagnostic, err := executeRemoteDaemonCommand(context.Background(), peer, requestID, []string{"rehydrate", target.String()})
	if err != nil {
		// Delivery is uncertain. The source remains durably cold and fenced by
		// NodeID; retrying rehydrate through any daemon is safe.
		return fmt.Errorf("migration ownership moved to %q but destination response is uncertain: %w; ttcd will reconcile operation %s", targetNode, err, requestID)
	}
	if status != 0 {
		// A nonzero response is not proof that destination execution never
		// happened: the daemon may have restarted after restoring the branch,
		// leaving an indeterminate mutation journal. Once ownership CAS has
		// succeeded, never move it backward. Target-side recovery verifies the
		// runtime and rolls the fenced transaction forward.
		return fmt.Errorf("migration ownership moved to %q but destination returned %s; ttcd will reconcile operation %s", targetNode, strings.TrimSpace(diagnostic), requestID)
	}
	if _, err := placements.CommitMigration(info.ID, target.branch, requestID, targetNode, targetRecord.Incarnation, info.ID, target.branch, time.Now()); err != nil {
		return fmt.Errorf("destination is live but publishing placement failed: %w", err)
	}
	if err := b.releaseMigratedSourceRuntime(info.ID, target.branch); err != nil && b.stderr != nil {
		// Ownership and target execution are already committed. A stale source
		// shell is non-runnable, so never roll back the cutover for cleanup.
		fmt.Fprintf(b.stderr, "ttc: warning: retaining fenced source runtime for %s: %v\n", target, err)
	}
	return nil
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
		for _, branch := range runtimeFamily.Branches {
			if info.BranchNodeID(branch.ID) == b.nodeID {
				physicalLocalOwned = true
				break
			}
		}
		if !physicalLocalOwned {
			result = errors.Join(result, b.purgeRuntime(info.ID))
			continue
		}
		for _, runtimeBranch := range runtimeFamily.Branches {
			if info.BranchNodeID(runtimeBranch.ID) == b.nodeID {
				continue
			}
			if _, err := b.command("timetravel", info.ID, "destroy", runtimeBranch.ID); err != nil {
				result = errors.Join(result, fmt.Errorf("purging non-owned runtime branch %s@%s: %w", info.Name, runtimeBranch.ID, err))
			}
		}
	}
	return result
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
	placements, err := cluster.OpenPlacementStore(filepath.Join(b.dataRoot, "cluster", "placements"))
	if err != nil {
		return nil, err
	}
	records, err := placements.List()
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
			if oldOwnerActive {
				// This may be an ordinary in-flight migration before cutover.
				// Never interfere with an incarnation that still owns both the
				// placement lease and the branch.
				continue
			}
			// Only the replacement source daemon may restore and roll back a
			// source-owned transaction. This also prevents two nodes from
			// attempting to rehydrate the same branch concurrently.
			if b.nodeID != placement.OwnerNode {
				continue
			}
			current, lookupErr := b.distributedSource.directory.Lookup(b.nodeID)
			if lookupErr != nil || !current.LeaseActive(time.Now()) {
				result = errors.Join(result, fmt.Errorf("source node %q cannot recover %s: lease unavailable", b.nodeID, target))
				continue
			}
			state := normalizeLifecycleState(branch.State)
			runtimeState, runtimeErr := b.runtimeBranchLifecycle(info.ID, placement.BranchID)
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
			if _, err := placements.RecoverSourceMigration(placement.FamilyID, placement.BranchID, placement.OperationID,
				placement.OwnerNode, placement.OwnerIncarnation, current.Incarnation, placement.Generation, time.Now()); err != nil {
				result = errors.Join(result, fmt.Errorf("publishing recovered migration source %s: %w", target, err))
				continue
			}
			recovered = append(recovered, target)
		case placement.TargetNode:
			// After the ownership CAS, only the target daemon rolls forward.
			// This is safe even while the source lease remains live: source code
			// never rolls ownership backward after CAS, and the target mutation
			// lock serializes recovery with an in-flight rehydrate request.
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
			if _, err := placements.CommitMigration(placement.FamilyID, placement.BranchID, placement.OperationID,
				placement.TargetNode, current.Incarnation, info.ID, placement.BranchID, time.Now()); err != nil {
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

func (b *runtimeBackend) localPlannerResources() ([]cluster.BranchResource, []cluster.SharedResource, error) {
	containers, err := b.containerStore().ListContainers()
	if err != nil {
		return nil, nil, err
	}
	var branches []cluster.BranchResource
	var shared []cluster.SharedResource
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
			return nil, nil, fmt.Errorf("accounting resident memory for %q: %w", name, err)
		}
		var resident boot.FamilyResidentMemory
		if err := json.Unmarshal(output, &resident); err != nil {
			return nil, nil, fmt.Errorf("decoding resident memory for %q: %w", name, err)
		}
		byID := make(map[string]boot.BranchResidentMemory, len(resident.Branches))
		for _, memory := range resident.Branches {
			byID[memory.ID] = memory
		}
		if resident.SharingEdgesTruncated {
			return nil, nil, fmt.Errorf("resident sharing summary for %q exceeded its bound and omitted %d bytes", name, resident.SharingEdgesOmittedBytes)
		}
		localIDs := make(map[string]string, len(local))
		for _, branch := range local {
			memory, ok := byID[branch.ID]
			if !ok {
				return nil, nil, fmt.Errorf("runtime omitted resident accounting for %s@%s", name, branch.ID)
			}
			id := selector{container: name, branch: branch.ID}.String()
			localIDs[branch.ID] = id
			branches = append(branches, cluster.BranchResource{
				ID: id, NodeID: b.nodeID, PrivateBytes: memory.PrivateBytes, Movable: true,
			})
		}
		accountedShared := make(map[string]uint64, len(local))
		for _, edge := range resident.SharingEdges {
			ids := make([]string, 0, len(edge.Branches))
			for _, branchID := range edge.Branches {
				id, ok := localIDs[branchID]
				if !ok {
					continue
				}
				ids = append(ids, id)
				accountedShared[branchID] += edge.Bytes
			}
			if len(ids) != len(edge.Branches) {
				return nil, nil, fmt.Errorf("resident sharing edge %q for %q includes a branch outside local placement metadata", edge.ID, name)
			}
			if len(ids) > 1 && edge.Bytes != 0 {
				shared = append(shared, cluster.SharedResource{
					ID: info.ID + "@" + edge.ID, Bytes: edge.Bytes, Branches: ids,
				})
			}
		}
		for _, branch := range local {
			memory := byID[branch.ID]
			if accountedShared[branch.ID] != memory.SharedBytes {
				return nil, nil, fmt.Errorf("resident sharing summary for %s@%s accounts %d shared bytes, want %d", name, branch.ID, accountedShared[branch.ID], memory.SharedBytes)
			}
		}
	}
	return branches, shared, nil
}

func betterPlacementPlan(candidate, best cluster.PlacementPlan) bool {
	if len(best.Branches) == 0 {
		return true
	}
	if candidate.EstimatedCut != best.EstimatedCut {
		return candidate.EstimatedCut < best.EstimatedCut
	}
	if candidate.EstimatedAdded != best.EstimatedAdded {
		return candidate.EstimatedAdded < best.EstimatedAdded
	}
	return candidate.TargetNode < best.TargetNode
}

func (b *runtimeBackend) rebalanceNode(sourceNode, targetNode string, local bool) (cluster.PlacementPlan, error) {
	if b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		return cluster.PlacementPlan{}, errors.New("elastic rebalance requires dynamic cluster discovery")
	}
	if sourceNode != b.nodeID {
		if local {
			return cluster.PlacementPlan{}, fmt.Errorf("node rebalance reached %q, want %q", b.nodeID, sourceNode)
		}
		peer, err := b.distributedSource.peer(sourceNode, false)
		if err != nil {
			return cluster.PlacementPlan{}, err
		}
		requestID, err := newDaemonRequestID()
		if err != nil {
			return cluster.PlacementPlan{}, err
		}
		args := []string{"node", "rebalance", "--_local"}
		if targetNode != "" {
			args = append(args, "--target", targetNode)
		}
		args = append(args, sourceNode)
		status, output, diagnostic, err := executeRemoteDaemonCommand(context.Background(), peer, requestID, args)
		if err != nil {
			return cluster.PlacementPlan{}, err
		}
		if status != 0 {
			return cluster.PlacementPlan{}, errors.New(strings.TrimSpace(diagnostic))
		}
		var plan cluster.PlacementPlan
		if err := json.Unmarshal([]byte(output), &plan); err != nil {
			return cluster.PlacementPlan{}, fmt.Errorf("decoding remote placement plan: %w", err)
		}
		return plan, nil
	}
	source, err := b.distributedSource.directory.Lookup(sourceNode)
	if err != nil {
		return cluster.PlacementPlan{}, err
	}
	if !source.LeaseActive(time.Now()) || source.MemoryTargetBytes == 0 {
		return cluster.PlacementPlan{}, fmt.Errorf("source node %q has no active lease or memory target", sourceNode)
	}
	if source.MemoryCurrentBytes <= source.MemoryTargetBytes {
		return cluster.PlacementPlan{SourceNode: sourceNode, SourceAfterBytes: source.MemoryCurrentBytes}, nil
	}
	targets, err := b.drainTargets(targetNode)
	if err != nil {
		request, requestErr := b.emitCapacityRequest(*source, "memory-pressure")
		if requestErr != nil {
			return cluster.PlacementPlan{}, fmt.Errorf("%w; publishing capacity request: %v", err, requestErr)
		}
		return cluster.PlacementPlan{}, fmt.Errorf("%w; capacity request published at %s", err, request)
	}
	branches, shared, err := b.localPlannerResources()
	if err != nil {
		return cluster.PlacementPlan{}, err
	}
	var best cluster.PlacementPlan
	var planErr error
	for _, target := range targets {
		if target.MemoryTargetBytes == 0 {
			continue
		}
		plan, err := cluster.PlanPressureRelief(
			cluster.NodeResource{ID: source.NodeID, CurrentBytes: source.MemoryCurrentBytes, TargetBytes: source.MemoryTargetBytes},
			cluster.NodeResource{ID: target.NodeID, CurrentBytes: target.MemoryCurrentBytes, TargetBytes: target.MemoryTargetBytes, ReservedBytes: target.IncomingReservedBytes},
			branches, shared,
		)
		if err != nil {
			planErr = errors.Join(planErr, fmt.Errorf("target %s: %w", target.NodeID, err))
			continue
		}
		if len(plan.Branches) != 0 && betterPlacementPlan(plan, best) {
			best = plan
		}
	}
	if len(best.Branches) == 0 {
		request, requestErr := b.emitCapacityRequest(*source, "memory-pressure")
		if requestErr != nil {
			planErr = errors.Join(planErr, fmt.Errorf("publishing capacity request: %w", requestErr))
		}
		if planErr != nil {
			return best, fmt.Errorf("no feasible pressure-relief placement; capacity request %s: %w", request, planErr)
		}
		return best, fmt.Errorf("no movable branch can relieve memory pressure; capacity request published at %s", request)
	}
	reservationID, err := newDaemonRequestID()
	if err != nil {
		return best, err
	}
	targetRecord, err := b.distributedSource.directory.Lookup(best.TargetNode)
	if err != nil {
		return best, err
	}
	if best.EstimatedAdded != 0 {
		if _, err := b.distributedSource.directory.ReserveIncoming(best.TargetNode, targetRecord.Incarnation, reservationID, b.nodeID, best.EstimatedAdded, time.Now(), 10*time.Minute); err != nil {
			return best, fmt.Errorf("reserving %d bytes on placement target %q: %w", best.EstimatedAdded, best.TargetNode, err)
		}
	}
	var executionErr error
	moved := 0
	for _, value := range best.Branches {
		target, err := parseSelector(value)
		if err != nil {
			executionErr = err
			break
		}
		if err := b.migrate(target, best.TargetNode); err != nil {
			executionErr = fmt.Errorf("executing placement plan at %s: %w", target, err)
			break
		}
		moved++
	}
	// Successful and partially successful reservations remain until the next
	// target heartbeat publishes post-migration cgroup memory (or their TTL
	// expires after source loss). This closes the stale-load window where a
	// second source could otherwise overbook the target. If nothing moved,
	// release now.
	if best.EstimatedAdded != 0 {
		var reservationErr error
		if moved == 0 {
			_, reservationErr = b.distributedSource.directory.ReleaseIncoming(best.TargetNode, targetRecord.Incarnation, reservationID, time.Now())
		} else {
			_, reservationErr = b.distributedSource.directory.CompleteIncoming(best.TargetNode, targetRecord.Incarnation, reservationID, time.Now())
		}
		if reservationErr != nil {
			executionErr = errors.Join(executionErr, fmt.Errorf("finalizing placement reservation %s: %w", reservationID, reservationErr))
		}
	}
	if executionErr != nil {
		return best, executionErr
	}
	return best, nil
}

// drainNode implements a restart-safe conservative drain. It rejects new
// placements first, then migrates active branches and transfers cold branch
// ownership. A failed step leaves the node draining so retry cannot race new
// work onto it. The daemon remains available for diagnostics until explicitly
// stopped, at which point its membership record becomes offline.
func (b *runtimeBackend) drainNode(sourceNode, targetNode string, local bool) (moved []selector, retErr error) {
	if b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		return nil, errors.New("elastic node drain requires dynamic cluster discovery")
	}
	if sourceNode != b.nodeID {
		if local {
			return nil, fmt.Errorf("node drain reached %q, want %q", b.nodeID, sourceNode)
		}
		peer, err := b.distributedSource.peer(sourceNode, false)
		if err != nil {
			return nil, err
		}
		requestID, err := newDaemonRequestID()
		if err != nil {
			return nil, err
		}
		args := []string{"node", "drain", "--_local"}
		if targetNode != "" {
			args = append(args, "--target", targetNode)
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
				return nil, fmt.Errorf("decoding remote drain result %q: %w", line, err)
			}
			moved = append(moved, selected)
		}
		return moved, nil
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
	targets, err := b.drainTargets(targetNode)
	if err != nil {
		request, requestErr := b.emitCapacityRequest(*source, "node-drain")
		if requestErr != nil {
			return nil, fmt.Errorf("%w; publishing capacity request: %v", err, requestErr)
		}
		return nil, fmt.Errorf("%w; capacity request published at %s", err, request)
	}
	containers, err := b.containerStore().ListContainers()
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
	moved = make([]selector, 0, len(pending))
	for index, item := range pending {
		destination := targets[index%len(targets)].NodeID
		switch item.state {
		case "running", "paused":
			if err := b.migrate(item.target, destination); err != nil {
				return moved, fmt.Errorf("migrating %s to %q: %w", item.target, destination, err)
			}
		case "created", "dehydrated", "exited", "failed", "dead":
			if err := b.containerStore().CompareAndSwapBranchNodeID(item.target.container, item.target.branch, b.nodeID, destination); err != nil {
				return moved, fmt.Errorf("transferring cold branch %s to %q: %w", item.target, destination, err)
			}
		default:
			return moved, fmt.Errorf("branch %s is transiently %s; retry the drain after it settles", item.target, item.state)
		}
		moved = append(moved, item.target)
	}
	transactionStore, err := cluster.OpenStore(filepath.Join(b.dataRoot, "cluster", "transactions"))
	if err != nil {
		return moved, err
	}
	if err := drainSourceDependencies(b, b.distributedSource, transactionStore, b.nodeID); err != nil {
		return moved, fmt.Errorf("draining remote page dependencies: %w", err)
	}
	return moved, nil
}

func parseClusterAdvertise(value string) (baseURL, serverName string, err error) {
	comma := strings.LastIndexByte(value, ',')
	if comma <= 0 || comma == len(value)-1 {
		return "", "", fmt.Errorf("invalid --cluster-advertise %q; want https://HOST:PORT,SERVER_NAME", value)
	}
	baseURL, serverName = value[:comma], value[comma+1:]
	if strings.TrimSpace(serverName) != serverName || serverName == "" {
		return "", "", fmt.Errorf("invalid TLS server name in --cluster-advertise %q", value)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("invalid --cluster-advertise URL %q", baseURL)
	}
	if parsed.Port() == "" {
		return "", "", fmt.Errorf("--cluster-advertise URL %q must include a port", baseURL)
	}
	if host := parsed.Hostname(); host == "" || host == "0.0.0.0" || host == "::" {
		return "", "", fmt.Errorf("--cluster-advertise URL %q must name a reachable host", baseURL)
	}
	return baseURL, serverName, nil
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

func maintainElasticNode(directory *cluster.NodeDirectory, initial cluster.NodeRecord, lease time.Duration, stderr io.Writer, done <-chan struct{}, fatal chan<- error) {
	interval := lease / 3
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	record := initial
	lastExpiry := initial.LeaseExpiresAt
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			current, err := currentNodeMemoryBytes()
			if err == nil {
				record, err = heartbeatElasticNode(directory, record, current, now, lease)
				if err == nil {
					lastExpiry = record.LeaseExpiresAt
					continue
				}
			}
			if stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: elastic cluster heartbeat failed: %v\n", err)
			}
			if !now.Before(lastExpiry) {
				select {
				case fatal <- fmt.Errorf("elastic cluster lease expired after heartbeat failure: %w", err):
				default:
				}
				return
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
			var plan cluster.PlacementPlan
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
			if stderr != nil && len(plan.Branches) != 0 {
				fmt.Fprintf(stderr, "ttcd: elastic rebalance moved %d branches from %s to %s (estimated freed %d bytes, cut %d bytes)\n", len(plan.Branches), plan.SourceNode, plan.TargetNode, plan.EstimatedFreed, plan.EstimatedCut)
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
	for {
		select {
		case <-done:
			return
		case <-timer.C:
		}
		intent, err := loadElasticDrainIntent(backend.dataRoot, nodeID)
		if err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "ttcd: elastic drain recovery for node %s cannot read its intent: %v\n", nodeID, err)
			}
			timer.Reset(interval)
			continue
		}
		if intent == nil {
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
			moved, drainErr = backend.drainNode(nodeID, intent.TargetNode, true)
			return drainErr
		})
		if err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "ttcd: elastic drain %s for node %s will retry: %v\n", intent.OperationID, nodeID, err)
			}
			timer.Reset(interval)
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

func heartbeatElasticNode(directory *cluster.NodeDirectory, record cluster.NodeRecord, current uint64, now time.Time, lease time.Duration) (cluster.NodeRecord, error) {
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
