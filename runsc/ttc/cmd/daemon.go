// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	"gvisor.dev/gvisor/runsc/version"
)

const (
	daemonAPIVersion       = 4
	daemonFrameStdin       = byte('I')
	daemonFrameStdinAck    = byte('A')
	daemonFrameEOF         = byte('C')
	daemonFrameResize      = byte('W')
	daemonFrameStdout      = byte('O')
	daemonFrameStderr      = byte('E')
	daemonFrameStatus      = byte('X')
	daemonFrameTerminal    = byte('T')
	daemonMaxFrame         = 1 << 20
	daemonRootlessProbeArg = "_rootless-userns-probe"
)

var (
	createDaemonRootlessProbe = createRootlessProbe
	daemonEffectiveUID        = os.Geteuid
	daemonKernelRelease       = currentHostKernelRelease
)

type daemonWireOptions struct {
	Root             string `json:"root"`
	Runsc            string `json:"runsc"`
	DataRoot         string `json:"data_root"`
	ClientExecutable string `json:"client_executable"`
}

type daemonRequest struct {
	Version        int               `json:"version"`
	Action         string            `json:"action"`
	RequestID      string            `json:"request_id,omitempty"`
	ClientTERM     string            `json:"client_term,omitempty"`
	StderrTerminal bool              `json:"stderr_terminal,omitempty"`
	StderrWidth    int               `json:"stderr_width,omitempty"`
	Options        daemonWireOptions `json:"options,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Stdin          bool              `json:"stdin,omitempty"`
}

type daemonServer struct {
	listener              net.Listener
	unsafeNoHostIsolation bool
	disableHostGSO        bool
	hostKernelRelease     string
	dataRoot              string
	root                  string
	runsc                 string
	clientExecutable      string
	volumeCapability      ttcvolume.Capability
	clusterCapabilities   *cluster.Capabilities
	clusterListen         string
	distributedSource     *distributedSourceRuntime
	clusterStore          *cluster.Store

	stopOnce sync.Once

	reconcileMu sync.Mutex
	reconciled  map[string]bool

	mutationMu    sync.Mutex
	mutationLocks map[string]*sync.Mutex

	eventMu  sync.Mutex
	eventSeq uint64

	stdinMu     sync.Mutex
	stdinLeases map[string]*daemonStdinLease

	sidecarMu sync.Mutex
	sidecars  map[string]*exec.Cmd
}

type daemonClusterOptions struct {
	Configured      bool
	Listen          string
	NodeID          string
	RepositoryID    string
	CertFile        string
	KeyFile         string
	CAFile          string
	MaxTransactions int
	PageListen      string
	PageAdvertise   string
	Peers           []string
}

type daemonClusterRuntime struct {
	listen       string
	tlsConfig    *tls.Config
	controlPlane *cluster.ControlPlane
	source       *distributedSourceRuntime
}

type daemonTargetRuntime struct {
	backend *runtimeBackend
	nodeID  string
	store   *cluster.Store
	done    <-chan struct{}
	watch   sync.Map
}

func (r *daemonTargetRuntime) Reserve(ctx context.Context, tx *cluster.Transaction) error {
	if r == nil || r.backend == nil || tx == nil {
		return errors.New("distributed target runtime is unavailable")
	}
	_, err := r.backend.containerStore().PrepareDistributedFork(ctx, ttcbundle.DistributedForkOptions{
		SourceContainer: tx.Identity.SourceContainer, TargetName: tx.Identity.TargetContainer,
		TargetBranch: tx.Identity.TargetBranch, TransactionID: tx.Identity.TransactionID, NodeID: r.nodeID,
	})
	return err
}

func (r *daemonTargetRuntime) Prepare(ctx context.Context, tx *cluster.Transaction, session cluster.RemoteSessionDescriptor) error {
	info, err := r.backend.containerStore().LoadDistributedForkTarget(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	tokenBytes, err := hex.DecodeString(session.SnapshotToken)
	if err != nil || len(tokenBytes) != 16 {
		return errors.New("invalid distributed snapshot token")
	}
	var token ttcremote.SnapshotToken
	copy(token[:], tokenBytes)
	restoreArgs := boot.DistributedForkRestoreArgs{
		SessionID: tx.Identity.TransactionID, FamilyID: tx.Identity.SourceContainer,
		TargetBranchID: tx.Identity.TargetBranch, Address: session.Address, SnapshotToken: token,
		MetadataBytes: session.MetadataBytes, MaxTransferBytes: session.MaxTransferBytes,
		MaxParallel: 4, Timeout: 30 * time.Second, RetryLimit: 2, PrefetchBytes: 64 << 10,
	}
	configured := r.backend.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC)
	var runtimeState struct {
		Status string `json:"status"`
	}
	stateOutput, stateErr := r.backend.command("state", info.ID)
	if stateErr == nil {
		if err := json.Unmarshal(stateOutput, &runtimeState); err != nil {
			return fmt.Errorf("decoding target runtime state: %w", err)
		}
	} else {
		status, err := r.backend.runConfiguredCommandWithArgs(configured, "create", "--bundle", info.BundleDir, info.ID)
		if err != nil {
			return fmt.Errorf("creating distributed target runtime: %w", err)
		}
		if status != 0 {
			return fmt.Errorf("creating distributed target runtime exited with status %d", status)
		}
		runtimeState.Status = "created"
	}
	if runtimeState.Status == "created" {
		encoded, err := json.Marshal(&restoreArgs)
		if err != nil {
			return err
		}
		reader, writer, err := os.Pipe()
		if err != nil {
			return err
		}
		if _, err := writer.Write(encoded); err != nil {
			reader.Close()
			writer.Close()
			return err
		}
		if err := writer.Close(); err != nil {
			reader.Close()
			return err
		}
		connectionCount := 1 + (restoreArgs.MaxParallel+1)*(restoreArgs.RetryLimit+1)
		connectionFiles := make([]*os.File, 0, connectionCount)
		for i := 0; i < connectionCount; i++ {
			conn, err := (&net.Dialer{Timeout: restoreArgs.Timeout}).DialContext(ctx, "tcp", session.Address)
			if err != nil {
				for _, file := range connectionFiles {
					file.Close()
				}
				reader.Close()
				return fmt.Errorf("connecting donated distributed page stream %d/%d: %w", i+1, connectionCount, err)
			}
			tcp, ok := conn.(*net.TCPConn)
			if !ok {
				conn.Close()
				for _, file := range connectionFiles {
					file.Close()
				}
				reader.Close()
				return fmt.Errorf("distributed page connection is %T, want TCP", conn)
			}
			file, err := tcp.File()
			conn.Close()
			if err != nil {
				for _, opened := range connectionFiles {
					opened.Close()
				}
				reader.Close()
				return err
			}
			// The isolated Sentry intentionally treats donated streams as plain
			// byte FDs and performs no host socket control operations. Install
			// kernel-enforced per-I/O timeouts before donating the descriptor so
			// an unreachable source cannot block a guest page fault forever.
			timeout := unix.NsecToTimeval(restoreArgs.Timeout.Nanoseconds())
			if err := unix.SetsockoptTimeval(int(file.Fd()), unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
				file.Close()
				for _, opened := range connectionFiles {
					opened.Close()
				}
				reader.Close()
				return fmt.Errorf("setting donated page stream receive timeout: %w", err)
			}
			if err := unix.SetsockoptTimeval(int(file.Fd()), unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout); err != nil {
				file.Close()
				for _, opened := range connectionFiles {
					opened.Close()
				}
				reader.Close()
				return fmt.Errorf("setting donated page stream send timeout: %w", err)
			}
			connectionFiles = append(connectionFiles, file)
		}
		files := append([]*os.File{reader}, connectionFiles...)
		_, err = r.backend.commandWithFiles(files, "timetravel", "--distributed-config-fd=3",
			"--distributed-connections-start-fd=4", "--distributed-connections-count="+strconv.Itoa(len(connectionFiles)),
			info.ID, "distributed-restore", tx.Identity.TransactionID)
		reader.Close()
		for _, file := range connectionFiles {
			file.Close()
		}
		if err != nil {
			return err
		}
		status, err := r.backend.runConfiguredCommandWithArgs(configured, "start", info.ID)
		if err != nil {
			return fmt.Errorf("starting paused distributed target: %w", err)
		}
		if status != 0 {
			return fmt.Errorf("starting paused distributed target exited with status %d", status)
		}
	} else if runtimeState.Status != "running" {
		return fmt.Errorf("distributed target runtime is unexpectedly %q", runtimeState.Status)
	}
	return r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TransactionID, "paused")
}

func (r *daemonTargetRuntime) Publish(_ context.Context, tx *cluster.Transaction, paused bool) error {
	info, err := r.backend.containerStore().LoadDistributedForkTarget(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	if !paused && info.State != "running" {
		if _, err := r.backend.command("timetravel", info.ID, "resume", defaultBranchID); err != nil {
			return fmt.Errorf("resuming distributed target: %w", err)
		}
		if err := r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TransactionID, "running"); err != nil {
			return err
		}
	}
	info, err = r.backend.containerStore().PublishDistributedForkTarget(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	if _, err := r.backend.ensureConsoleDrain(selector{container: info.ID, branch: defaultBranchID}, true); err != nil {
		return fmt.Errorf("starting distributed target console drain: %w", err)
	}
	r.monitorDependency(tx.Identity.TransactionID, info.ID)
	return nil
}

func (r *daemonTargetRuntime) Drain(_ context.Context, tx *cluster.Transaction) error {
	info, err := r.backend.containerStore().LoadDistributedForkTarget(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	_, err = r.backend.command("timetravel", info.ID, "distributed-drain", defaultBranchID)
	return err
}

func (r *daemonTargetRuntime) Cancel(_ context.Context, tx *cluster.Transaction) error {
	info, err := r.backend.containerStore().LoadDistributedForkTarget(tx.Identity.TransactionID)
	if err == nil {
		_, _ = r.backend.command("delete", "--force", info.ID)
	}
	return r.backend.containerStore().RemoveDistributedForkTarget(tx.Identity.TransactionID)
}

func (*daemonTargetRuntime) MarkIndependent(context.Context, *cluster.Transaction) error { return nil }

func (r *daemonTargetRuntime) monitorDependency(transactionID, containerID string) {
	if r == nil || r.store == nil || transactionID == "" || containerID == "" {
		return
	}
	if _, loaded := r.watch.LoadOrStore(transactionID, struct{}{}); loaded {
		return
	}
	go func() {
		defer r.watch.Delete(transactionID)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		failedPolls := 0
		for {
			select {
			case <-r.done:
				return
			case <-ticker.C:
			}
			tx, err := r.store.Load(transactionID)
			if err != nil || tx.Terminal() {
				return
			}
			if tx.State != cluster.TransactionPublished && tx.State != cluster.TransactionDraining {
				continue
			}
			output, statusErr := r.backend.command("timetravel", containerID, "distributed-dependency", defaultBranchID)
			if statusErr != nil {
				failedPolls++
				if failedPolls < 3 {
					continue
				}
				if _, stateErr := r.backend.command("state", containerID); stateErr == nil {
					continue
				}
				_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionFailed,
					"dependent target exited before remote memory drain completed", time.Now())
				return
			}
			failedPolls = 0
			var status pgalloc.AsyncPagesFileLoadStats
			if err := json.Unmarshal(output, &status); err != nil {
				continue
			}
			if !status.Done {
				continue
			}
			if status.Error != "" {
				_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionFailed,
					"remote memory loader failed: "+status.Error, time.Now())
				return
			}
			_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionIndependent, "", time.Now())
			return
		}
	}()
}

type daemonFrameWriter struct {
	conn net.Conn
	mu   *sync.Mutex
	typ  byte
}

// OwnConsoleSidecar tracks and reaps one ready console follower. Followers are
// intentionally not killed when the daemon shuts down: they retain the advisory
// lock and continue draining until a restarted daemon reconciles the family.
func (s *daemonServer) OwnConsoleSidecar(key string, command *exec.Cmd) error {
	return s.ownSidecar("console", key, command)
}

// OwnPortSidecar tracks and reaps a published-port listener.
func (s *daemonServer) OwnPortSidecar(key string, command *exec.Cmd) error {
	return s.ownSidecar("port", key, command)
}

// StopPortSidecar terminates a listener owned by this daemon. The boolean is
// false when the listener survived an older daemon and must be found through
// its advisory lock instead.
func (s *daemonServer) StopPortSidecar(key string) (bool, error) {
	s.sidecarMu.Lock()
	command := s.sidecars[key]
	s.sidecarMu.Unlock()
	if command == nil || command.Process == nil {
		return false, nil
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return true, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.sidecarMu.Lock()
		owned := s.sidecars[key] == command
		s.sidecarMu.Unlock()
		if !owned {
			return true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return true, err
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.sidecarMu.Lock()
		owned := s.sidecars[key] == command
		s.sidecarMu.Unlock()
		if !owned {
			return true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true, fmt.Errorf("port sidecar %q did not exit", key)
}

func (s *daemonServer) ownSidecar(kind, key string, command *exec.Cmd) error {
	s.sidecarMu.Lock()
	if _, ok := s.sidecars[key]; ok {
		s.sidecarMu.Unlock()
		return fmt.Errorf("%s sidecar %q is already daemon-owned", kind, key)
	}
	if s.sidecars == nil {
		s.sidecars = make(map[string]*exec.Cmd)
	}
	s.sidecars[key] = command
	s.sidecarMu.Unlock()
	go func() {
		_ = command.Wait()
		s.sidecarMu.Lock()
		if s.sidecars[key] == command {
			delete(s.sidecars, key)
		}
		s.sidecarMu.Unlock()
	}()
	return nil
}

func (s *daemonServer) ownedConsoleSidecars() int {
	s.sidecarMu.Lock()
	defer s.sidecarMu.Unlock()
	return len(s.sidecars)
}

func (w daemonFrameWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, writeDaemonFrame(w.conn, w.mu, w.typ, nil)
	}
	for written := 0; written < len(data); {
		end := written + daemonMaxFrame
		if end > len(data) {
			end = len(data)
		}
		if err := writeDaemonFrame(w.conn, w.mu, w.typ, data[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return len(data), nil
}

func defaultDaemonSocket() string {
	if dir := os.Getenv("TTC_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "ttcd.sock")
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "ttc", "ttcd.sock")
	}
	return filepath.Join(os.TempDir(), "ttc-"+strconv.Itoa(os.Geteuid()), "ttcd.sock")
}

func daemonIsolationMode(unsafeNoHostIsolation bool) string {
	if unsafeNoHostIsolation {
		return "unsafe-no-host-isolation"
	}
	return "rootless"
}

func directBackend(opts options, stdin io.Reader, stdout, stderr io.Writer) *runtimeBackend {
	progressTerminal, progressWidth := terminalWriterInfo(stderr)
	return &runtimeBackend{
		root:                  opts.root,
		runsc:                 opts.runsc,
		dataRoot:              opts.dataRoot,
		unsafeNoHostIsolation: opts.unsafeNoHostIsolation,
		disableHostGSO:        opts.disableHostGSO,
		allowSlowVolumeCopy:   opts.allowSlowVolumeCopy,
		clientExecutable:      opts.clientExecutable,
		clientTERM:            os.Getenv("TERM"),
		progressTerminal:      progressTerminal,
		progressWidth:         progressWidth,
		stdin:                 stdin,
		stdout:                stdout,
		stderr:                stderr,
	}
}

func (s *daemonServer) configureVolumeBackend(backend *runtimeBackend) {
	backend.volumeCopyMode = s.volumeCapability.Mode
	backend.allowSlowVolumeCopy = s.volumeCapability.Mode == ttcvolume.CopyModeFull
}

type daemonStopTarget struct {
	selector selector
	state    string
	depth    int
}

func stopDaemonContainers(backend backend) error {
	families, err := backend.listFamilies()
	if err != nil {
		return err
	}
	var targets []daemonStopTarget
	for _, family := range families {
		parents := make(map[string]string, len(family.Branches))
		for _, branch := range family.Branches {
			parents[branch.ID] = branch.ParentID
		}
		for _, branch := range family.Branches {
			switch branch.State {
			case "running", "paused", "restarting":
			default:
				continue
			}
			depth := 0
			seen := map[string]bool{branch.ID: true}
			for parent := branch.ParentID; parent != "" && !seen[parent]; parent = parents[parent] {
				seen[parent] = true
				depth++
			}
			targets = append(targets, daemonStopTarget{
				selector: selector{container: family.ID, branch: branch.ID},
				state:    branch.State,
				depth:    depth,
			})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].selector.container != targets[j].selector.container {
			return targets[i].selector.container < targets[j].selector.container
		}
		if targets[i].selector.branch == defaultBranchID || targets[j].selector.branch == defaultBranchID {
			return targets[j].selector.branch == defaultBranchID
		}
		if targets[i].depth != targets[j].depth {
			return targets[i].depth > targets[j].depth
		}
		return targets[i].selector.branch < targets[j].selector.branch
	})
	var stopErrors []error
	for _, target := range targets {
		if target.state == "paused" {
			if err := backend.resume(target.selector); err != nil {
				stopErrors = append(stopErrors, fmt.Errorf("resuming %s before stop: %w", target.selector, err))
				continue
			}
		}
		if err := runStop([]string{target.selector.String()}, io.Discard, io.Discard, backend); err != nil {
			stopErrors = append(stopErrors, fmt.Errorf("stopping %s: %w", target.selector, err))
		}
	}
	return errors.Join(stopErrors...)
}

func drainSourceDependencies(backend *runtimeBackend, source *distributedSourceRuntime, store *cluster.Store, nodeID string) error {
	if backend == nil || source == nil || store == nil || nodeID == "" {
		return nil
	}
	transactions, err := store.List()
	if err != nil {
		return fmt.Errorf("listing distributed dependencies: %w", err)
	}
	for i := range transactions {
		tx := transactions[i]
		if tx.Identity.SourceNodeID != nodeID || !tx.SourceNeeded ||
			(tx.State != cluster.TransactionPublished && tx.State != cluster.TransactionDraining) {
			continue
		}
		peer := source.peers[tx.Identity.TargetNodeID]
		if peer == nil {
			return fmt.Errorf("distributed target node %q for transaction %q is not configured", tx.Identity.TargetNodeID, tx.Identity.TransactionID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if tx.State == cluster.TransactionPublished {
			draining, err := peer.Drain(ctx, tx.Identity.TransactionID, tx.Revision)
			if err != nil {
				if current, statusErr := peer.Status(ctx, tx.Identity.TransactionID); statusErr != nil ||
					(current.State != cluster.TransactionDraining && current.State != cluster.TransactionIndependent) {
					cancel()
					return fmt.Errorf("draining distributed target %q: %w", tx.Identity.TargetContainer, err)
				} else {
					tx = *current
				}
			} else {
				tx = *draining
			}
		}
		ticker := time.NewTicker(100 * time.Millisecond)
		for tx.State != cluster.TransactionIndependent {
			select {
			case <-ctx.Done():
				ticker.Stop()
				cancel()
				return fmt.Errorf("draining distributed target %q: %w", tx.Identity.TargetContainer, ctx.Err())
			case <-ticker.C:
			}
			current, err := peer.Status(ctx, tx.Identity.TransactionID)
			if err != nil {
				continue
			}
			tx = *current
			if tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed {
				ticker.Stop()
				cancel()
				return fmt.Errorf("distributed target %q became %s while draining: %s", tx.Identity.TargetContainer, tx.State, tx.Failure)
			}
		}
		ticker.Stop()
		cancel()
		_, _ = backend.command("timetravel", tx.Identity.SourceContainer, "distributed-complete", tx.Identity.TransactionID)
	}
	return nil
}

func wireOptions(opts options) daemonWireOptions {
	return daemonWireOptions{
		Root: opts.root, Runsc: opts.runsc, DataRoot: opts.dataRoot,
		ClientExecutable: opts.clientExecutable,
	}
}

func optionsFromWire(w daemonWireOptions) options {
	return options{
		root: w.Root, runsc: w.Runsc, dataRoot: w.DataRoot,
		clientExecutable: w.ClientExecutable,
	}
}

// optionsFromRequest applies daemon-owned defaults to fields omitted by direct
// API clients. The CLI still sends these fields for compatibility, but SDKs
// should not need to know how ttcd was installed or where it keeps runtime
// state.
func (s *daemonServer) optionsFromRequest(w daemonWireOptions) options {
	opts := optionsFromWire(w)
	if opts.root == "" {
		opts.root = s.root
	}
	if opts.runsc == "" {
		opts.runsc = s.runsc
	}
	if opts.clientExecutable == "" {
		opts.clientExecutable = s.clientExecutable
	}
	opts.unsafeNoHostIsolation = s.unsafeNoHostIsolation
	opts.disableHostGSO = s.disableHostGSO
	opts.dataRoot = s.dataRoot
	return opts
}

type hostKernelVersion struct {
	major int
	minor int
	patch int
}

func (v hostKernelVersion) lessThan(other hostKernelVersion) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	if v.minor != other.minor {
		return v.minor < other.minor
	}
	return v.patch < other.patch
}

type hostKernelCompatibility struct {
	release        string
	version        hostKernelVersion
	disableHostGSO bool
}

func parseHostKernelVersion(release string) (hostKernelVersion, error) {
	if release == "" || release[0] < '0' || release[0] > '9' {
		return hostKernelVersion{}, fmt.Errorf("invalid host kernel release %q", release)
	}
	parseComponent := func(value string) (int, string, error) {
		end := 0
		for end < len(value) && value[end] >= '0' && value[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0, value, fmt.Errorf("invalid host kernel release %q", release)
		}
		component, err := strconv.Atoi(value[:end])
		if err != nil {
			return 0, value, fmt.Errorf("invalid host kernel release %q: %w", release, err)
		}
		return component, value[end:], nil
	}
	major, remaining, err := parseComponent(release)
	if err != nil || len(remaining) == 0 || remaining[0] != '.' {
		return hostKernelVersion{}, fmt.Errorf("invalid host kernel release %q", release)
	}
	minor, remaining, err := parseComponent(remaining[1:])
	if err != nil {
		return hostKernelVersion{}, err
	}
	patch := 0
	if len(remaining) != 0 && remaining[0] == '.' {
		patch, _, err = parseComponent(remaining[1:])
		if err != nil {
			return hostKernelVersion{}, err
		}
	}
	return hostKernelVersion{major: major, minor: minor, patch: patch}, nil
}

func evaluateHostKernelCompatibility(release string) (hostKernelCompatibility, error) {
	version, err := parseHostKernelVersion(release)
	if err != nil {
		return hostKernelCompatibility{}, err
	}
	minimum := hostKernelVersion{major: 3, minor: 17}
	if version.lessThan(minimum) {
		return hostKernelCompatibility{}, fmt.Errorf("host Linux kernel %s is unsupported; TTC requires Linux 3.17 or newer", release)
	}
	hostGSOMinimum := hostKernelVersion{major: 4, minor: 14, patch: 77}
	return hostKernelCompatibility{
		release:        release,
		version:        version,
		disableHostGSO: version.lessThan(hostGSOMinimum),
	}, nil
}

func currentHostKernelRelease() (string, error) {
	var name unix.Utsname
	if err := unix.Uname(&name); err != nil {
		return "", err
	}
	var release strings.Builder
	for _, value := range name.Release {
		if value == 0 {
			break
		}
		release.WriteByte(byte(value))
	}
	if release.Len() == 0 {
		return "", errors.New("uname returned an empty host kernel release")
	}
	return release.String(), nil
}

func siblingTTCExecutable() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	if filepath.Base(executable) == "ttc" {
		return executable
	}
	sibling := filepath.Join(filepath.Dir(executable), "ttc")
	if info, err := os.Stat(sibling); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
		return sibling
	}
	return ""
}

// RunDaemon serves the versioned TTC local API. It is used by the standalone
// ttcd binary and by ttc's private daemon entry point.
func RunDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == daemonRootlessProbeArg {
		return 0
	}
	socket := defaultDaemonSocket()
	unsafeNoHostIsolation := false
	allowSlowVolumeCopy := false
	daemonDataRoot := dataRoot()
	clusterOpts := daemonClusterOptions{MaxTransactions: 32, PageListen: "0.0.0.0:0"}
	for len(args) != 0 {
		switch {
		case args[0] == "--socket" && len(args) >= 2:
			socket = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--socket="):
			socket = strings.TrimPrefix(args[0], "--socket=")
			args = args[1:]
		case args[0] == "--unsafe-no-host-isolation":
			unsafeNoHostIsolation = true
			args = args[1:]
		case args[0] == "--allow-slow-volume-copy":
			allowSlowVolumeCopy = true
			args = args[1:]
		case args[0] == "--data-root" && len(args) >= 2:
			daemonDataRoot = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--data-root="):
			daemonDataRoot = strings.TrimPrefix(args[0], "--data-root=")
			args = args[1:]
		default:
			consumed, err := consumeDaemonClusterOption(args, &clusterOpts)
			if err != nil {
				fmt.Fprintf(stderr, "ttcd: %v\n", err)
				return 2
			}
			if consumed == 0 {
				fmt.Fprintf(stderr, "ttcd: unknown option %q\n", args[0])
				return 2
			}
			args = args[consumed:]
		}
	}
	if socket == "" {
		fmt.Fprintln(stderr, "ttcd: socket path must not be empty")
		return 2
	}
	if daemonDataRoot == "" {
		fmt.Fprintln(stderr, "ttcd: data root must not be empty")
		return 2
	}
	kernelRelease, err := daemonKernelRelease()
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: detecting host Linux kernel release: %v\n", err)
		return 1
	}
	kernelCompatibility, err := evaluateHostKernelCompatibility(kernelRelease)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	if kernelCompatibility.disableHostGSO {
		fmt.Fprintf(stderr, "ttcd: warning: host Linux kernel %s predates 4.14.77; disabling host GSO for compatibility; network performance, especially for large payloads, will be reduced\n", kernelRelease)
	}
	if unsafeNoHostIsolation {
		fmt.Fprintln(stderr, "ttcd: warning: host namespace and chroot isolation is disabled; run trusted workloads only")
	} else if err := preflightDaemonIsolation(false); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	manager, err := ttcvolume.OpenManager(daemonDataRoot, allowSlowVolumeCopy)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: initializing disk-backed volumes: %v\n", err)
		return 1
	}
	clusterRuntime, err := prepareDaemonCluster(clusterOpts, daemonDataRoot)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: configuring cluster peer service: %v\n", err)
		return 1
	}
	if err := serveDaemonWithCluster(socket, unsafeNoHostIsolation, kernelCompatibility, daemonDataRoot, manager.Capability(), clusterRuntime, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	return 0
}

func consumeDaemonClusterOption(args []string, opts *daemonClusterOptions) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	setString := func(name string, dst *string) (int, error) {
		if args[0] == name {
			if len(args) < 2 || args[1] == "" {
				return 0, fmt.Errorf("%s requires a value", name)
			}
			*dst = args[1]
			return 2, nil
		}
		if strings.HasPrefix(args[0], name+"=") {
			*dst = strings.TrimPrefix(args[0], name+"=")
			if *dst == "" {
				return 0, fmt.Errorf("%s requires a value", name)
			}
			return 1, nil
		}
		return 0, nil
	}
	if consumed, err := setString("--cluster-peer", new(string)); consumed != 0 || err != nil {
		if err != nil {
			return consumed, err
		}
		var value string
		if args[0] == "--cluster-peer" {
			value = args[1]
		} else {
			value = strings.TrimPrefix(args[0], "--cluster-peer=")
		}
		opts.Peers = append(opts.Peers, value)
		opts.Configured = true
		return consumed, nil
	}
	for name, dst := range map[string]*string{
		"--cluster-listen": &opts.Listen, "--cluster-node-id": &opts.NodeID,
		"--cluster-repository-id": &opts.RepositoryID, "--cluster-cert": &opts.CertFile,
		"--cluster-key": &opts.KeyFile, "--cluster-ca": &opts.CAFile,
		"--cluster-page-listen": &opts.PageListen, "--cluster-page-advertise": &opts.PageAdvertise,
	} {
		if consumed, err := setString(name, dst); consumed != 0 || err != nil {
			if consumed != 0 {
				opts.Configured = true
			}
			return consumed, err
		}
	}
	const maxName = "--cluster-max-transactions"
	var value string
	consumed := 0
	if args[0] == maxName {
		if len(args) < 2 {
			return 0, fmt.Errorf("%s requires a value", maxName)
		}
		value, consumed = args[1], 2
	} else if strings.HasPrefix(args[0], maxName+"=") {
		value, consumed = strings.TrimPrefix(args[0], maxName+"="), 1
	}
	if consumed != 0 {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("%s must be a positive integer", maxName)
		}
		opts.MaxTransactions = parsed
		opts.Configured = true
		return consumed, nil
	}
	return 0, nil
}

func prepareDaemonCluster(opts daemonClusterOptions, dataRoot string) (*daemonClusterRuntime, error) {
	if !opts.Configured {
		return nil, nil
	}
	if opts.Listen == "" || opts.NodeID == "" || opts.RepositoryID == "" || opts.CertFile == "" || opts.KeyFile == "" || opts.CAFile == "" {
		return nil, errors.New("cluster mode requires listen address, node ID, repository ID, certificate, key, and CA")
	}
	if opts.PageListen == "" {
		return nil, errors.New("cluster page listen address is empty")
	}
	if len(opts.Peers) != 0 && opts.PageAdvertise == "" {
		return nil, errors.New("cluster nodes with peers require --cluster-page-advertise HOST:PORT (PORT may be 0)")
	}
	tlsConfig, err := cluster.LoadServerTLSConfig(opts.CertFile, opts.KeyFile, opts.CAFile)
	if err != nil {
		return nil, err
	}
	store, err := cluster.OpenStore(filepath.Join(dataRoot, "cluster", "transactions"))
	if err != nil {
		return nil, err
	}
	capabilities := cluster.Capabilities{
		NodeID: opts.NodeID, ProtocolMin: cluster.ControlProtocolVersion, ProtocolMax: cluster.ControlProtocolVersion,
		BuildID: version.Version(), Architecture: runtime.GOARCH, BasePageSize: hostarch.PageSize,
		MaxTransferSize: 2 << 20, RepositoryID: opts.RepositoryID,
		Features: []string{
			cluster.FeatureRemotePull, cluster.FeatureReversePush, cluster.FeatureAckBatch,
			cluster.FeatureNetworkRecreate, cluster.FeatureRuntimePrefetch, cluster.FeatureFilesystemBranch,
		},
	}
	if err := capabilities.Validate(); err != nil {
		return nil, err
	}
	clients := make(map[string]*cluster.Client, len(opts.Peers))
	for _, peerSpec := range opts.Peers {
		nodeID, baseURL, serverName, err := parseClusterPeer(peerSpec)
		if err != nil {
			return nil, err
		}
		if nodeID == opts.NodeID {
			return nil, fmt.Errorf("cluster peer %q is the local node", nodeID)
		}
		if _, ok := clients[nodeID]; ok {
			return nil, fmt.Errorf("cluster peer %q is configured more than once", nodeID)
		}
		clientTLS, err := cluster.LoadClientTLSConfig(opts.CertFile, opts.KeyFile, opts.CAFile, serverName)
		if err != nil {
			return nil, fmt.Errorf("configuring cluster peer %q TLS: %w", nodeID, err)
		}
		client, err := cluster.NewClient(baseURL, clientTLS, 2*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("configuring cluster peer %q: %w", nodeID, err)
		}
		clients[nodeID] = client
	}
	return &daemonClusterRuntime{
		listen: opts.Listen, tlsConfig: tlsConfig,
		controlPlane: &cluster.ControlPlane{
			Capabilities: capabilities, Store: store, MaxTransactions: opts.MaxTransactions, Now: time.Now,
		},
		source: &distributedSourceRuntime{
			capabilities: capabilities, peers: clients,
			pageListen: opts.PageListen, pageAdvertise: opts.PageAdvertise,
		},
	}, nil
}

func parseClusterPeer(value string) (nodeID, baseURL, serverName string, err error) {
	equals := strings.IndexByte(value, '=')
	comma := strings.LastIndexByte(value, ',')
	if equals <= 0 || comma <= equals+1 || comma == len(value)-1 {
		return "", "", "", fmt.Errorf("invalid --cluster-peer %q; want NODE=https://HOST:PORT,SERVER_NAME", value)
	}
	nodeID = value[:equals]
	baseURL = value[equals+1 : comma]
	serverName = value[comma+1:]
	if strings.TrimSpace(nodeID) != nodeID || strings.TrimSpace(serverName) != serverName {
		return "", "", "", fmt.Errorf("invalid whitespace in --cluster-peer %q", value)
	}
	return nodeID, baseURL, serverName, nil
}

func preflightDaemonIsolation(unsafeNoHostIsolation bool) error {
	if unsafeNoHostIsolation || daemonEffectiveUID() == 0 {
		return nil
	}
	if err := createDaemonRootlessProbe(); err != nil {
		hint := "permit unprivileged user namespaces in the host kernel and LSM policy"
		if data, readErr := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); readErr == nil && strings.TrimSpace(string(data)) == "1" {
			hint = "the host AppArmor policy restricts unprivileged user namespaces; grant userns permission to TTC/runsc"
		}
		return fmt.Errorf("rootless mode is unavailable: creating a disposable user/mount namespace: %w; %s, or restart with --unsafe-no-host-isolation for trusted workloads", err, hint)
	}
	return nil
}

func createRootlessProbe() error {
	command := exec.Command("/proc/self/exe", daemonRootlessProbeArg)
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}

func secureDaemonDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checking socket directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("socket parent %q is not a directory", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("socket directory %q is owned by UID %d, not %d", dir, stat.Uid, os.Geteuid())
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("socket directory %q permissions %04o allow access by other users", dir, info.Mode().Perm())
	}
	return nil
}

func serveDaemon(socket string, unsafeNoHostIsolation bool, kernelCompatibility hostKernelCompatibility, dataRoot string, volumeCapability ttcvolume.Capability, stdout, stderr io.Writer) error {
	return serveDaemonWithCluster(socket, unsafeNoHostIsolation, kernelCompatibility, dataRoot, volumeCapability, nil, stdout, stderr)
}

func serveDaemonWithCluster(socket string, unsafeNoHostIsolation bool, kernelCompatibility hostKernelCompatibility, dataRoot string, volumeCapability ttcvolume.Capability, clusterRuntime *daemonClusterRuntime, stdout, stderr io.Writer) error {
	dir := filepath.Dir(socket)
	if err := secureDaemonDirectory(dir); err != nil {
		return err
	}
	startLock, err := os.OpenFile(socket+".start.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening daemon startup lock: %w", err)
	}
	defer startLock.Close()
	if err := syscall.Flock(int(startLock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking daemon startup: %w", err)
	}
	if conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("daemon is already listening on %s", socket)
	}
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socket, err)
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0600); err != nil {
		return fmt.Errorf("securing daemon socket: %w", err)
	}
	if err := syscall.Flock(int(startLock.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlocking daemon startup: %w", err)
	}
	server := &daemonServer{
		listener:              listener,
		unsafeNoHostIsolation: unsafeNoHostIsolation,
		disableHostGSO:        kernelCompatibility.disableHostGSO,
		hostKernelRelease:     kernelCompatibility.release,
		dataRoot:              dataRoot,
		root:                  runtimeRoot(),
		runsc:                 runscPath(),
		clientExecutable:      siblingTTCExecutable(),
		volumeCapability:      volumeCapability,
		reconciled:            make(map[string]bool),
		mutationLocks:         make(map[string]*sync.Mutex),
	}
	clusterDone := make(chan struct{})
	defer close(clusterDone)
	var clusterServer *http.Server
	clusterErr := make(chan error, 1)
	if clusterRuntime != nil {
		targetBackend := directBackend(options{
			root: server.root, runsc: server.runsc, dataRoot: server.dataRoot,
			unsafeNoHostIsolation: server.unsafeNoHostIsolation,
			disableHostGSO:        server.disableHostGSO,
		}, strings.NewReader(""), io.Discard, io.Discard)
		server.configureVolumeBackend(targetBackend)
		targetBackend.consoleSidecars = server
		targetBackend.nodeID = clusterRuntime.controlPlane.Capabilities.NodeID
		targetRuntime := &daemonTargetRuntime{
			backend: targetBackend, nodeID: clusterRuntime.controlPlane.Capabilities.NodeID,
			store: clusterRuntime.controlPlane.Store, done: clusterDone,
		}
		clusterRuntime.controlPlane.TargetRuntime = targetRuntime
		clusterListener, err := net.Listen("tcp", clusterRuntime.listen)
		if err != nil {
			return fmt.Errorf("listening for cluster peers on %s: %w", clusterRuntime.listen, err)
		}
		clusterRuntime.listen = clusterListener.Addr().String()
		server.clusterCapabilities = &clusterRuntime.controlPlane.Capabilities
		server.clusterStore = clusterRuntime.controlPlane.Store
		server.clusterListen = clusterRuntime.listen
		server.distributedSource = clusterRuntime.source
		clusterServer = &http.Server{
			Handler: clusterRuntime.controlPlane, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute,
		}
		tlsListener := tls.NewListener(clusterListener, clusterRuntime.tlsConfig)
		go func() {
			err := clusterServer.Serve(tlsListener)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			clusterErr <- err
			listener.Close()
		}()
		defer clusterServer.Close()
		resumeDistributedForkMonitors(targetRuntime, targetBackend, clusterRuntime)
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "ttcd listening on %s (API v%d, isolation=%s, volume-copy=%s)\n", socket, daemonAPIVersion, daemonIsolationMode(unsafeNoHostIsolation), volumeCapability.Mode)
		if clusterRuntime != nil {
			fmt.Fprintf(stdout, "ttcd cluster node %s listening on %s (repository=%s)\n", clusterRuntime.controlPlane.Capabilities.NodeID, clusterRuntime.listen, clusterRuntime.controlPlane.Capabilities.RepositoryID)
		}
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				if clusterServer != nil {
					select {
					case clusterServeErr := <-clusterErr:
						if clusterServeErr != nil {
							return fmt.Errorf("serving cluster peers: %w", clusterServeErr)
						}
					default:
					}
				}
				return nil
			}
			return fmt.Errorf("accepting client: %w", err)
		}
		go server.handle(conn, stderr)
	}
}

func checkDaemonPeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("non-Unix daemon connection %T", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var credential *syscall.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	if credential == nil {
		return errors.New("daemon peer credentials are unavailable")
	}
	if int(credential.Uid) != os.Geteuid() {
		return fmt.Errorf("daemon peer UID %d does not match server UID %d", credential.Uid, os.Geteuid())
	}
	return nil
}

func (s *daemonServer) handle(conn net.Conn, serverErrors io.Writer) {
	defer conn.Close()
	if err := checkDaemonPeer(conn); err != nil {
		if serverErrors != nil {
			fmt.Fprintf(serverErrors, "ttcd: rejecting peer: %v\n", err)
		}
		return
	}
	reader := bufio.NewReader(conn)
	header, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	var request daemonRequest
	if err := json.Unmarshal([]byte(header), &request); err != nil {
		return
	}
	var writeMu sync.Mutex
	writeError := func(err error) {
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStderr, []byte("ttcd: "+err.Error()+"\n"))
		_ = writeDaemonStatus(conn, &writeMu, 1)
	}
	if request.Version != daemonAPIVersion {
		writeError(fmt.Errorf("unsupported API version %d (server uses %d)", request.Version, daemonAPIVersion))
		return
	}
	switch request.Action {
	case "ping":
		status := map[string]any{
			"version":                  daemonAPIVersion,
			"pid":                      os.Getpid(),
			"isolation_mode":           daemonIsolationMode(s.unsafeNoHostIsolation),
			"unsafe_no_host_isolation": s.unsafeNoHostIsolation,
			"host_kernel_release":      s.hostKernelRelease,
			"host_gso_enabled":         !s.disableHostGSO,
			"data_root":                s.dataRoot,
			"volume_copy_mode":         s.volumeCapability.Mode,
			"volume_filesystem_type":   s.volumeCapability.FilesystemType,
			"volume_device":            s.volumeCapability.Device,
		}
		if s.clusterCapabilities != nil {
			status["cluster"] = map[string]any{
				"listen": s.clusterListen, "capabilities": s.clusterCapabilities,
			}
		}
		payload, _ := json.Marshal(status)
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStdout, append(payload, '\n'))
		_ = writeDaemonStatus(conn, &writeMu, 0)
		return
	case "stop":
		if request.Options != (daemonWireOptions{}) {
			opts := s.optionsFromRequest(request.Options)
			backend := directBackend(opts, strings.NewReader(""), io.Discard,
				daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
			s.configureVolumeBackend(backend)
			backend.consoleSidecars = s
			if s.clusterCapabilities != nil {
				backend.nodeID = s.clusterCapabilities.NodeID
			}
			unlock := s.lockDataRoot(opts.dataRoot)
			var err error
			if s.clusterCapabilities != nil {
				err = drainSourceDependencies(backend, s.distributedSource, s.clusterStore, s.clusterCapabilities.NodeID)
			}
			if err == nil {
				err = stopDaemonContainers(backend)
			}
			unlock()
			if err != nil {
				writeError(fmt.Errorf("stopping managed containers: %w", err))
				return
			}
		}
		_ = writeDaemonStatus(conn, &writeMu, 0)
		s.stopOnce.Do(func() { s.listener.Close() })
		return
	case "mutation-control":
		status := s.handleMutationControl(request.Options.DataRoot, request.Args, daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStdout}, daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
		_ = writeDaemonStatus(conn, &writeMu, status)
		return
	case "command":
	default:
		writeError(fmt.Errorf("unknown daemon action %q", request.Action))
		return
	}
	if len(request.Args) == 0 {
		writeError(errors.New("empty command"))
		return
	}

	stdinReader, stdinWriter := io.Pipe()
	if !request.Stdin {
		stdinWriter.Close()
	}
	terminalInput := newDaemonTerminalInput(stdinReader, conn, &writeMu)
	go receiveDaemonInput(reader, stdinWriter, request.Stdin, terminalInput)

	opts := s.optionsFromRequest(request.Options)
	cleanArgs, leaseKey, takeover, err := daemonAttachStdinLease(opts, request.Args, request.Stdin)
	if err != nil {
		stdinReader.Close()
		writeError(err)
		return
	}
	request.Args = cleanArgs
	if leaseKey != "" {
		release, err := s.acquireStdinLease(leaseKey, takeover, func() {
			_ = stdinWriter.CloseWithError(errDetached)
			_ = conn.Close()
		})
		if err != nil {
			stdinReader.Close()
			writeError(err)
			return
		}
		defer release()
	}
	backend := directBackend(opts, terminalInput,
		daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStdout},
		daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
	s.configureVolumeBackend(backend)
	backend.clientTERM = request.ClientTERM
	backend.progressTerminal = request.StderrTerminal
	backend.progressWidth = request.StderrWidth
	backend.consoleSidecars = s
	backend.distributedSource = s.distributedSource
	if s.clusterCapabilities != nil {
		backend.nodeID = s.clusterCapabilities.NodeID
	}
	if request.Args[0] == "events" {
		status := 0
		if err := s.streamEvents(opts.dataRoot, request.Args[1:], backend.stdout); err != nil {
			fmt.Fprintf(backend.stderr, "ttc: %v\n", err)
			status = 1
		}
		stdinReader.Close()
		_ = writeDaemonStatus(conn, &writeMu, status)
		return
	}
	s.reconcile(backend)
	unlock := s.lockMutation(opts.dataRoot, request.Args)
	if request.RequestID != "" && daemonCommandNeedsMutationJournal(request.Args) {
		record, replay, err := beginDaemonMutation(opts.dataRoot, request)
		if err != nil {
			unlock()
			stdinReader.Close()
			writeError(err)
			return
		}
		if replay {
			unlock()
			stdinReader.Close()
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
			return
		}
		var mutationStdout, mutationStderr bytes.Buffer
		mutationBackend := directBackend(opts, stdinReader, &mutationStdout, &mutationStderr)
		s.configureVolumeBackend(mutationBackend)
		mutationBackend.consoleSidecars = s
		mutationBackend.distributedSource = s.distributedSource
		if s.clusterCapabilities != nil {
			mutationBackend.nodeID = s.clusterCapabilities.NodeID
		}
		status := runCommand(request.Args, mutationBackend.stdout, mutationBackend.stderr, mutationBackend)
		s.recordCommandEvents(opts.dataRoot, request.Args, status)
		completed, err := completeDaemonMutation(opts.dataRoot, record, status, mutationStdout.Bytes(), mutationStderr.Bytes())
		if err != nil {
			fmt.Fprintf(&mutationStderr, "ttcd: %v; command outcome is indeterminate\n", err)
			status = 1
		} else {
			record = completed
		}
		unlock()
		stdinReader.Close()
		if err == nil {
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
		} else {
			writeDaemonBufferedResult(conn, &writeMu, mutationStdout.Bytes(), mutationStderr.Bytes(), status)
		}
		return
	}
	status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
	s.recordCommandEvents(opts.dataRoot, request.Args, status)
	unlock()
	stdinReader.Close()
	_ = writeDaemonStatus(conn, &writeMu, status)
}

func writeDaemonBufferedResult(conn net.Conn, writeMu *sync.Mutex, stdout, stderr []byte, status int) {
	stdoutWriter := daemonFrameWriter{conn: conn, mu: writeMu, typ: daemonFrameStdout}
	stderrWriter := daemonFrameWriter{conn: conn, mu: writeMu, typ: daemonFrameStderr}
	_, _ = stdoutWriter.Write(stdout)
	_, _ = stderrWriter.Write(stderr)
	_ = writeDaemonStatus(conn, writeMu, status)
}

func daemonCommandNeedsMutationLock(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" {
		args = args[1:]
		if len(args) == 0 {
			return false
		}
	}
	switch args[0] {
	case "create", "pull", "rmi", "rm", "_auto-remove", "pause", "resume", "unpause", "kill", "stop", "restart", "fork", "checkpoint", "restore":
		return true
	case "cp":
		_, _, _, err := parseCopyCommandArgs(args[1:])
		return err == nil
	case "run":
		return daemonCommandBoolFlag(args[1:], 'd', "--detach")
	case "start":
		return !daemonCommandBoolFlag(args[1:], 'a', "--attach")
	case "image":
		return len(args) > 1 && (args[1] == "pull" || args[1] == "rm" || args[1] == "remove")
	case "branch":
		return len(args) > 1 && (args[1] == "rm" || args[1] == "remove")
	case "port":
		return len(args) > 1 && (args[1] == "publish" || args[1] == "unpublish")
	default:
		return false
	}
}

// daemonCommandNeedsMutationJournal identifies state-changing commands whose
// small output can be durably buffered for retry. Container-to-host cp still
// takes the lifecycle lock, but its arbitrarily large archive is streamed.
func daemonCommandNeedsMutationJournal(args []string) bool {
	if !daemonCommandNeedsMutationLock(args) {
		return false
	}
	if len(args) != 0 && args[0] == "container" {
		args = args[1:]
	}
	// Explicit image pulls are content-addressed and naturally idempotent.
	// Stream their progress live instead of buffering it for mutation replay.
	if len(args) != 0 && args[0] == "pull" {
		return false
	}
	if len(args) > 1 && args[0] == "image" && args[1] == "pull" {
		return false
	}
	// Checkpoint publishes a caller-named host file and therefore cannot be
	// safely replayed by the data-root mutation journal.
	if len(args) != 0 && args[0] == "checkpoint" {
		return false
	}
	if len(args) == 0 || args[0] != "cp" {
		return true
	}
	_, _, destination, err := parseCopyCommandArgs(args[1:])
	if err != nil {
		return false
	}
	_, destinationContainer, err := splitCopyContainerEndpoint(destination)
	return err == nil && destinationContainer
}

func daemonCommandBoolFlag(args []string, short byte, long string) bool {
	value := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == long {
			value = true
			continue
		}
		if strings.HasPrefix(arg, long+"=") {
			if parsed, err := strconv.ParseBool(strings.TrimPrefix(arg, long+"=")); err == nil {
				value = parsed
			}
			continue
		}
		if len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.ContainsRune(arg[1:], rune(short)) {
			value = true
		}
	}
	return value
}

func (s *daemonServer) lockMutation(dataRoot string, args []string) func() {
	if !daemonCommandNeedsMutationLock(args) {
		return func() {}
	}
	return s.lockDataRoot(dataRoot)
}

func (s *daemonServer) lockDataRoot(dataRoot string) func() {
	key := filepath.Clean(dataRoot)
	if absolute, err := filepath.Abs(key); err == nil {
		key = absolute
	}
	s.mutationMu.Lock()
	if s.mutationLocks == nil {
		s.mutationLocks = make(map[string]*sync.Mutex)
	}
	lock := s.mutationLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.mutationLocks[key] = lock
	}
	s.mutationMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func receiveDaemonInput(reader *bufio.Reader, writer *io.PipeWriter, allowStdin bool, terminal *daemonTerminalInput) {
	var stdinQueue *daemonStdinQueue
	if allowStdin {
		stdinQueue = newDaemonStdinQueue(writer, func(bytes int) error {
			return writeDaemonFrame(terminal.conn, terminal.writeMu, daemonFrameStdinAck, encodeDaemonStdinAcknowledgement(bytes))
		})
	}
	stdinOpen := allowStdin
	fail := func(err error) {
		if stdinQueue != nil {
			stdinQueue.closeWithError(err)
		}
	}
	for {
		typ, payload, err := readDaemonFrame(reader)
		if err != nil {
			fail(err)
			return
		}
		switch typ {
		case daemonFrameStdin:
			if !stdinOpen {
				fail(errors.New("unexpected stdin frame"))
				return
			}
			if !stdinQueue.enqueue(payload) {
				fail(fmt.Errorf("daemon stdin exceeded the %d-byte flow-control window", daemonStdinWindowBytes))
				return
			}
		case daemonFrameEOF:
			if stdinOpen {
				stdinOpen = false
				stdinQueue.closeWithError(nil)
			}
		case daemonFrameResize:
			rows, cols, err := decodeDaemonTerminalSize(payload)
			if err != nil {
				fail(err)
				return
			}
			terminal.updateSize(rows, cols)
		default:
			fail(fmt.Errorf("unexpected client frame %q", typ))
			return
		}
	}
}

func (s *daemonServer) reconcile(backend *runtimeBackend) {
	if backend.dataRoot == "" {
		return
	}
	key := backend.dataRoot + "\x00" + backend.root
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if s.reconciled[key] {
		return
	}
	// listFamilies reconciles stale persisted running/paused state against runsc.
	families, err := backend.listFamilies()
	if err != nil {
		return
	}
	for _, family := range families {
		container, containerErr := backend.containerStore().LoadContainer(family.ID)
		for _, branch := range family.Branches {
			if branch.State != "running" && branch.State != "paused" {
				continue
			}
			target := selector{container: family.ID, branch: branch.ID}
			_, _ = backend.ensureConsoleDrain(target, false)
			if containerErr == nil {
				_ = backend.ensurePublishedPortsForBranch(container, branch.ID)
			}
			sessions, err := backend.execSessions(target)
			if err != nil {
				continue
			}
			for _, session := range sessions {
				if session.State == "running" {
					_, _ = backend.ensureExecConsoleDrain(target, session.ID, session.ConsoleID, false)
				}
			}
		}
	}
	s.reconciled[key] = true
}

func writeDaemonFrame(writer io.Writer, mu *sync.Mutex, typ byte, payload []byte) error {
	if len(payload) > daemonMaxFrame {
		return fmt.Errorf("daemon frame is too large: %d", len(payload))
	}
	header := [5]byte{typ}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	mu.Lock()
	defer mu.Unlock()
	if err := writeDaemonBytes(writer, header[:]); err != nil {
		return err
	}
	return writeDaemonBytes(writer, payload)
}

func writeDaemonBytes(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func readDaemonFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > daemonMaxFrame {
		return 0, nil, fmt.Errorf("daemon frame is too large: %d", length)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func writeDaemonStatus(writer io.Writer, mu *sync.Mutex, status int) error {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(status))
	return writeDaemonFrame(writer, mu, daemonFrameStatus, payload[:])
}

func daemonCommandUsesStdin(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" {
		args = args[1:]
		if len(args) == 0 {
			return false
		}
	}
	switch args[0] {
	case "cp":
		_, source, _, err := parseCopyCommandArgs(args[1:])
		return err == nil && source == "-"
	case "attach", "run", "start":
	case "exec":
		if len(args) > 1 && (args[1] == "ls" || args[1] == "wait" || args[1] == "kill") {
			return false
		}
	default:
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--" {
			break
		}
		if arg == "-i" || arg == "--interactive" {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(strings.TrimPrefix(arg, "-"), "i") {
			return true
		}
	}
	return false
}

func runViaDaemon(opts options, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	stderrTerminal, stderrWidth := terminalWriterInfo(stderr)
	request := daemonRequest{
		Version: daemonAPIVersion, Action: "command", Options: wireOptions(opts),
		Args: append([]string(nil), args...), Stdin: daemonCommandUsesStdin(args), ClientTERM: os.Getenv("TERM"),
		StderrTerminal: stderrTerminal, StderrWidth: stderrWidth,
	}
	if daemonCommandNeedsMutationJournal(args) && opts.dataRoot != "" {
		requestID, err := newDaemonRequestID()
		if err != nil {
			return 1, err
		}
		request.RequestID = requestID
	}
	attempts := 1
	if request.RequestID != "" && !request.Stdin {
		attempts = 2
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		conn, err := connectRunningDaemon(opts.daemonSocket)
		if err != nil {
			return 1, err
		}
		commandStdout, commandStderr := stdout, stderr
		var bufferedStdout, bufferedStderr bytes.Buffer
		if request.RequestID != "" {
			commandStdout, commandStderr = &bufferedStdout, &bufferedStderr
		}
		status, err := exchangeDaemon(conn, request, stdin, commandStdout, commandStderr)
		conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if request.RequestID != "" {
			if _, err := stdout.Write(bufferedStdout.Bytes()); err != nil {
				return 1, err
			}
			if _, err := stderr.Write(bufferedStderr.Bytes()); err != nil {
				return 1, err
			}
		}
		return status, nil
	}
	if request.RequestID != "" {
		return 1, fmt.Errorf("daemon mutation request %s: %w", request.RequestID, lastErr)
	}
	return 1, lastErr
}

func connectRunningDaemon(socket string) (net.Conn, error) {
	conn, err := connectDaemon(socket)
	if err != nil {
		return nil, fmt.Errorf("ttcd is not running; start it explicitly with 'ttc daemon start': %w", err)
	}
	return conn, nil
}

func connectDaemon(socket string) (net.Conn, error) {
	return net.DialTimeout("unix", socket, 250*time.Millisecond)
}

func exchangeDaemon(conn net.Conn, request daemonRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	header, err := json.Marshal(request)
	if err != nil {
		return 1, err
	}
	header = append(header, '\n')
	var writeMu sync.Mutex
	writeMu.Lock()
	_, err = conn.Write(header)
	writeMu.Unlock()
	if err != nil {
		return 1, err
	}
	stdinFlow := newDaemonStdinFlow()
	defer stdinFlow.close()
	if request.Stdin {
		go func() {
			buffer := make([]byte, 32<<10)
			for {
				n, readErr := stdin.Read(buffer)
				if n != 0 {
					if err := stdinFlow.reserve(n); err != nil {
						return
					}
					if err := writeDaemonFrame(conn, &writeMu, daemonFrameStdin, buffer[:n]); err != nil {
						return
					}
				}
				if readErr != nil {
					_ = writeDaemonFrame(conn, &writeMu, daemonFrameEOF, nil)
					return
				}
			}
		}()
	}

	terminal := newDaemonClientTerminal(stdin, func(typ byte, payload []byte) error {
		return writeDaemonFrame(conn, &writeMu, typ, payload)
	})
	defer terminal.Close()

	reader := bufio.NewReader(conn)
	for {
		typ, payload, err := readDaemonFrame(reader)
		if err != nil {
			return 1, err
		}
		switch typ {
		case daemonFrameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return 1, err
			}
		case daemonFrameStderr:
			if _, err := stderr.Write(payload); err != nil {
				return 1, err
			}
		case daemonFrameStdinAck:
			bytes, err := decodeDaemonStdinAcknowledgement(payload)
			if err != nil {
				return 1, err
			}
			if err := stdinFlow.acknowledge(bytes); err != nil {
				return 1, err
			}
		case daemonFrameTerminal:
			if err := terminal.configure(payload); err != nil {
				return 1, err
			}
		case daemonFrameStatus:
			if len(payload) != 4 {
				return 1, fmt.Errorf("invalid daemon status frame length %d", len(payload))
			}
			return int(binary.BigEndian.Uint32(payload)), nil
		default:
			return 1, fmt.Errorf("unexpected daemon frame %q", typ)
		}
	}
}

func daemonLogPath(socket string) string {
	return filepath.Join(filepath.Dir(socket), "ttcd.log")
}

func startDaemonProcess(socket, dataRoot string, unsafeNoHostIsolation, allowSlowVolumeCopy bool, clusterOpts daemonClusterOptions) (string, <-chan error, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return "", nil, fmt.Errorf("creating daemon directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("finding ttc executable: %w", err)
	}
	commandName := executable
	commandArgs := []string{"_daemon-serve", "--socket", socket, "--data-root", dataRoot}
	if sibling := filepath.Join(filepath.Dir(executable), "ttcd"); sibling != executable {
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			commandName = sibling
			commandArgs = []string{"--socket", socket, "--data-root", dataRoot}
		}
	}
	if unsafeNoHostIsolation {
		commandArgs = append(commandArgs, "--unsafe-no-host-isolation")
	}
	if allowSlowVolumeCopy {
		commandArgs = append(commandArgs, "--allow-slow-volume-copy")
	}
	if clusterOpts.Configured {
		commandArgs = append(commandArgs,
			"--cluster-listen", clusterOpts.Listen,
			"--cluster-node-id", clusterOpts.NodeID,
			"--cluster-repository-id", clusterOpts.RepositoryID,
			"--cluster-cert", clusterOpts.CertFile,
			"--cluster-key", clusterOpts.KeyFile,
			"--cluster-ca", clusterOpts.CAFile,
			"--cluster-max-transactions", strconv.Itoa(clusterOpts.MaxTransactions),
			"--cluster-page-listen", clusterOpts.PageListen,
		)
		if clusterOpts.PageAdvertise != "" {
			commandArgs = append(commandArgs, "--cluster-page-advertise", clusterOpts.PageAdvertise)
		}
		for _, peer := range clusterOpts.Peers {
			commandArgs = append(commandArgs, "--cluster-peer", peer)
		}
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return "", nil, err
	}
	defer devNull.Close()
	logPath := daemonLogPath(socket)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("opening ttcd log %q: %w", logPath, err)
	}
	defer logFile.Close()
	command := exec.Command(commandName, commandArgs...)
	command.Stdin = devNull
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return logPath, nil, fmt.Errorf("starting ttcd: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
		close(done)
	}()
	return logPath, done, nil
}

func reportDaemonStartupFailure(stderr io.Writer, logPath string, processErr error) {
	fmt.Fprintf(stderr, "ttc: ttcd did not become ready; startup log: %s\n", logPath)
	if processErr != nil {
		fmt.Fprintf(stderr, "ttc: ttcd exited: %v\n", processErr)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		fmt.Fprintf(stderr, "ttc: reading startup log: %v\n", err)
		return
	}
	if diagnostic := strings.TrimSpace(string(log)); diagnostic != "" {
		fmt.Fprintln(stderr, diagnostic)
	}
}

func reportDaemonStartupWarnings(stderr io.Writer, logPath string) {
	log, err := os.ReadFile(logPath)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(log), "\n") {
		if strings.Contains(line, "disabling host GSO") {
			fmt.Fprintln(stderr, line)
		}
	}
}

func runDaemonControl(opts options, args []string, stdout, stderr io.Writer) int {
	if opts.unsafeNoHostIsolation && (len(args) == 0 || args[0] != "start") {
		fmt.Fprintln(stderr, "ttc: --unsafe-no-host-isolation is valid with 'daemon start' only")
		return 2
	}
	clusterOpts := daemonClusterOptions{MaxTransactions: 32, PageListen: "0.0.0.0:0"}
	if len(args) != 0 && args[0] == "start" {
		startArgs := args[1:]
		for len(startArgs) != 0 {
			switch startArgs[0] {
			case "--unsafe-no-host-isolation":
				opts.unsafeNoHostIsolation = true
				startArgs = startArgs[1:]
			case "--allow-slow-volume-copy":
				opts.allowSlowVolumeCopy = true
				startArgs = startArgs[1:]
			default:
				consumed, err := consumeDaemonClusterOption(startArgs, &clusterOpts)
				if err != nil {
					fmt.Fprintf(stderr, "ttc: %v\n", err)
					return 2
				}
				if consumed == 0 {
					fmt.Fprintf(stderr, "ttc: unknown daemon start option %q\n", startArgs[0])
					return 2
				}
				startArgs = startArgs[consumed:]
			}
		}
		args = args[:1]
	}
	if len(args) != 0 && (args[0] == "requests" || args[0] == "request") {
		conn, err := connectRunningDaemon(opts.daemonSocket)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		defer conn.Close()
		status, err := exchangeDaemon(conn, daemonRequest{Version: daemonAPIVersion, Action: "mutation-control", Options: wireOptions(opts), Args: append([]string(nil), args...)}, strings.NewReader(""), stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		return status
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, "ttc: usage: ttc daemon start [isolation/volume/cluster options]|status|stop|requests|request inspect|request resolve")
		return 2
	}
	action := args[0]
	switch action {
	case "start":
		if conn, err := connectDaemon(opts.daemonSocket); err == nil {
			conn.Close()
			fmt.Fprintln(stdout, "ttcd already running; use 'ttc daemon status' to inspect its mode")
			return 0
		}
		if opts.unsafeNoHostIsolation {
			fmt.Fprintln(stderr, "ttc: warning: host namespace and chroot isolation is disabled; run trusted workloads only")
		} else if err := preflightDaemonIsolation(false); err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		logPath, processDone, err := startDaemonProcess(opts.daemonSocket, opts.dataRoot, opts.unsafeNoHostIsolation, opts.allowSlowVolumeCopy, clusterOpts)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if conn, err := connectDaemon(opts.daemonSocket); err == nil {
				conn.Close()
				reportDaemonStartupWarnings(stderr, logPath)
				fmt.Fprintln(stdout, "ttcd started")
				return 0
			}
			select {
			case processErr := <-processDone:
				reportDaemonStartupFailure(stderr, logPath, processErr)
				return 1
			case <-time.After(25 * time.Millisecond):
			}
		}
		reportDaemonStartupFailure(stderr, logPath, nil)
		return 1
	case "status", "stop":
		conn, err := connectDaemon(opts.daemonSocket)
		if err != nil {
			fmt.Fprintln(stderr, "ttc: ttcd is not running")
			return 1
		}
		defer conn.Close()
		request := daemonRequest{Version: daemonAPIVersion, Action: map[string]string{"status": "ping", "stop": "stop"}[action]}
		if action == "stop" {
			request.Options = wireOptions(opts)
		}
		status, err := exchangeDaemon(conn, request, strings.NewReader(""), stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		return status
	default:
		fmt.Fprintf(stderr, "ttc: unknown daemon command %q\n", action)
		return 2
	}
}
