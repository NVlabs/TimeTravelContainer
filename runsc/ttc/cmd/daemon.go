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
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	"gvisor.dev/gvisor/runsc/ttc/protocol"
	ttcstorage "gvisor.dev/gvisor/runsc/ttc/storage"
	"gvisor.dev/gvisor/runsc/version"
)

const (
	daemonAPIVersion       = protocol.DaemonAPIVersion
	defaultRuntimePlatform = "systrap"
	daemonStartTimeout     = 2 * time.Minute
	// A clustered TTC_ROOT is commonly hosted on NFS. A node that looked up the
	// root before the first member initialized it may retain a negative directory
	// entry for the server's acdirmax interval (commonly 60 seconds). Keep the
	// joining daemon alive long enough for that cache entry to expire instead of
	// turning a successful cluster initialization into a spurious join failure.
	clusterSharedRootVisibilityTimeout = 90 * time.Second
	kvmGetAPIVersion                   = uintptr(0xae00)
	kvmCreateVM                        = uintptr(0xae01)
	kvmAPIVersion                      = uintptr(12)
	daemonFrameStdin                   = byte('I')
	daemonFrameStdinAck                = byte('A')
	daemonFrameEOF                     = byte('C')
	daemonFrameResize                  = byte('W')
	daemonFrameStdout                  = byte('O')
	daemonFrameStderr                  = byte('E')
	daemonFrameStatus                  = byte('X')
	daemonFrameTerminal                = byte('T')
	daemonFrameTTYState                = byte('S')
	daemonMaxFrame                     = 1 << 20
	daemonRootlessProbeArg             = "_rootless-userns-probe"
	// Batch remote fork reserves every sibling before pausing the source. Keep
	// the default high enough for agent-harness fanout while retaining a bound
	// against an accidentally unbounded number of unfinished transactions.
	defaultClusterMaxTransactions = 4096
	// Cluster control traffic may share a constrained link with peer paging and
	// network-filesystem traffic. Thirty seconds leaves enough renewal headroom
	// for transient bulk-transfer queues while still detecting failed nodes
	// promptly; operators that need a different tradeoff can set
	// --cluster-lease when initializing the cluster.
	defaultClusterLeaseDuration = 30 * time.Second
)

var daemonAPIFeatures = []string{
	"batch-fork",
	"durable-wait",
	"elastic-cluster",
	"filesystem-object-paging",
	"named-exec",
	"network-quarantine",
	"storage-status",
	"tty-observation",
}

const repositoryMaintenanceInterval = 5 * time.Minute

// distributedArtifactVisibilityTimeout bounds recovery from cross-client NFS
// directory-cache lag after a source has atomically published and fsynced an
// immutable transaction artifact. The source retains these files until the
// target RPC completes, so ENOENT and ESTALE during this interval describe a
// visibility race rather than an absent logical object.
const distributedArtifactVisibilityTimeout = 30 * time.Second

const (
	daemonRootSchema                  = "snapshot-graph-v2"
	daemonStandaloneRootFormatVersion = 2
	daemonClusterRootFormatVersion    = 5
	daemonRootRoleStandaloneAuthority = "standalone-authority"
	daemonRootRoleClusterShared       = "cluster-shared"
	daemonRootRoleClusterLocal        = "cluster-local"
)

var (
	createDaemonRootlessProbe = createRootlessProbe
	daemonEffectiveUID        = os.Geteuid
	daemonKernelRelease       = currentHostKernelRelease
	daemonPlatformPreflight   = preflightDaemonPlatform
	daemonStatfs              = unix.Statfs
)

func openDistributedSharedArtifact(ctx context.Context, path string, expectedBytes *uint64) (*os.File, error) {
	return openDistributedSharedArtifactWith(ctx, path, expectedBytes, os.Open)
}

// openDistributedSharedArtifactWith reopens an immutable transaction artifact
// after the two transient lookup failures exposed by cache-coherent network
// filesystems: a negative lookup retained across the publisher's atomic rename
// and ESTALE while the client refreshes the renamed directory entry. It must
// not be used for general storage lookup; only a live transaction guarantees
// that the publisher has committed and continues to retain this exact path.
func openDistributedSharedArtifactWith(ctx context.Context, path string, expectedBytes *uint64, open func(string) (*os.File, error)) (*os.File, error) {
	ctx, cancel := context.WithTimeout(ctx, distributedArtifactVisibilityTimeout)
	defer cancel()
	delay := time.Millisecond
	var lastErr error
	for {
		file, err := open(path)
		if err == nil && expectedBytes != nil {
			stat, statErr := file.Stat()
			if statErr != nil {
				_ = file.Close()
				err = statErr
			} else if uint64(stat.Size()) != *expectedBytes {
				_ = file.Close()
				return nil, fmt.Errorf("shared distributed artifact size is %d, want %d", stat.Size(), *expectedBytes)
			}
		}
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ESTALE) {
			return nil, err
		}
		lastErr = err
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
		if delay < 250*time.Millisecond {
			delay *= 2
			if delay > 250*time.Millisecond {
				delay = 250 * time.Millisecond
			}
		}
	}
}

type daemonRequest struct {
	Version        int      `json:"version"`
	ClientBuildID  string   `json:"client_build_id,omitempty"`
	Action         string   `json:"action"`
	RequestID      string   `json:"request_id,omitempty"`
	ClientTERM     string   `json:"client_term,omitempty"`
	StderrTerminal bool     `json:"stderr_terminal,omitempty"`
	StderrWidth    int      `json:"stderr_width,omitempty"`
	Args           []string `json:"args,omitempty"`
	Stdin          bool     `json:"stdin,omitempty"`
	// TerminalControl keeps the daemon stream full-duplex for terminal-size
	// requests even when guest stdin is detached. This is distinct from Stdin:
	// `exec -td` needs resize frames, but must not attach the client's input.
	TerminalControl bool                   `json:"terminal_control,omitempty"`
	ManageStop      bool                   `json:"manage_stop,omitempty"`
	Route           []string               `json:"route,omitempty"`
	PlacementNode   string                 `json:"placement_node,omitempty"`
	PlacementClaims []daemonPlacementClaim `json:"placement_claims,omitempty"`
	// IngressPrepared means an entry daemon has already reserved every
	// node-local listener requested by run -p and removed those flags from Args.
	IngressPrepared bool `json:"ingress_prepared,omitempty"`
	// DistributedLeaf marks a coordinator-generated owner-local command. It
	// prevents a family mutation from being expanded again on the destination.
	DistributedLeaf bool `json:"distributed_leaf,omitempty"`
}

func (r daemonRequest) streamsClientFrames() bool {
	return r.Stdin || r.TerminalControl
}

// daemonPlacementClaim fences one routed branch command to the catalog
// generation observed by the entry daemon. The owner validates it again after
// taking its local lifecycle lock, so a delayed proxy cannot mutate a newer
// incarnation after migration.
type daemonPlacementClaim struct {
	FamilyID         string `json:"family_id"`
	BranchID         string `json:"branch_id"`
	OwnerNode        string `json:"owner_node"`
	OwnerIncarnation string `json:"owner_incarnation"`
	Generation       uint64 `json:"generation"`
	// AllowMaterializing is set only for the idempotent operation that turns a
	// durable process-free branch into a runtime. Materializing ownership never
	// authorizes any command that can execute guest code.
	AllowMaterializing bool `json:"allow_materializing,omitempty"`
}

// extractPlacementNode removes the user-facing --node admission constraint
// from commands whose execution location is selected before command parsing.
// Fork and migrate consume --node themselves because their source must first
// be routed to its current owner. A literal "--" ends TTC option parsing and
// protects command arguments that happen to be named --node.
func extractPlacementNode(args []string) ([]string, string, error) {
	if len(args) == 0 {
		return args, "", nil
	}
	command := args[0]
	if command == "container" {
		if len(args) < 2 {
			return args, "", nil
		}
		command = args[1]
	}
	switch command {
	case "run", "create", "start", "restore", "rehydrate":
	default:
		return args, "", nil
	}
	optionStart := 1
	if args[0] == "container" {
		optionStart = 2
	}
	clean := append([]string(nil), args[:optionStart]...)
	node := ""
	for i := optionStart; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			clean = append(clean, args[i:]...)
			break
		}
		// Go's flag package stops at the first positional operand. Honor the
		// same boundary so `ttc run IMAGE command --node value` passes that
		// argument to the guest instead of changing cluster placement.
		if arg == "" || arg[0] != '-' || arg == "-" {
			clean = append(clean, args[i:]...)
			break
		}
		value := ""
		matched := false
		switch {
		case arg == "--node":
			if i+1 >= len(args) || args[i+1] == "" {
				return nil, "", errors.New("--node requires a node name")
			}
			i++
			value, matched = args[i], true
		case strings.HasPrefix(arg, "--node="):
			value, matched = strings.TrimPrefix(arg, "--node="), true
			if value == "" {
				return nil, "", errors.New("--node requires a node name")
			}
		}
		if matched {
			if node != "" && node != value {
				return nil, "", fmt.Errorf("conflicting --node values %q and %q", node, value)
			}
			node = value
			continue
		}
		clean = append(clean, arg)
		if placementOptionConsumesValue(command, arg) && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			clean = append(clean, args[i])
		}
	}
	return clean, node, nil
}

func placementOptionConsumesValue(command, arg string) bool {
	name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
	if name == "node" {
		return true
	}
	switch command {
	case "restore":
		return name == "name"
	case "run", "create":
		switch name {
		case "name", "pull", "workdir", "w", "user", "u", "hostname", "h", "network", "net", "ip", "mac-address", "publish", "p", "add-host", "dns", "dns-search", "dns-option", "dns-opt", "entrypoint", "stop-signal", "stop-timeout", "pids-limit", "memory", "m", "cpus", "cpu-shares", "cpu-period", "cpu-quota", "shm-size", "group-add", "cap-add", "cap-drop", "ulimit", "env", "e", "env-file", "label", "l", "label-file", "volume", "v", "mount", "tmpfs", "log-driver", "log-opt":
			return true
		}
	}
	return false
}

type daemonServer struct {
	listener              net.Listener
	socket                string
	runtimePlatform       string
	cpuProfileName        string
	cpuFeatures           string
	unsafeNoHostIsolation bool
	disableHostGSO        bool
	hostKernelRelease     string
	dataRoot              string
	localRoot             string
	root                  string
	runsc                 string
	volumeCapability      ttcvolume.Capability
	clusterCapabilities   *cluster.Capabilities
	clusterListen         string
	distributedSource     *distributedSourceRuntime
	clusterStore          cluster.TransactionCatalog
	clusterDirectory      cluster.NodeCatalog
	clusterPlacements     cluster.PlacementCatalog
	publicationQueue      *ttcstorage.Queue
	snapshotGraph         *ttcsnapshot.Store
	snapshotData          *ttcsnapshot.LocalDataStore
	snapshotResolver      *ttcsnapshot.Resolver
	snapshotShared        *ttcsnapshot.SharedPackStore
	sharedReferences      *sharedReferenceService
	portIngress           *portIngressManager
	stderr                io.Writer

	stopOnce   sync.Once
	cleanStop  atomic.Bool
	retireMu   sync.Mutex
	retireNode func() error
	retired    bool

	reconcileMu sync.Mutex
	reconciled  map[string]bool

	// operations records the families long operations are working on
	// without the lifecycle lock.
	operations familyOperations

	mutationMu    sync.Mutex
	mutationLocks map[string]*sync.Mutex
	mutationCount atomic.Uint64

	eventMu  sync.Mutex
	eventSeq uint64

	stdinMu     sync.Mutex
	stdinLeases map[string]*daemonStdinLease

	sidecarMu sync.Mutex
	sidecars  map[string]*exec.Cmd
	// suppressedConsoleExits contains one-shot fences installed by explicit
	// stop/restart operations. The console follower still drains and records the
	// wait status, but it must not also run the spontaneous-exit finalizer: the
	// foreground operation owns discard and publication ordering for that exit.
	suppressedConsoleExits map[string]int
	// inheritedConsole records drainers that survived an older ttcd. They
	// cannot be reaped through exec.Cmd, but their advisory locks provide an
	// exact completion notification for the new daemon.
	inheritedConsole map[string]struct{}

	consoleClientMu      sync.Mutex
	consoleClients       map[string]map[*daemonConsoleClient]struct{}
	closingConsoleClient map[string]int
}

// retireBeforeReply completes clustered identity retirement before a
// successful stop response becomes observable. Closing the Unix listener and
// leaving retirement to serveDaemon defers used to let `ttc node stop` return
// while the node was still listed as draining/offline.
func (s *daemonServer) retireBeforeReply() error {
	if s == nil || s.retireNode == nil {
		return nil
	}
	s.retireMu.Lock()
	defer s.retireMu.Unlock()
	if s.retired {
		return nil
	}
	if err := s.retireNode(); err != nil {
		return err
	}
	s.retired = true
	return nil
}

// stopAfterReply closes the accepted connection before closing the listener.
// The accept loop owns the daemon's lifetime: closing the listener lets it
// return and permits the process to exit without waiting for this handler's
// deferred conn.Close. If the listener were closed first, process teardown
// could win that race and discard the final status frame, causing the client
// to retry an already-successful node stop against a removed socket.
func (s *daemonServer) stopAfterReply(conn net.Conn) {
	if s == nil {
		return
	}
	s.cleanStop.Store(true)
	_ = conn.Close()
	s.stopOnce.Do(func() { _ = s.listener.Close() })
}

func (s *daemonServer) mutationRoot() string {
	if s != nil && s.localRoot != "" {
		return s.localRoot
	}
	if s == nil {
		return ""
	}
	return s.dataRoot
}

type daemonConsoleClient struct {
	cancel      func()
	done        chan struct{}
	cancelOnce  sync.Once
	releaseOnce sync.Once
}

type daemonClusterOptions struct {
	Configured      bool
	ControlOnly     bool
	Listen          string
	NodeID          string
	RepositoryID    string
	Advertise       string
	AuthToken       string
	MemoryTarget    uint64
	AutoRebalance   bool
	CapacityCommand string
	CapacityWebhook string
	LeaseDuration   time.Duration
	MaxTransactions int
	PageListen      string
	PageAdvertise   string
	Peers           []string
	RaftBootstrap   bool
	PagingPolicy    ttcpolicy.Policy
	CPUProfileMode  string
	CPUFeatures     string
	CPUProfileSet   bool
	ResolvedCPU     daemonCPUProfile
}

func defaultDaemonClusterOptions() daemonClusterOptions {
	policy := ttcpolicy.DefaultPolicy()
	policy.MaxConcurrency = 4
	policy.AckInterval = time.Millisecond
	return daemonClusterOptions{MaxTransactions: defaultClusterMaxTransactions, PageListen: "0.0.0.0:0", LeaseDuration: defaultClusterLeaseDuration, PagingPolicy: policy, CPUProfileMode: "portable"}
}

type daemonClusterRuntime struct {
	listen            string
	authToken         string
	mux               *cluster.ProtocolMux
	controlListener   net.Listener
	controlPlane      *cluster.ControlPlane
	source            *distributedSourceRuntime
	directory         cluster.NodeCatalog
	placements        cluster.PlacementCatalog
	transactions      cluster.TransactionCatalog
	raft              *cluster.AuthorityRaftNode
	raftLocalDir      string
	raftCatalog       *cluster.RaftCatalog
	nodeRecord        cluster.NodeRecord
	lease             time.Duration
	clusterID         string
	recoverMembership func(context.Context) error
	cancelFreshJoin   func(context.Context) error
	autoRebalance     bool
	capacitySink      elasticCapacitySink
	cpuProfile        daemonCPUProfile
}

// controlOnlyCluster is an outbound-only cluster gateway. It deliberately has
// no Raft node, NodeRecord, placement capacity, TCP listener, or data-plane
// provider. Commands arriving on its per-user Unix socket are tunneled to a
// compute daemon, which performs the ordinary authoritative routing.
type controlOnlyCluster struct {
	nodeID       string
	repositoryID string
	authToken    string

	peersMu sync.RWMutex
	peers   []controlOnlyPeer
	active  atomic.Uint64
}

type controlOnlyPeer struct {
	nodeID string
	url    string
	client *cluster.Client
}

func (c *controlOnlyCluster) peerSnapshot() []controlOnlyPeer {
	c.peersMu.RLock()
	defer c.peersMu.RUnlock()
	return append([]controlOnlyPeer(nil), c.peers...)
}

func (c *controlOnlyCluster) seedURLs() []string {
	peers := c.peerSnapshot()
	urls := make([]string, 0, len(peers))
	for _, peer := range peers {
		urls = append(urls, peer.url)
	}
	return urls
}

// authorityState follows the Raft leader hint returned by any compute seed.
// A control-only daemon has no local Raft replica, so its seed is expected to
// become a follower during ordinary elections. Treating that as an endpoint
// failure leaves the gateway permanently pinned to a usable but non-authoritative
// node and breaks every command that needs a linearizable snapshot.
func (c *controlOnlyCluster) authorityState(ctx context.Context) (*cluster.AuthorityState, error) {
	if c == nil {
		return nil, errors.New("control-only cluster is unavailable")
	}
	peers := c.peerSnapshot()
	clients := make(map[string]*cluster.Client, len(peers))
	for _, peer := range peers {
		clients[peer.url] = peer.client
	}
	var readErrors error
	for _, peer := range peers {
		state, err := peer.client.AuthorityState(ctx)
		if err == nil {
			return state, nil
		}
		readErrors = errors.Join(readErrors, fmt.Errorf("%s: %w", peer.url, err))
		var redirect *cluster.AuthorityNotLeaderError
		if !errors.As(err, &redirect) || redirect.LeaderControlURL == "" || redirect.LeaderControlURL == peer.url {
			continue
		}
		leader := clients[redirect.LeaderControlURL]
		if leader == nil {
			leader, err = cluster.NewPlainClient(redirect.LeaderControlURL, c.nodeID, c.authToken, 2*time.Minute)
			if err != nil {
				readErrors = errors.Join(readErrors, fmt.Errorf("creating redirected leader client %s: %w", redirect.LeaderControlURL, err))
				continue
			}
			clients[redirect.LeaderControlURL] = leader
		}
		state, err = leader.AuthorityState(ctx)
		if err == nil {
			return state, nil
		}
		readErrors = errors.Join(readErrors, fmt.Errorf("%s: %w", redirect.LeaderControlURL, err))
	}
	return nil, fmt.Errorf("reading cluster authority through all compute peers: %w", readErrors)
}

// refreshPeers learns the current compute endpoints through a linearizable
// authority read. The gateway itself remains absent from Raft membership and
// never advertises a listener. Existing clients are reused so an in-flight
// stream is unaffected by a concurrent refresh.
func (c *controlOnlyCluster) refreshPeers(ctx context.Context) error {
	current := c.peerSnapshot()
	state, err := c.authorityState(ctx)
	if err != nil {
		return err
	}

	existing := make(map[string]*cluster.Client, len(current))
	for _, peer := range current {
		existing[peer.url] = peer.client
	}
	urls := make([]string, 0, len(state.Nodes))
	for _, record := range state.Nodes {
		if record.State == cluster.NodeOffline || record.ControlURL == "" {
			continue
		}
		urls = append(urls, record.ControlURL)
	}
	sort.Strings(urls)
	refreshed := make([]controlOnlyPeer, 0, len(urls))
	seen := make(map[string]struct{}, len(urls))
	for _, baseURL := range urls {
		if _, ok := seen[baseURL]; ok {
			continue
		}
		seen[baseURL] = struct{}{}
		client := existing[baseURL]
		if client == nil {
			var err error
			client, err = cluster.NewPlainClient(baseURL, c.nodeID, c.authToken, 2*time.Minute)
			if err != nil {
				return fmt.Errorf("creating client for discovered compute peer %s: %w", baseURL, err)
			}
		}
		nodeID := ""
		for _, record := range state.Nodes {
			if record.ControlURL == baseURL {
				nodeID = record.NodeID
				break
			}
		}
		refreshed = append(refreshed, controlOnlyPeer{nodeID: nodeID, url: baseURL, client: client})
	}
	if len(refreshed) == 0 {
		return errors.New("cluster authority contains no usable compute endpoint")
	}
	c.peersMu.Lock()
	c.peers = refreshed
	c.peersMu.Unlock()
	return nil
}

type controlOnlyPeerStatus struct {
	BuildID string `json:"build_id"`
	Cluster *struct {
		Capabilities cluster.Capabilities `json:"capabilities"`
	} `json:"cluster"`
}

func prepareControlOnlyCluster(opts daemonClusterOptions, sharedRoot string) (*controlOnlyCluster, error) {
	control := &controlOnlyCluster{nodeID: opts.NodeID, repositoryID: opts.RepositoryID, authToken: os.Getenv("TTC_CLUSTER_TOKEN")}
	seen := make(map[string]struct{})
	var probeErrors error
	for _, peerSpec := range opts.Peers {
		_, baseURL, _, err := parseClusterPeer(peerSpec)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[baseURL]; ok {
			continue
		}
		seen[baseURL] = struct{}{}
		client, err := cluster.NewPlainClient(baseURL, opts.NodeID, control.authToken, 2*time.Minute)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		status, probeErr := probeControlOnlyPeer(ctx, client)
		cancel()
		if probeErr != nil {
			probeErrors = errors.Join(probeErrors, fmt.Errorf("%s: %w", baseURL, probeErr))
			continue
		}
		if status.Cluster == nil {
			probeErrors = errors.Join(probeErrors, fmt.Errorf("%s: peer is not a clustered TTC daemon", baseURL))
			continue
		}
		if status.Cluster.Capabilities.RepositoryID != opts.RepositoryID {
			probeErrors = errors.Join(probeErrors, fmt.Errorf("%s: repository %q does not match TTC_ROOT repository %q", baseURL, status.Cluster.Capabilities.RepositoryID, opts.RepositoryID))
			continue
		}
		control.peers = append(control.peers, controlOnlyPeer{nodeID: status.Cluster.Capabilities.NodeID, url: baseURL, client: client})
	}
	if len(control.peers) == 0 {
		return nil, fmt.Errorf("no control seed is reachable and compatible with %q: %w", sharedRoot, probeErrors)
	}
	// Discovery is best-effort during startup: a compatible seed is sufficient
	// to start, and the background refresher will retry a temporarily
	// unavailable authority read.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = control.refreshPeers(ctx)
	cancel()
	return control, nil
}

func probeControlOnlyPeer(ctx context.Context, client *cluster.Client) (controlOnlyPeerStatus, error) {
	request, err := json.Marshal(daemonRequest{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "ping"})
	if err != nil {
		return controlOnlyPeerStatus{}, err
	}
	var response bytes.Buffer
	if err := client.ProxyDaemon(ctx, bytes.NewReader(append(request, '\n')), &response); err != nil {
		return controlOnlyPeerStatus{}, err
	}
	reader := bufio.NewReader(&response)
	var stdout, stderr bytes.Buffer
	for {
		typ, payload, err := readDaemonFrame(reader)
		if err != nil {
			return controlOnlyPeerStatus{}, fmt.Errorf("reading seed daemon response: %w", err)
		}
		switch typ {
		case daemonFrameStdout:
			stdout.Write(payload)
		case daemonFrameStderr:
			stderr.Write(payload)
		case daemonFrameStatus:
			if len(payload) != 4 {
				return controlOnlyPeerStatus{}, fmt.Errorf("invalid seed status frame length %d", len(payload))
			}
			if status := binary.BigEndian.Uint32(payload); status != 0 {
				return controlOnlyPeerStatus{}, fmt.Errorf("seed rejected control client: %s", strings.TrimSpace(stderr.String()))
			}
			var result controlOnlyPeerStatus
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				return controlOnlyPeerStatus{}, fmt.Errorf("decoding seed status: %w", err)
			}
			if result.BuildID != version.Version() {
				return controlOnlyPeerStatus{}, fmt.Errorf("seed build %q does not match control daemon build %q", result.BuildID, version.Version())
			}
			return result, nil
		}
	}
}

const daemonNodeIncarnationVersion = 1

type daemonNodeIncarnation struct {
	Version     int    `json:"version"`
	NodeID      string `json:"node_id"`
	Incarnation string `json:"incarnation"`
}

// localNodeIncarnation returns the identity of one exclusively locked local
// root epoch. A daemon process restart must retain this identity: the detached
// lease guardian first host-stops every Sentry, after which the replacement
// daemon can resume the same fenced storage and in-flight transactions. A
// discarded/recreated local root gets a fresh identity and therefore fences
// all dependencies on the lost epoch.
func localNodeIncarnation(localRoot, nodeID string) (string, error) {
	path := filepath.Join(localRoot, "cluster", "node-incarnation.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var record daemonNodeIncarnation
		if err := json.Unmarshal(data, &record); err != nil {
			return "", fmt.Errorf("decoding local node incarnation: %w", err)
		}
		decoded, decodeErr := hex.DecodeString(record.Incarnation)
		if record.Version != daemonNodeIncarnationVersion || record.NodeID != nodeID || decodeErr != nil || len(decoded) != 16 {
			return "", fmt.Errorf("invalid local node incarnation for %q", nodeID)
		}
		return record.Incarnation, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reading local node incarnation: %w", err)
	}
	incarnation, err := cluster.NewNodeIncarnation()
	if err != nil {
		return "", err
	}
	record := daemonNodeIncarnation{Version: daemonNodeIncarnationVersion, NodeID: nodeID, Incarnation: incarnation}
	if err := writeDurableJSON(path, record); err != nil {
		return "", fmt.Errorf("recording local node incarnation: %w", err)
	}
	return incarnation, nil
}

// removeLocalNodeIncarnation completes a clean identity retirement. A later
// daemon start on the same scratch root is intentionally a new incarnation;
// crash recovery, by contrast, retains this file and resumes the fenced epoch.
func removeLocalNodeIncarnation(localRoot, nodeID, incarnation string) error {
	path := filepath.Join(localRoot, "cluster", "node-incarnation.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var record daemonNodeIncarnation
	if err := json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("decoding retired local node incarnation: %w", err)
	}
	if record.NodeID != nodeID || record.Incarnation != incarnation {
		return errors.New("refusing to remove a different local node incarnation")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	return errors.Join(syncErr, directory.Close())
}

// discardRetiredLocalAuthority removes state that was authoritative only for a
// cluster node incarnation which the replicated catalog has forgotten. A
// subsequent join is a new execution epoch: replaying the old publication,
// graph-reference, runtime, or request journals could retain objects for
// transactions that no longer exist, or resurrect runtimes that the cluster
// has already declared lost.
//
// The image directory is deliberately retained. In clustered mode it is an
// immutable, digest-keyed extraction cache and contains no branch authority.
// Unknown top-level entries are also left alone so that a new TTC version does
// not silently delete data it does not understand. The local root is
// exclusively locked by the caller while this function runs.
func discardRetiredLocalAuthority(localRoot string) error {
	for _, name := range []string{
		"catalog",
		"cluster",
		"containers",
		"daemon",
		"filesystems",
		"graph-references",
		"objects",
		"publication-queue",
		"publication-staging",
		"runtime",
		"snapshot-graph",
	} {
		path := filepath.Join(localRoot, name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("discarding retired local %s state: %w", name, err)
		}
	}
	return syncDaemonRootDirectory(localRoot)
}

type daemonTargetRuntime struct {
	backend     *runtimeBackend
	nodeID      string
	incarnation string
	directory   cluster.NodeCatalog
	store       cluster.TransactionCatalog
	authority   *cluster.RaftCatalog
	done        <-chan struct{}
	watch       sync.Map
	watchCount  atomic.Int64
	pollOnce    sync.Once
	pollSlots   chan struct{}
	locks       sync.Map
	// independence prevents a caller whose HTTP deadline expired from
	// enqueueing another copy of the same release fence while the original
	// handler is still materializing the target. Different transactions may
	// still make progress concurrently up to the per-family serialization in
	// lockTarget.
	independence sync.Map
}

const dependencyMonitorConcurrency = 2

func dependencyMonitorInterval(watchers int64) time.Duration {
	if watchers < 1 {
		watchers = 1
	}
	// Keep aggregate branch-status observation near 16 polls/second. The data
	// plane remains fully asynchronous; this polling only drives adaptive hints,
	// accounting, and eventual independence bookkeeping.
	interval := time.Duration(watchers) * time.Second / 16
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	return interval
}

func (r *daemonTargetRuntime) tryDependencyPoll() (func(), bool) {
	r.pollOnce.Do(func() { r.pollSlots = make(chan struct{}, dependencyMonitorConcurrency) })
	select {
	case r.pollSlots <- struct{}{}:
		return func() { <-r.pollSlots }, true
	default:
		return nil, false
	}
}

func (r *daemonTargetRuntime) monitorTransaction(id string) (*cluster.Transaction, error) {
	if r.authority != nil {
		transaction, ok := r.authority.AppliedTransaction(id)
		if ok {
			return &transaction, nil
		}
		// A forwarded proposal can return just before this follower applies the
		// entry. Fall back to one linearizable lookup only for that narrow gap.
	}
	return r.store.Load(id)
}

func (r *daemonTargetRuntime) monitorSourceAvailable(id string) bool {
	if r.authority != nil {
		node, ok := r.authority.AppliedNode(id)
		// Lease expiry is interpreted and committed by the authority leader.
		// Comparing the leader's wall-clock deadline on another host would be
		// unsafe; a locally applied non-offline record remains observable.
		if ok {
			return node.State != cluster.NodeOffline
		}
		// As above, tolerate local apply lag without declaring source loss.
	}
	node, err := r.directory.Lookup(id)
	return err == nil && node.LeaseActive(time.Now()) && node.State != cluster.NodeOffline
}

func (r *daemonTargetRuntime) lockTarget(name string) func() {
	value, _ := r.locks.LoadOrStore(name, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (r *daemonTargetRuntime) Reserve(ctx context.Context, tx *cluster.Transaction, family cluster.RemoteFamilyDescriptor) error {
	if r == nil || r.backend == nil || tx == nil {
		return errors.New("distributed target runtime is unavailable")
	}
	if r.directory != nil {
		record, err := r.directory.Lookup(r.nodeID)
		if err != nil {
			return fmt.Errorf("checking elastic target admission: %w", err)
		}
		if record.Incarnation != r.incarnation || !record.AcceptsPlacement(time.Now()) {
			return fmt.Errorf("cluster node %q is not accepting placement (state=%s)", r.nodeID, record.State)
		}
	}
	var source ttcbundle.ContainerInfo
	if err := json.Unmarshal(family.Container, &source); err != nil {
		return fmt.Errorf("decoding remote family metadata: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(family.Spec, &spec); err != nil {
		return fmt.Errorf("decoding remote runtime specification: %w", err)
	}
	physicalFamily := source.DistributedFamilyID
	if physicalFamily == "" {
		physicalFamily = source.ID
	}
	if physicalFamily == "" {
		return errors.New("remote family metadata has no immutable family ID")
	}
	targetName := tx.Identity.TargetContainer
	targetID := ""
	authoritativeMigration := false
	if r.authority != nil {
		// Shared container metadata describes the whole logical family and may
		// retain a former owner after migration. Reuse only a physical Sentry
		// that Raft currently places on this node; otherwise create an isolated
		// transaction-scoped target. This applies equally to forks and migrations:
		// keeping siblings in one Sentry preserves family-local resource sharing
		// and avoids splitting the runtime's branch bookkeeping.
		// A stale migration source is not reusable because it has no active local
		// placement after cutover.
		targetName = tx.Identity.TransactionID
		targetID = tx.Identity.TransactionID
		records, listErr := r.authority.Placements().List()
		if listErr != nil {
			return fmt.Errorf("selecting distributed target runtime: %w", listErr)
		}
		if physical, found, selectErr := reusablePhysicalTarget(records, physicalFamily, r.nodeID); selectErr != nil {
			return selectErr
		} else if found {
			targetName = physical
			targetID = ""
		}
		if tx.Identity.Migration {
			placement, placementErr := r.authority.LookupPlacement(tx.Identity.SourceContainer, tx.Identity.SourceBranch)
			if placementErr != nil {
				return fmt.Errorf("validating migration placement: %w", placementErr)
			}
			authoritativeTx, transactionErr := r.authority.LoadTransaction(tx.Identity.TransactionID)
			if transactionErr != nil {
				return fmt.Errorf("validating migration transaction: %w", transactionErr)
			}
			if authoritativeTx.Identity != tx.Identity || placement.State != cluster.PlacementMigrating ||
				placement.OwnerNode != tx.Identity.SourceNodeID || placement.OwnerIncarnation != authoritativeTx.SourceIncarnation ||
				placement.TargetNode != r.nodeID || placement.OperationID != tx.Identity.TransactionID {
				return fmt.Errorf("migration %q does not own the authoritative %s@%s placement", tx.Identity.TransactionID, tx.Identity.SourceContainer, tx.Identity.SourceBranch)
			}
			authoritativeMigration = true
		}
	}
	defer r.lockTarget(physicalFamily)()
	// Migration restores use the transaction ID as their physical Sentry branch
	// name. An older incarnation with the same logical name may therefore remain
	// paused in this Sentry while it serves sparse pages to another node; neither
	// incarnation blocks the other and Raft placement selects the current one.
	_, err := r.backend.containerStore().PrepareDistributedFork(ctx, ttcbundle.DistributedForkOptions{
		SourceContainer: tx.Identity.SourceContainer, TargetName: targetName,
		SourcePhysicalBranch: tx.SourcePhysicalBranch,
		TargetBranch:         tx.Identity.TargetBranch, VisibleBranch: tx.Identity.VisibleTargetBranch(),
		ParentBranch:  tx.Identity.SourceBranch,
		TransactionID: tx.Identity.TransactionID, NodeID: r.nodeID,
		SourceNode: tx.Identity.SourceNodeID, Migration: tx.Identity.Migration,
		AuthoritativeMigration: authoritativeMigration,
		Source:                 &source, Spec: &spec, TargetID: targetID,
	})
	return err
}

// reusablePhysicalTarget returns one active physical Sentry for family on
// node. Placement is the authority here: transaction-scoped remnants from an
// earlier migration are deliberately invisible and therefore never reused.
//
// A family may legitimately have more than one physical Sentry on a node.
// This happens when branches arrive independently and only later converge on
// the same node. The physical split is an implementation detail, so it must
// not make a later fork or migration fail. Prefer the canonical family runtime
// when it exists; otherwise choose the runtime containing the most placed
// branches, with a stable lexical tie-breaker. New imports consequently
// converge rather than creating still more physical runtimes.
func reusablePhysicalTarget(records []cluster.PlacementRecord, family, node string) (string, bool, error) {
	counts := make(map[string]int)
	for _, record := range records {
		if record.FamilyID != family || record.OwnerNode != node ||
			(record.State != cluster.PlacementRunning && record.State != cluster.PlacementPaused) || record.PhysicalContainer == "" {
			continue
		}
		counts[record.PhysicalContainer]++
	}
	if len(counts) == 0 {
		return "", false, nil
	}
	if _, ok := counts[family]; ok {
		return family, true, nil
	}
	physical, best := "", -1
	for candidate, count := range counts {
		if count > best || count == best && candidate < physical {
			physical, best = candidate, count
		}
	}
	return physical, true, nil
}

func (r *daemonTargetRuntime) Prepare(ctx context.Context, tx *cluster.Transaction, session cluster.RemoteSessionDescriptor) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, targetBranch, peer, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	info, err = r.backend.containerStore().RefreshContainerImageRoot(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("materializing target image cache: %w", err)
	}
	portableMounts := make([]ttcbundle.PortableInternalMount, len(session.InternalMounts))
	for i, mount := range session.InternalMounts {
		portableMounts[i] = ttcbundle.PortableInternalMount{
			Destination: mount.Destination, Contents: append([]byte(nil), mount.Contents...), Mode: os.FileMode(mount.Mode),
		}
	}
	if err := r.backend.containerStore().InstallPortableInternalMounts(info.ID, portableMounts); err != nil {
		return fmt.Errorf("installing target-local internal mounts: %w", err)
	}
	filesystemConnections, err := r.backend.materializeDistributedFilesystemRoots(info, targetBranch.FilesystemBranch, session.FilesystemRoots, peer)
	if err != nil {
		return fmt.Errorf("materializing distributed filesystem metadata: %w", err)
	}
	// materializeDistributedFilesystemRoots installs one-shot restore aliases in
	// the active repository map. A later restore failure must not leave those
	// foreign identities attached to an otherwise healthy peer runtime.
	aliasesPending := len(session.FilesystemRoots) != 0
	defer func() {
		if aliasesPending {
			_ = r.backend.containerStore().CommitActiveRepositoryRestoreIDs(info.ID)
		}
	}()
	memoryDependencyBytes := session.StreamBytes
	if session.GraphArtifact != "" {
		memoryDependencyBytes = 0
	}
	if err := r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TransactionID, memoryDependencyBytes, session.FilesystemBytes); err != nil {
		return fmt.Errorf("recording distributed source dependency: %w", err)
	}
	var token ttcremote.SnapshotToken
	tokenBytes, err := hex.DecodeString(session.SnapshotToken)
	if err != nil || len(tokenBytes) != 16 {
		return errors.New("invalid distributed snapshot token")
	}
	copy(token[:], tokenBytes)
	restoreArgs := boot.DistributedForkRestoreArgs{
		SessionID: tx.Identity.TransactionID, FamilyID: tx.Identity.SourceContainer,
		TargetBranchID: tx.Identity.TargetBranch, Address: session.Address, SnapshotToken: token,
		Peer: peer, VisibleBranchID: targetBranch.ID, FilesystemBranch: targetBranch.FilesystemBranch,
		Quarantined:   targetBranch.Quarantined,
		MetadataBytes: session.MetadataBytes, MaxTransferBytes: session.MaxTransferBytes,
		MaxParallel: tx.Negotiated.PagingPolicy.MaxConcurrency, Timeout: 30 * time.Second, RetryLimit: 2,
		AckBatchPages: tx.Negotiated.PagingPolicy.AckBatchPages, AckInterval: tx.Negotiated.PagingPolicy.AckInterval,
		FilesystemConnections: filesystemConnections,
		PushPolicy:            ttcremote.TCPPushPolicy(tx.Negotiated.PagingPolicy.Push),
		SharedMetadata:        session.MetadataArtifact != "",
		DurableDehydration:    session.DurableDehydration,
		GraphSparse:           session.GraphArtifact != "",
		ExecSessions:          bootExecSessions(session.ExecSessions, info.ID),
	}
	restoreArgs.FilesystemRestoreIDs, err = r.backend.filesystemRestoreIDs(info.ID)
	if err != nil {
		return fmt.Errorf("resolving distributed filesystem identities: %w", err)
	}
	hasGraphIdentity := session.GraphSnapshot != 0 || len(session.GraphRootObjects) != 0
	if hasGraphIdentity {
		rootNames := graphMemoryRootNames(targetBranch.FilesystemBranch, len(session.GraphRootObjects))
		if err := validateGraphRootObjects(session.GraphSnapshot, rootNames, session.GraphRootObjects); err != nil {
			return fmt.Errorf("validating distributed graph memory identity: %w", err)
		}
		if err := r.backend.retainDistributedGraphReferences(tx.Identity.TransactionID, tx.Identity.TransactionID, info.ID, targetBranch.ID, session.GraphRootObjects); err != nil {
			return fmt.Errorf("retaining distributed graph memory origins: %w", err)
		}
		if err := r.backend.containerStore().UpdateDistributedForkMemoryGraph(tx.Identity.TransactionID, session.GraphSnapshot, session.GraphRootObjects, tx.Identity.TransactionID); err != nil {
			_ = r.backend.releaseDistributedGraphReferences(tx.Identity.TransactionID)
			return fmt.Errorf("recording distributed graph memory identity: %w", err)
		}
	}
	if restoreArgs.GraphSparse {
		restoreArgs.GraphRemote = true
		if _, err := rand.Read(restoreArgs.GraphRemoteToken[:]); err != nil {
			return fmt.Errorf("generating graph restore token: %w", err)
		}
	}
	switch tx.Negotiated.PagingPolicy.Prefetch {
	case ttcpolicy.PrefetchSequential, ttcpolicy.PrefetchAdaptive:
		restoreArgs.PrefetchBytes = tx.Negotiated.PagingPolicy.TransferBytes - tx.Negotiated.PagingPolicy.PageSize
	case ttcpolicy.PrefetchFixed:
		windowPages := tx.Negotiated.PagingPolicy.TransferPages() - 1
		restoreArgs.PrefetchBehindBytes = (windowPages / 2) * tx.Negotiated.PagingPolicy.PageSize
		restoreArgs.PrefetchBytes = (windowPages - windowPages/2) * tx.Negotiated.PagingPolicy.PageSize
	}
	configured := r.backend.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC)
	runtimeCreated := false
	var runtimeState struct {
		Status string `json:"status"`
	}
	stateOutput, stateErr := r.backend.command("state", info.ID)
	if stateErr == nil {
		if err := json.Unmarshal(stateOutput, &runtimeState); err != nil {
			return fmt.Errorf("decoding target runtime state: %w", err)
		}
	} else {
		// A new physical Sentry must bootstrap the exact visible branch and
		// filesystem identities regardless of whether this transaction was
		// reserved as a peer import. In particular, the first remote child of a
		// family on a node is represented by a transaction-scoped container, so
		// peer is false even though its visible branch is not "main".
		bundleDir, cleanup, err := r.backend.containerStore().PrepareDistributedBranchRestoreBundle(info.ID, targetBranch.ID, targetBranch.FilesystemBranch)
		if err != nil {
			return fmt.Errorf("preparing distributed target bundle: %w", err)
		}
		defer cleanup()
		var diagnostic bytes.Buffer
		commandBackend := *r.backend
		commandBackend.stderr = &diagnostic
		status, err := commandBackend.runConfiguredCommandWithArgs(configured, "create", "--bundle", bundleDir, info.ID)
		if err != nil {
			return fmt.Errorf("creating distributed target runtime: %w", err)
		}
		if status != 0 {
			return fmt.Errorf("creating distributed target runtime exited with status %d: %s", status, strings.TrimSpace(diagnostic.String()))
		}
		runtimeState.Status = "created"
		runtimeCreated = true
	}
	if peer && runtimeState.Status != "running" && runtimeState.Status != "created" {
		return fmt.Errorf("distributed peer target runtime is %q, want running", runtimeState.Status)
	}
	if runtimeState.Status == "created" || peer {
		// A metadata peer restored into a newly created empty Sentry is its
		// physical loader branch, not an import into an already-running family.
		restoreArgs.Peer = peer && !runtimeCreated
		encoded, err := json.Marshal(&restoreArgs)
		if err != nil {
			return err
		}
		reader, err := newConfigMemFD("ttc-distributed-restore-config", encoded)
		if err != nil {
			return err
		}
		connectionCount := restoreArgs.FilesystemConnections
		if !restoreArgs.SharedMetadata {
			connectionCount++
		}
		if !restoreArgs.GraphSparse {
			connectionCount += (restoreArgs.MaxParallel + 1) * (restoreArgs.RetryLimit + 1)
			if restoreArgs.PushPolicy != ttcremote.TCPPushDemandOnly {
				connectionCount++
			}
		}
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
		files := []*os.File{reader}
		connectionsStartFD := 4
		var sharedMetadata *os.File
		var graphRestore *graphMemoryRestore
		var durableTargets []*os.File
		durableActiveDir := ""
		keepDurableTargets := false
		defer func() {
			for _, file := range durableTargets {
				_ = file.Close()
			}
			if durableActiveDir != "" && !keepDurableTargets {
				_ = os.RemoveAll(durableActiveDir)
			}
		}()
		if restoreArgs.SharedMetadata {
			sharedMetadata, err = fetchRemoteArtifact(ctx, session.Address, token, session.MetadataArtifact, session.MetadataBytes)
			if err != nil {
				reader.Close()
				return fmt.Errorf("fetching peer distributed metadata: %w", err)
			}
			files = append(files, sharedMetadata)
			connectionsStartFD++
		}
		if restoreArgs.GraphSparse {
			graphManifest, openErr := fetchRemoteArtifact(ctx, session.Address, token, session.GraphArtifact, session.GraphArtifactBytes)
			if openErr != nil {
				reader.Close()
				for _, file := range connectionFiles {
					file.Close()
				}
				return fmt.Errorf("fetching peer graph memory manifest: %w", openErr)
			}
			graphRestore, err = r.backend.openDistributedGraphRestore(graphManifest, tx.Identity.TransactionID, restoreArgs.GraphRemoteToken)
			if err != nil {
				graphManifest.Close()
				reader.Close()
				for _, file := range connectionFiles {
					file.Close()
				}
				return fmt.Errorf("preparing graph memory resolver: %w", err)
			}
			files = append(files, graphRestore.manifest, graphRestore.objects)
			connectionsStartFD += 2
			files = append(files, graphRestore.remote)
			connectionsStartFD++
		}
		if peer && restoreArgs.DurableDehydration {
			if sharedMetadata == nil {
				return errors.New("durable peer metadata is unavailable")
			}
			if _, err := sharedMetadata.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("rewinding durable peer metadata: %w", err)
			}
			durable, readErr := kernel.ReadDurableDehydrationMetadata(sharedMetadata)
			if readErr != nil {
				return fmt.Errorf("reading durable peer metadata: %w", readErr)
			}
			if _, err := sharedMetadata.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("rewinding durable peer metadata after inspection: %w", err)
			}
			durableActiveDir = memoryBranchDir(r.backend.activeDataRoot(), info.ID, "active", targetBranch)
			if err := os.RemoveAll(durableActiveDir); err != nil {
				return err
			}
			if err := os.MkdirAll(durableActiveDir, 0700); err != nil {
				return err
			}
			for i := 1; i < len(durable.Memories); i++ {
				file, err := os.OpenFile(filepath.Join(durableActiveDir, fmt.Sprintf("memory-%04d.mem", i)), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
				if err != nil {
					return fmt.Errorf("creating durable distributed peer memory %d: %w", i, err)
				}
				pgalloc.IMAWorkAroundForMemFile(file.Fd())
				durableTargets = append(durableTargets, file)
			}
			files = append(files, durableTargets...)
			connectionsStartFD += len(durableTargets)
		}
		files = append(files, connectionFiles...)
		commandArgs := []string{"timetravel", "--distributed-config-fd=3"}
		if restoreArgs.SharedMetadata {
			commandArgs = append(commandArgs, "--distributed-metadata-fd=4")
		}
		if restoreArgs.GraphSparse {
			graphStartFD := 5
			commandArgs = append(commandArgs,
				"--dehydration-graph-manifest-fd="+strconv.Itoa(graphStartFD),
				"--dehydration-graph-objects-fd="+strconv.Itoa(graphStartFD+1))
			if restoreArgs.GraphRemote {
				commandArgs = append(commandArgs, "--dehydration-memory-remote-fd="+strconv.Itoa(graphStartFD+2))
			}
		}
		commandArgs = append(commandArgs,
			"--distributed-connections-start-fd="+strconv.Itoa(connectionsStartFD), "--distributed-connections-count="+strconv.Itoa(len(connectionFiles)),
			info.ID, "distributed-restore", tx.Identity.TransactionID)
		_, err = r.backend.commandWithFiles(files, commandArgs...)
		reader.Close()
		if sharedMetadata != nil {
			sharedMetadata.Close()
		}
		if graphRestore != nil {
			graphRestore.close()
		}
		for _, file := range connectionFiles {
			file.Close()
		}
		if err != nil {
			return fmt.Errorf("%w (filesystem restore aliases: %v)", err, restoreArgs.FilesystemRestoreIDs)
		}
		keepDurableTargets = true
		if runtimeCreated {
			var diagnostic bytes.Buffer
			commandBackend := *r.backend
			commandBackend.stderr = &diagnostic
			status, err := commandBackend.runConfiguredCommandWithArgs(configured, "start", info.ID)
			if err != nil {
				return fmt.Errorf("starting paused distributed target: %w", err)
			}
			if status != 0 {
				return fmt.Errorf("starting paused distributed target exited with status %d: %s", status, strings.TrimSpace(diagnostic.String()))
			}
		}
		if err := r.backend.containerStore().CommitActiveRepositoryRestoreIDs(info.ID); err != nil {
			return fmt.Errorf("committing distributed filesystem identities: %w", err)
		}
		aliasesPending = false
	} else if runtimeState.Status != "running" {
		return fmt.Errorf("distributed target runtime is unexpectedly %q", runtimeState.Status)
	}
	if len(session.ExecConsoleLogs) != 0 {
		var consoleToken [16]byte
		tokenBytes, err := hex.DecodeString(session.SnapshotToken)
		if err != nil || len(tokenBytes) != len(consoleToken) {
			return errors.New("invalid distributed console snapshot token")
		}
		copy(consoleToken[:], tokenBytes)
		if err := r.backend.installRemoteExecConsoleLogs(ctx, session.Address, consoleToken, info.ID, targetBranch.ID, session.ExecConsoleLogs); err != nil {
			return err
		}
	}
	if err := r.backend.installRemoteExecSessionStates(info.ID, targetBranch.ID, session.ExecSessions); err != nil {
		return err
	}
	return r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TransactionID, "paused")
}

func (r *daemonTargetRuntime) Publish(_ context.Context, tx *cluster.Transaction, paused bool) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	if !paused && branch.State != "running" {
		if _, err := r.backend.command("timetravel", info.ID, "resume", branch.ID); err != nil {
			return fmt.Errorf("resuming distributed target: %w", err)
		}
		if err := r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TransactionID, "running"); err != nil {
			return err
		}
	}
	if !tx.Identity.Migration {
		info, err = r.backend.containerStore().PublishDistributedForkTarget(tx.Identity.TransactionID)
		if err != nil {
			return err
		}
	} else {
		// A migration target is deliberately invisible until the source has
		// committed the placement record. Starting any sidecars here would make
		// them consult the still-source-owned placement and route back to the old
		// node. Activate performs all externally visible publication work after
		// that compare-and-swap succeeds.
		return nil
	}
	// Console followers are recoverable sidecars, not part of branch authority.
	// In particular, a detached restored init may have no attachable console.
	// Starting followers here would delay publication by the control timeout;
	// normal daemon reconciliation starts every attachable init/exec follower
	// after the Raft placement is visible.
	r.monitorDependency(tx.Identity.TransactionID, info.ID)
	return nil
}

func (r *daemonTargetRuntime) PublishedTarget(tx *cluster.Transaction) (family, branch, physicalContainer, physicalBranch, incarnation string, err error) {
	if r == nil || tx == nil {
		err = errors.New("distributed target runtime is unavailable")
		return
	}
	info, targetBranch, _, loadErr := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if loadErr != nil {
		err = loadErr
		return
	}
	family = info.DistributedFamilyID
	if family == "" {
		family = info.ID
	}
	return family, targetBranch.ID, info.ID, targetBranch.ID, r.incarnation, nil
}

func (r *daemonTargetRuntime) Activate(_ context.Context, tx *cluster.Transaction) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	state, stateErr := r.backend.runtimeBranchLifecycle(info.ID, branch.ID)
	if stateErr != nil {
		return stateErr
	}
	if tx.DesiredPaused {
		if state != "paused" {
			return fmt.Errorf("distributed target is %q, want paused", state)
		}
	} else if state != "running" && state != "paused" {
		return fmt.Errorf("distributed target is %q, want paused or running", state)
	}
	info, err = r.backend.containerStore().PublishDistributedForkTarget(tx.Identity.TransactionID)
	if err != nil {
		return fmt.Errorf("publishing distributed target ownership: %w", err)
	}
	if !tx.DesiredPaused && state == "paused" {
		if _, err := r.backend.command("timetravel", info.ID, "resume", branch.ID); err != nil {
			return fmt.Errorf("resuming distributed target: %w", err)
		}
	}
	targetState := "running"
	if tx.DesiredPaused {
		targetState = "paused"
	}
	if err := r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TransactionID, targetState); err != nil {
		return err
	}
	if err := r.backend.ensurePublishedPortsForBranch(info, branch.ID); err != nil {
		return fmt.Errorf("starting distributed target published ports: %w", err)
	}
	// Publish may already have installed the console followers while keeping the
	// guest paused. Activation must preserve and reuse those followers instead
	// of treating their locks as a failed retry.
	if _, err := r.backend.ensureConsoleDrain(selector{container: info.ID, branch: branch.ID}, false); err != nil {
		return fmt.Errorf("starting distributed target console drain: %w", err)
	}
	sessions, err := r.backend.execSessions(selector{container: info.ID, branch: branch.ID})
	if err != nil {
		return fmt.Errorf("listing migrated target exec sessions: %w", err)
	}
	for _, session := range sessions {
		if session.State != "running" {
			continue
		}
		if _, err := r.backend.ensureExecConsoleDrain(selector{container: info.ID, branch: branch.ID}, session.ID, session.ConsoleID, false); err != nil {
			return fmt.Errorf("starting distributed target exec console %q: %w", session.ID, err)
		}
	}
	r.monitorDependency(tx.Identity.TransactionID, info.ID)
	return nil
}

func (r *daemonTargetRuntime) forwardedDependencyError(tx *cluster.Transaction, err error) error {
	if !errors.Is(err, os.ErrNotExist) || r.authority == nil || tx == nil {
		return err
	}
	forwarded, authorityErr := r.authority.VolatileDependencyForwarded(
		tx.Identity.SourceContainer, tx.Identity.VisibleTargetBranch(), tx.Identity.TransactionID, r.nodeID)
	if authorityErr != nil {
		return errors.Join(err, fmt.Errorf("checking forwarded dependency: %w", authorityErr))
	}
	if forwarded {
		return errors.New("distributed dependency forwarding is still in progress")
	}
	return err
}

func (r *daemonTargetRuntime) Drain(_ context.Context, tx *cluster.Transaction) error {
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return r.forwardedDependencyError(tx, err)
	}
	action := "distributed-drain"
	if branch.DependentMemoryBytes == 0 {
		action = "distributed-filesystem-drain"
	}
	_, err = r.backend.command("timetravel", info.ID, action, branch.ID)
	return err
}

func (r *daemonTargetRuntime) Handoff(ctx context.Context, tx *cluster.Transaction, graph cluster.GraphHandoffDescriptor) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	return r.handoffLocked(ctx, tx, graph)
}

func (r *daemonTargetRuntime) handoffLocked(ctx context.Context, tx *cluster.Transaction, graph cluster.GraphHandoffDescriptor) error {
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return r.forwardedDependencyError(tx, err)
	}
	objects := strings.Split(graph.GraphRootObjects, ",")
	if err := validateGraphRootObjects(graph.GraphSnapshot, graphMemoryRootNames(branch.FilesystemBranch, len(objects)), objects); err != nil {
		return fmt.Errorf("validating graph handoff roots: %w", err)
	}
	previousReferenceOperation := graphReferenceOperationForBranch(info, branch)
	handoffReferenceOperation := graphHandoffReferenceOperation(tx.Identity.TransactionID, graph.GraphSnapshot)
	if err := r.backend.retainDistributedGraphReferences(tx.Identity.TransactionID, handoffReferenceOperation, info.ID, branch.ID, objects); err != nil {
		return fmt.Errorf("retaining graph handoff roots: %w", err)
	}
	retained := true
	defer func() {
		if retained {
			_ = r.backend.releaseDistributedGraphReferences(handoffReferenceOperation)
		}
	}()
	if err := r.backend.containerStore().UpdateDistributedForkMemoryGraph(tx.Identity.TransactionID, graph.GraphSnapshot, objects, handoffReferenceOperation); err != nil {
		return fmt.Errorf("recording graph handoff roots: %w", err)
	}
	// The persistent branch record now owns this reference intent. A later
	// handoff retry must retain it even if the current Sentry switch fails.
	retained = false
	if previousReferenceOperation != "" && previousReferenceOperation != handoffReferenceOperation {
		if err := r.backend.releaseDistributedGraphReferencesEventually(previousReferenceOperation); err != nil {
			return fmt.Errorf("releasing superseded graph roots: %w", err)
		}
	}
	manifest, err := openDistributedSharedArtifact(ctx, filepath.Join(r.backend.dataRoot, graph.GraphManifest), nil)
	if err != nil {
		return fmt.Errorf("opening graph handoff manifest: %w", err)
	}
	var token ttcsnapshot.ObjectRelayToken
	if _, err := rand.Read(token[:]); err != nil {
		manifest.Close()
		return err
	}
	restore, err := r.backend.openDistributedGraphRestore(manifest, tx.Identity.TransactionID, token)
	if err != nil {
		manifest.Close()
		return fmt.Errorf("opening graph handoff resolver: %w", err)
	}
	defer restore.close()
	files := []*os.File{restore.manifest, restore.objects}
	args := []string{
		"timetravel", "--dehydration-graph-manifest-fd=3", "--dehydration-graph-objects-fd=4",
		"--distributed-session=" + strconv.FormatUint(graph.GraphSnapshot, 10),
	}
	if restore.remote != nil {
		files = append(files, restore.remote)
		args = append(args, "--dehydration-memory-remote-fd=5", "--distributed-family="+hex.EncodeToString(token[:]))
	}
	args = append(args, info.ID, "distributed-handoff", branch.ID)
	if _, err := r.backend.commandWithFiles(files, args...); err != nil {
		return fmt.Errorf("switching distributed target to graph memory: %w", err)
	}
	return nil
}

func (r *daemonTargetRuntime) graphHandoffStatus(tx *cluster.Transaction) (boot.DistributedForkHandoffStatus, error) {
	if r == nil || r.backend == nil || tx == nil || tx.GraphHandoff == nil {
		return boot.DistributedForkHandoffStatus{}, errors.New("distributed graph handoff is unavailable")
	}
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return boot.DistributedForkHandoffStatus{}, r.forwardedDependencyError(tx, err)
	}
	output, err := r.backend.command("timetravel", info.ID, "distributed-handoff-status", branch.ID)
	if err != nil {
		return boot.DistributedForkHandoffStatus{}, err
	}
	var status boot.DistributedForkHandoffStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return boot.DistributedForkHandoffStatus{}, fmt.Errorf("decoding graph memory handoff status: %w", err)
	}
	if status.Snapshot != tx.GraphHandoff.GraphSnapshot {
		return boot.DistributedForkHandoffStatus{}, fmt.Errorf("graph memory handoff snapshot is %d, want %d", status.Snapshot, tx.GraphHandoff.GraphSnapshot)
	}
	return status, nil
}

func (r *daemonTargetRuntime) waitGraphHandoff(ctx context.Context, tx *cluster.Transaction) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := r.graphHandoffStatus(tx)
		if err != nil {
			return err
		}
		if status.Done {
			if status.Error != "" {
				return fmt.Errorf("graph memory handoff failed: %s", status.Error)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// refreshDistributedGraphHandoffsAtStartup replaces every retained target's
// immutable-memory relay before the node is admitted for new work. Snapshot
// identity is unchanged; only unresolved page faults are redirected to a new
// detached helper. This repairs roots created by older daemons whose relay was
// daemon-owned and also makes helper failure recoverable by restarting ttcd.
func refreshDistributedGraphHandoffsAtStartup(r *daemonTargetRuntime) error {
	if r == nil || r.store == nil || r.authority == nil {
		return nil
	}
	transactions, err := r.store.List()
	if err != nil {
		return err
	}
	for i := range transactions {
		tx := &transactions[i]
		if tx.Identity.TargetNodeID != r.nodeID || tx.GraphHandoff == nil ||
			(tx.State != cluster.TransactionPublished && tx.State != cluster.TransactionDraining && tx.State != cluster.TransactionIndependent) {
			continue
		}
		// TargetNodeID records where the transaction was first restored; it is
		// deliberately immutable and therefore cannot authorize recovery. A
		// later dehydrate/rehydrate or migration may move the visible branch to
		// another node while the terminal transaction remains in the catalog.
		// Refreshing such a historical handoff can retain already-collected
		// roots and, worse, redirect a Sentry owned by another incarnation.
		state, err := r.authority.AuthorityState()
		if err != nil {
			return fmt.Errorf("reading graph handoff placement authority: %w", err)
		}
		if !transactionPlacementOwnedBy(*tx, state, r.nodeID, r.incarnation) {
			continue
		}
		_, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("locating graph handoff target %q: %w", tx.Identity.TransactionID, err)
		}
		// A graph handoff is a reader switch in a live Sentry, not a durable
		// branch repair. Dehydrated, exited, and interrupted-dehydration targets
		// have no reader to refresh; their immutable cold manifests are the
		// authority. Retrying an obsolete transaction here can instead resurrect
		// already-retired volatile origins and prevent the daemon from starting.
		if !distributedGraphHandoffHasLiveRuntime(branch) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err = r.Handoff(ctx, tx, *tx.GraphHandoff)
		if err == nil {
			err = r.waitGraphHandoff(ctx, tx)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("refreshing graph handoff %q: %w", tx.Identity.TransactionID, err)
		}
	}
	return nil
}

func transactionPlacementOwnedBy(tx cluster.Transaction, state *cluster.AuthorityState, nodeID, incarnation string) bool {
	if state == nil || nodeID == "" || incarnation == "" {
		return false
	}
	for _, placement := range state.Placements {
		if cluster.TransactionTargetsPlacement(tx, placement) &&
			placement.OwnerNode == nodeID && placement.OwnerIncarnation == incarnation &&
			(placement.State == cluster.PlacementRunning || placement.State == cluster.PlacementPaused) {
			return true
		}
	}
	return false
}

func distributedGraphHandoffHasLiveRuntime(branch ttcbundle.BranchInfo) bool {
	if branch.Unplaced {
		return false
	}
	switch normalizeLifecycleState(branch.State) {
	case "running", "paused":
		// GraphHandoff remains in the immutable transaction record after the
		// target has faulted every page and retired its backing roots. At that
		// point the live Sentry is self-contained: there is no relay reader to
		// refresh, and the former origin may correctly have been reclaimed from
		// Raft. Treating the historical handoff as live would make daemon restart
		// depend on an object that the data plane has already proved unnecessary.
		return branch.DependentMemoryBytes != 0 || branch.SnapshotID != 0 || len(branch.MemoryRootObjects) != 0
	default:
		return false
	}
}

func (r *daemonTargetRuntime) Cancel(_ context.Context, tx *cluster.Transaction) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	var familyID string
	info, branch, peer, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err == nil {
		familyID = info.ID
		if tx.Identity.Migration && branch.DistributedMigration {
			if _, pending := info.DistributedForkImports[tx.Identity.TransactionID]; !pending {
				return fmt.Errorf("published migration %q cannot be canceled", tx.Identity.TransactionID)
			}
		}
		if peer {
			// Preparation can fail before a target Sentry exists. The
			// runtime-only release helper is idempotent in that case. It also
			// handles a pending import that is the only physical branch in this
			// node's Sentry, where DestroyBranch deliberately rejects removal
			// of the last branch.
			if destroyErr := r.backend.releaseMigratedSourceRuntime(info.ID, branch.ID); destroyErr != nil {
				return fmt.Errorf("destroying canceled distributed target branch %q: %w", branch.ID, destroyErr)
			}
		} else {
			if _, deleteErr := r.backend.command("delete", "--force", info.ID); deleteErr != nil {
				return fmt.Errorf("deleting canceled distributed target family %q: %w", info.ID, deleteErr)
			}
		}
		if releaseErr := r.backend.releaseDistributedGraphReferences(tx.Identity.TransactionID); releaseErr != nil {
			return releaseErr
		}
	}
	if err := r.backend.containerStore().RemoveDistributedForkTarget(tx.Identity.TransactionID); err != nil {
		return err
	}
	if familyID != "" {
		return r.backend.removeLocalFamilyStateIfUnowned(familyID)
	}
	return nil
}

func (r *daemonTargetRuntime) MarkIndependent(ctx context.Context, tx *cluster.Transaction) error {
	if r == nil || r.backend == nil || tx == nil {
		return nil
	}
	if _, loaded := r.independence.LoadOrStore(tx.Identity.TransactionID, struct{}{}); loaded {
		return errors.New("distributed independence operation is already in progress")
	}
	defer r.independence.Delete(tx.Identity.TransactionID)
	// Branches imported into one physical family share a Sentry and its TTC
	// repositories. Transaction locks are deliberately scoped to one remote
	// fork, so sibling handoffs can otherwise interleave the repository-wide
	// materialization barrier with source-release acknowledgement. Serialize
	// this complete release fence per family: source relays remain live until
	// every filesystem dependency observed by this operation is local.
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TransactionID)
	if err != nil {
		return r.forwardedDependencyError(tx, err)
	}
	if tx.GraphHandoff != nil {
		status, err := r.graphHandoffStatus(tx)
		if err != nil {
			// Raft may contain a bound handoff intent whose first runtime attempt
			// failed before the Sentry recorded it. Reinstall the exact immutable
			// descriptor and let the ordinary retry observe completion. This also
			// repairs transactions produced by older daemons with this race.
			if retryErr := r.handoffLocked(ctx, tx, *tx.GraphHandoff); retryErr != nil {
				return fmt.Errorf("reading graph memory handoff status: %w; reinstalling handoff: %v", err, retryErr)
			}
			return errors.New("graph memory handoff was reinstalled and is still in progress")
		}
		if !status.Done {
			return errors.New("graph memory handoff is still in progress")
		}
		if status.Error != "" {
			return fmt.Errorf("graph memory handoff failed: %s", status.Error)
		}
	} else if branch.DependentMemoryBytes != 0 {
		output, err := r.backend.command("timetravel", info.ID, "distributed-dependency", branch.ID)
		if err != nil {
			return fmt.Errorf("reading distributed memory drain status: %w", err)
		}
		var status pgalloc.AsyncPagesFileLoadStats
		if err := json.Unmarshal(output, &status); err != nil {
			return fmt.Errorf("decoding distributed memory drain status: %w", err)
		}
		if !status.Done {
			return errors.New("distributed memory drain is still in progress")
		}
		if status.Error != "" {
			return fmt.Errorf("distributed memory drain failed: %s", status.Error)
		}
	}
	// Independence is a release fence, not merely a control-plane state. A
	// concurrent source graph handoff and an eager node drain may both attempt
	// this transition. Never acknowledge either one until the target has
	// actually replaced every volatile filesystem source. Otherwise the source
	// may close its relay after the state transition while the guest still has
	// deferred files, permanently stranding the migrated branch.
	if _, err := r.backend.command("timetravel", info.ID, "distributed-filesystem-drain", branch.ID); err != nil {
		return fmt.Errorf("starting distributed filesystem drain: %w", err)
	}
	output, err := r.backend.command("timetravel", info.ID, "distributed-filesystem-drain-status", branch.ID)
	if err != nil {
		return fmt.Errorf("reading distributed filesystem drain status: %w", err)
	}
	var filesystemStatus boot.DistributedForkFilesystemDrainStatus
	if err := json.Unmarshal(output, &filesystemStatus); err != nil {
		return fmt.Errorf("decoding distributed filesystem drain status: %w", err)
	}
	if !filesystemStatus.Done {
		return errors.New("distributed filesystem drain is still in progress")
	}
	if filesystemStatus.Error != "" {
		return fmt.Errorf("distributed filesystem drain failed: %s", filesystemStatus.Error)
	}
	return r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TransactionID, 0, 0)
}

type adaptivePrefetchController struct {
	policy               ttcpolicy.Policy
	ahead                uint64
	behind               uint64
	maxAhead             uint64
	lastLoaded           uint64
	lastWaited           uint64
	lastWaiters          int
	lastFaults           uint64
	lastSequentialFaults uint64
	localityFaults       uint64
	localitySequential   uint64
	lastChange           time.Time
	quietSamples         int
	initialized          bool
}

func newAdaptivePrefetchController(policy ttcpolicy.Policy) *adaptivePrefetchController {
	perWorkerBudget := policy.MaxOutstandingBytes / uint64(policy.MaxConcurrency)
	maxRead := min(perWorkerBudget, uint64(2<<20))
	maxRead -= maxRead % policy.PageSize
	if maxRead < policy.TransferBytes {
		maxRead = policy.TransferBytes
	}
	return &adaptivePrefetchController{
		policy:   policy,
		ahead:    policy.TransferBytes - policy.PageSize,
		maxAhead: maxRead - policy.PageSize,
	}
}

// observe applies deliberately slow hysteresis: blocking faults grow the
// window quickly, while sustained non-blocking throughput probes for a smaller
// window one step at a time. It returns a change only after the prior setting
// has had time to affect multiple fault/read cycles.
func (c *adaptivePrefetchController) observe(status pgalloc.AsyncPagesFileLoadStats, now time.Time) (uint64, uint64, bool) {
	if !c.initialized {
		c.initialized = true
		c.lastChange = now.Add(-50 * time.Millisecond)
		if status.PrefetchBytes != 0 || status.PrefetchBehindBytes != 0 {
			c.ahead, c.behind = status.PrefetchBytes, status.PrefetchBehindBytes
		}
		if status.Faults == 0 {
			c.lastLoaded = status.BytesLoaded
			c.lastWaited = status.BytesWaited
			c.lastWaiters = status.TotalWaiters
			c.lastFaults = status.Faults
			c.lastSequentialFaults = status.SequentialFaults
			return 0, 0, false
		}
	}
	loaded := status.BytesLoaded - min(status.BytesLoaded, c.lastLoaded)
	waited := status.BytesWaited - min(status.BytesWaited, c.lastWaited)
	waiters := status.TotalWaiters - min(status.TotalWaiters, c.lastWaiters)
	faults := status.Faults - min(status.Faults, c.lastFaults)
	sequentialFaults := status.SequentialFaults - min(status.SequentialFaults, c.lastSequentialFaults)
	c.lastLoaded, c.lastWaited, c.lastWaiters = status.BytesLoaded, status.BytesWaited, status.TotalWaiters
	c.lastFaults, c.lastSequentialFaults = status.Faults, status.SequentialFaults
	c.localityFaults += faults
	c.localitySequential += sequentialFaults

	// Random faults should reduce speculative amplification even when every
	// fault blocks. Exact adjacency is a deliberately conservative signal:
	// ambiguous or concurrent samples retain the current setting.
	if c.localityFaults >= 4 && c.localitySequential*4 <= c.localityFaults {
		c.quietSamples = 0
		if c.ahead == 0 {
			c.localityFaults, c.localitySequential = 0, 0
			return 0, 0, false
		}
		if now.Sub(c.lastChange) < 50*time.Millisecond {
			return 0, 0, false
		}
		total := (c.ahead + c.policy.PageSize) / 2
		if total < c.policy.PageSize {
			total = c.policy.PageSize
		}
		total -= total % c.policy.PageSize
		c.ahead = total - c.policy.PageSize
		c.behind = 0
		c.localityFaults, c.localitySequential = 0, 0
		c.lastChange = now
		return c.behind, c.ahead, true
	}

	if waited != 0 || waiters != 0 || status.Waiters != 0 {
		c.quietSamples = 0
		if c.localityFaults < 4 || c.localitySequential*4 < c.localityFaults*3 || now.Sub(c.lastChange) < 50*time.Millisecond {
			return 0, 0, false
		}
		if c.ahead >= c.maxAhead {
			c.localityFaults, c.localitySequential = 0, 0
			return 0, 0, false
		}
		// Once an adjacent run is established, request the full negotiated
		// transport window immediately. RTT-bound sequential workloads are too
		// short for a slow doubling ladder; later random or quiet samples still
		// shrink with hysteresis.
		c.ahead = c.maxAhead
		c.behind = 0
		c.localityFaults, c.localitySequential = 0, 0
		c.lastChange = now
		return c.behind, c.ahead, true
	}
	if loaded >= 4*(c.ahead+c.policy.PageSize) {
		c.quietSamples++
	} else {
		c.quietSamples = 0
	}
	if c.quietSamples < 8 || now.Sub(c.lastChange) < 2*time.Second || c.ahead <= c.policy.TransferBytes-c.policy.PageSize {
		return 0, 0, false
	}
	c.quietSamples = 0
	total := max((c.ahead+c.policy.PageSize)/2, c.policy.TransferBytes)
	total -= total % c.policy.PageSize
	c.ahead = total - c.policy.PageSize
	c.behind = 0
	c.lastChange = now
	return c.behind, c.ahead, true
}

func (r *daemonTargetRuntime) monitorDependency(transactionID, containerID string) {
	if r == nil || r.store == nil || transactionID == "" || containerID == "" {
		return
	}
	if _, loaded := r.watch.LoadOrStore(transactionID, struct{}{}); loaded {
		return
	}
	r.watchCount.Add(1)
	go func() {
		defer func() {
			r.watch.Delete(transactionID)
			r.watchCount.Add(-1)
		}()
		failedPolls := 0
		sourceFailedPolls := 0
		var adaptive *adaptivePrefetchController
		for {
			timer := time.NewTimer(dependencyMonitorInterval(r.watchCount.Load()))
			select {
			case <-r.done:
				timer.Stop()
				return
			case <-timer.C:
			}
			tx, err := r.monitorTransaction(transactionID)
			if err != nil || tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed {
				return
			}
			if tx.State == cluster.TransactionIndependent {
				// Independence means all future reads resolve without the source
				// node. A graph-backed demand loader may still report nonresident
				// pages, but those are cache misses, not volatile dependencies.
				_ = r.backend.containerStore().UpdateDistributedForkDependencies(transactionID, 0, 0)
				return
			}
			if tx.State != cluster.TransactionPublished && tx.State != cluster.TransactionDraining && tx.State != cluster.TransactionIndependent {
				continue
			}
			if tx.SourceNeeded && r.directory != nil {
				if !r.monitorSourceAvailable(tx.Identity.SourceNodeID) {
					sourceFailedPolls++
					if sourceFailedPolls < 3 {
						continue
					}
					if info, loadErr := r.backend.containerStore().LoadDistributedForkTarget(transactionID); loadErr == nil {
						if branch, branchErr := persistentBranchInfo(info, tx.Identity.VisibleTargetBranch()); branchErr == nil &&
							(branch.DependentMemoryBytes != 0 || branch.DependentFilesystemBytes != 0) {
							reason := "unrecoverable volatile-state loss: source node disappeared before memory and filesystem dependencies became durable"
							_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionFailed, reason, time.Now())
							r.corruptDependentTarget(info.ID, branch.ID, reason)
							return
						}
					}
				} else {
					sourceFailedPolls = 0
				}
			}
			// Target publication precedes its post-placement activation record.
			// Do not let dependency completion overtake that record: recovery
			// uses Activated to decide whether a committed target may be resumed.
			if !tx.Activated {
				continue
			}
			// The logical branch name is deliberately decoupled from the private
			// branch ID installed in the target Sentry. In particular, migration
			// keeps the user-visible name stable while each hop receives a fresh
			// transaction-scoped runtime branch. Polling VisibleTargetBranch here
			// therefore observed a nonexistent branch after the first hop and left
			// source-side heuristics as the only independence driver. Resolve the
			// transaction's exact physical branch for every poll instead.
			_, physicalBranch, _, branchErr := r.backend.containerStore().DistributedForkTargetBranch(transactionID)
			if branchErr != nil {
				if tx.State == cluster.TransactionIndependent || tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed {
					return
				}
				continue
			}
			targetBranch := physicalBranch.ID
			releasePoll, admitted := r.tryDependencyPoll()
			if !admitted {
				continue
			}
			if adaptive == nil && tx.Negotiated.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
				adaptive = newAdaptivePrefetchController(tx.Negotiated.PagingPolicy)
			}
			output, statusErr := r.backend.command("timetravel", containerID, "distributed-dependency", targetBranch)
			releasePoll()
			if statusErr != nil {
				// Process-free dehydration intentionally removes the target Kernel
				// while exporting all of its pages. Do not confuse that durable
				// lifecycle transition with an unexpected target exit.
				if info, loadErr := r.backend.containerStore().LoadContainer(containerID); loadErr == nil {
					if branch, branchErr := persistentBranchInfo(info, targetBranch); branchErr == nil {
						switch normalizeLifecycleState(branch.State) {
						case "dehydrating":
							failedPolls = 0
							continue
						case "dehydrated":
							if tx.State == cluster.TransactionIndependent {
								return
							}
							failedPolls = 0
							continue
						}
					}
				}
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
			if adaptive != nil && !status.Done {
				behind, ahead, changed := adaptive.observe(status, time.Now())
				if changed {
					_, _ = r.backend.command(
						"timetravel",
						"--distributed-prefetch-bytes", strconv.FormatUint(ahead, 10),
						"--distributed-prefetch-behind-bytes", strconv.FormatUint(behind, 10),
						containerID, "distributed-prefetch", targetBranch,
					)
				}
			}
			info, infoErr := r.backend.containerStore().LoadDistributedForkTarget(transactionID)
			var filesystemBytes uint64
			if infoErr == nil {
				if branch, branchErr := persistentBranchInfo(info, targetBranch); branchErr == nil {
					filesystemBytes = branch.DependentFilesystemBytes
				}
			}
			if !status.Done {
				_ = r.backend.containerStore().UpdateDistributedForkDependencies(transactionID, status.BytesOutstanding, filesystemBytes)
				continue
			}
			if status.Error != "" {
				_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionFailed,
					"remote memory loader failed: "+status.Error, time.Now())
				return
			}
			_ = r.backend.containerStore().UpdateDistributedForkDependencies(transactionID, 0, filesystemBytes)
			if tx.State != cluster.TransactionIndependent {
				// Resource byte counters are accounting, not authorization. A
				// metadata-only filesystem can have zero logical data bytes while
				// its namespace still depends on a remote catalog. Execute the same
				// target runtime release fence used by the control endpoint before
				// committing independence; otherwise the source may close the only
				// resolver for an empty-file tree.
				if err := r.MarkIndependent(context.Background(), tx); err != nil {
					continue
				}
				if _, err := r.store.Advance(transactionID, tx.Revision, cluster.TransactionIndependent, "", time.Now()); err != nil {
					continue
				}
			}
			_ = r.backend.containerStore().UpdateDistributedForkDependencies(transactionID, 0, 0)
			// The graph reference remains branch-scoped until the branch itself
			// is replaced or removed; becoming independent only removes its
			// volatile provider dependency.
			return
		}
	}()
}

func (r *daemonTargetRuntime) corruptDependentTarget(family, branch, reason string) {
	if state, err := r.backend.runtimeBranchLifecycle(family, branch); err == nil && state == "running" {
		_, _ = r.backend.command("timetravel", family, "pause", branch)
	}
	if r.authority != nil {
		_, _ = r.authority.MarkBranchCorrupted(family, branch, reason)
	}
	_ = r.backend.containerStore().UpdateBranchState(family, branch, "corrupted")
}

type daemonFrameWriter struct {
	conn net.Conn
	mu   *sync.Mutex
	typ  byte
}

type daemonJSONFrameWriter struct {
	conn   net.Conn
	mu     *sync.Mutex
	typ    byte
	buffer []byte
}

func (w *daemonJSONFrameWriter) Write(data []byte) (int, error) {
	w.buffer = append(w.buffer, data...)
	for {
		newline := bytes.IndexByte(w.buffer, '\n')
		if newline < 0 {
			return len(data), nil
		}
		payload := append([]byte(nil), w.buffer[:newline]...)
		w.buffer = w.buffer[newline+1:]
		if len(payload) != 0 {
			if err := writeDaemonFrame(w.conn, w.mu, w.typ, payload); err != nil {
				return 0, err
			}
		}
	}
}

// OwnConsoleSidecar tracks and reaps one ready console follower. Followers are
// intentionally not killed when the daemon shuts down: they retain the advisory
// lock and continue draining until a restarted daemon reconciles the family.
func (s *daemonServer) OwnConsoleSidecar(key string, command *exec.Cmd, target selector, observeInitExit bool) error {
	var exited func()
	if observeInitExit {
		exited = func() {
			if !s.consumeSuppressedConsoleExit(key) {
				s.observeInitConsoleExit(target)
			}
		}
	}
	return s.ownSidecar("console", key, command, exited)
}

// ObserveInheritedConsoleSidecar adopts completion observation, rather than
// process ownership, for a console drainer that survived an older ttcd. This
// closes the restart window in which the old drainer writes wait-status after
// startup reconciliation has already run: without a new callback, the branch
// would remain running/materializing forever despite its completed init.
func (s *daemonServer) ObserveInheritedConsoleSidecar(key string, target selector, observeInitExit bool) {
	if s == nil || key == "" {
		return
	}
	s.sidecarMu.Lock()
	if _, owned := s.sidecars[key]; owned {
		s.sidecarMu.Unlock()
		return
	}
	if s.inheritedConsole == nil {
		s.inheritedConsole = make(map[string]struct{})
	}
	if _, watching := s.inheritedConsole[key]; watching {
		s.sidecarMu.Unlock()
		return
	}
	s.inheritedConsole[key] = struct{}{}
	s.sidecarMu.Unlock()
	go func() {
		lockFile, err := os.OpenFile(key, os.O_CREATE|os.O_RDWR, 0600)
		if err == nil {
			err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
			_ = lockFile.Close()
		}
		s.sidecarMu.Lock()
		delete(s.inheritedConsole, key)
		s.sidecarMu.Unlock()
		if err == nil && observeInitExit && !s.consumeSuppressedConsoleExit(key) {
			s.observeInitConsoleExit(target)
		}
	}()
}

// SuppressConsoleExitOnce reserves the next completion notification for an
// explicit lifecycle operation. commit(true) leaves the one-shot fence for the
// follower to consume after it records the wait status. commit(false) cancels
// an unused fence when the operation failed before terminating the init.
//
// The fence is keyed by the console lock. A new init incarnation cannot acquire
// that lock until the old follower has exited, so the notification cannot be
// stolen by a later restart that reuses the branch name.
func (s *daemonServer) SuppressConsoleExitOnce(key string) func(bool) {
	if s == nil || key == "" {
		return func(bool) {}
	}
	s.sidecarMu.Lock()
	observed := s.sidecars[key] != nil
	if !observed {
		_, observed = s.inheritedConsole[key]
	}
	if observed {
		if s.suppressedConsoleExits == nil {
			s.suppressedConsoleExits = make(map[string]int)
		}
		s.suppressedConsoleExits[key]++
	}
	s.sidecarMu.Unlock()
	var once sync.Once
	return func(commit bool) {
		once.Do(func() {
			if commit || !observed {
				return
			}
			s.sidecarMu.Lock()
			s.decrementSuppressedConsoleExitLocked(key)
			s.sidecarMu.Unlock()
		})
	}
}

func (s *daemonServer) consumeSuppressedConsoleExit(key string) bool {
	if s == nil || key == "" {
		return false
	}
	s.sidecarMu.Lock()
	defer s.sidecarMu.Unlock()
	if s.suppressedConsoleExits[key] == 0 {
		return false
	}
	s.decrementSuppressedConsoleExitLocked(key)
	return true
}

func (s *daemonServer) decrementSuppressedConsoleExitLocked(key string) {
	if count := s.suppressedConsoleExits[key]; count <= 1 {
		delete(s.suppressedConsoleExits, key)
	} else {
		s.suppressedConsoleExits[key] = count - 1
	}
}

// StopConsoleSidecar terminates a console follower owned by this daemon.
func (s *daemonServer) StopConsoleSidecar(key string) (bool, error) {
	return s.stopOwnedSidecar(key)
}

// OwnPortSidecar tracks and reaps a published-port listener.
func (s *daemonServer) OwnPortSidecar(key string, command *exec.Cmd) error {
	return s.ownSidecar("port", key, command, nil)
}

// StopPortSidecar terminates a listener owned by this daemon. The boolean is
// false when the listener survived an older daemon and must be found through
// its advisory lock instead.
func (s *daemonServer) StopPortSidecar(key string) (bool, error) {
	return s.stopOwnedSidecar(key)
}

func consoleClientKey(target selector) string {
	return target.container + "@" + target.branch
}

func consoleClientFamilyKey(container string) string {
	return container + "@*"
}

// RegisterConsoleClient tracks a daemon request that has persistent console
// artifacts open. Registration precedes opening those files, so a concurrent
// branch/family removal either waits for this client or rejects the new reader
// after quiescing has begun.
func (s *daemonServer) RegisterConsoleClient(target selector, cancel func()) (func(), error) {
	if target.container == "" || target.branch == "" || cancel == nil {
		return nil, errors.New("console client requires a branch and cancellation function")
	}
	key := consoleClientKey(target)
	familyKey := consoleClientFamilyKey(target.container)
	client := &daemonConsoleClient{cancel: cancel, done: make(chan struct{})}
	s.consoleClientMu.Lock()
	if s.closingConsoleClient[key] != 0 || s.closingConsoleClient[familyKey] != 0 {
		s.consoleClientMu.Unlock()
		return nil, fmt.Errorf("console clients for %s are being detached", target)
	}
	if s.consoleClients == nil {
		s.consoleClients = make(map[string]map[*daemonConsoleClient]struct{})
	}
	clients := s.consoleClients[key]
	if clients == nil {
		clients = make(map[*daemonConsoleClient]struct{})
		s.consoleClients[key] = clients
	}
	clients[client] = struct{}{}
	s.consoleClientMu.Unlock()
	release := func() {
		client.releaseOnce.Do(func() {
			s.consoleClientMu.Lock()
			if clients := s.consoleClients[key]; clients != nil {
				delete(clients, client)
				if len(clients) == 0 {
					delete(s.consoleClients, key)
				}
			}
			s.consoleClientMu.Unlock()
			close(client.done)
		})
	}
	return release, nil
}

// QuiesceConsoleClients disconnects and waits for console artifact readers.
// An empty branch means the whole family. The returned release function lifts
// the registration fence after the caller has finished deleting artifacts.
func (s *daemonServer) QuiesceConsoleClients(target selector) (func(), error) {
	if target.container == "" {
		return nil, errors.New("console-client quiescing requires a container")
	}
	closingKey := consoleClientKey(target)
	if target.branch == "" {
		closingKey = consoleClientFamilyKey(target.container)
	}
	s.consoleClientMu.Lock()
	if s.closingConsoleClient == nil {
		s.closingConsoleClient = make(map[string]int)
	}
	s.closingConsoleClient[closingKey]++
	var clients []*daemonConsoleClient
	if target.branch == "" {
		prefix := target.container + "@"
		for key, registered := range s.consoleClients {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			for client := range registered {
				clients = append(clients, client)
			}
		}
	} else {
		for client := range s.consoleClients[consoleClientKey(target)] {
			clients = append(clients, client)
		}
	}
	s.consoleClientMu.Unlock()
	release := func() {
		s.consoleClientMu.Lock()
		if count := s.closingConsoleClient[closingKey]; count <= 1 {
			delete(s.closingConsoleClient, closingKey)
		} else {
			s.closingConsoleClient[closingKey] = count - 1
		}
		s.consoleClientMu.Unlock()
	}
	for _, client := range clients {
		client.cancelOnce.Do(client.cancel)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for _, client := range clients {
		select {
		case <-client.done:
		case <-deadline.C:
			release()
			return nil, fmt.Errorf("timed out waiting for %d console client(s) of %s to detach", len(clients), target)
		}
	}
	return release, nil
}

func (s *daemonServer) stopOwnedSidecar(key string) (bool, error) {
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
	return true, fmt.Errorf("sidecar %q did not exit", key)
}

func (s *daemonServer) ownSidecar(kind, key string, command *exec.Cmd, exited func()) error {
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
		if exited != nil {
			exited()
		}
	}()
	return nil
}

func (s *daemonServer) observeInitConsoleExit(target selector) {
	if s == nil || s.dataRoot == "" {
		return
	}
	backend := directBackend(s.optionsFromRequest(), strings.NewReader(""), io.Discard, s.stderr)
	s.configureCommandBackend(backend)
	// Releasing the family's last live branch makes its resident
	// dehydrations durable, which gives the lifecycle lock up meanwhile.
	unlock := backend.holdFamilyLifecycle(backend.familyID(target.container))
	defer unlock()
	changed, err := backend.finalizeObservedInitExit(target)
	if err == nil && changed {
		err = backend.reconcilePlacementCatalog()
	}
	var remoteOwner *remoteBranchOwnershipError
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.As(err, &remoteOwner) && s.stderr != nil {
		fmt.Fprintf(s.stderr, "ttcd: warning: finalizing natural exit for %s: %v\n", target, err)
	}
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
	if socket := os.Getenv("TTC_SOCKET"); socket != "" {
		return socket
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
		localRoot:             opts.localRoot,
		daemonSocket:          opts.daemonSocket,
		runtimePlatform:       opts.runtimePlatform,
		cpuFeatures:           opts.cpuFeatures,
		unsafeNoHostIsolation: opts.unsafeNoHostIsolation,
		disableHostGSO:        opts.disableHostGSO,
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
	backend.publicationQueue = s.publicationQueue
	backend.snapshotGraph = s.snapshotGraph
	backend.snapshotData = s.snapshotData
	backend.snapshotResolver = s.snapshotResolver
	backend.snapshotShared = s.snapshotShared
	backend.sharedReferences = s.sharedReferences
	backend.portIngress = s.portIngress
	backend.lockLifecycle = func() func() { return s.lockDataRoot(backend.dataRoot) }
	backend.operations = &s.operations
}

// configureCommandBackend installs every daemon-owned service needed by a
// backend that executes user-visible lifecycle commands. Keep this separate
// from configureVolumeBackend: target-runtime construction deliberately wires
// some cluster services in stages, whereas an ordinary command backend must
// never observe a partially configured distributed runtime.
func (s *daemonServer) configureCommandBackend(backend *runtimeBackend) {
	s.configureVolumeBackend(backend)
	backend.consoleSidecars = s
	backend.distributedSource = s.distributedSource
	if s.clusterCapabilities != nil {
		backend.nodeID = s.clusterCapabilities.NodeID
	}
}

type daemonStopTarget struct {
	selector selector
}

func branchOwnedByBackend(backend backend, family Family, branch Branch) bool {
	runtime, ok := backend.(*runtimeBackend)
	if !ok || runtime.nodeID == "" {
		return true
	}
	return branch.NodeID == runtime.nodeID
}

func stopDaemonContainers(backend backend) error {
	families, err := backend.listFamilies()
	if err != nil {
		return err
	}
	var targets []daemonStopTarget
	for _, family := range families {
		if runtime, ok := backend.(*runtimeBackend); ok {
			metadataPath := filepath.Join(runtime.dataRoot, "containers", family.ID, "container.json")
			if _, err := os.Stat(metadataPath); errors.Is(err, os.ErrNotExist) {
				// Shared metadata may have been removed by a failed cross-node
				// cleanup while this node still owns a physical Sentry. It is no
				// longer possible to perform branch-aware shutdown, but leaving the
				// orphan behind would make every future daemon stop fail forever.
				if err := runtime.removeOrphanRuntime(family.ID); err != nil {
					return fmt.Errorf("removing orphan runtime %q: %w", family.ID, err)
				}
				continue
			} else if err != nil {
				return fmt.Errorf("inspecting metadata for runtime %q: %w", family.ID, err)
			}
		}
		for _, branch := range family.Branches {
			if !branchOwnedByBackend(backend, family, branch) {
				continue
			}
			switch branch.State {
			case "running", "paused", "dehydrated", "restarting", "exited":
			default:
				continue
			}
			targets = append(targets, daemonStopTarget{
				selector: selector{container: family.ID, branch: branch.ID},
			})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].selector.container != targets[j].selector.container {
			return targets[i].selector.container < targets[j].selector.container
		}
		return targets[i].selector.branch < targets[j].selector.branch
	})
	var stopErrors []error
	for _, target := range targets {
		if err := runStop([]string{target.selector.String()}, io.Discard, io.Discard, backend); err != nil {
			stopErrors = append(stopErrors, fmt.Errorf("stopping %s: %w", target.selector, err))
		}
	}
	return errors.Join(stopErrors...)
}

func dehydrateDaemonContainers(backend backend) error {
	db, ok := backend.(dehydrationBackend)
	if !ok {
		return errors.New("backend does not support dehydration")
	}
	families, err := backend.listFamilies()
	if err != nil {
		return err
	}
	for _, family := range families {
		for _, branch := range family.Branches {
			if !branchOwnedByBackend(backend, family, branch) {
				continue
			}
			switch branch.State {
			case "running", "paused":
				if err := db.dehydrate(selector{container: family.ID, branch: branch.ID}); err != nil {
					return fmt.Errorf("dehydrating %s@%s: %w", family.ID, branch.ID, err)
				}
			case "dehydrated", "exited", "stopped", "created":
			default:
				return fmt.Errorf("branch %s@%s is %s", family.ID, branch.ID, branch.State)
			}
		}
	}
	// A family whose last live branch went some other way may still have a
	// runtime holding resident captures, which only a standalone daemon
	// keeps. Dehydrating everything includes them.
	if runtime, ok := backend.(*runtimeBackend); ok && runtime.dataRoot != "" && runtime.distributedSource == nil && runtime.publicationQueue == nil {
		containers, err := runtime.containerStore().ListContainersForGC()
		if err != nil {
			return err
		}
		for _, info := range containers {
			if err := runtime.deleteRuntimeIfProcessFree(info); err != nil {
				return fmt.Errorf("releasing the runtime of %s: %w", info.Name, err)
			}
		}
	}
	return nil
}

// publishCreatedDaemonContainers seals prepared branches that have no runtime
// but still own node-local mutable filesystem state. They are not active
// execution, so every node-stop policy may make them cold and unplaced.
func publishCreatedDaemonContainers(backend backend) (bool, error) {
	families, err := backend.listFamilies()
	if err != nil {
		return false, err
	}
	published := false
	for _, family := range families {
		for _, branch := range family.Branches {
			if !branchOwnedByBackend(backend, family, branch) || branch.State != "created" {
				continue
			}
			target := branchOperationTarget{
				selector: selector{container: family.ID, branch: branch.ID},
				state:    branch.State,
			}
			if err := stopBranchLifecycle(target, 0, 0, backend, true); err != nil {
				return published, fmt.Errorf("publishing prepared branch %s@%s: %w", family.ID, branch.ID, err)
			}
			published = true
		}
	}
	return published, nil
}

func rejectActiveDaemonContainers(backend backend) error {
	families, err := backend.listFamilies()
	if err != nil {
		return err
	}
	var active []string
	for _, family := range families {
		for _, branch := range family.Branches {
			if !branchOwnedByBackend(backend, family, branch) {
				continue
			}
			switch branch.State {
			case "running", "paused", "restarting", "dehydrating", "rehydrating":
				active = append(active, selector{container: family.ID, branch: branch.ID}.String()+" ("+branch.State+")")
			}
		}
	}
	if len(active) != 0 {
		sort.Strings(active)
		return fmt.Errorf("managed branches are still active: %s; use --migrate-all, --dehydrate-all, or --stop-all", strings.Join(active, ", "))
	}
	return nil
}

func drainSourceDependencies(backend *runtimeBackend, source *distributedSourceRuntime, store cluster.TransactionCatalog, nodeID string) error {
	return drainSourceDependenciesMatching(backend, source, store, nodeID, "", "")
}

// distributedSourceNoLongerNeeded resolves the race between a source-side
// dependency scan and target-side transaction completion. Transactions may be
// retired after they become terminal and their placement and volatile
// references disappear. Consequently, a transaction that was source-backed
// when List returned can legitimately be absent by the time the source asks
// the target to drain it. Re-read the linearizable authority before treating a
// lost target reply as a failure; absence is itself proof that Raft admitted
// the terminal retirement.
func distributedSourceNoLongerNeeded(store cluster.TransactionCatalog, transactionID string) (bool, error) {
	if store == nil || transactionID == "" {
		return false, nil
	}
	tx, err := store.Load(transactionID)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !tx.SourceNeeded || tx.Terminal(), nil
}

// distributedDrainRetryable reports target-local states that are expected to
// settle without operator intervention. The transaction status alone cannot
// distinguish a target that is still copying from one whose asynchronous page
// loader has terminated permanently: both remain Draining until the runtime
// fence succeeds. Retrying a permanent error forever wedges node retirement.
func distributedDrainRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	message := err.Error()
	for _, transient := range []string{
		"still in progress",
		"already in progress",
		"was reinstalled",
		"has not been started",
	} {
		if strings.Contains(message, transient) {
			return true
		}
	}
	return false
}

// distributedDrainStepAdvanced reports the only successful state advances a
// concurrent target worker may commit between a drain RPC and the following
// status read. In that case an RPC revision conflict describes stale input,
// not a failed drain, and the source should continue from the newer state.
func distributedDrainStepAdvanced(previous, current cluster.TransactionState) bool {
	switch previous {
	case cluster.TransactionPublished:
		return current == cluster.TransactionDraining || current == cluster.TransactionIndependent
	case cluster.TransactionDraining:
		return current == cluster.TransactionIndependent
	default:
		return false
	}
}

func migrationSourceRuntime(tx cluster.Transaction) (string, string) {
	container, branch := tx.SourcePhysicalContainer, tx.SourcePhysicalBranch
	if container == "" {
		container = tx.Identity.SourceContainer
	}
	if branch == "" {
		branch = tx.Identity.SourceBranch
	}
	return container, branch
}

func drainSourceDependenciesMatching(backend *runtimeBackend, source *distributedSourceRuntime, store cluster.TransactionCatalog, nodeID, containerID, branchID string) error {
	return drainSourceDependenciesSelected(backend, source, store, nodeID, func(tx cluster.Transaction) bool {
		if containerID != "" && tx.Identity.SourceContainer != containerID {
			return false
		}
		return branchID == "" || tx.Identity.SourceBranch == branchID
	})
}

func drainSourceDependenciesSelected(backend *runtimeBackend, source *distributedSourceRuntime, store cluster.TransactionCatalog, nodeID string, selected func(cluster.Transaction) bool) error {
	if backend == nil || source == nil || store == nil || nodeID == "" {
		return nil
	}
	transactions, err := store.List()
	if err != nil {
		return fmt.Errorf("listing distributed dependencies: %w", err)
	}
	pending := make([]cluster.Transaction, 0, len(transactions))
	for i := range transactions {
		tx := transactions[i]
		if tx.Identity.SourceNodeID != nodeID || !transactionNeedsSourceSettlement(tx) {
			continue
		}
		if selected != nil && !selected(tx) {
			continue
		}
		pending = append(pending, tx)
	}
	// Memory drains are branch-local and may run concurrently. Filesystem
	// materialization remains protected by daemonTargetRuntime's per-family
	// lock, so siblings converge through one repository-wide barrier rather
	// than racing it. Bounded parallelism is essential during node retirement:
	// serially waiting for every remote branch makes evacuation time grow by a
	// full network round trip (and often a 30-second retry window) per branch.
	errs := make([]error, len(pending))
	runBoundedBatch(len(pending), distributedBatchControlConcurrency, func(i int) {
		errs[i] = drainSourceDependency(backend, source, store, pending[i])
	})
	return errors.Join(errs...)
}

func drainSourceDependency(backend *runtimeBackend, source *distributedSourceRuntime, store cluster.TransactionCatalog, tx cluster.Transaction) error {
	// Drive both halves of the independence barrier. Drain materializes
	// remote memory and advances Published -> Draining. MarkIndependent
	// verifies memory completion, materializes every deferred filesystem
	// object, and only then advances Draining -> Independent. Merely polling
	// after Drain leaves filesystem-backed transactions in Draining forever.
	//
	// Each RPC is bounded so a lost peer is observable, but the operation as
	// a whole has no data-size timeout: a healthy target on a throttled shared
	// filesystem may legitimately need longer than any fixed deadline. Both
	// transitions and their runtime operations are idempotent, so a request
	// whose response was lost can be retried safely.
	var peer *cluster.Client
	if tx.State != cluster.TransactionIndependent {
		var err error
		peer, err = source.peer(tx.Identity.TargetNodeID, false)
		if err != nil {
			return fmt.Errorf("finding distributed target node %q for transaction %q: %w", tx.Identity.TargetNodeID, tx.Identity.TransactionID, err)
		}
	}
	for tx.State != cluster.TransactionIndependent {
		var (
			current *cluster.Transaction
			stepErr error
		)
		previousState := tx.State
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		switch tx.State {
		case cluster.TransactionPublished:
			current, stepErr = peer.Drain(ctx, tx.Identity.TransactionID, tx.Revision)
		case cluster.TransactionDraining:
			current, stepErr = peer.MarkIndependent(ctx, tx.Identity.TransactionID, tx.Revision)
		case cluster.TransactionCanceled, cluster.TransactionFailed:
			cancel()
			return fmt.Errorf("distributed target %q became %s while draining: %s", tx.Identity.TargetContainer, tx.State, tx.Failure)
		default:
			cancel()
			return fmt.Errorf("distributed target %q is unexpectedly %s while draining", tx.Identity.TargetContainer, tx.State)
		}
		cancel()
		if stepErr == nil && current != nil {
			tx = *current
			continue
		}

		// The target may have committed the transition after the caller's
		// deadline or returned the expected "still in progress" conflict.
		// Re-read its state before deciding whether the retry is necessary.
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		current, statusErr := peer.Status(ctx, tx.Identity.TransactionID)
		cancel()
		if statusErr == nil {
			tx = *current
			if tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed {
				return fmt.Errorf("distributed target %q became %s while draining: %s", tx.Identity.TargetContainer, tx.State, tx.Failure)
			}
			if distributedDrainStepAdvanced(previousState, tx.State) {
				continue
			}
			if !distributedDrainRetryable(stepErr) {
				return fmt.Errorf("draining distributed target %q failed permanently while transaction remained %s: %w", tx.Identity.TargetContainer, tx.State, stepErr)
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		settled, authorityErr := distributedSourceNoLongerNeeded(store, tx.Identity.TransactionID)
		if authorityErr != nil {
			return fmt.Errorf("rechecking distributed target %q while draining: %w", tx.Identity.TargetContainer, authorityErr)
		}
		if settled {
			tx.State = cluster.TransactionIndependent
			break
		}
		return fmt.Errorf("draining distributed target %q: %w", tx.Identity.TargetContainer, errors.Join(stepErr, statusErr))
	}
	if tx.Identity.Migration {
		// An evacuating daemon cannot leave physical cleanup to the ordinary
		// asynchronous migration monitor: node-stop may close the daemon and
		// discard its local root immediately after this barrier. Independence
		// proves the target no longer reads the source, so synchronously release
		// both the transfer pin and the paused source Kernel. Both operations
		// are idempotent with a monitor that happened to win the race.
		sourceContainer, sourceBranch := migrationSourceRuntime(tx)
		unlockSource := source.lockSourceFamily(sourceContainer)
		_, _ = backend.command("timetravel", sourceContainer, "distributed-complete", tx.Identity.TransactionID)
		if err := backend.releaseDistributedGraphReferences(graphTransferReferenceOperation(tx.Identity.TransactionID)); err != nil {
			unlockSource()
			return fmt.Errorf("releasing migrated graph roots for %q: %w", tx.Identity.TransactionID, err)
		}
		if err := backend.releaseMigratedSourceRuntime(sourceContainer, sourceBranch); err != nil {
			unlockSource()
			return fmt.Errorf("releasing migrated source %s@%s: %w", tx.Identity.SourceContainer, tx.Identity.SourceBranch, err)
		}
		unlockSource()
	} else {
		_, _ = backend.command("timetravel", tx.Identity.SourceContainer, "distributed-complete", tx.Identity.TransactionID)
	}
	return nil
}

// settleDistributedSourceTransactions is the source-side barrier used before
// a multi-node family mutation destroys any target. Family removal is
// deliberately executed as branch-local child commands; without this barrier
// an early target delete can make a later source drain fail, leaving a
// half-removed family and retained source pages. Transaction IDs are validated
// against Raft and this node's source authority before any work is performed.
func (b *runtimeBackend) settleDistributedSourceTransactions(transactionIDs []string) error {
	if b == nil || b.distributedSource == nil || b.nodeID == "" || len(transactionIDs) == 0 {
		return nil
	}
	store, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	wanted := make(map[string]struct{}, len(transactionIDs))
	for _, transactionID := range transactionIDs {
		if transactionID == "" {
			return errors.New("empty distributed transaction ID")
		}
		tx, err := store.Load(transactionID)
		if errors.Is(err, os.ErrNotExist) {
			// A retry after the terminal transaction was retired is complete.
			continue
		}
		if err != nil {
			return fmt.Errorf("loading distributed transaction %q: %w", transactionID, err)
		}
		if tx.Identity.SourceNodeID != b.nodeID {
			return fmt.Errorf("distributed transaction %q is sourced by node %q, not %q", transactionID, tx.Identity.SourceNodeID, b.nodeID)
		}
		wanted[transactionID] = struct{}{}
	}
	return drainSourceDependenciesSelected(b, b.distributedSource, store, b.nodeID, func(tx cluster.Transaction) bool {
		_, ok := wanted[tx.Identity.TransactionID]
		return ok
	})
}

// cancelDistributedForkTargets removes published remote-fork targets that are
// themselves part of a destructive rm operation. No state from such a target
// can survive the mutation, so forcing it through the ordinary independence
// barrier would copy its entire resident memory and filesystem only to delete
// those bytes immediately afterward. The target cleanup and Raft cancellation
// are one idempotent control-plane operation; source monitors observe the
// terminal transaction and release their capture sessions asynchronously.
func (b *runtimeBackend) cancelDistributedForkTargets(transactionIDs []string) error {
	if b == nil || b.distributedSource == nil || b.nodeID == "" || len(transactionIDs) == 0 {
		return nil
	}
	store, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	for _, transactionID := range transactionIDs {
		tx, err := store.Load(transactionID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("loading removable distributed transaction %q: %w", transactionID, err)
		}
		if tx.Identity.SourceNodeID != b.nodeID {
			return fmt.Errorf("distributed transaction %q is sourced by node %q, not %q", transactionID, tx.Identity.SourceNodeID, b.nodeID)
		}
		if tx.Identity.Migration {
			return fmt.Errorf("migration transaction %q cannot use remote-fork cancellation", transactionID)
		}
		switch tx.State {
		case cluster.TransactionCanceled, cluster.TransactionFailed, cluster.TransactionIndependent:
			continue
		case cluster.TransactionDraining:
			if err := drainSourceDependency(b, b.distributedSource, store, *tx); err != nil {
				return fmt.Errorf("finishing removable distributed target %q: %w", transactionID, err)
			}
			continue
		}
		peer, err := b.distributedSource.peer(tx.Identity.TargetNodeID, false)
		if err != nil {
			return fmt.Errorf("finding removable distributed target node %q for transaction %q: %w", tx.Identity.TargetNodeID, transactionID, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		canceled, cancelErr := cancelRemoteForkTarget(ctx, peer, transactionID, tx.Revision)
		cancel()
		if cancelErr != nil {
			return fmt.Errorf("canceling removable distributed target %q: %w", transactionID, cancelErr)
		}
		if canceled == nil {
			return fmt.Errorf("canceling removable distributed target %q returned no transaction", transactionID)
		}
		switch canceled.State {
		case cluster.TransactionCanceled, cluster.TransactionFailed, cluster.TransactionIndependent:
			// Independent means draining won the race with removal. The physical
			// target was not canceled and the metadata child below will delete it
			// through the ordinary branch path.
		case cluster.TransactionDraining:
			if err := drainSourceDependency(b, b.distributedSource, store, *canceled); err != nil {
				return fmt.Errorf("finishing raced distributed target %q: %w", transactionID, err)
			}
		default:
			return fmt.Errorf("canceling removable distributed target %q returned %s", transactionID, canceled.State)
		}
	}
	return nil
}

// cancelRemoteForkTarget retries optimistic cancellation across transaction
// progress that races a family removal. Cancellation is fenced in Raft before
// target cleanup, and a canceled retry repeats that idempotent cleanup. If
// draining wins the revision race, the target remains intact and the caller
// rolls it forward to independence instead.
func cancelRemoteForkTarget(ctx context.Context, peer *cluster.Client, transactionID string, revision uint64) (*cluster.Transaction, error) {
	var lastErr error
	for range 16 { // More than the complete finite transaction state sequence.
		canceled, err := peer.Cancel(ctx, transactionID, revision)
		if err == nil && canceled != nil {
			switch canceled.State {
			case cluster.TransactionCanceled, cluster.TransactionFailed, cluster.TransactionDraining, cluster.TransactionIndependent:
				return canceled, nil
			}
		}
		if err == nil {
			err = errors.New("cancellation returned a nonterminal transaction")
		}
		lastErr = errors.Join(lastErr, err)
		current, statusErr := peer.Status(ctx, transactionID)
		if statusErr != nil {
			return nil, errors.Join(lastErr, statusErr)
		}
		switch current.State {
		case cluster.TransactionCanceled, cluster.TransactionFailed, cluster.TransactionDraining, cluster.TransactionIndependent:
			return current, nil
		}
		if current.Revision == revision {
			return nil, lastErr
		}
		revision = current.Revision
	}
	return nil, fmt.Errorf("canceling transaction %q did not converge: %w", transactionID, lastErr)
}

// removeCanceledForkPlacement commits the metadata half of a remote-fork
// cancellation. TargetRuntime.Cancel has already destroyed the physical
// branch, its filesystems, and its graph references, so routing this through
// ordinary branch destruction would incorrectly require that runtime to still
// exist. Raft placement removal also performs the user-visible lineage splice.
func (b *runtimeBackend) removeCanceledForkPlacement(target selector, transactionID string) error {
	if b == nil || b.nodeID == "" || b.distributedSource == nil {
		return errors.New("canceled-fork placement removal requires a clustered daemon")
	}
	transactions, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	tx, err := transactions.Load(transactionID)
	if errors.Is(err, os.ErrNotExist) {
		placements, placementErr := b.placementCatalog()
		if placementErr != nil {
			return placementErr
		}
		if _, lookupErr := placements.Lookup(target.container, target.branch); errors.Is(lookupErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("canceled transaction %q disappeared while placement %s remained", transactionID, target)
	}
	if err != nil {
		return err
	}
	if tx.Identity.Migration || tx.Identity.TargetContainer != target.container {
		return fmt.Errorf("transaction %q does not identify canceled fork target %s", transactionID, target)
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	record, err := placements.Lookup(target.container, target.branch)
	if errors.Is(err, os.ErrNotExist) {
		return transactions.Delete(transactionID, tx.Revision)
	}
	if err != nil {
		return err
	}
	if !canceledForkOwnsPlacement(*tx, *record, b.nodeID) {
		return fmt.Errorf("canceled fork target %s placement changed before removal", target)
	}
	if tx.State == cluster.TransactionIndependent {
		// The target crossed the draining barrier after the coordinator planned
		// cancellation but before its preflight ran. Cancellation intentionally
		// leaves an independent runtime intact; remove it through the same path as
		// any other running branch so Sentry, filesystems, and placement metadata
		// are reclaimed together.
		return b.destroy(target, true)
	}
	if tx.State != cluster.TransactionCanceled && tx.State != cluster.TransactionFailed {
		return fmt.Errorf("transaction %q is %s, neither canceled nor independent", transactionID, tx.State)
	}
	if tx.SourceNeeded {
		return fmt.Errorf("canceled transaction %q still retains its source", transactionID)
	}
	if err := placements.Remove(record.FamilyID, record.BranchID, record.Generation); err != nil {
		return err
	}
	if err := b.retireTransactionsForRemovedPlacement(*record); err != nil {
		return err
	}
	_, err = b.reconcileVolatileSnapshotOrigins()
	return err
}

// canceledForkOwnsPlacement verifies the authority that survives target
// runtime cancellation. Ordinary remote-fork publication deliberately keeps
// physical identity exclusively in PlacementRecord; unlike migrations, its
// Transaction has empty TargetPhysical* fields. The stable transaction-to-
// placement fence is therefore the exact logical target plus the target node
// that still owns that placement.
func canceledForkOwnsPlacement(tx cluster.Transaction, placement cluster.PlacementRecord, localNode string) bool {
	operationMatches := tx.Identity.TransactionID != "" &&
		(placement.OperationID == tx.Identity.TransactionID || placement.LastOperationID == tx.Identity.TransactionID)
	branchMatches := tx.Identity.VisibleTargetBranch() == placement.BranchID || operationMatches
	return !tx.Identity.Migration &&
		tx.Identity.TargetContainer == placement.FamilyID &&
		branchMatches &&
		tx.Identity.TargetNodeID == placement.OwnerNode &&
		placement.OwnerNode == localNode
}

// prepareRemovedMigrationSources reclaims retired physical migration sources
// before rm deletes their target placements. Independence proves that the live
// target no longer faults from the source. Keeping the target placement and
// terminal transaction authoritative during cleanup prevents the source's
// canonical family metadata from being mistaken for a newly unowned branch.
func (b *runtimeBackend) prepareRemovedMigrationSources(transactionIDs []string) error {
	if b == nil || b.distributedSource == nil || b.nodeID == "" || len(transactionIDs) == 0 {
		return nil
	}
	store, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	for _, transactionID := range transactionIDs {
		tx, err := store.Load(transactionID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("loading removed migration transaction %q: %w", transactionID, err)
		}
		if !tx.Identity.Migration || tx.State != cluster.TransactionIndependent {
			return fmt.Errorf("transaction %q is not an independent migration", transactionID)
		}
		if placement, lookupErr := placements.Lookup(tx.Identity.SourceContainer, tx.Identity.SourceBranch); lookupErr == nil {
			sourceContainer, sourceBranch := migrationSourceRuntime(*tx)
			if placement.PhysicalContainer == sourceContainer && placement.PhysicalBranch == sourceBranch {
				return fmt.Errorf("migration source %q is still the authoritative physical placement", transactionID)
			}
		} else if !errors.Is(lookupErr, os.ErrNotExist) {
			return fmt.Errorf("checking migration target placement %q: %w", transactionID, lookupErr)
		}
		if err := b.cleanupRemovedMigrationSource(*tx); err != nil {
			return err
		}
	}
	return nil
}

func (b *runtimeBackend) cleanupRemovedMigrationSource(tx cluster.Transaction) error {
	transactionID := tx.Identity.TransactionID
	if tx.Identity.SourceNodeID == b.nodeID {
		sourceContainer, sourceBranch := migrationSourceRuntime(tx)
		unlockSource := b.distributedSource.lockSourceFamily(sourceContainer)
		defer unlockSource()
		_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
		if err := b.releaseDistributedGraphReferences(graphTransferReferenceOperation(transactionID)); err != nil {
			return fmt.Errorf("releasing removed migration roots %q: %w", transactionID, err)
		}
		if err := b.releaseMigratedSourceRuntimeDiscard(sourceContainer, sourceBranch); err != nil {
			return fmt.Errorf("discarding removed migration source %s@%s: %w", tx.Identity.SourceContainer, tx.Identity.SourceBranch, err)
		}
		if err := b.removeLocalFamilyStateIfUnowned(tx.Identity.SourceContainer); err != nil {
			return fmt.Errorf("reclaiming removed migration source family %s: %w", tx.Identity.SourceContainer, err)
		}
		return nil
	}
	// A live coordinator may retire the metadata for a source incarnation that
	// Raft has already fenced. Its node-local runtime and graph-reference intent
	// are disposable residue: the lease guardian stopped execution before the
	// deadline, and startup cache reconciliation removes them if that physical
	// node returns. This keeps family deletion available after an abrupt node
	// loss without weakening the exact-incarnation fence.
	if b.distributedSource.authority == nil {
		return fmt.Errorf("transaction %q is sourced by node %q, not cleanup node %q", transactionID, tx.Identity.SourceNodeID, b.nodeID)
	}
	state, err := b.distributedSource.authority.AuthorityState()
	if err != nil {
		return fmt.Errorf("checking retired migration source %q: %w", transactionID, err)
	}
	if !migrationSourceIncarnationFenced(state, tx, time.Now()) {
		return fmt.Errorf("transaction %q is sourced by live node %q incarnation %q, not cleanup node %q", transactionID, tx.Identity.SourceNodeID, tx.SourceIncarnation, b.nodeID)
	}
	return nil
}

// migrationSourceIncarnationFenced reports whether Raft proves that a
// migration's former source cannot still execute. A replacement incarnation
// is itself a fence for the old one; an offline/expired exact incarnation is
// fenced by the lease protocol; and graceful retirement records an explicit
// durable fence after local cleanup.
func migrationSourceIncarnationFenced(state *cluster.AuthorityState, tx cluster.Transaction, now time.Time) bool {
	if state == nil || tx.Identity.SourceNodeID == "" || tx.SourceIncarnation == "" {
		return false
	}
	if retired, ok := state.RetiredNodes[tx.Identity.SourceNodeID]; ok && retired == tx.SourceIncarnation {
		return true
	}
	record, ok := state.Nodes[tx.Identity.SourceNodeID]
	if !ok {
		return false
	}
	if record.Incarnation != tx.SourceIncarnation {
		return true
	}
	return !record.LeaseActive(now)
}

// discardRemovedMigrationSources completes distributed rm after logical
// placement deletion. Source cleanup is idempotently rechecked, then the
// terminal transaction can be retired without losing its recovery intent.
func (b *runtimeBackend) discardRemovedMigrationSources(transactionIDs []string) error {
	if b == nil || b.distributedSource == nil || b.nodeID == "" || len(transactionIDs) == 0 {
		return nil
	}
	store, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	for _, transactionID := range transactionIDs {
		tx, err := store.Load(transactionID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("loading removed migration transaction %q: %w", transactionID, err)
		}
		if !tx.Identity.Migration || tx.State != cluster.TransactionIndependent {
			return fmt.Errorf("transaction %q is not an independent migration", transactionID)
		}
		if _, err := placements.Lookup(tx.Identity.SourceContainer, tx.Identity.SourceBranch); err == nil {
			return fmt.Errorf("cannot retire migration source %q while placement %s@%s still exists", transactionID, tx.Identity.SourceContainer, tx.Identity.SourceBranch)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checking removed migration placement %q: %w", transactionID, err)
		}
		if err := b.cleanupRemovedMigrationSource(*tx); err != nil {
			return err
		}
		current, err := store.Load(transactionID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := store.Delete(transactionID, current.Revision); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("retiring removed migration transaction %q: %w", transactionID, err)
		}
	}
	return nil
}

// reconcileRemovedMigrationSources resumes the rm postflight after a daemon
// or coordinator crash. Placement deletion is the irreversible proof that the
// logical target no longer exists; the retained terminal transaction supplies
// the exact old physical identity until cleanup succeeds.
func (b *runtimeBackend) reconcileRemovedMigrationSources() error {
	if b == nil || b.distributedSource == nil || b.nodeID == "" {
		return nil
	}
	store, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	transactions, err := store.List()
	if err != nil {
		return err
	}
	var ids []string
	for _, tx := range transactions {
		if !tx.Identity.Migration || tx.State != cluster.TransactionIndependent || tx.Identity.SourceNodeID != b.nodeID {
			continue
		}
		if _, err := placements.Lookup(tx.Identity.SourceContainer, tx.Identity.SourceBranch); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		ids = append(ids, tx.Identity.TransactionID)
	}
	sort.Strings(ids)
	return b.discardRemovedMigrationSources(ids)
}

// ensureDistributedSourceIndependent prevents branch-destructive operations
// from invalidating an already-published remote child. In the normal path the
// asynchronous graph handoff has already completed, so this is a no-op.
func (b *runtimeBackend) ensureDistributedSourceIndependent(containerID, branchID string) error {
	if b == nil || b.distributedSource == nil || b.nodeID == "" || b.dataRoot == "" {
		return nil
	}
	store, err := b.transactionCatalog()
	if err != nil {
		return err
	}
	return drainSourceDependenciesMatching(b, b.distributedSource, store, b.nodeID, containerID, branchID)
}

// optionsFromRequest applies daemon-owned configuration. Runtime state, the
// runtime executable, platform, isolation, and persistent data all belong to
// the daemon selected by the socket; accepting client overrides would let two
// independently configured daemons accidentally operate on the same runtime.
func (s *daemonServer) optionsFromRequest() options {
	opts := options{}
	opts.root = s.root
	opts.runsc = s.runsc
	opts.unsafeNoHostIsolation = s.unsafeNoHostIsolation
	opts.disableHostGSO = s.disableHostGSO
	opts.runtimePlatform = s.runtimePlatform
	opts.cpuFeatures = s.cpuFeatures
	opts.dataRoot = s.dataRoot
	opts.localRoot = s.localRoot
	opts.daemonSocket = s.socket
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

func normalizeRuntimePlatform(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "systrap", "kvm":
		return value, nil
	default:
		return "", fmt.Errorf("unsupported runtime platform %q; supported platforms are systrap and kvm", value)
	}
}

func runtimePlatformFromEnvironment() string {
	value := os.Getenv("TTC_RUNTIME_PLATFORM")
	if normalized, err := normalizeRuntimePlatform(value); err == nil {
		return normalized
	}
	return defaultRuntimePlatform
}

func preflightDaemonPlatform(runtimePlatform string) error {
	if runtimePlatform != "kvm" {
		return nil
	}
	device, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("runtime platform kvm requires read/write access to /dev/kvm: %w", err)
	}
	defer device.Close()
	version, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(), kvmGetAPIVersion, 0)
	if errno != 0 {
		return fmt.Errorf("querying /dev/kvm API version: %w", errno)
	}
	if version != kvmAPIVersion {
		return fmt.Errorf("unsupported /dev/kvm API version %d; want %d", version, kvmAPIVersion)
	}
	vmFD, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(), kvmCreateVM, 0)
	if errno != 0 {
		return fmt.Errorf("creating a KVM VM: %w", errno)
	}
	if err := unix.Close(int(vmFD)); err != nil {
		return fmt.Errorf("closing KVM preflight VM: %w", err)
	}
	return nil
}

// RunDaemon serves the versioned TTC local API from the ttcd binary.
func RunDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == daemonRootlessProbeArg {
		return 0
	}
	socket := defaultDaemonSocket()
	runtimePlatform := defaultRuntimePlatform
	unsafeNoHostIsolation := false
	clusterOpts := defaultDaemonClusterOptions()
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
		case args[0] == "--platform":
			if len(args) < 2 || args[1] == "" {
				fmt.Fprintln(stderr, "ttcd: --platform requires systrap or kvm")
				return 2
			}
			runtimePlatform = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--platform="):
			runtimePlatform = strings.TrimPrefix(args[0], "--platform=")
			args = args[1:]
		default:
			if consumed, err := consumeDaemonCPUOption(args, &clusterOpts); consumed != 0 || err != nil {
				if err != nil {
					fmt.Fprintf(stderr, "ttcd: %v\n", err)
					return 2
				}
				args = args[consumed:]
				continue
			}
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
	roots, err := daemonRootsFromEnvironment(clusterOpts.Configured && !clusterOpts.ControlOnly)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 2
	}
	sharedRole := daemonRootRoleStandaloneAuthority
	if clusterOpts.Configured {
		sharedRole = daemonRootRoleClusterShared
	}
	if err := inspectDaemonStorageRoot(roots.shared, sharedRole); err != nil {
		fmt.Fprintf(stderr, "ttcd: validating TTC_ROOT: %v\n", err)
		return 1
	}
	if clusterOpts.Configured && !clusterOpts.ControlOnly {
		if err := inspectDaemonStorageRoot(roots.local, daemonRootRoleClusterLocal); err != nil {
			fmt.Fprintf(stderr, "ttcd: validating TTC_LOCAL_ROOT: %v\n", err)
			return 1
		}
	}
	if os.Getenv("TTC_SNAPSHOT_GRAPH_STRICT") == "1" {
		if err := validateStrictSnapshotGraphRoot(roots.shared); err != nil {
			fmt.Fprintf(stderr, "ttcd: validating strict TTC_ROOT: %v\n", err)
			return 1
		}
		if clusterOpts.Configured && !clusterOpts.ControlOnly {
			if err := validateStrictSnapshotGraphRoot(roots.local); err != nil {
				fmt.Fprintf(stderr, "ttcd: validating strict TTC_LOCAL_ROOT: %v\n", err)
				return 1
			}
		}
	}
	if clusterOpts.Configured {
		if err := resolveSimpleClusterOptions(&clusterOpts, roots); err != nil {
			fmt.Fprintf(stderr, "ttcd: configuring cluster identity: %v\n", err)
			return 2
		}
	}
	if clusterOpts.ControlOnly {
		if unsafeNoHostIsolation {
			fmt.Fprintln(stderr, "ttcd: --unsafe-no-host-isolation is not applicable to a control-only daemon")
			return 2
		}
		control, err := prepareControlOnlyCluster(clusterOpts, roots.shared)
		if err != nil {
			fmt.Fprintf(stderr, "ttcd: configuring control-only cluster access: %v\n", err)
			return 1
		}
		if err := serveControlOnlyDaemon(socket, roots.shared, control, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "ttcd: %v\n", err)
			return 1
		}
		return 0
	}
	daemonDataRoot := roots.shared
	daemonRoot := roots.runtime
	if !clusterOpts.Configured {
		// TTC_ROOT is authoritative standalone storage and may itself be on a
		// shared filesystem. runsc's live control sockets and process-state files
		// are ephemeral host runtime state, not persistent container data; keep
		// them beside the explicitly local daemon socket. Clustered daemons use
		// TTC_LOCAL_ROOT instead because peers need a stable node-local scratch
		// hierarchy independent of the client socket location.
		daemonRoot = standaloneRuntimeRoot(socket)
	}
	daemonRunsc, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: finding embedded runtime executable: %v\n", err)
		return 1
	}
	runtimePlatform, err = normalizeRuntimePlatform(runtimePlatform)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 2
	}
	if err := daemonPlatformPreflight(runtimePlatform); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
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
	if err := prepareDaemonStorageRoot(roots.shared, sharedRole, clusterOpts.RepositoryID, ""); err != nil {
		fmt.Fprintf(stderr, "ttcd: preparing TTC_ROOT: %v\n", err)
		return 1
	}
	var cpuProfile daemonCPUProfile
	if !clusterOpts.Configured {
		cpuProfile, err = applyStandaloneCPUProfile(roots.shared, clusterOpts.CPUProfileMode, clusterOpts.CPUFeatures, runtimePlatform, clusterOpts.CPUProfileSet)
		if err != nil {
			fmt.Fprintf(stderr, "ttcd: configuring CPU profile: %v\n", err)
			return 1
		}
	}
	if clusterOpts.Configured {
		if err := prepareDaemonStorageRoot(roots.local, daemonRootRoleClusterLocal, clusterOpts.RepositoryID, clusterOpts.NodeID); err != nil {
			fmt.Fprintf(stderr, "ttcd: preparing TTC_LOCAL_ROOT: %v\n", err)
			return 1
		}
	}
	localLock, err := lockDaemonLocalRoot(roots.local)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: locking TTC local root: %v\n", err)
		return 1
	}
	defer localLock.Close()
	if err := prepareDaemonRuntimeStorage(socket, daemonRoot); err != nil {
		fmt.Fprintf(stderr, "ttcd: preparing node-local runtime storage: %v\n", err)
		return 1
	}
	manager, err := ttcvolume.OpenManager(roots.local)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: initializing disk-backed volumes: %v\n", err)
		return 1
	}
	store := ttcbundle.Store{Root: daemonDataRoot, ActiveRoot: roots.local}
	// Namespace recovery must precede every startup inventory. In particular,
	// image-root pruning uses a conservative container scan and should never be
	// reached with a half-deleted directory still in containers/.
	if _, err := store.RecoverContainerRemovals(); err != nil {
		fmt.Fprintf(stderr, "ttcd: warning: recovering removed containers before startup inventory: %v\n", err)
	}
	if _, err := store.PruneImageRoots(context.Background()); err != nil {
		fmt.Fprintf(stderr, "ttcd: pruning stale image roots: %v\n", err)
		return 1
	}
	if _, _, err := store.PruneSharedImagePacks(context.Background()); err != nil {
		fmt.Fprintf(stderr, "ttcd: warning: pruning shared image packs: %v\n", err)
	}
	clusterRuntime, err := prepareDaemonCluster(clusterOpts, daemonDataRoot, roots.local, runtimePlatform, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: configuring cluster peer service: %v\n", err)
		return 1
	}
	if clusterRuntime != nil {
		cpuProfile = clusterRuntime.cpuProfile
	}
	if err := serveDaemonWithCluster(socket, runtimePlatform, cpuProfile, unsafeNoHostIsolation, kernelCompatibility, daemonDataRoot, roots.local, daemonRoot, daemonRunsc, manager.Capability(), clusterRuntime, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		if errors.Is(err, errLeaseSelfFenced) {
			fmt.Fprintln(stderr, "ttcd: restarting to wait for the cluster; this node's sandboxes stay stopped until it answers")
			return DaemonRestartExitCode
		}
		return 1
	}
	return 0
}

// DaemonRestartExitCode is what RunDaemon returns when the daemon must start
// again in the same process, as ttcd does by executing itself anew: its node
// lost its execution lease, stopped its Sentries, and must now wait for the
// cluster rather than end, which on a batch system would end its job and lose
// everything the node was running.
const DaemonRestartExitCode = 75

type daemonRoots struct {
	shared  string
	local   string
	runtime string
}

func resolveSimpleClusterOptions(opts *daemonClusterOptions, roots daemonRoots) error {
	if opts.ControlOnly {
		if opts.RaftBootstrap {
			return errors.New("--control-only cannot be combined with --cluster-init")
		}
		if len(opts.Peers) == 0 {
			return errors.New("--control-only requires at least one --cluster-join HOST:PORT")
		}
		if opts.Listen != "" || opts.Advertise != "" || opts.PageAdvertise != "" {
			return errors.New("a control-only daemon is outbound-only; --listen, --advertise, and --cluster-page-advertise are not permitted")
		}
		if opts.MemoryTarget != 0 || opts.AutoRebalance || opts.CapacityCommand != "" || opts.CapacityWebhook != "" {
			return errors.New("placement capacity and automatic rebalancing options are not applicable to a control-only daemon")
		}
		if opts.NodeID == "" {
			hostname, err := os.Hostname()
			if err != nil || strings.TrimSpace(hostname) == "" {
				return errors.New("cannot derive control client name from hostname; set --node")
			}
			opts.NodeID = "control-" + strings.TrimSpace(hostname)
		}
		if !transactionIDPatternForDaemon(opts.NodeID) {
			return fmt.Errorf("invalid control client name %q; set --node to letters, digits, '.', '_', or '-'", opts.NodeID)
		}
		marker, err := readClusterSharedRootMarker(roots.shared, clusterSharedRootVisibilityTimeout)
		if err != nil {
			return fmt.Errorf("control-only mode requires an initialized cluster TTC_ROOT: %w", err)
		}
		if opts.RepositoryID != "" && opts.RepositoryID != marker.RepositoryID {
			return fmt.Errorf("configured repository %q does not match TTC_ROOT repository %q", opts.RepositoryID, marker.RepositoryID)
		}
		opts.RepositoryID = marker.RepositoryID
		// No peer may dial this daemon. In particular it owns neither a page
		// listener nor a Raft/control address.
		opts.PageListen = ""
		return nil
	}
	if opts.RaftBootstrap && len(opts.Peers) != 0 {
		return errors.New("--cluster-init and --cluster-join are mutually exclusive")
	}
	if opts.Listen == "" {
		opts.Listen = "0.0.0.0:7443"
	}
	if opts.NodeID == "" {
		path := filepath.Join(roots.local, ".ttc-root-format")
		if data, err := os.ReadFile(path); err == nil {
			var marker daemonRootMarker
			if err := json.Unmarshal(data, &marker); err != nil {
				return err
			}
			opts.NodeID = marker.NodeID
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			hostname, err := os.Hostname()
			if err != nil || strings.TrimSpace(hostname) == "" {
				return errors.New("cannot derive cluster node name from hostname; set --node")
			}
			opts.NodeID = strings.TrimSpace(hostname)
		}
	}
	if opts.RepositoryID == "" {
		visibilityWait := clusterSharedRootVisibilityTimeout
		if opts.RaftBootstrap {
			// The first node owns initialization; a genuinely fresh root must not
			// pay the joiner's NFS visibility grace period.
			visibilityWait = 0
		}
		if marker, err := readClusterSharedRootMarker(roots.shared, visibilityWait); err == nil {
			opts.RepositoryID = marker.RepositoryID
		} else if opts.RaftBootstrap && errors.Is(err, os.ErrNotExist) {
			id, err := newDaemonRequestID()
			if err != nil {
				return err
			}
			opts.RepositoryID = "repo-" + strings.TrimPrefix(id, "req-")
		} else if opts.RaftBootstrap {
			return err
		} else {
			return fmt.Errorf("joining a cluster requires an initialized shared TTC_ROOT: %w", err)
		}
	}
	_, port, err := net.SplitHostPort(opts.Listen)
	if err != nil {
		return fmt.Errorf("invalid --listen %q: %w", opts.Listen, err)
	}
	if opts.Advertise == "" {
		advertiseHost, err := os.Hostname()
		if err != nil || advertiseHost == "" {
			return errors.New("cannot derive cluster advertise hostname; set --advertise")
		}
		opts.Advertise = net.JoinHostPort(advertiseHost, port)
	} else {
		_, _, err = net.SplitHostPort(opts.Advertise)
		if err != nil {
			return fmt.Errorf("invalid --advertise %q: %w", opts.Advertise, err)
		}
	}
	return nil
}

// readClusterSharedRootMarker reads and validates the shared-root marker. The
// bounded wait is required for NFS clients that cached the root as nonexistent
// immediately before another node initialized it. Errors other than ENOENT are
// never retried: malformed, inaccessible, and incompatible roots fail closed.
func readClusterSharedRootMarker(root string, wait time.Duration) (daemonRootMarker, error) {
	markerPath := filepath.Join(root, ".ttc-root-format")
	deadline := time.Now().Add(wait)
	for {
		data, err := os.ReadFile(markerPath)
		if err == nil {
			var marker daemonRootMarker
			if err := json.Unmarshal(data, &marker); err != nil {
				return daemonRootMarker{}, fmt.Errorf("decoding cluster TTC_ROOT marker: %w", err)
			}
			want, err := daemonRootMarkerFor(daemonRootRoleClusterShared, marker.RepositoryID, "")
			if err != nil || marker != want {
				return daemonRootMarker{}, errors.New("TTC_ROOT is not a current-format clustered shared root")
			}
			return marker, nil
		}
		if !errors.Is(err, os.ErrNotExist) || wait <= 0 || !time.Now().Before(deadline) {
			return daemonRootMarker{}, err
		}
		delay := 100 * time.Millisecond
		if remaining := time.Until(deadline); remaining < delay {
			delay = remaining
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
}

// daemonRootsFromEnvironment is the only source of daemon storage paths.
// Keeping this out of the client prevents client/daemon disagreement and makes
// it possible for a client to operate without access to either root.
func daemonRootsFromEnvironment(clustered bool) (daemonRoots, error) {
	if legacy := strings.TrimSpace(os.Getenv("TTC_DATA_ROOT")); legacy != "" {
		return daemonRoots{}, errors.New("TTC_DATA_ROOT is no longer supported; configure a fresh TTC_ROOT (existing data roots are not migrated)")
	}
	shared := strings.TrimSpace(os.Getenv("TTC_ROOT"))
	if shared == "" {
		return daemonRoots{}, errors.New("TTC_ROOT must be explicitly set to a fresh absolute path")
	}
	if !filepath.IsAbs(shared) {
		return daemonRoots{}, fmt.Errorf("TTC_ROOT %q is not an absolute path", shared)
	}
	shared = filepath.Clean(shared)
	local := shared
	if clustered {
		local = strings.TrimSpace(os.Getenv("TTC_LOCAL_ROOT"))
		if local == "" {
			return daemonRoots{}, errors.New("TTC_LOCAL_ROOT must be explicitly set for cluster mode")
		}
		if !filepath.IsAbs(local) {
			return daemonRoots{}, fmt.Errorf("TTC_LOCAL_ROOT %q is not an absolute path", local)
		}
		local = filepath.Clean(local)
		same, err := pathsReferToSameLocation(shared, local)
		if err != nil {
			return daemonRoots{}, err
		}
		if same {
			return daemonRoots{}, errors.New("TTC_LOCAL_ROOT must be distinct from shared TTC_ROOT in cluster mode")
		}
	}
	return daemonRoots{shared: shared, local: local, runtime: filepath.Join(local, "runtime", "runsc")}, nil
}

func standaloneRuntimeRoot(socket string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(socket)), "runsc")
}

func pathsReferToSameLocation(first, second string) (bool, error) {
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	if firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo) {
		return true, nil
	}
	if firstErr != nil && !errors.Is(firstErr, os.ErrNotExist) {
		return false, fmt.Errorf("stating storage root %q: %w", first, firstErr)
	}
	if secondErr != nil && !errors.Is(secondErr, os.ErrNotExist) {
		return false, fmt.Errorf("stating storage root %q: %w", second, secondErr)
	}
	resolve := func(path string) (string, error) {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolving storage root %q: %w", path, err)
		}
		parent, base := filepath.Dir(path), filepath.Base(path)
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, base), nil
		}
		return filepath.Clean(path), nil
	}
	a, err := resolve(first)
	if err != nil {
		return false, err
	}
	b, err := resolve(second)
	if err != nil {
		return false, err
	}
	if a == b {
		return true, nil
	}
	if filepath.Base(a) != filepath.Base(b) {
		return false, nil
	}
	firstParent, firstParentErr := os.Stat(filepath.Dir(a))
	secondParent, secondParentErr := os.Stat(filepath.Dir(b))
	return firstParentErr == nil && secondParentErr == nil && os.SameFile(firstParent, secondParent), nil
}

type daemonRootMarker struct {
	Version      int    `json:"version"`
	Schema       string `json:"schema"`
	Role         string `json:"role"`
	RepositoryID string `json:"repository_id,omitempty"`
	NodeID       string `json:"node_id,omitempty"`
}

const daemonRootMarkerTemporaryPrefix = ".ttc-root-format.tmp-"

func freshDaemonRootEntryAllowed(root string, entry os.DirEntry, startupLog os.FileInfo) bool {
	if entry.Name() == "lost+found" || strings.HasPrefix(entry.Name(), daemonRootMarkerTemporaryPrefix) && entry.Type().IsRegular() {
		return true
	}
	// The thin client opens ttcd.log before exec so startup diagnostics survive
	// a daemon failure. If TTC_SOCKET's directory is the new TTC_ROOT, that log
	// is necessarily visible before the daemon can publish the format marker.
	// Admit only the exact regular file currently attached to stderr; a stale
	// or user-created ttcd.log must still make an unmarked root fail closed.
	if startupLog == nil {
		return false
	}
	if entry.Name() == "ttcd.log" && entry.Type().IsRegular() {
		info, err := entry.Info()
		return err == nil && os.SameFile(info, startupLog)
	}
	if !entry.IsDir() {
		return false
	}
	// TTC_SOCKET commonly lives in a subdirectory of TTC_LOCAL_ROOT (for
	// example, $TTC_LOCAL_ROOT/run/ttcd.sock). The thin client necessarily
	// creates that directory and opens its startup log before ttcd can publish
	// the root marker. Admit the subtree only when every entry is a directory on
	// the path to the exact regular file currently attached to stderr. Empty,
	// stale, symlinked, or otherwise populated trees remain old-format roots.
	found := false
	path := filepath.Join(root, entry.Name())
	err := filepath.WalkDir(path, func(candidate string, child os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if candidate == path || child.IsDir() {
			return nil
		}
		if found || child.Name() != "ttcd.log" || !child.Type().IsRegular() {
			return errors.New("unexpected startup-root entry")
		}
		info, err := child.Info()
		if err != nil || !os.SameFile(info, startupLog) {
			return errors.New("startup log does not match daemon stderr")
		}
		found = true
		return nil
	})
	return err == nil && found
}

func daemonStartupLogInfo() os.FileInfo {
	info, err := os.Stat("/proc/self/fd/2")
	if err != nil {
		return nil
	}
	return info
}

func daemonRootMarkerFor(role, repositoryID, nodeID string) (daemonRootMarker, error) {
	version := daemonStandaloneRootFormatVersion
	switch role {
	case daemonRootRoleStandaloneAuthority:
		if repositoryID != "" || nodeID != "" {
			return daemonRootMarker{}, errors.New("standalone TTC root cannot name a cluster repository or node")
		}
	case daemonRootRoleClusterShared:
		version = daemonClusterRootFormatVersion
		if repositoryID == "" || nodeID != "" {
			return daemonRootMarker{}, errors.New("cluster shared root requires a repository ID and no node ID")
		}
	case daemonRootRoleClusterLocal:
		version = daemonClusterRootFormatVersion
		if repositoryID == "" || nodeID == "" {
			return daemonRootMarker{}, errors.New("cluster local root requires repository and node IDs")
		}
	default:
		return daemonRootMarker{}, fmt.Errorf("unknown TTC root role %q", role)
	}
	return daemonRootMarker{Version: version, Schema: daemonRootSchema, Role: role, RepositoryID: repositoryID, NodeID: nodeID}, nil
}

func daemonRootVersionForRole(role string) (int, error) {
	switch role {
	case daemonRootRoleStandaloneAuthority:
		return daemonStandaloneRootFormatVersion, nil
	case daemonRootRoleClusterShared, daemonRootRoleClusterLocal:
		return daemonClusterRootFormatVersion, nil
	default:
		return 0, fmt.Errorf("unknown TTC root role %q", role)
	}
}

// inspectDaemonStorageRoot validates an existing root without modifying it.
// Daemon startup performs this before platform probes, worker creation, Raft
// admission, or opening the daemon socket.
func inspectDaemonStorageRoot(root, role string) error {
	wantVersion, err := daemonRootVersionForRole(role)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", root)
	}
	markerPath := filepath.Join(root, ".ttc-root-format")
	data, err := os.ReadFile(markerPath)
	if err == nil {
		var got daemonRootMarker
		if err := json.Unmarshal(data, &got); err != nil {
			return fmt.Errorf("invalid root marker: %w", err)
		}
		if got.Version != wantVersion {
			return fmt.Errorf("TTC root uses incompatible format %d; this build requires snapshot-graph format %d and does not migrate existing roots; select an empty root", got.Version, wantVersion)
		}
		if got.Schema != daemonRootSchema || got.Role != role {
			return fmt.Errorf("TTC root marker schema/role %q/%q does not match required %q/%q", got.Schema, got.Role, daemonRootSchema, role)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	startupLog := daemonStartupLogInfo()
	for _, entry := range entries {
		if !freshDaemonRootEntryAllowed(root, entry, startupLog) {
			return fmt.Errorf("%q is not a fresh TTC root (found %q); existing TTC data roots are not migrated", root, entry.Name())
		}
	}
	return nil
}

// validateStrictSnapshotGraphRoot is a qualification guard for the destructive
// format boundary. It makes tests fail if a current-format operation recreates
// a pathname that belonged exclusively to the removed storage engines. It is
// intentionally enabled only by TTC_SNAPSHOT_GRAPH_STRICT=1: walking a large
// authoritative root at every production startup would violate TTC's bounded
// startup requirement.
func validateStrictSnapshotGraphRoot(root string) error {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		legacy := relative == "cold/inflight" || relative == "distributed-filesystem-metadata" ||
			strings.HasPrefix(relative, "distributed-filesystem-metadata/") ||
			strings.Contains(relative, "/memory/chunks/chunks/") ||
			strings.Contains(relative, "/promoted-repositories/") ||
			(strings.Contains(relative, "/memory/") && (entry.Name() == "application.mem" || entry.Name() == "dehydration-source"))
		if legacy {
			return fmt.Errorf("new-format root contains removed legacy storage artifact %q", relative)
		}
		return nil
	})
}

func prepareDaemonStorageRoot(root, role, repositoryID, nodeID string) error {
	want, err := daemonRootMarkerFor(role, repositoryID, nodeID)
	if err != nil {
		return err
	}
	if err := inspectDaemonStorageRoot(root, role); err != nil {
		// A concurrent initializer may publish the marker after inspection's
		// stat but before its directory read. Accept only the exact marker this
		// caller intended; every other inspection error remains fail-closed.
		if data, readErr := os.ReadFile(filepath.Join(root, ".ttc-root-format")); readErr == nil {
			var got daemonRootMarker
			if decodeErr := json.Unmarshal(data, &got); decodeErr == nil && got == want {
				return syncDaemonRootDirectory(root)
			}
		}
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	markerPath := filepath.Join(root, ".ttc-root-format")
	data, err := os.ReadFile(markerPath)
	if err == nil {
		var got daemonRootMarker
		if err := json.Unmarshal(data, &got); err != nil {
			return fmt.Errorf("invalid root marker: %w", err)
		}
		if got != want {
			return fmt.Errorf("root marker %+v does not match requested %+v", got, want)
		}
		return syncDaemonRootDirectory(root)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	// The marker may have appeared after the read above returned ENOENT. Since
	// publication uses link(2), observing its directory entry also observes the
	// complete marker; validate the winner rather than treating it as legacy
	// residue.
	for _, entry := range entries {
		if entry.Name() != ".ttc-root-format" {
			continue
		}
		published, readErr := os.ReadFile(markerPath)
		if readErr != nil {
			return readErr
		}
		var got daemonRootMarker
		if decodeErr := json.Unmarshal(published, &got); decodeErr != nil {
			return fmt.Errorf("invalid concurrently-created root marker: %w", decodeErr)
		}
		if got != want {
			return fmt.Errorf("root marker %+v does not match requested %+v", got, want)
		}
		return syncDaemonRootDirectory(root)
	}
	startupLog := daemonStartupLogInfo()
	for _, entry := range entries {
		if !freshDaemonRootEntryAllowed(root, entry, startupLog) {
			return fmt.Errorf("%q is not a fresh TTC root (found %q); existing TTC data roots are not migrated", root, entry.Name())
		}
	}
	data, err = json.Marshal(want)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(root, daemonRootMarkerTemporaryPrefix)
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	err = os.Link(temporaryPath, markerPath)
	if errors.Is(err, os.ErrExist) {
		// Another node may have initialized a shared root concurrently. Validate
		// what it published instead of replacing its marker.
		published, readErr := os.ReadFile(markerPath)
		if readErr != nil {
			return readErr
		}
		var got daemonRootMarker
		if err := json.Unmarshal(published, &got); err != nil {
			return fmt.Errorf("invalid concurrently-created root marker: %w", err)
		}
		if got != want {
			return fmt.Errorf("root marker %+v does not match requested %+v", got, want)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	return syncDaemonRootDirectory(root)
}

func syncDaemonRootDirectory(root string) error {
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// localAuthorityRecovery is what a member starting on an existing local root
// must discard of it before it rejoins its cluster. An ordinary restart keeps
// everything; an offline incarnation must not reopen a stale Raft WAL; and a
// removed node must not replay mutable local journals under replacement
// authority.
type localAuthorityRecovery int

const (
	// keepLocalAuthority: the authority still counts this incarnation live,
	// or no seed could say otherwise; its Raft replica rejoins as it is.
	keepLocalAuthority localAuthorityRecovery = iota
	// rejoinLocalAuthority: this incarnation's lease expired while it was
	// away. What it runs and stores is still its own, but its Raft replica
	// may describe a configuration it was removed from; only that goes, and
	// it rejoins as a new member of the current one.
	rejoinLocalAuthority
	// resetLocalAuthority: the node was removed, and what it held with it.
	resetLocalAuthority
)

// localNodeAuthorityRecovery asks the first seed that answers what this
// incarnation must discard; see localIncarnationRecovery.
func localNodeAuthorityRecovery(nodeID, incarnation, authToken string, clients map[string]*cluster.Client) (localAuthorityRecovery, error) {
	ids := make([]string, 0, len(clients))
	for id := range clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		state, err := authorityStateThroughSeed(ctx, clients[id], nodeID, authToken)
		cancel()
		if err != nil {
			continue
		}
		return localIncarnationRecovery(state, nodeID, incarnation)
	}
	return keepLocalAuthority, nil
}

func localIncarnationRecovery(state *cluster.AuthorityState, nodeID, incarnation string) (localAuthorityRecovery, error) {
	if state == nil {
		return keepLocalAuthority, errors.New("cluster authority state is nil")
	}
	if record, ok := state.Nodes[nodeID]; ok {
		if record.Incarnation != incarnation {
			return keepLocalAuthority, fmt.Errorf("cluster node %q already belongs to incarnation %q; local scratch belongs to %q", nodeID, record.Incarnation, incarnation)
		}
		// Expiry leaves an incarnation's placements, volatile dependencies and
		// unpublished origins its own, for when it returns. Its Raft WAL may
		// still describe a configuration from which this server was removed
		// while offline.
		if record.State == cluster.NodeOffline {
			return rejoinLocalAuthority, nil
		}
		return keepLocalAuthority, nil
	}
	// `node rm` gives up what the node held, and deliberately forgets its
	// retirement tombstone after removing the Raft member. Absence from a
	// linearizable authority snapshot is therefore itself the durable
	// authorization to discard this excluded local replica.
	return resetLocalAuthority, nil
}

func authorityStateThroughSeed(ctx context.Context, seed *cluster.Client, nodeID, authToken string) (*cluster.AuthorityState, error) {
	peer := seed
	seen := make(map[string]bool)
	for redirects := 0; redirects < 5; redirects++ {
		state, err := peer.AuthorityState(ctx)
		if err == nil {
			return state, nil
		}
		var redirect *cluster.AuthorityNotLeaderError
		if !errors.As(err, &redirect) || redirect.LeaderControlURL == "" {
			return nil, err
		}
		key := redirect.LeaderID + "\x00" + redirect.LeaderControlURL
		if seen[key] {
			return nil, fmt.Errorf("raft leader redirect loop through %q", redirect.LeaderID)
		}
		seen[key] = true
		var clientErr error
		peer, clientErr = cluster.NewPlainClient(redirect.LeaderControlURL, nodeID, authToken, 2*time.Minute)
		if clientErr != nil {
			return nil, fmt.Errorf("configuring redirected Raft leader %q: %w", redirect.LeaderID, clientErr)
		}
	}
	return nil, errors.New("too many Raft leader redirects while reading cluster authority")
}

func lockDaemonLocalRoot(root string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(root, ".ttcd.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another ttcd is already using %q: %w", root, err)
	}
	return lock, nil
}

func consumeDaemonClusterOption(args []string, opts *daemonClusterOptions) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	if args[0] == "--cluster-auto-rebalance" {
		opts.AutoRebalance = true
		opts.Configured = true
		return 1, nil
	}
	if args[0] == "--cluster-raft-bootstrap" {
		opts.RaftBootstrap = true
		opts.Configured = true
		return 1, nil
	}
	if args[0] == "--cluster-init" {
		opts.RaftBootstrap = true
		opts.Configured = true
		return 1, nil
	}
	if args[0] == "--control-only" {
		opts.ControlOnly = true
		opts.Configured = true
		return 1, nil
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
	var clusterJoin string
	if consumed, err := setString("--cluster-join", &clusterJoin); consumed != 0 || err != nil {
		if err != nil {
			return consumed, err
		}
		// A seed has no node name of its own, and peers are keyed by name, so
		// the second and later seeds are numbered.
		name := "seed"
		for i := 2; slices.ContainsFunc(opts.Peers, func(peer string) bool { return strings.HasPrefix(peer, name+"=") }); i++ {
			name = fmt.Sprintf("seed-%d", i)
		}
		opts.Peers = append(opts.Peers, name+"="+clusterJoin)
		opts.Configured = true
		return consumed, nil
	}
	for name, dst := range map[string]*string{
		"--cluster-listen": &opts.Listen, "--cluster-node-id": &opts.NodeID,
		"--listen": &opts.Listen, "--node": &opts.NodeID,
		"--cluster-repository-id": &opts.RepositoryID,
		"--cluster-advertise":     &opts.Advertise, "--advertise": &opts.Advertise,
		"--cluster-capacity-command": &opts.CapacityCommand,
		"--cluster-capacity-webhook": &opts.CapacityWebhook,
		"--cluster-page-listen":      &opts.PageListen, "--cluster-page-advertise": &opts.PageAdvertise,
	} {
		if consumed, err := setString(name, dst); consumed != 0 || err != nil {
			if consumed != 0 {
				opts.Configured = true
			}
			return consumed, err
		}
	}
	optionValue := func(name string) (string, int, bool, error) {
		if args[0] == name {
			if len(args) < 2 || args[1] == "" {
				return "", 0, true, fmt.Errorf("%s requires a value", name)
			}
			return args[1], 2, true, nil
		}
		if strings.HasPrefix(args[0], name+"=") {
			value := strings.TrimPrefix(args[0], name+"=")
			if value == "" {
				return "", 0, true, fmt.Errorf("%s requires a value", name)
			}
			return value, 1, true, nil
		}
		return "", 0, false, nil
	}
	for name, dst := range map[string]*uint64{
		"--cluster-transfer-bytes":        &opts.PagingPolicy.TransferBytes,
		"--cluster-max-outstanding-bytes": &opts.PagingPolicy.MaxOutstandingBytes,
	} {
		value, consumed, matched, err := optionValue(name)
		if !matched {
			continue
		}
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed == 0 {
			return 0, fmt.Errorf("%s must be a positive byte count", name)
		}
		*dst = parsed
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-memory-target"); matched {
		if err != nil {
			return 0, err
		}
		parsed, err := ttcbundle.ParseShmSize(value)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("--cluster-memory-target must be a positive byte size")
		}
		opts.MemoryTarget = uint64(parsed)
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--memory-target"); matched {
		if err != nil {
			return 0, err
		}
		parsed, err := ttcbundle.ParseShmSize(value)
		if err != nil || parsed <= 0 {
			return 0, errors.New("--memory-target must be a positive byte size")
		}
		opts.MemoryTarget = uint64(parsed)
		opts.Configured = true
		return consumed, nil
	}
	for name, dst := range map[string]*int{
		"--cluster-max-parallel":    &opts.PagingPolicy.MaxConcurrency,
		"--cluster-ack-batch-pages": &opts.PagingPolicy.AckBatchPages,
	} {
		value, consumed, matched, err := optionValue(name)
		if !matched {
			continue
		}
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		*dst = parsed
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-ack-interval"); matched {
		if err != nil {
			return 0, err
		}
		interval, err := time.ParseDuration(value)
		if err != nil || interval < 0 {
			return 0, fmt.Errorf("--cluster-ack-interval must be a non-negative duration")
		}
		opts.PagingPolicy.AckInterval = interval
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-lease"); matched {
		if err != nil {
			return 0, err
		}
		lease, err := time.ParseDuration(value)
		if err != nil || lease < time.Second || lease > 10*time.Minute {
			return 0, fmt.Errorf("--cluster-lease must be between 1s and 10m")
		}
		opts.LeaseDuration = lease
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-prefetch"); matched {
		if err != nil {
			return 0, err
		}
		opts.PagingPolicy.Prefetch = ttcpolicy.PrefetchPolicy(value)
		opts.PagingPolicy.Adaptive = opts.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive
		opts.Configured = true
		return consumed, nil
	}
	if value, consumed, matched, err := optionValue("--cluster-push"); matched {
		if err != nil {
			return 0, err
		}
		opts.PagingPolicy.Push = ttcpolicy.PushPolicy(value)
		opts.Configured = true
		return consumed, nil
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

func prepareDaemonCluster(opts daemonClusterOptions, dataRoot, localRoot, runtimePlatform string, stderr io.Writer) (*daemonClusterRuntime, error) {
	if !opts.Configured {
		return nil, nil
	}
	// A distributed systrap runtime must be able to make the persisted CPU
	// profile authoritative. Some x86 CPUs and older kernels reject
	// ARCH_SET_CPUID; on those hosts applications execute native CPUID even
	// though /proc/cpuinfo shows the restricted profile. Moving such a process
	// to a host with a different XCR0/XSAVE layout can corrupt its userspace
	// XSAVE buffers. Refuse cluster membership before creating any runtime.
	if err := validateClusterCPUEnforcement(runtimePlatform, hostSupportsCPUIDFaulting()); err != nil {
		return nil, err
	}
	if opts.LeaseDuration == 0 {
		opts.LeaseDuration = defaultClusterLeaseDuration
	}
	if opts.Listen == "" || opts.Advertise == "" || opts.NodeID == "" || opts.RepositoryID == "" {
		return nil, errors.New("cluster mode requires listen/advertise addresses, node ID, and repository ID")
	}
	opts.AuthToken = os.Getenv("TTC_CLUSTER_TOKEN")
	raftLocalDir := filepath.Join(localRoot, "cluster", "raft")
	_, localRaftErr := os.Stat(filepath.Join(raftLocalDir, "raft.db"))
	localRaftState := localRaftErr == nil
	if localRaftErr != nil && !errors.Is(localRaftErr, os.ErrNotExist) {
		return nil, localRaftErr
	}
	if !opts.RaftBootstrap && !localRaftState && len(opts.Peers) == 0 {
		return nil, errors.New("a new Raft member requires --cluster-peer; use --cluster-raft-bootstrap only for the first node")
	}
	resolvedCPU, err := resolveDaemonCPUProfile(opts.CPUProfileMode, opts.CPUFeatures, runtimePlatform)
	if err != nil {
		return nil, fmt.Errorf("resolving cluster CPU profile: %w", err)
	}
	opts.ResolvedCPU = resolvedCPU
	pendingClusterPolicy, err := applyImmutableClusterConfig(dataRoot, opts.RaftBootstrap, localRaftState, &opts)
	if err != nil {
		return nil, err
	}
	if err := validateDaemonCPUProfile(opts.ResolvedCPU, runtimePlatform); err != nil {
		return nil, fmt.Errorf("validating cluster CPU profile: %w", err)
	}
	clusterID, err := prepareClusterRaftIdentity(dataRoot, raftLocalDir, opts.RepositoryID, opts.NodeID, opts.RaftBootstrap, localRaftState)
	if err != nil {
		return nil, err
	}
	capacitySink, err := newElasticCapacitySink(opts.CapacityCommand, opts.CapacityWebhook)
	if err != nil {
		return nil, err
	}
	if opts.PageListen == "" {
		return nil, errors.New("cluster page listen address is empty")
	}
	var advertiseURL, advertiseServerName string
	if opts.Advertise != "" {
		var err error
		advertiseURL, advertiseServerName, err = parseClusterAdvertise(opts.Advertise)
		if err != nil {
			return nil, err
		}
		if opts.PageAdvertise == "" {
			opts.PageAdvertise, err = advertisedPageAddress(advertiseURL)
			if err != nil {
				return nil, err
			}
		}
	}
	if (len(opts.Peers) != 0 || opts.Advertise != "") && opts.PageAdvertise == "" {
		return nil, errors.New("cluster nodes with peers require --cluster-page-advertise HOST:0")
	}
	if err := requireEphemeralClusterPagePort("--cluster-page-listen", opts.PageListen); err != nil {
		return nil, err
	}
	if opts.PageAdvertise != "" {
		if err := requireEphemeralClusterPagePort("--cluster-page-advertise", opts.PageAdvertise); err != nil {
			return nil, err
		}
	}
	opts.PagingPolicy.PageSize = hostarch.PageSize
	if err := opts.PagingPolicy.Validate(); err != nil {
		return nil, fmt.Errorf("cluster paging policy: %w", err)
	}
	if opts.PagingPolicy.TransferBytes < pgalloc.SparseChunkBytes {
		return nil, fmt.Errorf("cluster transfer size %d is smaller than TTC's %d-byte atomic memory chunk", opts.PagingPolicy.TransferBytes, pgalloc.SparseChunkBytes)
	}
	capabilities := cluster.Capabilities{
		NodeID: opts.NodeID, ProtocolMin: cluster.ControlProtocolVersion, ProtocolMax: cluster.ControlProtocolVersion,
		BuildID: version.Version(), Architecture: runtime.GOARCH, BasePageSize: hostarch.PageSize,
		CPUProfile: opts.ResolvedCPU.Mode, CPUFeatures: append([]string(nil), opts.ResolvedCPU.Features...),
		XsaveABI:        opts.ResolvedCPU.XsaveABI,
		MaxTransferSize: 2 << 20, RepositoryID: opts.RepositoryID, PagingPolicy: opts.PagingPolicy,
		Features: []string{
			cluster.FeatureRemotePull, cluster.FeatureReversePush, cluster.FeatureAckBatch,
			cluster.FeatureNetworkRecreate, cluster.FeatureRuntimePrefetch, cluster.FeatureFilesystemBranch,
			cluster.FeatureSharedMemorySnapshot,
		},
	}
	if err := capabilities.Validate(); err != nil {
		return nil, err
	}
	clients := make(map[string]*cluster.Client, len(opts.Peers))
	for _, peerSpec := range opts.Peers {
		nodeID, baseURL, _, err := parseClusterPeer(peerSpec)
		if err != nil {
			return nil, err
		}
		if nodeID == opts.NodeID {
			return nil, fmt.Errorf("cluster peer %q is the local node", nodeID)
		}
		if _, ok := clients[nodeID]; ok {
			return nil, fmt.Errorf("cluster peer %q is configured more than once", nodeID)
		}
		client, err := cluster.NewPlainClient(baseURL, opts.NodeID, opts.AuthToken, 2*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("configuring cluster peer %q: %w", nodeID, err)
		}
		clients[nodeID] = client
	}
	incarnation, err := localNodeIncarnation(localRoot, opts.NodeID)
	if err != nil {
		return nil, err
	}
	// A removed or expired member's WAL describes a configuration in which it
	// still participates. Opening that WAL and waiting for its historical quorum
	// cannot re-admit it to the current configuration. Once a configured seed
	// proves that this incarnation is offline, discard its Raft replica and
	// rejoin; once it proves the node absent, discard its node-local authority
	// and rotate the execution epoch. Immutable image cache and shared snapshot
	// authority remain either way.
	if !opts.RaftBootstrap && len(clients) != 0 {
		recovery, err := localNodeAuthorityRecovery(opts.NodeID, incarnation, opts.AuthToken, clients)
		if err != nil {
			return nil, err
		}
		if recovery == rejoinLocalAuthority {
			if err := os.RemoveAll(raftLocalDir); err != nil {
				return nil, fmt.Errorf("discarding the Raft replica of an expired incarnation: %w", err)
			}
			localRaftState = false
		}
		if recovery == resetLocalAuthority {
			// An offline or absent incarnation has no remaining execution or
			// storage authority. Its other local journals are just as stale as its
			// Raft WAL and must not be replayed under the replacement incarnation.
			// Reap the guardian-stopped runtime before deleting the fence record
			// and runsc sockets needed to identify and clean those processes.
			if err := terminateRetiredElasticRuntimeRoot(filepath.Join(localRoot, "runtime", "runsc")); err != nil {
				return nil, fmt.Errorf("reaping retired local runtimes: %w", err)
			}
			if err := discardRetiredLocalAuthority(localRoot); err != nil {
				return nil, err
			}
			localRaftState = false
			incarnation, err = localNodeIncarnation(localRoot, opts.NodeID)
			if err != nil {
				return nil, err
			}
		}
	}
	sharedListener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("listening for cluster control and Raft on %s: %w", opts.Listen, err)
	}
	protocolMux, err := cluster.NewProtocolMux(sharedListener)
	if err != nil {
		sharedListener.Close()
		return nil, err
	}
	closeMux := true
	defer func() {
		if closeMux {
			_ = protocolMux.Close()
		}
	}()
	advertiseURL, err = resolveClusterAdvertise(advertiseURL, protocolMux.Addr().String())
	if err != nil {
		return nil, err
	}
	parsedAdvertise, err := url.Parse(advertiseURL)
	if err != nil || parsedAdvertise.Host == "" {
		return nil, fmt.Errorf("resolving shared cluster advertise address %q", advertiseURL)
	}
	raftNode, err := cluster.OpenAuthorityRaftNode(cluster.AuthorityRaftOptions{
		NodeID: opts.NodeID, LocalDir: raftLocalDir,
		Listener: protocolMux.RaftListener(), AdvertiseHost: parsedAdvertise.Host,
		AuthToken: opts.AuthToken, Bootstrap: opts.RaftBootstrap,
		StabilityTimeout: clusterRaftStabilityTimeout(opts.LeaseDuration),
	})
	if err != nil {
		return nil, err
	}
	authorityJoinRequest := cluster.AuthorityJoinRequest{
		NodeID: opts.NodeID, Incarnation: incarnation,
		ControlURL: advertiseURL, ControlServerName: advertiseServerName,
		RaftAddress: raftNode.Address(), ClusterID: clusterID,
		Capabilities: capabilities, MemoryTargetBytes: opts.MemoryTarget,
	}
	// A restarted server may have been removed from the current Raft
	// configuration after its execution lease expired. Its local WAL still
	// contains the last configuration it observed, including the addresses of
	// surviving members, but a bootstrap node normally has no --cluster-join
	// seed in its command line. Reuse those persisted addresses as recovery
	// seeds. The leader treats AddNonvoter for an existing voter as a no-op and
	// admits a removed server as a learner, so the same path safely covers both
	// an ordinary restart and a restart after authoritative removal.
	if raftNode.HadExistingState() {
		configuration, configErr := raftNode.Configuration()
		if configErr != nil {
			raftNode.Close()
			return nil, fmt.Errorf("reading persisted Raft peer configuration: %w", configErr)
		}
		for _, server := range configuration.Servers {
			peerID := string(server.ID)
			if peerID == opts.NodeID || clients[peerID] != nil {
				continue
			}
			address := string(server.Address)
			if _, _, splitErr := net.SplitHostPort(address); splitErr != nil {
				raftNode.Close()
				return nil, fmt.Errorf("invalid persisted Raft peer %q address %q: %w", peerID, address, splitErr)
			}
			client, clientErr := cluster.NewPlainClient("http://"+address, opts.NodeID, opts.AuthToken, 2*time.Minute)
			if clientErr != nil {
				raftNode.Close()
				return nil, fmt.Errorf("configuring persisted Raft recovery peer %q: %w", peerID, clientErr)
			}
			clients[peerID] = client
		}
	}
	var cancelFreshJoin func(context.Context) error
	closeRaft := true
	defer func() {
		if closeRaft {
			if cancelFreshJoin != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = cancelFreshJoin(ctx)
				cancel()
			}
			_ = raftNode.Close()
		}
	}()
	if raftNode.HadExistingState() {
		// Keep the Raft listener alive even when this is the first member being
		// restarted after a whole-cluster outage. Waiting for a leader here and
		// then exiting makes sequential service startup impossible: no process
		// survives long enough to form a quorum. The control server is installed
		// before registerClusterNodeWithRetry, which supplies the bounded
		// readiness wait once the remaining members return.
	} else if opts.RaftBootstrap {
		deadline := time.Now().Add(15 * time.Second)
		for !raftNode.IsLeader() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if !raftNode.IsLeader() {
			return nil, errors.New("bootstrapped TTC Raft group did not elect its leader")
		}
	} else {
		joined := false
		var joinErr error
		cancelRequest := cluster.AuthorityCancelJoinRequest{NodeID: opts.NodeID, Incarnation: incarnation, RaftAddress: raftNode.Address()}
		cancelFreshJoin = func(ctx context.Context) error {
			var result error
			for _, peer := range clients {
				if err := cancelAuthorityJoinThroughSeed(ctx, peer, cancelRequest, opts.NodeID, opts.AuthToken); err == nil {
					return nil
				} else {
					result = errors.Join(result, err)
				}
			}
			return result
		}
		// A node whose Sentries a fence stopped keeps trying, however long the
		// cluster is away; see registerClusterNodeWithRetry.
		wait := holdsFencedRuntimes(localRoot)
		for report := time.Now().Add(clusterWaitReport); ; {
			for _, peer := range clients {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				joinErr = joinAuthorityThroughSeed(ctx, peer, authorityJoinRequest, opts.NodeID, opts.AuthToken)
				cancel()
				if joinErr == nil {
					joined = true
					break
				}
			}
			if joined || !wait {
				break
			}
			if !time.Now().Before(report) {
				fmt.Fprintf(stderr, "ttcd: waiting for the cluster, with this node's sandboxes stopped: %v\n", joinErr)
				report = time.Now().Add(clusterWaitReport)
			}
			time.Sleep(time.Second)
		}
		if !joined {
			// A timeout can lose the successful AddNonvoter response, so cancel
			// even when no seed acknowledged the join. A freshly opened Bolt
			// store still counts as existing Raft state; retaining it would make
			// the next attempt wait forever as a member that was never admitted.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = cancelFreshJoin(ctx)
			cancel()
			cancelFreshJoin = nil
			_ = raftNode.Close()
			closeRaft = false
			_ = os.RemoveAll(raftLocalDir)
			return nil, fmt.Errorf("joining TTC Raft group through configured peers: %w", joinErr)
		}
		deadline := time.Now().Add(15 * time.Second)
		for raftNode.Leader() == "" && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if raftNode.Leader() == "" {
			return nil, errors.New("joined TTC Raft member could not discover the elected leader")
		}
	}
	if opts.RaftBootstrap {
		base := immutableClusterConfigFromOptions(opts)
		policy, err := reconcileClusterRuntimePolicy(raftNode, dataRoot, base, pendingClusterPolicy)
		if err != nil {
			return nil, err
		}
		applyClusterRuntimePolicy(&opts, policy)
		capabilities.PagingPolicy = opts.PagingPolicy
		if err := capabilities.Validate(); err != nil {
			return nil, err
		}
	} else if localRaftState {
		// A node that was removed while offline may restart with a stale local
		// Raft log in which no leader (and sometimes no ClusterPolicy) is yet
		// visible. It is a joining node, not the authority allowed to change
		// policy. applyImmutableClusterConfig already loaded the successfully
		// published cluster policy from the shared root; retain that policy until
		// membership recovery re-adds this process as a learner and catches its
		// log up. Requiring this stale process to become leader deadlocks exactly
		// the supported remove-then-rejoin path.
		capabilities.PagingPolicy = opts.PagingPolicy
		if err := capabilities.Validate(); err != nil {
			return nil, err
		}
	}
	catalog, err := cluster.NewRaftCatalog(raftNode)
	if err != nil {
		return nil, err
	}
	nodeRecord := cluster.NodeRecord{
		NodeID: opts.NodeID, Incarnation: incarnation,
		ControlURL: advertiseURL, ControlServerName: advertiseServerName, RaftAddress: raftNode.Address(),
		Capabilities: capabilities, MemoryTargetBytes: opts.MemoryTarget,
	}
	transactions := catalog.Transactions()
	placements := catalog.Placements()
	source := &distributedSourceRuntime{
		capabilities: capabilities, incarnation: incarnation, peers: clients,
		directory: catalog, placements: placements, transactions: transactions, authority: catalog,
		authToken:    opts.AuthToken,
		dynamicPeers: make(map[string]dynamicClusterPeer),
		pageListen:   opts.PageListen, pageAdvertise: opts.PageAdvertise,
	}
	catalog.SetLeaderClientResolver(func(nodeID string) (*cluster.Client, error) {
		if nodeID != "" {
			if peer := clients[nodeID]; peer != nil {
				return peer, nil
			}
		} else {
			// A re-admitted learner may not have observed the current leader yet.
			// Any seed can answer directly or return the ordinary leader redirect.
			// Sort the IDs to keep restart behavior and diagnostics deterministic.
			ids := make([]string, 0, len(clients))
			for id := range clients {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			if len(ids) != 0 {
				return clients[ids[0]], nil
			}
		}
		// A node only needs one seed to join. After elections, discover any
		// leader from the replicated membership map rather than requiring every
		// peer to be repeated on every daemon command line.
		state, err := raftNode.AppliedState()
		if err != nil {
			return nil, err
		}
		record, ok := state.Nodes[nodeID]
		if !ok {
			return nil, fmt.Errorf("raft leader %q has no replicated node record", nodeID)
		}
		return source.peerForRecord(record)
	})
	closeRaft = false
	closeMux = false
	return &daemonClusterRuntime{
		listen: protocolMux.Addr().String(), authToken: opts.AuthToken,
		mux: protocolMux, controlListener: protocolMux.HTTPListener(),
		controlPlane: &cluster.ControlPlane{
			Capabilities: capabilities, Store: transactions, MaxTransactions: opts.MaxTransactions, Now: time.Now,
			Authority:     &cluster.AuthorityService{Node: raftNode, Capabilities: capabilities, ClusterID: clusterID},
			TopologyState: catalog.LinearizableState, AuthToken: opts.AuthToken,
		},
		source: source, directory: catalog, placements: placements, transactions: transactions,
		raft: raftNode, raftLocalDir: raftLocalDir, raftCatalog: catalog, nodeRecord: nodeRecord, lease: opts.LeaseDuration, clusterID: clusterID,
		recoverMembership: func(ctx context.Context) error {
			if len(clients) == 0 {
				return errors.New("no cluster seed is configured for Raft membership recovery")
			}
			var result error
			for _, peer := range clients {
				if err := joinAuthorityThroughSeed(ctx, peer, authorityJoinRequest, opts.NodeID, opts.AuthToken); err == nil {
					return nil
				} else {
					result = errors.Join(result, err)
				}
			}
			return result
		},
		cancelFreshJoin: cancelFreshJoin,
		autoRebalance:   opts.AutoRebalance, capacitySink: capacitySink, cpuProfile: opts.ResolvedCPU,
	}, nil
}

// clusterRaftStabilityTimeout keeps transient data-plane congestion from
// looking like leadership failure. It remains a fraction of the execution
// lease so a genuine leader loss can be resolved before runtime guardians
// reach their conservative fencing deadline.
func clusterRaftStabilityTimeout(lease time.Duration) time.Duration {
	timeout := lease / 10
	if timeout < time.Second {
		return time.Second
	}
	if timeout > 5*time.Second {
		return 5 * time.Second
	}
	return timeout
}

const daemonImmutableClusterConfigVersion = 2

// daemonImmutableClusterConfig contains only cluster-wide policy. Addresses,
// node names, local capacity and provisioning hooks are deliberately absent:
// they belong to one node and may change as elastic workers join and leave.
type daemonImmutableClusterConfig struct {
	Version         int              `json:"version"`
	PagingPolicy    ttcpolicy.Policy `json:"paging_policy"`
	LeaseDuration   time.Duration    `json:"lease_duration_ns"`
	MaxTransactions int              `json:"max_transactions"`
	AutoRebalance   bool             `json:"auto_rebalance"`
	CPUProfile      daemonCPUProfile `json:"cpu_profile"`
}

func immutableClusterConfigFromOptions(opts daemonClusterOptions) daemonImmutableClusterConfig {
	return daemonImmutableClusterConfig{
		Version: daemonImmutableClusterConfigVersion, PagingPolicy: opts.PagingPolicy,
		LeaseDuration: opts.LeaseDuration, MaxTransactions: opts.MaxTransactions,
		AutoRebalance: opts.AutoRebalance,
		CPUProfile:    opts.ResolvedCPU,
	}
}

func equalImmutableClusterConfig(a, b daemonImmutableClusterConfig) bool {
	return a.Version == b.Version && a.PagingPolicy == b.PagingPolicy &&
		a.LeaseDuration == b.LeaseDuration && a.MaxTransactions == b.MaxTransactions &&
		a.AutoRebalance == b.AutoRebalance && a.CPUProfile.Version == b.CPUProfile.Version &&
		a.CPUProfile.Mode == b.CPUProfile.Mode && slices.Equal(a.CPUProfile.Features, b.CPUProfile.Features) && a.CPUProfile.XsaveABI == b.CPUProfile.XsaveABI
}

func clusterRuntimePolicy(config daemonImmutableClusterConfig) cluster.ClusterRuntimePolicy {
	return cluster.ClusterRuntimePolicy{
		Version: 1, PagingPolicy: config.PagingPolicy, LeaseDuration: config.LeaseDuration,
		MaxTransactions: config.MaxTransactions, AutoRebalance: config.AutoRebalance,
	}
}

func applyClusterRuntimePolicy(opts *daemonClusterOptions, policy cluster.ClusterRuntimePolicy) {
	opts.PagingPolicy = policy.PagingPolicy
	opts.LeaseDuration = policy.LeaseDuration
	opts.MaxTransactions = policy.MaxTransactions
	opts.AutoRebalance = policy.AutoRebalance
}

func equalClusterRuntimePolicy(a, b cluster.ClusterRuntimePolicy) bool {
	a.Revision, b.Revision = 0, 0
	return a == b
}

// applyImmutableClusterConfig reads the shared bootstrap record. It returns a
// requested operational-policy change instead of applying it: only the Raft
// state machine can prove that every execution lease is inactive.
func applyImmutableClusterConfig(sharedRoot string, bootstrap, localState bool, opts *daemonClusterOptions) (*daemonImmutableClusterConfig, error) {
	path := filepath.Join(sharedRoot, ".ttc-cluster-config.json")
	want := immutableClusterConfigFromOptions(*opts)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if !bootstrap || localState {
			return nil, errors.New("shared TTC_ROOT has no immutable cluster configuration; only a fresh first node may create it")
		}
		encoded, err := json.Marshal(want)
		if err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, fmt.Errorf("creating immutable cluster configuration: %w", err)
		}
		_, writeErr := file.Write(append(encoded, '\n'))
		closeErr := errors.Join(file.Sync(), file.Close())
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(path)
			return nil, errors.Join(writeErr, closeErr)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var got daemonImmutableClusterConfig
	if err := json.Unmarshal(data, &got); err != nil {
		return nil, fmt.Errorf("decoding immutable cluster configuration: %w", err)
	}
	if got.Version != daemonImmutableClusterConfigVersion {
		return nil, fmt.Errorf("unsupported immutable cluster configuration version %d", got.Version)
	}
	if opts.CPUProfileSet && (got.CPUProfile.Mode != want.CPUProfile.Mode || !slices.Equal(got.CPUProfile.Features, want.CPUProfile.Features)) {
		return nil, fmt.Errorf("requested CPU profile differs from immutable cluster profile %q", got.CPUProfile.Mode)
	}
	compareWant := want
	if !opts.CPUProfileSet {
		compareWant.CPUProfile = got.CPUProfile
	}
	var pending *daemonImmutableClusterConfig
	if bootstrap && !equalImmutableClusterConfig(got, compareWant) {
		if !localState {
			return nil, errors.New("cluster-wide paging, lease, transaction, or rebalance policy differs from the initialized cluster")
		}
		pending = &compareWant
	}
	opts.PagingPolicy = got.PagingPolicy
	opts.LeaseDuration = got.LeaseDuration
	opts.MaxTransactions = got.MaxTransactions
	opts.AutoRebalance = got.AutoRebalance
	opts.ResolvedCPU = got.CPUProfile
	return pending, nil
}

func persistClusterRuntimePolicy(sharedRoot string, base daemonImmutableClusterConfig, policy cluster.ClusterRuntimePolicy) error {
	base.PagingPolicy = policy.PagingPolicy
	base.LeaseDuration = policy.LeaseDuration
	base.MaxTransactions = policy.MaxTransactions
	base.AutoRebalance = policy.AutoRebalance
	return writeDurableJSON(filepath.Join(sharedRoot, ".ttc-cluster-config.json"), base)
}

func reconcileClusterRuntimePolicy(node *cluster.AuthorityRaftNode, sharedRoot string, base daemonImmutableClusterConfig, pending *daemonImmutableClusterConfig) (cluster.ClusterRuntimePolicy, error) {
	state, err := node.AppliedState()
	if err != nil {
		return cluster.ClusterRuntimePolicy{}, err
	}
	effective := state.ClusterPolicy
	desired := clusterRuntimePolicy(base)
	changeRequested := pending != nil
	if pending != nil {
		desired = clusterRuntimePolicy(*pending)
	}
	// An existing member can replay its committed FSM only after the restarted
	// group elects a leader. Do not require that election before opening the
	// control server when no policy change was requested: doing so makes a
	// fully stopped multi-voter cluster impossible to start sequentially. The
	// shared bootstrap record is the last policy atomically persisted after a
	// Raft commit, so it is a safe temporary projection while the remaining
	// voters return. registerClusterNodeWithRetry below keeps this process alive
	// until quorum is restored and the committed policy is replayed.
	if effective.Revision == 0 && !changeRequested && node.HadExistingState() {
		if err := desired.Validate(); err != nil {
			return cluster.ClusterRuntimePolicy{}, fmt.Errorf("persisted cluster operational policy: %w", err)
		}
		return desired, nil
	}
	if effective.Revision == 0 || (changeRequested && !equalClusterRuntimePolicy(effective, desired)) {
		deadline := time.Now().Add(15 * time.Second)
		for !node.IsLeader() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if !node.IsLeader() {
			return cluster.ClusterRuntimePolicy{}, errors.New("cluster operational policy can only change after the fully stopped cluster's surviving Raft member becomes leader")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		latest, err := node.LinearizableState(ctx)
		cancel()
		if err != nil {
			return cluster.ClusterRuntimePolicy{}, fmt.Errorf("reading cluster operational policy after leader election: %w", err)
		}
		effective = latest.ClusterPolicy
		catalog, err := cluster.NewRaftCatalog(node)
		if err != nil {
			return cluster.ClusterRuntimePolicy{}, err
		}
		if effective.Revision == 0 || (changeRequested && !equalClusterRuntimePolicy(effective, desired)) {
			configured, err := catalog.ConfigureClusterPolicy(effective.Revision, desired)
			if err != nil {
				return cluster.ClusterRuntimePolicy{}, fmt.Errorf("configuring cluster operational policy: %w", err)
			}
			effective = *configured
		}
	}
	if err := effective.Validate(); err != nil {
		return cluster.ClusterRuntimePolicy{}, fmt.Errorf("replicated cluster operational policy: %w", err)
	}
	if err := persistClusterRuntimePolicy(sharedRoot, base, effective); err != nil {
		return cluster.ClusterRuntimePolicy{}, fmt.Errorf("persisting replicated cluster operational policy: %w", err)
	}
	return effective, nil
}

// joinAuthorityThroughSeed follows Raft-leader redirects at the control-plane
// layer. A deployment only needs to configure one reachable seed; the seed
// need not currently be the leader.
func joinAuthorityThroughSeed(ctx context.Context, seed *cluster.Client, request cluster.AuthorityJoinRequest, nodeID, authToken string) error {
	peer := seed
	seen := make(map[string]bool)
	redirects := 0
	for {
		err := peer.JoinAuthority(ctx, request)
		if err == nil {
			return nil
		}
		if isTransientAuthorityJoinTransport(err) {
			// Whole-cluster restart commonly launches members concurrently. The
			// first seed process may not have opened its listener when another
			// member makes its initial connection. Keep this joiner's Raft listener
			// alive and retry the configured seed within the existing bounded
			// context, just as we already do during leader election.
			peer = seed
			seen = make(map[string]bool)
			redirects = 0
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("waiting for a reachable Raft seed while joining cluster: %w", ctx.Err())
			case <-timer.C:
			}
			continue
		}
		var redirect *cluster.AuthorityNotLeaderError
		if !errors.As(err, &redirect) {
			return err
		}
		if redirect.LeaderControlURL == "" {
			// A surviving member can expose its control listener before the
			// restarted Raft group has elected a leader. Keep the fresh joiner
			// listening and retry the configured seed instead of tearing down its
			// local state during this ordinary election window.
			peer = seed
			seen = make(map[string]bool)
			redirects = 0
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("waiting for a Raft leader while joining cluster: %w", ctx.Err())
			case <-timer.C:
			}
			continue
		}
		redirects++
		if redirects > 5 {
			return errors.New("too many Raft leader redirects while joining cluster")
		}
		key := redirect.LeaderID + "\x00" + redirect.LeaderControlURL
		if seen[key] {
			return fmt.Errorf("raft leader redirect loop through %q", redirect.LeaderID)
		}
		seen[key] = true
		var clientErr error
		peer, clientErr = cluster.NewPlainClient(redirect.LeaderControlURL, nodeID, authToken, 2*time.Minute)
		if clientErr != nil {
			return fmt.Errorf("configuring redirected Raft leader %q: %w", redirect.LeaderID, clientErr)
		}
	}
}

func isTransientAuthorityJoinTransport(err error) bool {
	if err == nil {
		return false
	}
	var transport *url.Error
	return errors.As(err, &transport)
}

// cancelAuthorityJoinThroughSeed follows the same leader redirect rules as
// admission. This path is intentionally available before a node has a catalog
// record, since that is precisely the failure window it closes.
func cancelAuthorityJoinThroughSeed(ctx context.Context, seed *cluster.Client, request cluster.AuthorityCancelJoinRequest, nodeID, authToken string) error {
	peer := seed
	seen := make(map[string]bool)
	for redirects := 0; redirects < 5; redirects++ {
		err := peer.CancelAuthorityJoin(ctx, request)
		if err == nil {
			return nil
		}
		var redirect *cluster.AuthorityNotLeaderError
		if !errors.As(err, &redirect) {
			return err
		}
		if redirect.LeaderControlURL == "" {
			return fmt.Errorf("cancelling join through a follower that cannot resolve its Raft leader: %w", err)
		}
		key := redirect.LeaderID + "\x00" + redirect.LeaderControlURL
		if seen[key] {
			return fmt.Errorf("raft leader redirect loop through %q", redirect.LeaderID)
		}
		seen[key] = true
		var clientErr error
		peer, clientErr = cluster.NewPlainClient(redirect.LeaderControlURL, nodeID, authToken, 2*time.Minute)
		if clientErr != nil {
			return fmt.Errorf("configuring redirected Raft leader %q: %w", redirect.LeaderID, clientErr)
		}
	}
	return errors.New("too many Raft leader redirects while cancelling cluster join")
}

type daemonRaftClusterIdentity struct {
	Version      int       `json:"version"`
	ClusterID    string    `json:"cluster_id"`
	RepositoryID string    `json:"repository_id"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

func prepareClusterRaftIdentity(sharedRoot, localRaftDir, repositoryID, nodeID string, bootstrap, localState bool) (string, error) {
	sharedPath := filepath.Join(sharedRoot, ".ttc-raft-cluster.json")
	var identity daemonRaftClusterIdentity
	createdIdentity := false
	data, err := os.ReadFile(sharedPath)
	if errors.Is(err, os.ErrNotExist) {
		if !bootstrap || localState {
			return "", errors.New("shared TTC_ROOT has no Raft cluster identity; only a fresh first node may bootstrap it")
		}
		clusterID, idErr := newDaemonRequestID()
		if idErr != nil {
			return "", idErr
		}
		identity = daemonRaftClusterIdentity{Version: 1, ClusterID: clusterID, RepositoryID: repositoryID, CreatedBy: nodeID, CreatedAt: time.Now().UTC()}
		createdIdentity = true
		encoded, marshalErr := json.Marshal(identity)
		if marshalErr != nil {
			return "", marshalErr
		}
		file, createErr := os.OpenFile(sharedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr != nil {
			return "", fmt.Errorf("creating Raft cluster identity: %w", createErr)
		}
		_, writeErr := file.Write(encoded)
		closeErr := errors.Join(file.Sync(), file.Close())
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(sharedPath)
			return "", errors.Join(writeErr, closeErr)
		}
	} else if err != nil {
		return "", err
	} else if err := json.Unmarshal(data, &identity); err != nil {
		return "", fmt.Errorf("decoding Raft cluster identity: %w", err)
	}
	if identity.Version != 1 || !transactionIDPatternForDaemon(identity.ClusterID) || identity.RepositoryID != repositoryID {
		return "", errors.New("shared TTC_ROOT has an incompatible Raft cluster identity")
	}
	localPath := filepath.Join(localRaftDir, "cluster-id")
	localIdentity, localIdentityErr := os.ReadFile(localPath)
	localIdentityMatches := localIdentityErr == nil && strings.TrimSpace(string(localIdentity)) == identity.ClusterID
	if bootstrap && !localState && !createdIdentity && !localIdentityMatches {
		return "", fmt.Errorf("refusing to bootstrap a second Raft group for cluster %q", identity.ClusterID)
	}
	if err := os.MkdirAll(localRaftDir, 0700); err != nil {
		return "", err
	}
	if local, err := os.ReadFile(localPath); err == nil {
		if strings.TrimSpace(string(local)) != identity.ClusterID {
			return "", errors.New("TTC_LOCAL_ROOT belongs to another Raft cluster")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(localPath, []byte(identity.ClusterID+"\n"), 0600); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	return identity.ClusterID, nil
}

func transactionIDPatternForDaemon(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func requireEphemeralClusterPagePort(option, address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid %s address %q: %w", option, address, err)
	}
	if port != "0" {
		return fmt.Errorf("%s must use port 0; each concurrent remote fork requires an independent page listener", option)
	}
	return nil
}

func parseClusterPeer(value string) (nodeID, baseURL, serverName string, err error) {
	equals := strings.IndexByte(value, '=')
	if equals <= 0 || equals == len(value)-1 {
		return "", "", "", fmt.Errorf("invalid --cluster-peer %q; want NODE=HOST:PORT", value)
	}
	nodeID = value[:equals]
	host := value[equals+1:]
	if strings.TrimSpace(nodeID) != nodeID || strings.TrimSpace(host) != host {
		return "", "", "", fmt.Errorf("invalid whitespace in --cluster-peer %q", value)
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		return "", "", "", fmt.Errorf("invalid --cluster-peer address %q: %w", host, err)
	}
	return nodeID, "http://" + host, "", nil
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

type countingDaemonWriter struct {
	writer io.Writer
	bytes  int64
}

// daemonStatusWriter forwards a daemon frame stream unchanged while learning
// its terminal status. It intentionally does not buffer stdout/stderr, which
// may be an unbounded attached console stream.
type daemonStatusWriter struct {
	writer         io.Writer
	header         [5]byte
	headerBytes    int
	frameType      byte
	frameRemaining uint32
	statusPayload  [4]byte
	statusBytes    int
	status         int
	statusSeen     bool
}

func (w *daemonStatusWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.observe(data[:n])
	return n, err
}

func (w *daemonStatusWriter) observe(data []byte) {
	for len(data) != 0 {
		if w.headerBytes < len(w.header) {
			take := min(len(w.header)-w.headerBytes, len(data))
			copy(w.header[w.headerBytes:], data[:take])
			w.headerBytes += take
			data = data[take:]
			if w.headerBytes != len(w.header) {
				continue
			}
			w.frameType = w.header[0]
			w.frameRemaining = binary.BigEndian.Uint32(w.header[1:])
			w.statusBytes = 0
		}
		take := min(int(w.frameRemaining), len(data))
		if w.frameType == daemonFrameStatus && w.statusBytes < len(w.statusPayload) {
			copied := min(take, len(w.statusPayload)-w.statusBytes)
			copy(w.statusPayload[w.statusBytes:], data[:copied])
			w.statusBytes += copied
		}
		data = data[take:]
		w.frameRemaining -= uint32(take)
		if w.frameRemaining == 0 {
			if w.frameType == daemonFrameStatus && w.statusBytes == len(w.statusPayload) {
				w.status = int(binary.BigEndian.Uint32(w.statusPayload[:]))
				w.statusSeen = true
			}
			w.headerBytes = 0
		}
	}
}

func (w *countingDaemonWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.bytes += int64(n)
	return n, err
}

type controlOnlyDaemonServer struct {
	listener     net.Listener
	cluster      *controlOnlyCluster
	root         string
	localRoot    string
	mutationRoot string
	portIngress  *portIngressManager
	portMu       sync.Mutex
	stopOnce     sync.Once
}

// serveControlOnlyDaemon exposes only the local Unix API. It never opens a TCP
// listener and never registers a cluster node, so compute nodes cannot select
// it for placement or initiate a connection to it.
func serveControlOnlyDaemon(socket, sharedRoot string, control *controlOnlyCluster, stdout, stderr io.Writer) error {
	if control == nil || len(control.peerSnapshot()) == 0 {
		return errors.New("control-only cluster has no compute seed")
	}
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
	server := &controlOnlyDaemonServer{listener: listener, cluster: control, root: sharedRoot, localRoot: dir}
	mutationRoot, err := os.MkdirTemp(dir, ".ttc-control-mutations-")
	if err != nil {
		return fmt.Errorf("creating control-only mutation journal: %w", err)
	}
	defer os.RemoveAll(mutationRoot)
	server.mutationRoot = mutationRoot
	server.portIngress = newRoutedPortIngressManager(controlOnlyPortIngressRouter{cluster: control, root: sharedRoot})
	defer server.portIngress.Close()
	discoveryDone := make(chan struct{})
	defer close(discoveryDone)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-discoveryDone:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = control.refreshPeers(ctx)
				cancel()
			}
		}
	}()
	if stdout != nil {
		fmt.Fprintf(stdout, "ttcd control-only gateway listening on %s (repository=%s, seed=%s)\n", socket, control.repositoryID, control.seedURLs()[0])
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accepting client: %w", err)
		}
		go server.handle(conn, stderr)
	}
}

func (s *controlOnlyDaemonServer) handle(conn net.Conn, serverErrors io.Writer) {
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
	if request.Action != "stop" && request.Version != daemonAPIVersion {
		writeError(fmt.Errorf("unsupported API version %d (server uses %d)", request.Version, daemonAPIVersion))
		return
	}
	if request.Action != "stop" && request.ClientBuildID != "" && request.ClientBuildID != version.Version() {
		writeError(fmt.Errorf("client build %q does not match daemon build %q; only 'ttc daemon stop' is allowed across versions", request.ClientBuildID, version.Version()))
		return
	}
	switch request.Action {
	case "ping":
		seeds := s.cluster.seedURLs()
		payload, _ := json.Marshal(map[string]any{
			"version": daemonAPIVersion, "build_id": version.Version(), "features": daemonAPIFeatures,
			"pid": os.Getpid(), "role": "control-only", "control_only": true,
			"shared_root": s.root,
			"cluster":     map[string]any{"control_only": true, "repository_id": s.cluster.repositoryID, "seeds": seeds},
		})
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStdout, append(payload, '\n'))
		_ = writeDaemonStatus(conn, &writeMu, 0)
		return
	case "stop":
		if len(request.Args) != 0 {
			writeError(errors.New("container stop policies are not applicable to a control-only daemon"))
			return
		}
		_ = writeDaemonStatus(conn, &writeMu, 0)
		s.stopOnce.Do(func() { _ = s.listener.Close() })
		return
	}
	// Published ports belong to the daemon contacted by the client, not to the
	// current branch owner. A control-only gateway therefore executes these
	// commands locally and tunnels accepted streams outbound to the owner. All
	// other commands retain the ordinary compute-proxy path below.
	if request.Action == "command" && len(request.Args) != 0 && request.Args[0] == "port" {
		s.portMu.Lock()
		defer s.portMu.Unlock()
		if request.RequestID != "" {
			if record, replay, err := replayCompletedDaemonMutation(s.mutationRoot, request); err != nil {
				writeError(err)
				return
			} else if replay {
				writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
				return
			}
		}
		var stdout, stderr bytes.Buffer
		backend := &runtimeBackend{portIngress: s.portIngress, stdout: &stdout, stderr: &stderr}
		var record daemonMutationRecord
		if request.RequestID != "" {
			var replay bool
			var err error
			record, replay, err = beginDaemonMutation(s.mutationRoot, request)
			if err != nil {
				writeError(err)
				return
			}
			if replay {
				writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
				return
			}
		}
		status := runCommand(request.Args, &stdout, &stderr, backend)
		if request.RequestID != "" {
			completed, err := completeDaemonMutation(s.mutationRoot, record, status, stdout.Bytes(), stderr.Bytes())
			if err != nil {
				fmt.Fprintf(&stderr, "ttcd: %v; command outcome is indeterminate\n", err)
				status = 1
			} else {
				record = completed
				writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
				return
			}
		}
		writeDaemonBufferedResult(conn, &writeMu, stdout.Bytes(), stderr.Bytes(), status)
		return
	}
	var preparedIngress *preparedRunIngress
	ingressCommitted := false
	defer func() {
		if preparedIngress != nil && !ingressCommitted {
			s.portIngress.rollbackPrepared(preparedIngress)
		}
	}()
	if request.Action == "command" && !request.IngressPrepared {
		cleanArgs, prepared, err := s.portIngress.prepareRun(request.Args)
		if err != nil {
			writeError(err)
			return
		}
		request.Args = cleanArgs
		preparedIngress = prepared
		request.IngressPrepared = prepared != nil
		if prepared != nil {
			encoded, err := json.Marshal(&request)
			if err != nil {
				writeError(err)
				return
			}
			header = string(append(encoded, '\n'))
		}
	}
	peers := s.cluster.peerSnapshot()
	if len(peers) == 0 {
		writeError(errors.New("control-only cluster has no known compute peer"))
		return
	}
	index := s.cluster.active.Load() % uint64(len(peers))
	peer := peers[index]
	client := peer.client
	copyRoute, copyCommand, err := parseDistributedCopyRoute(request.Args)
	if err != nil {
		writeError(err)
		return
	}
	if copyCommand && copyRoute.sourceContainer && copyRoute.destination != "-" {
		if err := proxyDistributedCopyDownload(context.Background(), client, s.localRoot, request, copyRoute, conn, &writeMu); err != nil {
			s.cluster.active.Add(1)
			writeError(fmt.Errorf("proxying distributed cp through compute seed %s: %w", peer.url, err))
		}
		return
	}
	// A request without stdin or terminal control contains no bytes after its
	// JSON header. Do not
	// leave the upstream HTTP transport reading from the still-open local Unix
	// connection: if the selected compute node has just retired, net/http waits
	// for that request-body goroutine and failover never observes the dial
	// error. Interactive stdin and terminal control keep the full-duplex reader
	// by construction.
	input := controlOnlyProxyInput(header, reader, request.streamsClientFrames())
	var copyUploadFlow *daemonStdinFlow
	var cleanupCopyUpload func()
	if copyCommand && !copyRoute.sourceContainer && copyRoute.source != "-" {
		request, input, copyUploadFlow, cleanupCopyUpload, err = prepareDistributedCopyUpload(s.localRoot, request, copyRoute)
		if err != nil {
			writeError(err)
			return
		}
		defer cleanupCopyUpload()
	}
	if copyUploadFlow != nil {
		if err := proxyDistributedCopyUpload(context.Background(), client, input, copyUploadFlow, conn, &writeMu); err != nil {
			s.cluster.active.Add(1)
			writeError(fmt.Errorf("proxying distributed cp through compute seed %s: %w", peer.url, err))
		}
		return
	}
	output := &countingDaemonWriter{writer: conn}
	var tracked *daemonStatusWriter
	var proxyOutput io.Writer = output
	if preparedIngress != nil {
		tracked = &daemonStatusWriter{writer: output}
		proxyOutput = tracked
	}
	if err := client.ProxyDaemon(context.Background(), input, proxyOutput); err != nil {
		s.cluster.active.Add(1)
		if output.bytes != 0 {
			// A response without a terminal status is indeterminate. Keep a
			// prepared listener because the idempotent run may already exist.
			ingressCommitted = preparedIngress != nil && (tracked == nil || !tracked.statusSeen || tracked.status == 0)
			return
		}
		firstErr := fmt.Errorf("%s: %w", peer.url, err)
		// Requests without a streaming stdin are replayable until a peer emits a
		// response byte. This makes graceful compute retirement invisible to a
		// control-only client even when it races the periodic topology refresh.
		if !request.Stdin {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = s.cluster.refreshPeers(ctx)
			cancel()
			refreshed := s.cluster.peerSnapshot()
			for _, candidate := range refreshed {
				if candidate.url == peer.url {
					continue
				}
				retryOutput := io.Writer(output)
				if preparedIngress != nil {
					tracked = &daemonStatusWriter{writer: output}
					retryOutput = tracked
				}
				if retryErr := candidate.client.ProxyDaemon(context.Background(), strings.NewReader(header), retryOutput); retryErr == nil {
					ingressCommitted = preparedIngress != nil && tracked != nil && tracked.statusSeen && tracked.status == 0
					return
				} else if output.bytes != 0 {
					return
				} else {
					firstErr = errors.Join(firstErr, fmt.Errorf("%s: %w", candidate.url, retryErr))
				}
			}
		}
		writeError(fmt.Errorf("proxying through compute peers: %w", firstErr))
		return
	}
	if tracked != nil {
		ingressCommitted = tracked.statusSeen && tracked.status == 0
	}
}

func controlOnlyProxyInput(header string, stdin io.Reader, streaming bool) io.Reader {
	if !streaming {
		return strings.NewReader(header)
	}
	return io.MultiReader(strings.NewReader(header), stdin)
}

// prepareDaemonRuntimeStorage ensures that control sockets and runsc's live
// state remain on node-local storage. Cluster mode places runtime state below
// TTC_LOCAL_ROOT; standalone mode places it beside TTC_SOCKET so TTC_ROOT may
// hold authoritative data on a shared filesystem without putting live Unix
// sockets there.
func prepareDaemonRuntimeStorage(socket, root string) error {
	paths := []struct {
		name string
		path string
	}{
		{name: "socket directory", path: filepath.Dir(socket)},
		{name: "runsc state root", path: root},
	}
	checked := make(map[string]struct{}, len(paths))
	for _, candidate := range paths {
		clean := filepath.Clean(candidate.path)
		if _, ok := checked[clean]; ok {
			continue
		}
		checked[clean] = struct{}{}
		if err := os.MkdirAll(clean, 0700); err != nil {
			return fmt.Errorf("creating %s %q: %w", candidate.name, clean, err)
		}
		var statfs unix.Statfs_t
		if err := daemonStatfs(clean, &statfs); err != nil {
			return fmt.Errorf("checking %s filesystem %q: %w", candidate.name, clean, err)
		}
		if int64(statfs.Type) == int64(unix.NFS_SUPER_MAGIC) {
			return fmt.Errorf("%s %q is on NFS; TTC_SOCKET and TTC_LOCAL_ROOT runtime state must be node-local (only TTC_ROOT may be shared)", candidate.name, clean)
		}
	}
	return nil
}

func serveDaemon(socket, runtimePlatform string, unsafeNoHostIsolation bool, kernelCompatibility hostKernelCompatibility, dataRoot, root, runsc string, volumeCapability ttcvolume.Capability, stdout, stderr io.Writer) error {
	profile, err := applyStandaloneCPUProfile(dataRoot, "portable", "", runtimePlatform, false)
	if err != nil {
		return err
	}
	return serveDaemonWithCluster(socket, runtimePlatform, profile, unsafeNoHostIsolation, kernelCompatibility, dataRoot, dataRoot, root, runsc, volumeCapability, nil, stdout, stderr)
}

func serveDaemonWithCluster(socket, runtimePlatform string, cpuProfile daemonCPUProfile, unsafeNoHostIsolation bool, kernelCompatibility hostKernelCompatibility, dataRoot, localRoot, root, runsc string, volumeCapability ttcvolume.Capability, clusterRuntime *daemonClusterRuntime, stdout, stderr io.Writer) (err error) {
	snapshotGraph, err := ttcsnapshot.Open(filepath.Join(localRoot, "catalog", "snapshot-graph"))
	if err != nil {
		return fmt.Errorf("opening TTC snapshot graph: %w", err)
	}
	defer snapshotGraph.Close()
	snapshotData, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(localRoot, "objects"))
	if err != nil {
		return fmt.Errorf("opening TTC snapshot object store: %w", err)
	}
	var sharedObjects *ttcsnapshot.SharedPackStore
	if clusterRuntime != nil {
		sharedObjects, err = ttcsnapshot.OpenSharedPackStore(filepath.Join(dataRoot, "snapshot-packs"))
		if err != nil {
			return fmt.Errorf("opening shared snapshot packs: %w", err)
		}
	}
	if err := validatePersistedMemoryGraph(ttcbundle.Store{Root: dataRoot, ActiveRoot: localRoot}); err != nil {
		return fmt.Errorf("validating snapshot-graph branch records: %w", err)
	}
	removeRetiredRaftState := false
	if clusterRuntime != nil && clusterRuntime.raftLocalDir != "" {
		// This defer is intentionally registered before raft.Close below, so it
		// runs afterward. A cleanly retired node must rejoin as a fresh Raft
		// member; retaining the removed member's Bolt state leaves it waiting
		// forever for a configuration in which it no longer participates.
		defer func() {
			if removeRetiredRaftState {
				if err := os.RemoveAll(clusterRuntime.raftLocalDir); err != nil && stderr != nil {
					fmt.Fprintf(stderr, "ttcd: warning: removing retired Raft state: %v\n", err)
				}
			}
		}()
	}
	if clusterRuntime != nil && clusterRuntime.mux != nil {
		defer clusterRuntime.mux.Close()
	}
	if clusterRuntime != nil && clusterRuntime.raft != nil {
		defer clusterRuntime.raft.Close()
	}
	// A fresh node first joins Raft as a learner, then commits its execution
	// identity and proves full-mesh topology before becoming ready. If any
	// startup step in that window fails, remove the learner after the shutdown
	// defer has fenced/offlined a possibly committed joining record and before
	// the local Raft transport closes.
	cancelFreshJoin := (func(context.Context) error)(nil)
	if clusterRuntime != nil {
		cancelFreshJoin = clusterRuntime.cancelFreshJoin
	}
	defer func() {
		if cancelFreshJoin == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := cancelFreshJoin(ctx); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: cancelling incomplete Raft admission: %v\n", err)
		}
	}()
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
		socket:                socket,
		runtimePlatform:       runtimePlatform,
		cpuProfileName:        cpuProfile.Mode,
		cpuFeatures:           cpuProfile.annotation(),
		unsafeNoHostIsolation: unsafeNoHostIsolation,
		disableHostGSO:        kernelCompatibility.disableHostGSO,
		hostKernelRelease:     kernelCompatibility.release,
		dataRoot:              dataRoot,
		localRoot:             localRoot,
		root:                  root,
		runsc:                 runsc,
		volumeCapability:      volumeCapability,
		snapshotGraph:         snapshotGraph,
		snapshotData:          snapshotData,
		stderr:                stderr,
		reconciled:            make(map[string]bool),
		mutationLocks:         make(map[string]*sync.Mutex),
	}
	server.portIngress = newPortIngressManager(server)
	defer server.portIngress.Close()
	if err := compactDaemonMutations(server.mutationRoot(), daemonMutationRetention); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "ttcd: warning: compacting daemon mutation history: %v\n", err)
	}
	var sharedReferences *sharedReferenceService
	if clusterRuntime != nil {
		sharedReferences = newSharedReferenceService(dataRoot, localRoot, clusterRuntime.nodeRecord.NodeID, clusterRuntime.nodeRecord.Incarnation, clusterRuntime.raftCatalog, sharedObjects, snapshotGraph, clusterRuntime.source)
		server.sharedReferences = sharedReferences
	}
	var publicationQueue *ttcstorage.Queue
	if clusterRuntime != nil {
		publisher := &branchPublisher{
			sharedRoot: dataRoot, localRoot: localRoot,
			graph: snapshotGraph, graphData: snapshotData, graphShared: sharedObjects, sharedRefs: sharedReferences,
		}
		publisher.event = func(event DaemonEvent) { server.recordEvent(dataRoot, event) }
		publisher.nodeID = clusterRuntime.nodeRecord.NodeID
		publisher.incarnation = clusterRuntime.nodeRecord.Incarnation
		publisher.placements = clusterRuntime.placements
		publisher.transactions = clusterRuntime.transactions
		publisher.authority = clusterRuntime.raftCatalog
		publisher.peers = clusterRuntime.source
		publicationQueue, err = ttcstorage.Open(localRoot, publisher)
		if err != nil {
			return fmt.Errorf("opening durable publication queue: %w", err)
		}
		defer publicationQueue.Close()
	}
	server.publicationQueue = publicationQueue
	// Recover namespace-atomic deletions before announcing readiness. Failure
	// to physically remove one tombstone is non-fatal: it is outside the active
	// container namespace and periodic maintenance will retry it.
	unlockRecovery := server.lockDataRoot(dataRoot)
	_, recoveryErr := (ttcbundle.Store{Root: dataRoot, ActiveRoot: localRoot}).RecoverContainerRemovals()
	unlockRecovery()
	if recoveryErr != nil && stderr != nil {
		fmt.Fprintf(stderr, "ttcd: warning: recovering removed containers: %v\n", recoveryErr)
	}
	maintenanceDone := make(chan struct{})
	defer close(maintenanceDone)
	var maintenanceTransactions cluster.TransactionCatalog
	if clusterRuntime != nil {
		maintenanceTransactions = clusterRuntime.transactions
	}
	var maintenanceAuthority *cluster.RaftCatalog
	if clusterRuntime != nil {
		maintenanceAuthority = clusterRuntime.raftCatalog
	}
	go maintainRepositories(dataRoot, localRoot, server.root, server.runsc, publicationQueue, maintenanceTransactions, maintenanceAuthority, snapshotGraph, snapshotData, clusterRuntime == nil, stderr, maintenanceDone, func() func() {
		return server.lockDataRoot(dataRoot)
	}, server.operations.any)
	clusterDone := make(chan struct{})
	var elasticHeartbeatStopped <-chan struct{}
	var localCacheCleanupStopped <-chan struct{}
	var sharedGCDone chan struct{}
	var sharedGCStopped <-chan struct{}
	var elasticRecord *cluster.NodeRecord
	var clusterTargetBackend *runtimeBackend
	var leaseGuardian *os.File
	retireClusterNode := func(clean bool) error {
		if elasticRecord == nil || clusterRuntime == nil || clusterRuntime.directory == nil {
			return nil
		}
		record := *elasticRecord
		var result error
		if !clean && clusterTargetBackend != nil {
			if err := fenceElasticRuntimeRoot(clusterTargetBackend.activeDataRoot(), clusterTargetBackend.root, record.NodeID, record.Incarnation); err != nil {
				result = errors.Join(result, fmt.Errorf("fencing runtimes during unclean cluster shutdown: %w", err))
			}
		}
		offline, transitionErr := clusterRuntime.directory.Transition(record.NodeID, record.Incarnation, cluster.NodeOffline, time.Now(), clusterRuntime.lease)
		if transitionErr != nil {
			result = errors.Join(result, fmt.Errorf("publishing cluster node offline state: %w", transitionErr))
		}
		if !clean {
			return result
		}
		intent, intentErr := loadElasticDrainIntent(dataRoot, record.NodeID)
		if intentErr != nil {
			result = errors.Join(result, fmt.Errorf("loading completed drain intent: %w", intentErr))
		} else if intent != nil && intent.State == elasticDrainComplete && transitionErr == nil {
			retired, err := retireDaemonRaftMember(clusterRuntime, offline.NodeID, offline.Incarnation)
			if err != nil {
				result = errors.Join(result, fmt.Errorf("retiring drained Raft member: %w", err))
			} else {
				removeRetiredRaftState = retired
				if err := removeLocalNodeIncarnation(localRoot, offline.NodeID, offline.Incarnation); err != nil {
					result = errors.Join(result, fmt.Errorf("rotating retired node incarnation: %w", err))
				}
			}
		}
		if err := removeCompletedElasticDrainIntent(dataRoot, record.NodeID); err != nil {
			result = errors.Join(result, fmt.Errorf("consuming completed drain intent: %w", err))
		}
		if result == nil {
			// The discovery identity and, where applicable, Raft membership are
			// gone. Suppress duplicate work in the shutdown defer.
			elasticRecord = nil
		}
		return result
	}
	defer func() {
		// Relinquishing shared-data mutation authority is a Raft operation. Stop
		// and join that worker while the execution heartbeat is still renewing;
		// otherwise a slow final collection can outlive the node lease and turn a
		// clean shutdown into a spurious stale-lease warning.
		if sharedGCDone != nil {
			close(sharedGCDone)
		}
		sharedGCStopTimeout := 5 * time.Second
		if clusterRuntime != nil && clusterRuntime.lease > sharedGCStopTimeout {
			sharedGCStopTimeout = clusterRuntime.lease
		}
		waitDaemonWorkerFor(sharedGCStopped, "shared GC coordinator", sharedGCStopTimeout, stderr)
		close(clusterDone)
		waitDaemonWorker(elasticHeartbeatStopped, "elastic heartbeat", stderr)
		waitDaemonWorkerFor(localCacheCleanupStopped, "local cache cleanup", 5*time.Second, stderr)
		if err := retireClusterNode(server.cleanStop.Load()); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: retiring cluster node: %v\n", err)
		}
		if leaseGuardian != nil {
			_ = leaseGuardian.Close()
		}
	}()
	var clusterServer *http.Server
	clusterErr := make(chan error, 1)
	// A lease that runs out fences this node's Sentries and closes its
	// listeners, and only then reports why. Whatever ends serving after a
	// fence, the fence is the reason, and it must be returned for the daemon to
	// restart and wait for its cluster; see DaemonRestartExitCode.
	var selfFenced atomic.Bool
	heartbeatErr := make(chan error, 1)
	defer func() {
		if selfFenced.Load() {
			err = errors.Join(<-heartbeatErr, err)
		}
	}()
	if clusterRuntime != nil {
		graphProvider := &graphObjectProvider{
			graph: snapshotGraph, data: snapshotData, authority: clusterRuntime.raftCatalog,
			transactions: clusterRuntime.transactions, nodeID: clusterRuntime.nodeRecord.NodeID,
			incarnation: clusterRuntime.nodeRecord.Incarnation, shared: sharedReferences,
		}
		clusterRuntime.controlPlane.ObjectProvider = graphProvider
		clusterRuntime.controlPlane.SnapshotProvider = graphProvider
		clusterRuntime.controlPlane.SnapshotReferences = graphProvider
		clusterRuntime.controlPlane.SharedReferences = sharedReferences
		clusterRuntime.controlPlane.ReferenceInventory = sharedReferences
		resolver, err := ttcsnapshot.NewResolver(snapshotGraph, snapshotData,
			raftOriginLocator{authority: clusterRuntime.raftCatalog},
			clusterPeerSources{runtime: clusterRuntime.source}, sharedObjects)
		if err != nil {
			return fmt.Errorf("opening distributed snapshot resolver: %w", err)
		}
		server.snapshotResolver = resolver
		server.snapshotShared = sharedObjects
		targetBackend := directBackend(options{
			root: server.root, runsc: server.runsc, dataRoot: server.dataRoot, localRoot: server.localRoot,
			runtimePlatform:       server.runtimePlatform,
			cpuFeatures:           server.cpuFeatures,
			unsafeNoHostIsolation: server.unsafeNoHostIsolation,
			disableHostGSO:        server.disableHostGSO,
		}, strings.NewReader(""), io.Discard, io.Discard)
		clusterTargetBackend = targetBackend
		server.configureVolumeBackend(targetBackend)
		targetBackend.consoleSidecars = server
		targetBackend.nodeID = clusterRuntime.controlPlane.Capabilities.NodeID
		targetBackend.distributedSource = clusterRuntime.source
		graphProvider.publish = func(ctx context.Context, snapshot ttcsnapshot.SnapshotID) error {
			// Snapshot IDs are cluster-global, but memory and filesystem origins
			// have different node-local stores. The peer publication endpoint is
			// deliberately generic so recursive cross-origin publication works
			// even when a filesystem dependency resides on another node.
			if _, ok := targetBackend.snapshotGraph.Snapshot(snapshot); !ok {
				return targetBackend.publishVolatileFilesystemOriginContext(ctx, snapshot, make(map[ttcsnapshot.SnapshotID]struct{}))
			}
			publisher := &branchPublisher{
				sharedRoot: targetBackend.dataRoot,
				nodeID:     targetBackend.nodeID, incarnation: clusterRuntime.nodeRecord.Incarnation,
				authority: clusterRuntime.raftCatalog, graph: targetBackend.snapshotGraph,
				graphData: targetBackend.snapshotData, graphShared: targetBackend.snapshotShared,
				sharedRefs: targetBackend.sharedReferences, peers: targetBackend.distributedSource,
			}
			return publisher.publishVolatileGraphOrigin(ctx, snapshot)
		}
		targetRuntime := &daemonTargetRuntime{
			backend: targetBackend, nodeID: clusterRuntime.controlPlane.Capabilities.NodeID,
			incarnation: clusterRuntime.nodeRecord.Incarnation, directory: clusterRuntime.directory,
			store: clusterRuntime.controlPlane.Store, authority: clusterRuntime.raftCatalog, done: clusterDone,
		}
		clusterRuntime.controlPlane.TargetRuntime = targetRuntime
		clusterRuntime.controlPlane.CommandProxy = daemonClusterProxy{socket: socket}
		clusterRuntime.controlPlane.PortProxy = daemonPortProxy{backend: targetBackend}
		clusterRuntime.controlPlane.RenewExecution = func(context.Context) error {
			_, err := targetBackend.renewDistributedExecutionAuthority()
			return err
		}
		clusterListener := clusterRuntime.controlListener
		if clusterListener == nil {
			return errors.New("cluster protocol multiplexer has no HTTP listener")
		}
		// Reference-intent recovery may need to contact either a volatile
		// provider or the shared-reference coordinator. Serve those requests
		// before doing any recovery ourselves so simultaneous daemon restarts
		// cannot deadlock waiting for peers that are waiting on us.
		// Restore, durability, and drain requests may deliberately wait through
		// a full authority-lease fence before completing. Do not impose a server
		// WriteTimeout that can expire first and turn a structured retry/result
		// into a transport EOF. Request bodies are bounded, ReadHeaderTimeout
		// protects admission, and each operation has its own transactional and
		// client-side deadline.
		clusterServer = &http.Server{
			Handler: clusterRuntime.controlPlane, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute,
		}
		go func() {
			err := clusterServer.Serve(clusterListener)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			clusterErr <- err
			listener.Close()
		}()
		defer shutdownClusterServer(clusterServer)
		if clusterRuntime.directory != nil {
			clusterRuntime.nodeRecord.MemoryCurrentBytes, _ = currentNodeMemoryBytes(localRoot)
			drainIntent, err := loadElasticDrainIntent(dataRoot, clusterRuntime.nodeRecord.NodeID)
			if err != nil {
				clusterListener.Close()
				return fmt.Errorf("loading elastic drain recovery state: %w", err)
			}
			waitingForQuorum := clusterRuntime.raft != nil && clusterRuntime.raft.HadExistingState() && clusterRuntime.raft.Leader() == ""
			if waitingForQuorum {
				// The Raft/control listener is already serving. Advertise this
				// intentional intermediate state to the local launcher so an
				// operator can start the remaining voters sequentially. This is
				// not readiness: the Unix request loop and every execution grant
				// remain unavailable until registration below commits.
				if err := writeDaemonQuorumWait(socket); err != nil {
					clusterListener.Close()
					return fmt.Errorf("publishing quorum-wait startup state: %w", err)
				}
			}
			registered, err := registerClusterNodeWithRetry(clusterRuntime.directory, clusterRuntime.nodeRecord, clusterRuntime.lease, clusterRuntime.recoverMembership, holdsFencedRuntimes(localRoot), stderr)
			if waitingForQuorum {
				if removeErr := os.Remove(protocol.DaemonQuorumWaitPath(socket)); removeErr != nil && !os.IsNotExist(removeErr) && err == nil {
					err = fmt.Errorf("clearing quorum-wait startup state: %w", removeErr)
				}
			}
			if err != nil {
				clusterListener.Close()
				return fmt.Errorf("registering elastic cluster node: %w", err)
			}
			// From this point the node has a committed incarnation and must be
			// taken offline by the shutdown defer if any subsequent admission
			// check fails.
			elasticRecord = registered
			leaseGuardian, err = startLeaseGuardian(targetBackend.activeDataRoot(), targetBackend.root, registered.NodeID, registered.Incarnation, clusterRuntime.lease)
			if err != nil {
				_ = fenceElasticRuntimeRoot(targetBackend.activeDataRoot(), targetBackend.root, registered.NodeID, registered.Incarnation)
				clusterListener.Close()
				return fmt.Errorf("starting execution-lease guardian: %w", err)
			}
			// Registration starts the execution lease. Recovery below may replay a
			// large crash journal or reconcile many placements, so renewal must
			// begin now rather than after the Ready transition.
			fence := func() error {
				selfFenced.Store(true)
				_ = listener.Close()
				_ = clusterServer.Close()
				return fenceElasticRuntimeRoot(targetBackend.activeDataRoot(), targetBackend.root, registered.NodeID, registered.Incarnation)
			}
			var heartbeatFatal <-chan error
			elasticHeartbeatStopped, heartbeatFatal, err = startElasticNodeHeartbeat(
				clusterRuntime.directory, clusterRuntime.raftCatalog, *registered, clusterRuntime.lease,
				stderr, clusterDone, targetBackend, leaseGuardian, fence)
			if err != nil {
				clusterListener.Close()
				return fmt.Errorf("starting elastic cluster heartbeat: %w", err)
			}
			localCacheCleanupStopped = maintainOrphanLocalCacheCleanup(
				clusterRuntime.raftCatalog, targetBackend, clusterRuntime.lease/4, stderr, clusterDone,
				func() func() { return server.lockDataRootIdle(dataRoot) })
			go func() {
				select {
				case err := <-heartbeatFatal:
					heartbeatErr <- err
					listener.Close()
					clusterServer.Close()
				case <-clusterDone:
				}
			}()
			// Start shared-reference election while the node is still joining.
			// A recovered retain must be durable before any local runtime is
			// admitted; if this node becomes the coordinator, its ledger must be
			// available to that recovery first.
			gcStopped := make(chan struct{})
			sharedGCDone = make(chan struct{})
			sharedGCStopped = gcStopped
			go func() {
				defer close(gcStopped)
				maintainSharedGCCoordinator(clusterRuntime.raftCatalog, sharedObjects, sharedReferences, registered.NodeID, registered.Incarnation, stderr, sharedGCDone)
			}()
			if err := targetBackend.reconcileInterruptedClusterMemoryGraph(); err != nil {
				clusterListener.Close()
				return fmt.Errorf("recovering interrupted memory captures: %w", err)
			}
			if err := reconcileDistributedGraphReferencesAtStartup(targetBackend, clusterRuntime.lease); err != nil {
				clusterListener.Close()
				return fmt.Errorf("recovering distributed graph references: %w", err)
			}
			// The detached lease guardian host-stops Sentries when the previous
			// daemon exits. Placement reconciliation inventories runsc state and may
			// need a container lock that an in-flight console follower or control
			// command held when the Sentry was stopped. Re-authorize the exact
			// PID/start-time set against the replicated placement catalog before
			// taking those locks; otherwise restart can deadlock behind its own
			// execution fence. This does not admit new work: the node is still
			// Joining and the Unix command listener has not entered its accept loop.
			if err := resumeFencedElasticRuntimes(targetBackend, registered.NodeID, registered.Incarnation); err != nil {
				clusterListener.Close()
				return fmt.Errorf("resuming placement-authorized fenced runtimes for recovery: %w", err)
			}
			// While joining, recover every retained placement into this exact
			// incarnation. The ready transition then fences any old placement that
			// was not found locally; only afterward may new work arrive. The early
			// resume above is physical lock recovery, not scheduler admission.
			if err := targetBackend.reconcilePlacementCatalog(); err != nil {
				clusterListener.Close()
				return fmt.Errorf("recovering fenced branch placements: %w", err)
			}
			if err := refreshDistributedGraphHandoffsAtStartup(targetRuntime); err != nil {
				clusterListener.Close()
				return fmt.Errorf("refreshing distributed memory relays: %w", err)
			}
			if err := validateClusterTopology(clusterRuntime, *registered); err != nil {
				clusterListener.Close()
				return fmt.Errorf("validating full-mesh cluster topology: %w", err)
			}
			admittedState := cluster.NodeReady
			if drainIntent != nil {
				admittedState = cluster.NodeDraining
			}
			ready, err := clusterRuntime.directory.Transition(registered.NodeID, registered.Incarnation, admittedState, time.Now(), clusterRuntime.lease)
			if err != nil {
				clusterListener.Close()
				return fmt.Errorf("admitting elastic cluster node: %w", err)
			}
			if ready.State == cluster.NodeReady && ready.MemoryTargetBytes != 0 && ready.MemoryCurrentBytes > ready.MemoryTargetBytes {
				ready, err = clusterRuntime.directory.Transition(ready.NodeID, ready.Incarnation, cluster.NodePressured, time.Now(), clusterRuntime.lease)
				if err != nil {
					clusterListener.Close()
					return fmt.Errorf("marking elastic cluster node pressured: %w", err)
				}
			}
			// The exact incarnation is now admitted. From here ordinary offline,
			// drain, lease expiry, and Raft reconciliation own its lifecycle.
			cancelFreshJoin = nil
			elasticRecord = ready
			server.retireNode = func() error { return retireClusterNode(true) }
			// Origin abandonment is an execution-authority mutation and is not
			// permitted while the node is joining. Reconcile only after the
			// ready/pressured transition has admitted this incarnation.
			if _, err := targetBackend.reconcileVolatileSnapshotOrigins(); err != nil {
				clusterListener.Close()
				return fmt.Errorf("reconciling volatile snapshot origins: %w", err)
			}
			// Enroll any legacy/untracked local branch now that ordinary placement
			// creation is authorized. Repeat the resume idempotently in case a
			// concurrent guardian fence landed after the first recovery pass.
			if err := targetBackend.reconcileObservedInitExits(); err != nil {
				clusterListener.Close()
				return fmt.Errorf("reconciling completed init exits: %w", err)
			}
			if err := targetBackend.reconcilePlacementCatalog(); err != nil {
				clusterListener.Close()
				return fmt.Errorf("re-authorizing fenced branch placements: %w", err)
			}
			if err := resumeFencedElasticRuntimes(targetBackend, ready.NodeID, ready.Incarnation); err != nil {
				clusterListener.Close()
				return fmt.Errorf("resuming re-authorized fenced runtimes: %w", err)
			}
		}
		server.clusterCapabilities = &clusterRuntime.controlPlane.Capabilities
		server.clusterStore = clusterRuntime.controlPlane.Store
		server.clusterDirectory = clusterRuntime.directory
		server.clusterPlacements = clusterRuntime.placements
		server.clusterListen = clusterRuntime.listen
		server.distributedSource = clusterRuntime.source
		if elasticRecord != nil {
			go maintainAuthorityExpirations(clusterRuntime.raft, clusterRuntime.raftCatalog, clusterRuntime.lease, stderr, clusterDone)
		}
		resumeDistributedForkMonitors(targetRuntime, targetBackend, clusterRuntime)
		go maintainDistributedGraphReferences(targetBackend, stderr, clusterDone, func() func() {
			return server.lockDataRootIdle(server.dataRoot)
		})
		if elasticRecord != nil {
			go maintainElasticMigrationRecovery(targetBackend, clusterRuntime.lease, stderr, clusterDone, func(run func() error) error {
				unlock := server.lockDataRootIdle(server.dataRoot)
				defer unlock()
				server.reconcile(targetBackend)
				return run()
			})
			go maintainElasticDrain(targetBackend, elasticRecord.NodeID, clusterRuntime.lease, stderr, clusterDone, func(run func() error) error {
				unlock := server.lockDataRootIdle(server.dataRoot)
				defer unlock()
				server.reconcile(targetBackend)
				return run()
			}, func() {
				// A recovered node-stop intent has exactly the same terminal
				// semantics as the original command: once evacuation finishes,
				// this daemon must leave the cluster automatically.
				server.cleanStop.Store(true)
				server.stopOnce.Do(func() { _ = server.listener.Close() })
			})
		}
		if clusterRuntime.autoRebalance && elasticRecord != nil {
			go maintainElasticRebalancer(targetBackend, elasticRecord.NodeID, clusterRuntime.lease, stderr, clusterDone, func(run func() error) error {
				unlock := server.lockDataRootIdle(server.dataRoot)
				defer unlock()
				return run()
			})
		}
		if elasticRecord != nil && clusterRuntime.capacitySink != nil && clusterRuntime.capacitySink.Configured() {
			go maintainElasticCapacityRequests(dataRoot, elasticRecord.NodeID, clusterRuntime.directory, clusterRuntime.capacitySink, stderr, clusterDone)
		}
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "ttcd listening on %s (API v%d, platform=%s, isolation=%s, volume-copy=%s)\n", socket, daemonAPIVersion, runtimePlatform, daemonIsolationMode(unsafeNoHostIsolation), volumeCapability.Mode)
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

func topologyMemberSet(records []cluster.NodeRecord, now time.Time) map[string]string {
	members := make(map[string]string)
	for _, record := range records {
		if record.State != cluster.NodeOffline && record.LeaseActive(now) {
			members[record.NodeID] = record.Incarnation + "\x00" + record.ControlURL
		}
	}
	return members
}

// validateClusterTopology proves that every currently live member can reach
// the joining daemon and that the joining daemon can reach each member. It
// repeats when membership changes during the probe, so readiness is never
// committed from a stale partial view.
func validateClusterTopology(runtime *daemonClusterRuntime, self cluster.NodeRecord) error {
	if runtime == nil || runtime.directory == nil || runtime.source == nil {
		return errors.New("cluster topology services are unavailable")
	}
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		records, err := runtime.directory.List()
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		before := topologyMemberSet(records, time.Now())
		failed := false
		for _, record := range records {
			if record.NodeID == self.NodeID || before[record.NodeID] == "" {
				continue
			}
			peer, err := runtime.source.peerForRecord(record)
			if err == nil {
				nonce, nonceErr := newDaemonRequestID()
				if nonceErr != nil {
					return nonceErr
				}
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				err = peer.ProbeTopology(ctx, cluster.TopologyProbeRequest{
					NodeID: self.NodeID, Incarnation: self.Incarnation,
					ControlURL: self.ControlURL, Nonce: nonce,
				})
				cancel()
			}
			if err != nil {
				lastErr = fmt.Errorf("node %q is not bidirectionally reachable: %w", record.NodeID, err)
				failed = true
				break
			}
		}
		if failed {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		afterRecords, err := runtime.directory.List()
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if after := topologyMemberSet(afterRecords, time.Now()); maps.Equal(before, after) {
			return nil
		}
		lastErr = errors.New("cluster membership changed during topology validation")
	}
	if lastErr == nil {
		lastErr = errors.New("topology validation timed out")
	}
	return lastErr
}

func maintainDistributedGraphReferences(backend *runtimeBackend, stderr io.Writer, done <-chan struct{}, lock func() func()) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		unlock := func() {}
		if lock != nil {
			unlock = lock()
		}
		err := backend.reconcileDistributedGraphReferences(false)
		backingErr := backend.reconcileConsumedMemoryBacking()
		unlock()
		if err != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: reconciling distributed graph references: %v\n", err)
		}
		if backingErr != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: reconciling consumed runtime memory backing: %v\n", backingErr)
		}
	}
}

// reconcileDistributedGraphReferencesAtStartup waits for the cluster's Raft
// leader, volatile providers, and shared-reference coordinator to become
// reachable during a simultaneous restart. The node remains joining and no
// guest runtime is resumed while this runs.
func reconcileDistributedGraphReferencesAtStartup(backend *runtimeBackend, lease time.Duration) error {
	timeout := 2*lease + 30*time.Second
	if timeout < time.Minute {
		timeout = time.Minute
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := backend.reconcileDistributedGraphReferences(true); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			return lastErr
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// validatePersistedMemoryGraph rejects incomplete intermediate development
// layouts at daemon startup. A root name is only a branch-local handle; every
// graph-backed branch must also retain the immutable root object IDs needed
// after its origin moves to shared storage or another node.
func validatePersistedMemoryGraph(store ttcbundle.Store) error {
	containers, err := store.ListContainersForGC()
	if err != nil {
		return err
	}
	for _, container := range containers {
		for _, branch := range container.Branches {
			if branch.SnapshotID == 0 && len(branch.MemoryRoots) == 0 && len(branch.MemoryRootObjects) == 0 && branch.ControlRoot == "" && branch.ControlRootObject == "" {
				continue
			}
			if err := validateGraphRootObjects(branch.SnapshotID, branch.MemoryRoots, branch.MemoryRootObjects); err != nil {
				return fmt.Errorf("%s@%s: %w; this root predates immutable root-object records and is not migrated", container.Name, branch.ID, err)
			}
			names := make(map[string]struct{}, len(branch.MemoryRoots))
			for _, name := range branch.MemoryRoots {
				if name == "" {
					return fmt.Errorf("%s@%s has an empty memory root name", container.Name, branch.ID)
				}
				if _, ok := names[name]; ok {
					return fmt.Errorf("%s@%s repeats memory root %q", container.Name, branch.ID, name)
				}
				names[name] = struct{}{}
			}
			if err := validateGraphControlRoot(branch.SnapshotID, branch.ControlRoot, branch.ControlRootObject); err != nil {
				return fmt.Errorf("%s@%s: %w; this root predates graph-native process control state and is not migrated", container.Name, branch.ID, err)
			}
			if normalizeLifecycleState(branch.State) == "dehydrated" && branch.ControlRoot == "" {
				return fmt.Errorf("%s@%s: dehydrated branch omits graph-native process control state; this root is not migrated", container.Name, branch.ID)
			}
		}
		for _, backing := range container.RuntimeMemoryBackings {
			if err := validateGraphRootObjects(backing.SnapshotID, backing.MemoryRoots, backing.MemoryRootObjects); err != nil {
				return fmt.Errorf("%s runtime memory backing %s: %w", container.Name, backing.ID, err)
			}
			if err := validateGraphControlRoot(backing.SnapshotID, backing.ControlRoot, backing.ControlRootObject); err != nil {
				return fmt.Errorf("%s runtime memory backing %s: %w", container.Name, backing.ID, err)
			}
		}
	}
	return nil
}

// shutdownClusterServer lets an in-flight proxied node-stop response drain
// after that command closes the local daemon listener. Closing the HTTP server
// immediately races the final daemon status frame and presents a successful
// remote stop to its caller as EOF.
func shutdownClusterServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
	}
}

// waitDaemonWorker keeps an unavailable Raft leader from turning an ordinary
// startup failure or daemon stop into an immortal half-started process. Some
// catalog interfaces predate context-aware reads, so a worker may be blocked
// inside a linearizable barrier even after clusterDone closes. The process is
// already committed to exit at this point; bounding the join lets subsequent
// deferred cleanup close the Raft transport and local stores.
func waitDaemonWorker(done <-chan struct{}, name string, stderr io.Writer) {
	waitDaemonWorkerFor(done, name, 5*time.Second, stderr)
}

func waitDaemonWorkerFor(done <-chan struct{}, name string, timeout time.Duration, stderr io.Writer) {
	if done == nil {
		return
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		if stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: %s did not stop before shutdown; closing its cluster resources\n", name)
		}
	}
}

// registerClusterNodeWithRetry keeps an existing Raft member alive while a
// restarted quorum elects a leader and waits out execution leases granted in
// the prior term. Exiting during this interval can collapse the quorum before
// any member is able to re-register and expose its control endpoint.
// holdsFencedRuntimes reports whether Sentries of this node's local root are
// stopped by an execution fence, waiting for the cluster to authorize them
// again.
func holdsFencedRuntimes(localRoot string) bool {
	_, err := os.Stat(elasticFencePath(localRoot))
	return err == nil
}

// clusterWaitReport is how often a daemon waiting for its cluster says so.
const clusterWaitReport = 30 * time.Second

// registerClusterNodeWithRetry registers record, retrying for a bounded time,
// or for as long as it takes if wait is set: a daemon whose Sentries a fence
// stopped must not end while they wait, however long the cluster is away.
func registerClusterNodeWithRetry(directory cluster.NodeCatalog, record cluster.NodeRecord, lease time.Duration, recoverMembership func(context.Context) error, wait bool, stderr io.Writer) (*cluster.NodeRecord, error) {
	if directory == nil {
		return nil, errors.New("cluster node directory is unavailable")
	}
	timeout := 2*lease + 30*time.Second
	if timeout < time.Minute {
		timeout = time.Minute
	}
	deadline := time.Now().Add(timeout)
	report := time.Now().Add(clusterWaitReport)
	var lastErr error
	for {
		registered, err := directory.Register(record, time.Now(), lease)
		if err == nil {
			return registered, nil
		}
		lastErr = err
		if !wait && !time.Now().Before(deadline) {
			return nil, lastErr
		}
		if wait && !time.Now().Before(report) {
			fmt.Fprintf(stderr, "ttcd: waiting for the cluster, with this node's sandboxes stopped: %v\n", lastErr)
			report = time.Now().Add(clusterWaitReport)
		}
		// An offline server may have been removed from the live Raft
		// configuration while this daemon was absent. Re-adding the same ID and
		// address is idempotent for an existing voter and revives a removed
		// member as a learner. The cluster HTTP listener is already serving, so
		// simultaneous restarts cannot deadlock on these recovery attempts.
		if recoverMembership != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = recoverMembership(ctx)
			cancel()
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func retireDaemonRaftMember(runtime *daemonClusterRuntime, nodeID, incarnation string) (bool, error) {
	if runtime == nil || runtime.raft == nil || runtime.source == nil {
		return false, errors.New("raft retirement is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	record, err := runtime.directory.Lookup(nodeID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("reading retiring node authority: %w", err)
	}
	if err == nil {
		if record.Incarnation != incarnation || record.State != cluster.NodeOffline {
			return false, errors.New("node must commit its exact incarnation offline before identity retirement")
		}
		if _, err := runtime.raftCatalog.RemoveNode(nodeID, incarnation, record.Revision); err != nil {
			return false, fmt.Errorf("removing retired node identity: %w", err)
		}
	}
	var lastErr error
	for {
		leaderID := runtime.raft.LeaderID()
		var err error
		if leaderID == "" || leaderID == nodeID {
			err = runtime.raft.RetireMember(ctx, nodeID)
		} else {
			peer, peerErr := runtime.source.peer(leaderID, false)
			if peerErr != nil {
				err = peerErr
			} else {
				err = peer.RetireAuthority(ctx, cluster.AuthorityRetireRequest{NodeID: nodeID, Incarnation: incarnation})
			}
		}
		if err == nil {
			return true, nil
		}
		if strings.Contains(err.Error(), "refusing to retire the last Raft voter") {
			// The final voter is intentionally retained: its local Raft state is
			// the restart point for a fully stopped cluster.
			return false, nil
		}
		var redirect *cluster.AuthorityNotLeaderError
		if errors.As(err, &redirect) && redirect.LeaderID != "" {
			peer, peerErr := runtime.source.peer(redirect.LeaderID, false)
			if peerErr == nil {
				err = peer.RetireAuthority(ctx, cluster.AuthorityRetireRequest{NodeID: nodeID, Incarnation: incarnation})
				if err == nil {
					return true, nil
				}
				if strings.Contains(err.Error(), "refusing to retire the last Raft voter") {
					return false, nil
				}
			} else {
				err = peerErr
			}
		}
		// A re-admitted member's local LeaderID may transiently name the leader
		// from its stale pre-removal log. Do not make graceful shutdown wait for
		// the full two-minute retirement deadline in that case. Ask every
		// currently discoverable peer; only the leader will perform the removal,
		// and RetireMember is idempotent once this server has disappeared from the
		// configuration.
		records, listErr := runtime.directory.List()
		if listErr == nil {
			sort.Slice(records, func(i, j int) bool { return records[i].NodeID < records[j].NodeID })
			for _, record := range records {
				if record.NodeID == nodeID || record.State == cluster.NodeOffline {
					continue
				}
				peer, peerErr := runtime.source.peer(record.NodeID, false)
				if peerErr != nil {
					err = peerErr
					continue
				}
				peerErr = peer.RetireAuthority(ctx, cluster.AuthorityRetireRequest{NodeID: nodeID, Incarnation: incarnation})
				if peerErr == nil {
					return true, nil
				}
				if strings.Contains(peerErr.Error(), "refusing to retire the last Raft voter") {
					return false, nil
				}
				err = peerErr
			}
		}
		lastErr = err
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func maintainRepositories(dataRoot, localRoot, runtimeRoot, runsc string, publications *ttcstorage.Queue, transactions cluster.TransactionCatalog, authority *cluster.RaftCatalog, snapshotGraph *ttcsnapshot.Store, snapshotData *ttcsnapshot.LocalDataStore, standalone bool, stderr io.Writer, done <-chan struct{}, lock func() func(), busy func() bool) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	consumption := time.NewTicker(30 * time.Second)
	defer consumption.Stop()
	for {
		full := false
		select {
		case <-done:
			return
		case <-timer.C:
			full = true
		case <-consumption.C:
		}
		unlock := func() {}
		if lock != nil {
			unlock = lock()
		}
		if busy != nil && busy() {
			// A long operation is working without the lifecycle lock, and
			// what this pass repairs - interrupted removals, unreachable
			// data, construction pins - may be its work in progress.
			unlock()
			if full {
				timer.Reset(time.Minute)
			}
			continue
		}
		if full {
			store := ttcbundle.Store{Root: dataRoot, ActiveRoot: localRoot}
			if _, err := store.RecoverContainerRemovals(); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: recovering removed containers: %v\n", err)
			}
			if _, err := store.GarbageCollectRepositories(); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: collecting unreachable volume data: %v\n", err)
			}
			if _, err := store.PruneImageRoots(context.Background()); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: pruning node-local image cache: %v\n", err)
			}
			if _, _, err := store.PruneSharedImagePacks(context.Background()); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: pruning shared image packs: %v\n", err)
			}
			var authorityState *cluster.AuthorityState
			var authorityErr error
			if authority != nil {
				authorityState, authorityErr = authority.AuthorityState()
			}
			if authorityErr != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: reading authoritative cold roots: %v\n", authorityErr)
			} else if err := collectColdStorage(dataRoot, localRoot, publications, transactions, authorityState); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: collecting cold packs: %v\n", err)
			}
		}
		if standalone {
			backend := &runtimeBackend{root: runtimeRoot, runsc: runsc, dataRoot: dataRoot, localRoot: localRoot, snapshotGraph: snapshotGraph, snapshotData: snapshotData, stderr: stderr}
			if full {
				if err := backend.reconcileStandaloneMemoryGraph(); err != nil && stderr != nil {
					fmt.Fprintf(stderr, "ttcd: warning: reconciling standalone memory graph: %v\n", err)
				}
			}
			if err := backend.reconcileConsumedMemoryBacking(); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: reconciling consumed standalone memory backing: %v\n", err)
			}
		}
		unlock()
		if full && !standalone {
			// Immutable image-cache eviction has its own image-root and
			// gofer-lifetime locks. Keep its file fsyncs outside the lifecycle
			// critical section so cache pressure cannot stall ps/exec/fork.
			store := ttcbundle.Store{Root: dataRoot, ActiveRoot: localRoot}
			target, err := ttcbundle.DefaultImageCacheTarget(localRoot)
			if err == nil {
				_, err = store.PruneImageCache(target)
			}
			if err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: enforcing node-local image cache capacity: %v\n", err)
			}
		}
		if full {
			timer.Reset(repositoryMaintenanceInterval)
		}
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
	// Stop is deliberately a minimal, stable escape hatch. In particular, a
	// newly installed client must be able to stop an older daemon before
	// starting its matching sibling. Peer-UID validation above still applies.
	if request.Action != "stop" && request.Version != daemonAPIVersion {
		writeError(fmt.Errorf("unsupported API version %d (server uses %d)", request.Version, daemonAPIVersion))
		return
	}
	if request.Action != "stop" && request.ClientBuildID != "" && request.ClientBuildID != version.Version() {
		writeError(fmt.Errorf("client build %q does not match daemon build %q; only 'ttc daemon stop' is allowed across versions", request.ClientBuildID, version.Version()))
		return
	}
	var preparedIngress *preparedRunIngress
	ingressCommitted := false
	defer func() {
		if preparedIngress != nil && !ingressCommitted {
			s.portIngress.rollbackPrepared(preparedIngress)
		}
	}()
	if request.Action == "command" {
		cleanArgs, placementNode, err := extractPlacementNode(request.Args)
		if err != nil {
			writeError(err)
			return
		}
		request.Args = cleanArgs
		if placementNode != "" {
			if request.PlacementNode != "" && request.PlacementNode != placementNode {
				writeError(fmt.Errorf("routed placement changed from node %q to %q", request.PlacementNode, placementNode))
				return
			}
			request.PlacementNode = placementNode
		}
		if s.portIngress.Clustered() && !request.IngressPrepared {
			cleanArgs, prepared, err := s.portIngress.prepareRun(request.Args)
			if err != nil {
				writeError(err)
				return
			}
			request.Args = cleanArgs
			preparedIngress = prepared
			request.IngressPrepared = prepared != nil
		}
	}
	switch request.Action {
	case "ping":
		status := map[string]any{
			"version":                  daemonAPIVersion,
			"build_id":                 version.Version(),
			"features":                 daemonAPIFeatures,
			"pid":                      os.Getpid(),
			"runtime_platform":         s.runtimePlatform,
			"cpu_profile":              s.cpuProfileName,
			"cpu_features":             strings.Split(s.cpuFeatures, ","),
			"isolation_mode":           daemonIsolationMode(s.unsafeNoHostIsolation),
			"unsafe_no_host_isolation": s.unsafeNoHostIsolation,
			"host_kernel_release":      s.hostKernelRelease,
			"host_gso_enabled":         !s.disableHostGSO,
			"data_root":                s.dataRoot,
			"shared_root":              s.dataRoot,
			"local_root":               s.localRoot,
			"volume_copy_mode":         s.volumeCapability.Mode,
			"volume_filesystem_type":   s.volumeCapability.FilesystemType,
			"volume_device":            s.volumeCapability.Device,
		}
		if s.clusterCapabilities != nil {
			clusterStatus := map[string]any{
				"listen": s.clusterListen, "capabilities": s.clusterCapabilities,
			}
			if s.clusterDirectory != nil {
				if nodes, err := s.clusterDirectory.List(); err == nil {
					clusterStatus["nodes"] = nodes
				} else {
					clusterStatus["directory_error"] = err.Error()
				}
			}
			status["cluster"] = clusterStatus
		}
		payload, _ := json.Marshal(status)
		_ = writeDaemonFrame(conn, &writeMu, daemonFrameStdout, append(payload, '\n'))
		_ = writeDaemonStatus(conn, &writeMu, 0)
		return
	case "stop":
		if request.ManageStop {
			opts := s.optionsFromRequest()
			backend := directBackend(opts, strings.NewReader(""), io.Discard,
				daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
			s.configureCommandBackend(backend)
			unlock := s.lockDataRoot(opts.dataRoot)
			var err error
			if s.clusterCapabilities != nil {
				// A distributed daemon stop is also a node retirement. Reuse the
				// node-stop state machine so the node is cordoned, all authoritative
				// state and peer dependencies are drained, its offline record is
				// committed, and deferred shutdown can transfer leadership and
				// remove the retiring Raft member. Merely applying the branch policy
				// here would leave a cleanly stopped voter in the quorum and strand a
				// surviving two-node cluster.
				mode := ""
				switch {
				case slices.Contains(request.Args, "--migrate-all"):
					mode = "migrate"
				case slices.Contains(request.Args, "--dehydrate-all"):
					mode = "dehydrate"
				case slices.Contains(request.Args, "--stop-all"):
					mode = "stop"
				}
				_, err = backend.stopNode(s.clusterCapabilities.NodeID, mode, true)
			} else {
				switch {
				case slices.Contains(request.Args, "--migrate-all"):
					err = errors.New("--migrate-all requires distributed mode")
				case slices.Contains(request.Args, "--dehydrate-all"):
					err = dehydrateDaemonContainers(backend)
				case slices.Contains(request.Args, "--stop-all"):
					err = stopDaemonContainers(backend)
				default:
					err = rejectActiveDaemonContainers(backend)
				}
			}
			unlock()
			if err != nil {
				writeError(fmt.Errorf("stopping managed containers: %w", err))
				return
			}
			if s.publicationQueue != nil {
				if err := s.publicationQueue.Drain(context.Background()); err != nil {
					writeError(fmt.Errorf("draining durable publications: %w", err))
					return
				}
			}
			if err := s.retireBeforeReply(); err != nil {
				writeError(fmt.Errorf("retiring cluster node: %w", err))
				return
			}
		}
		_ = writeDaemonStatus(conn, &writeMu, 0)
		s.stopAfterReply(conn)
		return
	case "mutation-control":
		status := s.handleMutationControl(s.mutationRoot(), request.Args, daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStdout}, daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
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
	if s.distributedSource != nil && s.clusterCapabilities != nil && !request.DistributedLeaf {
		handled, status, stdout, stderr, err := s.executeDistributedStats(request)
		if err != nil {
			writeError(err)
			return
		}
		if handled {
			writeDaemonBufferedResult(conn, &writeMu, stdout, stderr, status)
			return
		}
	}
	if s.distributedSource != nil && s.clusterCapabilities != nil && !request.DistributedLeaf {
		handled, status, stdout, stderr, err := s.executeDistributedFamilyMutation(request)
		if err != nil {
			writeError(err)
			return
		}
		if handled {
			writeDaemonBufferedResult(conn, &writeMu, stdout, stderr, status)
			return
		}
	}
	if s.distributedSource != nil && s.clusterCapabilities != nil {
		owner := ""
		automaticPlacement := false
		failedAdmissionNodes := make(map[string]struct{})
		var err error
		if len(request.PlacementClaims) == 0 {
			owner, request.PlacementClaims, err = daemonCommandPlacementWithCatalog(s.dataRoot, s.clusterPlacements, request.Args)
		} else {
			for _, claim := range request.PlacementClaims {
				if owner != "" && owner != claim.OwnerNode {
					err = fmt.Errorf("routed command claims span cluster nodes %q and %q", owner, claim.OwnerNode)
					break
				}
				owner = claim.OwnerNode
			}
		}
		if err != nil {
			writeError(err)
			return
		}
		if owner != "" && daemonCommandMayAwaitColdUnplacement(s.dataRoot, s.clusterPlacements, request.Args) {
			// Dehydrate/stop return after the process-free local capture is safe,
			// while publication to shared storage is intentionally asynchronous.
			// Wait outside every lifecycle lock for its atomic Raft commit, then
			// redo routing so the destination can claim the unplaced branch. This
			// is required even when the requested destination is the former owner:
			// otherwise start can carry a stale running-placement claim across the
			// publication commit and fail at the destination as soon as Raft
			// unplaces the branch.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			owner, request.PlacementClaims, err = awaitDaemonColdUnplacement(ctx, s.dataRoot, s.clusterPlacements, request.Args)
			cancel()
			if err != nil {
				writeError(err)
				return
			}
		}
		if owner != "" && request.PlacementNode != "" && owner != request.PlacementNode {
			operands, processFree, relocationErr := processFreeStartRelocationOperands(s.dataRoot, s.clusterPlacements, request.Args)
			if relocationErr != nil {
				writeError(fmt.Errorf("checking process-free placement relocation: %w", relocationErr))
				return
			}
			if !processFree {
				writeError(fmt.Errorf("branch is already placed on node %q, not requested node %q", owner, request.PlacementNode))
				return
			}
			stopArgs := append([]string{"stop"}, operands...)
			var status int
			var diagnostic string
			if owner == s.clusterCapabilities.NodeID {
				// Routing reaches the current owner before it can honor a
				// conflicting placement request. Re-enter through the local Unix
				// protocol so the ordinary mutation journal and lifecycle lock own
				// publication; the cluster peer service intentionally rejects a
				// node dialing itself.
				var stdout, stderr bytes.Buffer
				status, err = runViaDaemon(options{daemonSocket: s.socket}, stopArgs, strings.NewReader(""), &stdout, &stderr)
				diagnostic = stderr.String()
			} else {
				peer, peerErr := s.distributedSource.peer(owner, false)
				if peerErr != nil {
					writeError(fmt.Errorf("connecting to process-free branch owner %q: %w", owner, peerErr))
					return
				}
				publicationRequestID, idErr := newDaemonRequestID()
				if idErr != nil {
					writeError(idErr)
					return
				}
				status, _, diagnostic, err = executeRemoteDaemonCommand(context.Background(), peer, publicationRequestID, stopArgs)
			}
			if err != nil || status != 0 {
				writeError(errors.Join(err, fmt.Errorf("publishing process-free branch on node %q failed with status %d: %s", owner, status, strings.TrimSpace(diagnostic))))
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			owner, request.PlacementClaims, err = awaitDaemonColdUnplacement(ctx, s.dataRoot, s.clusterPlacements, request.Args)
			cancel()
			if err != nil {
				writeError(fmt.Errorf("waiting to relocate process-free branch: %w", err))
				return
			}
		}
		if owner == "" && daemonCommandNeedsPlacementAdmission(request.Args) {
			if request.PlacementNode != "" {
				owner = request.PlacementNode
			} else {
				records, listErr := s.clusterDirectory.List()
				if listErr != nil {
					writeError(fmt.Errorf("selecting cluster placement: %w", listErr))
					return
				}
				selected, selectErr := selectAdmissionNode(records, *s.clusterCapabilities, time.Now())
				if selectErr != nil {
					writeError(selectErr)
					return
				}
				owner = selected.NodeID
				request.PlacementNode = owner
				automaticPlacement = true
			}
		}
		for owner != "" && owner != s.clusterCapabilities.NodeID {
			if slices.Contains(request.Route, owner) || slices.Contains(request.Route, s.clusterCapabilities.NodeID) {
				writeError(fmt.Errorf("cluster command routing loop through %v to node %q", request.Route, owner))
				return
			}
			peer, err := s.distributedSource.peer(owner, false)
			if err != nil {
				writeError(err)
				return
			}
			proxiedRequest := request
			proxiedRequest.Route = append(append([]string(nil), request.Route...), s.clusterCapabilities.NodeID)
			copyRoute, copyCommand, err := parseDistributedCopyRoute(proxiedRequest.Args)
			if err != nil {
				writeError(err)
				return
			}
			if copyCommand && copyRoute.sourceContainer && copyRoute.destination != "-" {
				if err := proxyDistributedCopyDownload(context.Background(), peer, s.localRoot, proxiedRequest, copyRoute, conn, &writeMu); err != nil {
					writeError(fmt.Errorf("proxying distributed cp from node %q: %w", owner, err))
				}
				return
			}
			var input io.Reader
			var copyUploadFlow *daemonStdinFlow
			var cleanupCopyUpload func()
			if copyCommand && !copyRoute.sourceContainer && copyRoute.source != "-" {
				proxiedRequest, input, copyUploadFlow, cleanupCopyUpload, err = prepareDistributedCopyUpload(s.localRoot, proxiedRequest, copyRoute)
				if err != nil {
					writeError(err)
					return
				}
				defer cleanupCopyUpload()
			}
			encoded, err := json.Marshal(&proxiedRequest)
			if err != nil {
				writeError(err)
				return
			}
			if input == nil {
				input = bytes.NewReader(append(encoded, '\n'))
			}
			if request.streamsClientFrames() && cleanupCopyUpload == nil {
				input = io.MultiReader(input, reader)
			}
			if copyUploadFlow != nil {
				if err := proxyDistributedCopyUpload(context.Background(), peer, input, copyUploadFlow, conn, &writeMu); err == nil {
					return
				} else {
					writeError(fmt.Errorf("proxying command to node %q: %w", owner, err))
					return
				}
			}
			output := io.Writer(conn)
			var tracked *daemonStatusWriter
			if preparedIngress != nil {
				tracked = &daemonStatusWriter{writer: conn}
				output = tracked
			}
			if err := peer.ProxyDaemon(context.Background(), input, output); err == nil {
				if tracked != nil {
					ingressCommitted = tracked.statusSeen && tracked.status == 0
				}
				return
			} else if !automaticPlacement || request.Stdin || !errors.Is(err, syscall.ECONNREFUSED) {
				writeError(fmt.Errorf("proxying command to node %q: %w", owner, err))
				return
			}

			// A refused connection proves that no request reached this
			// automatically selected peer. The branch is still unplaced, so retry
			// admission without waiting for the peer lease to expire. Explicit
			// placement and streaming input are never replayed.
			failedAdmissionNodes[owner] = struct{}{}
			records, listErr := s.clusterDirectory.List()
			if listErr != nil {
				writeError(fmt.Errorf("reselecting cluster placement after node %q refused the connection: %w", owner, listErr))
				return
			}
			selected, selectErr := selectAdmissionNodeExcluding(records, *s.clusterCapabilities, time.Now(), failedAdmissionNodes)
			if selectErr != nil {
				writeError(fmt.Errorf("reselecting cluster placement after node %q refused the connection: %w", owner, selectErr))
				return
			}
			owner = selected.NodeID
			request.PlacementNode = owner
		}
	}
	if s.clusterDirectory != nil && daemonCommandNeedsPlacementAdmission(request.Args) {
		record, err := s.clusterDirectory.Lookup(s.clusterCapabilities.NodeID)
		if err != nil {
			writeError(fmt.Errorf("checking cluster admission: %w", err))
			return
		}
		if !record.AcceptsPlacement(time.Now()) {
			writeError(fmt.Errorf("cluster node %q is not accepting new work (state=%s)", record.NodeID, record.State))
			return
		}
	}

	stdinReader, stdinWriter := io.Pipe()
	if !request.Stdin {
		stdinWriter.Close()
	}
	terminalInput := newDaemonTerminalInput(stdinReader, conn, &writeMu)
	go receiveDaemonInput(reader, stdinWriter, request.Stdin, terminalInput)

	opts := s.optionsFromRequest()
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
	s.configureCommandBackend(backend)
	backend.clientTERM = request.ClientTERM
	backend.progressTerminal = request.StderrTerminal
	backend.progressWidth = request.StderrWidth
	clientCanceled := make(chan struct{})
	var cancelClientOnce sync.Once
	backend.clientCanceled = clientCanceled
	backend.cancelClient = func() {
		cancelClientOnce.Do(func() {
			close(clientCanceled)
			_ = conn.Close()
		})
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
	if len(request.Args) >= 2 && request.Args[0] == "tty" && request.Args[1] == "watch" {
		backend.stdout = &daemonJSONFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameTTYState}
		status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
		stdinReader.Close()
		_ = writeDaemonStatus(conn, &writeMu, status)
		return
	}
	holdsLifecycleLock := daemonCommandNeedsMutationLock(request.Args) || daemonCommandReadsLifecycle(request.Args)
	var rawUnlock func()
	if daemonCommandListsInventory(request.Args) {
		// A listing must not wait for a mutation that takes minutes, such
		// as a durable dehydration. If one holds the lock, list without it:
		// read the records and ask the runtimes, but leave reconciliation,
		// and any repair it would make, to a command that holds the lock.
		var locked bool
		if rawUnlock, locked = s.tryLockDataRoot(opts.dataRoot); !locked {
			holdsLifecycleLock = false
			backend.inventoryReadOnly = true
		}
	} else {
		rawUnlock = s.lockMutation(opts.dataRoot, request.Args)
	}
	backend.lifecycleLockHeld = holdsLifecycleLock
	backend.mutating = daemonCommandNeedsMutationLock(request.Args)
	backend.commandName = daemonCommandName(request.Args)
	hold := &lifecycleHold{acquire: func() func() { return s.lockDataRoot(opts.dataRoot) }, release: rawUnlock}
	unlock := hold.unlock
	if daemonCommandReleasesLifecycleAfterStart(request.Args) {
		backend.lifecycleReady = unlock
	}
	if !daemonCommandKeepsLifecycleLock(request.Args) {
		s.configureLongOperations(backend, hold)
	}
	// A retry after a lost response must consult the durable result before
	// mutable-state validation. The committed command may have removed its own
	// runtime or Raft placement, in which case revalidating the original claim
	// would turn a successful idempotent retry into a false stale-route error.
	if request.RequestID != "" && daemonCommandNeedsMutationJournal(request.Args) {
		record, replay, err := replayCompletedDaemonMutation(s.mutationRoot(), request)
		if err != nil {
			unlock()
			stdinReader.Close()
			writeError(err)
			return
		}
		if replay {
			unlock()
			stdinReader.Close()
			ingressCommitted = preparedIngress != nil && record.Status == 0
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
			return
		}
	}
	// Reconciliation lists runsc state and may reap runtimes that have lost
	// their Sentry. Keep it in the same lifecycle critical section as the
	// command: otherwise a read-only `ttc ps` can observe `runsc create` after
	// its state file is published but before its control server is ready and
	// mistake the in-progress runtime for stale state.
	if !backend.inventoryReadOnly {
		s.reconcile(backend)
	}
	if operation, busy := s.operations.target(request.Args); busy {
		unlock()
		stdinReader.Close()
		writeError(operation.refusal())
		return
	}
	placements, placementErr := backend.placementCatalog()
	if placementErr != nil && len(request.PlacementClaims) != 0 {
		unlock()
		stdinReader.Close()
		writeError(placementErr)
		return
	}
	if err := validateDaemonPlacementClaimsWithCatalog(opts.dataRoot, backend.nodeID, request.Args, request.PlacementClaims, placements); err != nil {
		unlock()
		stdinReader.Close()
		writeError(err)
		return
	}
	if err := s.claimUnplacedLifecycleTargets(backend, request.Args); err != nil {
		unlock()
		stdinReader.Close()
		writeError(err)
		return
	}
	if request.RequestID != "" && daemonCommandNeedsMutationJournal(request.Args) {
		mutationRoot := s.mutationRoot()
		record, replay, err := beginDaemonMutation(mutationRoot, request)
		if err != nil {
			unlock()
			stdinReader.Close()
			writeError(err)
			return
		}
		if replay {
			unlock()
			stdinReader.Close()
			ingressCommitted = preparedIngress != nil && record.Status == 0
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
			return
		}
		var mutationStdout, mutationStderr bytes.Buffer
		mutationBackend := directBackend(opts, stdinReader, &mutationStdout, &mutationStderr)
		s.configureVolumeBackend(mutationBackend)
		mutationBackend.lifecycleLockHeld = holdsLifecycleLock
		mutationBackend.mutating = backend.mutating
		mutationBackend.commandName = backend.commandName
		if !daemonCommandKeepsLifecycleLock(request.Args) {
			s.configureLongOperations(mutationBackend, hold)
		}
		mutationBackend.consoleSidecars = s
		mutationBackend.distributedSource = s.distributedSource
		if s.clusterCapabilities != nil {
			mutationBackend.nodeID = s.clusterCapabilities.NodeID
		}
		status := runCommand(request.Args, mutationBackend.stdout, mutationBackend.stderr, mutationBackend)
		s.recordCommandEvents(opts.dataRoot, request.Args, status)
		if status == 0 {
			// Publication runs outside the lifecycle lock. It may unplace a
			// dehydration after the pre-command claim but before rehydrate
			// consumes the durable image. Repeat the idempotent claim at the
			// successful execution boundary before catalog reconciliation.
			if err := s.claimUnplacedLifecycleTargets(mutationBackend, request.Args); err != nil {
				fmt.Fprintf(&mutationStderr, "ttcd: claiming completed rehydration placement: %v\n", err)
				status = 1
			} else if daemonCommandNeedsMutationLock(request.Args) {
				if err := mutationBackend.reconcilePlacementCatalog(); err != nil {
					fmt.Fprintf(&mutationStderr, "ttcd: warning: reconciling placement catalog: %v\n", err)
				}
			}
		}
		if status == 0 && mutationBackend.daemonStopRequested {
			if err := s.retireBeforeReply(); err != nil {
				fmt.Fprintf(&mutationStderr, "ttcd: retiring cluster node: %v\n", err)
				status = 1
			}
		}
		completed, err := completeDaemonMutation(mutationRoot, record, status, mutationStdout.Bytes(), mutationStderr.Bytes())
		if err != nil {
			fmt.Fprintf(&mutationStderr, "ttcd: %v; command outcome is indeterminate\n", err)
			status = 1
		} else {
			record = completed
			s.maybeCompactDaemonMutations(mutationRoot)
		}
		unlock()
		stdinReader.Close()
		if err == nil {
			ingressCommitted = preparedIngress != nil && record.Status == 0
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
		} else {
			writeDaemonBufferedResult(conn, &writeMu, mutationStdout.Bytes(), mutationStderr.Bytes(), status)
		}
		if status == 0 && mutationBackend.daemonStopRequested {
			s.stopAfterReply(conn)
		}
		return
	}
	status := runCommand(request.Args, backend.stdout, backend.stderr, backend)
	s.recordCommandEvents(opts.dataRoot, request.Args, status)
	if status == 0 {
		if err := s.claimUnplacedLifecycleTargets(backend, request.Args); err != nil {
			fmt.Fprintf(backend.stderr, "ttcd: claiming completed rehydration placement: %v\n", err)
			status = 1
		} else if daemonCommandNeedsMutationLock(request.Args) {
			if err := backend.reconcilePlacementCatalog(); err != nil {
				fmt.Fprintf(backend.stderr, "ttcd: warning: reconciling placement catalog: %v\n", err)
			}
		}
	}
	if status == 0 && backend.daemonStopRequested {
		if err := s.retireBeforeReply(); err != nil {
			fmt.Fprintf(backend.stderr, "ttcd: retiring cluster node: %v\n", err)
			status = 1
		}
	}
	unlock()
	stdinReader.Close()
	ingressCommitted = preparedIngress != nil && status == 0
	_ = writeDaemonStatus(conn, &writeMu, status)
	if status == 0 && backend.daemonStopRequested {
		s.stopAfterReply(conn)
	}
}

func (s *daemonServer) maybeCompactDaemonMutations(dataRoot string) {
	if s == nil || s.mutationCount.Add(1)%daemonMutationCompactionInterval != 0 {
		return
	}
	_ = compactDaemonMutations(dataRoot, daemonMutationRetention)
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
	case "create", "pull", "load", "save", "commit", "rmi", "rm", "_auto-remove-now", "_purge-runtime", "_remove-family-record", "pause", "resume", "unpause", "quarantine", "unquarantine", "dehydrate", "rehydrate", "kill", "stop", "restart", "fork", "checkpoint", "restore", "migrate":
		return true
	case "node":
		return len(args) > 1 && args[1] != "ls" && args[1] != "_planner-resources"
	case "cp":
		_, _, _, err := parseCopyCommandArgs(args[1:])
		return err == nil
	case "run":
		return daemonCommandBoolFlag(args[1:], 'd', "--detach")
	case "start":
		return !daemonCommandBoolFlag(args[1:], 'a', "--attach")
	case "image":
		return len(args) > 1 && (args[1] == "pull" || args[1] == "load" || args[1] == "save" || args[1] == "rm" || args[1] == "remove")
	case "port":
		return len(args) > 1 && (args[1] == "publish" || args[1] == "unpublish")
	default:
		return false
	}
}

// daemonCommandListsInventory identifies the lifecycle readers that only list
// and describe: they may run without the lifecycle lock, as long as they make
// no repairs. `ps --size` is not one of them: sizing opens repositories.
func daemonCommandListsInventory(args []string) bool {
	if len(args) != 0 && args[0] == "container" {
		args = args[1:]
	}
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "ls", "inspect", "top":
		return true
	case "ps":
		return !daemonCommandBoolFlag(args[1:], 's', "--size")
	default:
		return false
	}
}

// daemonCommandReadsLifecycle reports commands whose runtime inventory must be
// a coherent snapshot. They do not enter the mutation journal, but must not
// probe or reconcile runsc state while create/delete or another lifecycle
// mutation is only partially complete.
func daemonCommandReadsLifecycle(args []string) bool {
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
	case "ps", "ls", "inspect", "top", "stats", "diff":
		return true
	default:
		return daemonCommandReleasesLifecycleAfterStart(args)
	}
}

// daemonCommandReleasesLifecycleAfterStart identifies attached lifecycle
// commands. They need exclusion only through runtime publication; holding the
// lock while the client remains attached would block the daemon for the whole
// lifetime of the guest process.
func daemonCommandReleasesLifecycleAfterStart(args []string) bool {
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
	case "run":
		return !daemonCommandBoolFlag(args[1:], 'd', "--detach")
	case "start":
		return daemonCommandBoolFlag(args[1:], 'a', "--attach")
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
	// A load consumes a stream the client holds. Replaying it would need the
	// archive a second time, and a pipe cannot supply one.
	if len(args) != 0 && args[0] == "load" {
		return false
	}
	if len(args) > 1 && args[0] == "image" && args[1] == "load" {
		return false
	}
	// Save is read-only but holds the lifecycle lock so image replacement and
	// garbage collection cannot race the archive traversal. Commit reads live
	// branch state and may temporarily pause it. Neither operation is suitable
	// for replay from a buffered mutation record.
	if len(args) != 0 && (args[0] == "save" || args[0] == "commit") {
		return false
	}
	if len(args) > 1 && args[0] == "image" && args[1] == "save" {
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
	if !daemonCommandNeedsMutationLock(args) && !daemonCommandReadsLifecycle(args) {
		return func() {}
	}
	return s.lockDataRoot(dataRoot)
}

func (s *daemonServer) lockDataRoot(dataRoot string) func() {
	lock := s.dataRootLock(dataRoot)
	lock.Lock()
	return lock.Unlock
}

// tryLockDataRoot is lockDataRoot for a caller that would rather go on
// without the lock than wait for it.
func (s *daemonServer) tryLockDataRoot(dataRoot string) (func(), bool) {
	lock := s.dataRootLock(dataRoot)
	if !lock.TryLock() {
		return func() {}, false
	}
	return lock.Unlock, true
}

func (s *daemonServer) dataRootLock(dataRoot string) *sync.Mutex {
	key := filepath.Clean(dataRoot)
	if absolute, err := filepath.Abs(key); err == nil {
		key = absolute
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if s.mutationLocks == nil {
		s.mutationLocks = make(map[string]*sync.Mutex)
	}
	lock := s.mutationLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.mutationLocks[key] = lock
	}
	return lock
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
			// The client is gone — not "done sending input", which is the EOF
			// frame below, but disconnected: a closed laptop, a dropped ssh
			// pipe, a process killed mid-turn.
			//
			// Reported as a detach rather than as the read error it is,
			// because that is what the commands on the other side understand.
			// An attach told only that its stdin ended keeps running: it sits
			// in its drain loop waiting for output that an idle session never
			// produces, holding the terminal's stdin lock against everyone,
			// including the same client coming back. A tool with a window open
			// is silent for minutes at a time, and one lost connection made
			// its session unreachable for good.
			fail(errDetached)
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
	// A family a long operation is working on is left to it, and so are the
	// node-wide repairs, which could take a runtime it is building, or an
	// offline copy's helper, for a stale one; and the graph-wide repairs below,
	// which take any construction pin they find for a crashed operation's.
	// Reconcile again once it finishes.
	busy := s.operations.any()
	// A daemon may have died after branch ownership moved but before deleting
	// its old physical shell. Reclaim only runtimes and branches this node no
	// longer owns; shared metadata remains authoritative.
	if !busy {
		_ = backend.cleanupNonOwnedRuntimeBranches()
	}
	lifecycleReconciled, lifecycleErr := backend.reconcileInterruptedStandaloneLifecycles()
	if lifecycleErr != nil && backend.stderr != nil {
		fmt.Fprintf(backend.stderr, "ttcd: warning: reconciling interrupted standalone lifecycle: %v\n", lifecycleErr)
	}
	// listFamilies reconciles stale persisted running/paused state against runsc.
	families, err := backend.listFamilies()
	if err != nil {
		return
	}
	if err := backend.reconcileObservedInitExits(); err != nil {
		if backend.stderr != nil {
			fmt.Fprintf(backend.stderr, "ttcd: warning: reconciling completed init exits: %v\n", err)
		}
	}
	quarantineReconciled := lifecycleErr == nil && lifecycleReconciled && !busy
	for _, family := range families {
		if _, working := s.operations.lookup(family.ID); working {
			continue
		}
		container, containerErr := backend.containerStore().LoadContainer(family.ID)
		if containerErr != nil {
			quarantineReconciled = false
		}
		for _, branch := range family.Branches {
			if branch.State != "running" && branch.State != "paused" {
				continue
			}
			target := selector{container: family.ID, branch: branch.ID}
			_, _ = backend.ensureConsoleDrain(target, false)
			if containerErr == nil {
				if durable, branchErr := persistentBranchInfo(container, branch.ID); branchErr == nil {
					action := "unquarantine"
					if durable.Quarantined {
						action = "quarantine"
					}
					// Recover a daemon crash between changing the live link and
					// committing the durable policy bit. Kernel-side transitions are
					// idempotent, so durable metadata always wins.
					if _, err := backend.command("timetravel", family.ID, action, branch.ID); err != nil {
						quarantineReconciled = false
					}
				} else {
					quarantineReconciled = false
				}
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
	if busy {
		return
	}
	if err := backend.reconcileStandaloneMemoryGraph(); err != nil {
		quarantineReconciled = false
		if backend.stderr != nil {
			fmt.Fprintf(backend.stderr, "ttcd: warning: reconciling standalone memory graph: %v\n", err)
		}
	}
	if err := backend.reconcileConsumedMemoryBacking(); err != nil {
		quarantineReconciled = false
		if backend.stderr != nil {
			fmt.Fprintf(backend.stderr, "ttcd: warning: reconciling consumed standalone memory backing: %v\n", err)
		}
	}
	_ = backend.reconcilePlacementCatalog()
	if err := backend.reconcileRemovedMigrationSources(); err != nil {
		quarantineReconciled = false
		if backend.stderr != nil {
			fmt.Fprintf(backend.stderr, "ttcd: warning: reconciling removed migration sources: %v\n", err)
		}
	}
	if quarantineReconciled {
		s.reconciled[key] = true
	}
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

// imageLoadReadsStdin reports whether Docker-style load reads the client's
// standard input rather than a daemon-host file named by --input.
func imageLoadReadsStdin(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-i" || arg == "--input" || arg == "-input":
			return false
		case strings.HasPrefix(arg, "--input=") || strings.HasPrefix(arg, "-input="):
			return false
		case arg == "--platform" || arg == "-platform":
			i++
		}
	}
	return true
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
	case "load":
		return imageLoadReadsStdin(args[1:])
	case "image":
		return len(args) > 1 && args[1] == "load" && imageLoadReadsStdin(args[2:])
	case "restore":
		for _, arg := range args[1:] {
			if arg == "-" {
				return true
			}
		}
		return false
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

// daemonCommandUsesTerminalControl reports commands that may ask the client
// for its current terminal size. Terminal control frames must be routable even
// when -i is absent: detached TTY sessions are a normal automation primitive.
func daemonCommandUsesTerminalControl(args []string) bool {
	return protocol.CommandUsesTerminalControl(args)
}

func runViaDaemon(opts options, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	var err error
	args, err = normalizeDaemonImageArchivePaths(args)
	if err != nil {
		return 1, err
	}
	stderrTerminal, stderrWidth := terminalWriterInfo(stderr)
	request := daemonRequest{
		Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "command",
		Args: append([]string(nil), args...), Stdin: daemonCommandUsesStdin(args), ClientTERM: os.Getenv("TERM"),
		TerminalControl: daemonCommandUsesTerminalControl(args),
		StderrTerminal:  stderrTerminal, StderrWidth: stderrWidth,
	}
	if daemonCommandNeedsMutationJournal(args) {
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

// RunAutoRemoveClient is the daemon-owned helper used by detached --rm
// containers. It waits through the normal daemon command path without
// depending on, or executing, the lightweight ttc client binary.
func RunAutoRemoveClient(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[0] != "--socket" || args[1] == "" || args[2] == "" {
		fmt.Fprintln(stderr, "ttcd: usage: ttcd _auto-remove-client --socket PATH CONTAINER@BRANCH")
		return 2
	}
	opts := options{daemonSocket: args[1]}
	status, err := runViaDaemon(opts, []string{"wait", args[2]}, strings.NewReader(""), io.Discard, stderr)
	if err == nil && status == 0 {
		status, err = runViaDaemon(opts, []string{"_auto-remove-now", args[2]}, strings.NewReader(""), stdout, stderr)
	}
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: auto-remove helper: %v\n", err)
		return 1
	}
	return status
}

// normalizeDaemonImageArchivePaths gives load/save the same client-relative
// path behavior as Docker. The command itself runs in ttcd, whose working
// directory may be unrelated to the shell that invoked ttc.
func normalizeDaemonImageArchivePaths(args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	commandIndex := 0
	if args[0] == "image" {
		if len(args) < 2 {
			return args, nil
		}
		commandIndex = 1
	}
	var short, long string
	switch args[commandIndex] {
	case "load":
		short, long = "-i", "--input"
	case "save":
		short, long = "-o", "--output"
	default:
		return args, nil
	}
	result := append([]string(nil), args...)
	for i := commandIndex + 1; i < len(result); i++ {
		arg := result[i]
		if arg == short || arg == long || arg == "-"+strings.TrimPrefix(long, "--") {
			if i+1 >= len(result) {
				return result, nil // Let flag parsing produce the canonical error.
			}
			if result[i+1] == "" {
				return result, nil
			}
			absolute, err := filepath.Abs(result[i+1])
			if err != nil {
				return nil, fmt.Errorf("resolving image archive path: %w", err)
			}
			result[i+1] = absolute
			return result, nil
		}
		for _, prefix := range []string{short + "=", long + "=", "-" + strings.TrimPrefix(long, "--") + "="} {
			if !strings.HasPrefix(arg, prefix) {
				continue
			}
			path := strings.TrimPrefix(arg, prefix)
			if path == "" {
				return result, nil
			}
			absolute, err := filepath.Abs(path)
			if err != nil {
				return nil, fmt.Errorf("resolving image archive path: %w", err)
			}
			result[i] = prefix + absolute
			return result, nil
		}
	}
	return result, nil
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
		case daemonFrameTTYState:
			if _, err := stdout.Write(append(payload, '\n')); err != nil {
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

func writeDaemonQuorumWait(socket string) error {
	state := protocol.DaemonQuorumWait{PID: os.Getpid(), APIVersion: daemonAPIVersion, BuildID: version.Version()}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := protocol.DaemonQuorumWaitPath(socket)
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ttcd-quorum-wait-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func startDaemonProcess(socket, runtimePlatform string, unsafeNoHostIsolation bool, clusterOpts daemonClusterOptions) (string, <-chan error, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return "", nil, fmt.Errorf("creating daemon directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("finding ttc executable: %w", err)
	}
	commandName := filepath.Join(filepath.Dir(executable), "ttcd")
	info, err := os.Stat(commandName)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return "", nil, fmt.Errorf("matching executable ttcd was not found beside %s", executable)
	}
	output, err := exec.Command(commandName, "--version").Output()
	if err != nil {
		return "", nil, fmt.Errorf("checking ttcd version: %w", err)
	}
	wantVersion := "ttcd version " + version.Version()
	if strings.TrimSpace(string(output)) != wantVersion {
		return "", nil, fmt.Errorf("ttc build %q does not match %s", version.Version(), strings.TrimSpace(string(output)))
	}
	commandArgs := []string{"--socket", socket}
	if !clusterOpts.ControlOnly {
		commandArgs = append(commandArgs, "--platform", runtimePlatform, "--cpu-profile", clusterOpts.CPUProfileMode)
		if clusterOpts.CPUFeatures != "" {
			commandArgs = append(commandArgs, "--cpu-features", clusterOpts.CPUFeatures)
		}
		if unsafeNoHostIsolation {
			commandArgs = append(commandArgs, "--unsafe-no-host-isolation")
		}
	}
	if clusterOpts.Configured {
		if clusterOpts.ControlOnly {
			commandArgs = append(commandArgs, "--control-only")
			if clusterOpts.NodeID != "" {
				commandArgs = append(commandArgs, "--cluster-node-id", clusterOpts.NodeID)
			}
			for _, peer := range clusterOpts.Peers {
				commandArgs = append(commandArgs, "--cluster-peer", peer)
			}
		} else {
			commandArgs = append(commandArgs,
				"--cluster-listen", clusterOpts.Listen,
				"--cluster-node-id", clusterOpts.NodeID,
				"--cluster-repository-id", clusterOpts.RepositoryID,
				"--cluster-lease", clusterOpts.LeaseDuration.String(),
				"--cluster-max-transactions", strconv.Itoa(clusterOpts.MaxTransactions),
				"--cluster-page-listen", clusterOpts.PageListen,
				"--cluster-transfer-bytes", strconv.FormatUint(clusterOpts.PagingPolicy.TransferBytes, 10),
				"--cluster-max-outstanding-bytes", strconv.FormatUint(clusterOpts.PagingPolicy.MaxOutstandingBytes, 10),
				"--cluster-max-parallel", strconv.Itoa(clusterOpts.PagingPolicy.MaxConcurrency),
				"--cluster-ack-batch-pages", strconv.Itoa(clusterOpts.PagingPolicy.AckBatchPages),
				"--cluster-ack-interval", clusterOpts.PagingPolicy.AckInterval.String(),
				"--cluster-prefetch", string(clusterOpts.PagingPolicy.Prefetch),
				"--cluster-push", string(clusterOpts.PagingPolicy.Push),
			)
			if clusterOpts.Advertise != "" {
				commandArgs = append(commandArgs, "--cluster-advertise", clusterOpts.Advertise)
			}
			if clusterOpts.MemoryTarget != 0 {
				commandArgs = append(commandArgs, "--cluster-memory-target", strconv.FormatUint(clusterOpts.MemoryTarget, 10))
			}
			if clusterOpts.AutoRebalance {
				commandArgs = append(commandArgs, "--cluster-auto-rebalance")
			}
			if clusterOpts.RaftBootstrap {
				commandArgs = append(commandArgs, "--cluster-raft-bootstrap")
			}
			if clusterOpts.CapacityCommand != "" {
				commandArgs = append(commandArgs, "--cluster-capacity-command", clusterOpts.CapacityCommand)
			}
			if clusterOpts.CapacityWebhook != "" {
				commandArgs = append(commandArgs, "--cluster-capacity-webhook", clusterOpts.CapacityWebhook)
			}
			if clusterOpts.PageAdvertise != "" {
				commandArgs = append(commandArgs, "--cluster-page-advertise", clusterOpts.PageAdvertise)
			}
			for _, peer := range clusterOpts.Peers {
				commandArgs = append(commandArgs, "--cluster-peer", peer)
			}
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
	clusterOpts := defaultDaemonClusterOptions()
	if len(args) != 0 && args[0] == "start" {
		startArgs := args[1:]
		for len(startArgs) != 0 {
			switch {
			case startArgs[0] == "--platform":
				if len(startArgs) < 2 || startArgs[1] == "" {
					fmt.Fprintln(stderr, "ttc: --platform requires systrap or kvm")
					return 2
				}
				opts.runtimePlatform = startArgs[1]
				startArgs = startArgs[2:]
			case strings.HasPrefix(startArgs[0], "--platform="):
				opts.runtimePlatform = strings.TrimPrefix(startArgs[0], "--platform=")
				startArgs = startArgs[1:]
			case startArgs[0] == "--unsafe-no-host-isolation":
				opts.unsafeNoHostIsolation = true
				startArgs = startArgs[1:]
			default:
				if consumed, err := consumeDaemonCPUOption(startArgs, &clusterOpts); consumed != 0 || err != nil {
					if err != nil {
						fmt.Fprintf(stderr, "ttc: %v\n", err)
						return 2
					}
					startArgs = startArgs[consumed:]
					continue
				}
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
		runtimePlatform, err := normalizeRuntimePlatform(opts.runtimePlatform)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 2
		}
		opts.runtimePlatform = runtimePlatform
		if clusterOpts.ControlOnly && opts.unsafeNoHostIsolation {
			fmt.Fprintln(stderr, "ttc: --unsafe-no-host-isolation is not applicable to --control-only")
			return 2
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
		status, err := exchangeDaemon(conn, daemonRequest{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "mutation-control", Args: append([]string(nil), args...)}, strings.NewReader(""), stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		return status
	}
	stopMode := ""
	if len(args) == 2 && args[0] == "stop" && (args[1] == "--dehydrate-all" || args[1] == "--stop-all" || args[1] == "--migrate-all") {
		stopMode = args[1]
		args = args[:1]
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, "ttc: usage: ttc daemon start [--platform systrap|kvm] [isolation/volume/cluster options]|status|stop|requests|request inspect|request resolve")
		return 2
	}
	action := args[0]
	switch action {
	case "start":
		if conn, err := connectDaemon(opts.daemonSocket); err == nil {
			status, checkErr := exchangeDaemon(conn, daemonRequest{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: "ping"}, strings.NewReader(""), io.Discard, stderr)
			conn.Close()
			if checkErr != nil || status != 0 {
				if checkErr != nil {
					fmt.Fprintf(stderr, "ttc: checking running daemon: %v\n", checkErr)
				}
				return 1
			}
			fmt.Fprintln(stdout, "ttcd already running; use 'ttc daemon status' to inspect its mode")
			return 0
		}
		if opts.unsafeNoHostIsolation {
			fmt.Fprintln(stderr, "ttc: warning: host namespace and chroot isolation is disabled; run trusted workloads only")
		}
		logPath, processDone, err := startDaemonProcess(opts.daemonSocket, opts.runtimePlatform, opts.unsafeNoHostIsolation, clusterOpts)
		if err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
			return 1
		}
		deadline := time.Now().Add(daemonStartTimeout)
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
		request := daemonRequest{Version: daemonAPIVersion, ClientBuildID: version.Version(), Action: map[string]string{"status": "ping", "stop": "stop"}[action]}
		if action == "stop" {
			// Stop intentionally omits the build ID so it works across releases.
			request.ClientBuildID = ""
			request.ManageStop = true
			if stopMode != "" {
				request.Args = []string{stopMode}
			}
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
