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
	"encoding/binary"
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
	"time"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

type daemonClusterProxy struct {
	socket string
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
		_, abortErr := placements.AbortMigration(info.ID, target.branch, requestID, time.Now())
		if restore {
			if rehydrateErr := b.rehydrate(target); rehydrateErr != nil {
				return fmt.Errorf("%w; aborting placement: %v; restoring source: %v", cause, abortErr, rehydrateErr)
			}
		}
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
		return fmt.Errorf("migration ownership moved to %q but destination response is uncertain: %w; run `ttc rehydrate %s` to reconcile", targetNode, err, target)
	}
	if status != 0 {
		if swapErr := b.containerStore().CompareAndSwapBranchNodeID(info.ID, target.branch, targetNode, b.nodeID); swapErr != nil {
			return fmt.Errorf("destination rejected migration: %s; ownership rollback failed: %v", strings.TrimSpace(diagnostic), swapErr)
		}
		return abort(fmt.Errorf("destination rejected migration: %s", strings.TrimSpace(diagnostic)), true)
	}
	if _, err := placements.CommitMigration(info.ID, target.branch, requestID, targetNode, targetRecord.Incarnation, info.ID, target.branch, time.Now()); err != nil {
		return fmt.Errorf("destination is live but publishing placement failed: %w", err)
	}
	return nil
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

// drainNode implements a restart-safe conservative drain. It rejects new
// placements first, then migrates active branches and transfers cold branch
// ownership. A failed step leaves the node draining so retry cannot race new
// work onto it. The daemon remains available for diagnostics until explicitly
// stopped, at which point its membership record becomes offline.
func (b *runtimeBackend) drainNode(sourceNode, targetNode string, local bool) ([]selector, error) {
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
	lease := source.LeaseExpiresAt.Sub(source.UpdatedAt)
	if source.State != cluster.NodeDraining {
		if source.State != cluster.NodeReady && source.State != cluster.NodePressured {
			return nil, fmt.Errorf("node %q cannot begin drain from state %s", b.nodeID, source.State)
		}
		if _, err := b.distributedSource.directory.Transition(source.NodeID, source.Incarnation, cluster.NodeDraining, time.Now(), lease); err != nil {
			return nil, fmt.Errorf("cordoning node %q: %w", b.nodeID, err)
		}
	}
	targets, err := b.drainTargets(targetNode)
	if err != nil {
		return nil, err
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
	moved := make([]selector, 0, len(pending))
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

func heartbeatElasticNode(directory *cluster.NodeDirectory, record cluster.NodeRecord, current uint64, now time.Time, lease time.Duration) (cluster.NodeRecord, error) {
	updated, err := directory.Heartbeat(record.NodeID, record.Incarnation, current, record.IncomingReservedBytes, now, lease)
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
