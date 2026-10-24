// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"runtime/debug"
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
	"gvisor.dev/gvisor/pkg/cputopology"
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
	daemonAPIVersion   = protocol.DaemonAPIVersion
	daemonStartTimeout = 2 * time.Minute
	// A clustered TTC_ROOT is commonly hosted on NFS. A node that looked up the
	// root before the first member initialized it may retain a negative directory
	// entry for the server's acdirmax interval (commonly 60 seconds). Keep the
	// joining daemon alive long enough for that cache entry to expire instead of
	// turning a successful cluster initialization into a spurious join failure.
	clusterSharedRootVisibilityTimeout = 90 * time.Second
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
	// ArchiveFile says the client passed, with the header, the regular file
	// a checkpoint or an image save is written into, or a restore or an
	// image load reads, which it would otherwise stream.
	ArchiveFile bool `json:"archive_file,omitempty"`
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
	// AutomaticPlacement says the entry daemon chose PlacementNode itself.
	AutomaticPlacement bool `json:"automatic_placement,omitempty"`
	// Requester is who asked for the request, which the daemon its client
	// connected to says; see daemonRequester.
	Requester *daemonRequester `json:"requester,omitempty"`
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
		case "name", "pull", "workdir", "w", "user", "u", "hostname", "h", "network", "net", "ip", "mac-address", "interface-name", "publish", "p", "add-host", "dns", "dns-search", "dns-option", "dns-opt", "entrypoint", "stop-signal", "stop-timeout", "pids-limit", "memory", "m", "memory-swap", "cpus", "cpu-shares", "cpu-period", "cpu-quota", "shm-size", "group-add", "cap-add", "cap-drop", "ulimit", "env", "e", "env-file", "label", "l", "label-file", "volume", "v", "mount", "tmpfs", "log-driver", "log-opt":
			return true
		}
	}
	return false
}

type daemonServer struct {
	listener        net.Listener
	socket          string
	runtimePlatform string
	cpuProfileName  string
	cpuFeatures     string
	// cpuTopologyMode is the CPU topology this daemon's guests see, and
	// cpuTopology its annotation (see cpuTopologyAnnotation).
	cpuTopologyMode       string
	cpuTopology           string
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
	// noCPUIDFaulting is set in a cluster whose systrap nodes need no CPUID
	// faulting; they run systrap without syscall patching.
	noCPUIDFaulting bool
	// access says who may use the daemon besides its owner.
	access daemonAccess
	// routeHints has the node of each family that a command was last routed
	// to: see routingPlacements.
	routeHints routeHints

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
	mutationLocks map[string]*sync.RWMutex
	familyLocks   map[string]chan struct{}
	// namesTaken has, by data root, the names of the families being made
	// beside other commands: see familyMaking.
	namesTaken    map[string]struct{}
	mutationCount atomic.Uint64

	eventMu    sync.Mutex
	eventSeq   uint64
	eventLog   openEventLog
	eventSyncs eventLogSyncer

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

	// followers are connections to the families' console followers, by
	// console directory, and familyConsoles the consoles they follow whose
	// ends this daemon observes, by lock.
	followerMu     sync.Mutex
	followers      map[string]*familyFollowerClient
	followerStarts map[string]*sync.Mutex
	familyConsoles map[string]daemonFollowedConsole

	consoleClientMu      sync.Mutex
	consoleClients       map[string]map[*daemonConsoleClient]struct{}
	closingConsoleClient map[string]int
	// gfx starts GPU renderers for sandboxes of --gfx families.
	gfx *gfxService
	// nvproxy gives --nvproxy families the host's NVIDIA GPUs.
	nvproxy *nvproxyService
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
	Configured  bool
	ControlOnly bool
	// ControlHolder runs this daemon as a login node's control holder: it
	// holds the cluster's authority while no compute daemon runs, and stops
	// once one does; see controlHolder. It runs no container.
	ControlHolder   bool
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
	// Peers are a control-only daemon's first compute daemons, which it
	// finds in the shared root unless they are given.
	Peers []string
	// RepositoryGenerated is set when this daemon made RepositoryID up for a
	// shared root it found without one; another may have made it first.
	RepositoryGenerated bool
	PagingPolicy        ttcpolicy.Policy
	CPUProfileMode      string
	CPUFeatures         string
	CPUProfileSet       bool
	// NoCPUIDFaulting asks for a cluster whose systrap nodes need no CPUID
	// faulting (--experimental-no-cpuid-faulting). A node that does not ask
	// adopts what its cluster was created with.
	NoCPUIDFaulting bool
	// CPUTopology is the CPU topology --cpu-topology chose, single or host,
	// and CPUTopologySet whether it did: a daemon that does not choose shows
	// the single topology, or a cluster's node the cluster's.
	CPUTopology    string
	CPUTopologySet bool
	ResolvedCPU    daemonCPUProfile
}

func defaultDaemonClusterOptions() daemonClusterOptions {
	policy := ttcpolicy.DefaultPolicy()
	policy.MaxConcurrency = 4
	policy.AckInterval = time.Millisecond
	return daemonClusterOptions{MaxTransactions: defaultClusterMaxTransactions, PageListen: "0.0.0.0:0", LeaseDuration: defaultClusterLeaseDuration, PagingPolicy: policy, CPUProfileMode: "portable"}
}

type daemonClusterRuntime struct {
	listen          string
	authToken       string
	controlListener net.Listener
	controlPlane    *cluster.ControlPlane
	source          *distributedSourceRuntime
	directory       cluster.NodeCatalog
	placements      cluster.PlacementCatalog
	transactions    cluster.TransactionCatalog
	// authority is this daemon's part in the cluster authority: the
	// authority itself while it holds it.
	authority  *cluster.AuthorityNode
	catalog    *cluster.AuthorityCatalog
	nodeRecord cluster.NodeRecord
	lease      time.Duration
	clusterID  string
	// shared is the part of the cluster in the shared root; heartbeat keeps
	// this daemon alive in it, liveness judges others', and authorityRecord
	// names the node that holds the authority, self if this daemon does.
	shared          cluster.SharedCluster
	heartbeat       *cluster.Heartbeat
	liveness        *cluster.Liveness
	authorityMu     sync.Mutex
	authorityRecord cluster.AuthorityRecord
	self            cluster.AuthorityRecord
	autoRebalance   bool
	capacitySink    elasticCapacitySink
	cpuProfile      daemonCPUProfile
}

func (r *daemonClusterRuntime) currentAuthority() cluster.AuthorityRecord {
	r.authorityMu.Lock()
	defer r.authorityMu.Unlock()
	return r.authorityRecord
}

func (r *daemonClusterRuntime) setAuthority(record cluster.AuthorityRecord) {
	r.authorityMu.Lock()
	defer r.authorityMu.Unlock()
	r.authorityRecord = record
}

// controlOnlyCluster is an outbound-only cluster gateway. It deliberately has
// no authority state, NodeRecord, placement capacity, TCP listener, or data-plane
// provider. Commands arriving on its per-user Unix socket are tunneled to a
// compute daemon, which performs the ordinary authoritative routing.
type controlOnlyCluster struct {
	nodeID       string
	repositoryID string
	authToken    string
	// sharedRoot is where the gateway finds compute nodes when it knows of
	// none alive, and liveness judges from it which ones stalled, as
	// observePeers records in stalled every heartbeat.
	sharedRoot string
	// shared is the cluster in sharedRoot, whose records the gateway reads
	// every heartbeat through files it keeps open.
	shared    cluster.SharedCluster
	liveness  cluster.Liveness
	stalledMu sync.Mutex
	stalled   map[string]bool
	// holder runs this login node's control holder while the cluster needs
	// one; see controlHolder.
	holder *controlHolder

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

// observePeers records which daemons' counters in the shared root stood
// still for three heartbeats, or stopped. It runs every heartbeat, as the
// counters advance, so that it sees a stall within a heartbeat of when it is
// three.
func (c *controlOnlyCluster) observePeers() {
	if c.sharedRoot == "" {
		return
	}
	members, err := c.shared.ReadMembers()
	if err != nil {
		return
	}
	stalled := make(map[string]bool)
	for id, member := range members {
		if member.Stopped || c.liveness.StillFor(member) > 3*cluster.HeartbeatInterval {
			stalled[id] = true
		}
	}
	c.stalledMu.Lock()
	c.stalled = stalled
	c.stalledMu.Unlock()
	if c.holder != nil {
		c.holder.consider(c.shared, members, &c.liveness)
	}
}

// responsivePeers returns the peers whose daemons' counters in the shared
// root advance: one stalled a while, as a stopped process is, still accepts
// connections, but answers none. It returns every peer if none does.
func (c *controlOnlyCluster) responsivePeers() []controlOnlyPeer {
	peers := c.peerSnapshot()
	c.stalledMu.Lock()
	stalled := c.stalled
	c.stalledMu.Unlock()
	responsive := make([]controlOnlyPeer, 0, len(peers))
	for _, peer := range peers {
		if stalled[peer.nodeID] {
			continue
		}
		responsive = append(responsive, peer)
	}
	if len(responsive) == 0 {
		return peers
	}
	return responsive
}

func (c *controlOnlyCluster) seedURLs() []string {
	peers := c.peerSnapshot()
	urls := make([]string, 0, len(peers))
	for _, peer := range peers {
		urls = append(urls, peer.url)
	}
	return urls
}

// authorityState reads the cluster authority through any compute seed,
// following the holder it names. A control-only daemon keeps no authority
// state, and its seed need not hold the authority. Treating a refusal that
// names the holder as an endpoint failure would pin the gateway to a usable
// node that cannot answer, and break every command that needs a linearizable
// snapshot.
func (c *controlOnlyCluster) authorityState(ctx context.Context, query cluster.AuthorityQuery) (*cluster.AuthorityState, error) {
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
		state, err := peer.client.AuthorityQuery(ctx, query)
		if err == nil {
			return state, nil
		}
		readErrors = errors.Join(readErrors, fmt.Errorf("%s: %w", peer.url, err))
		var redirect *cluster.AuthorityNotHolderError
		if !errors.As(err, &redirect) || redirect.ControlURL == "" || redirect.ControlURL == peer.url {
			continue
		}
		holder := clients[redirect.ControlURL]
		if holder == nil {
			holder, err = cluster.NewPlainClient(redirect.ControlURL, c.nodeID, c.authToken, 2*time.Minute)
			if err != nil {
				readErrors = errors.Join(readErrors, fmt.Errorf("creating redirected holder client %s: %w", redirect.ControlURL, err))
				continue
			}
			clients[redirect.ControlURL] = holder
		}
		state, err = holder.AuthorityQuery(ctx, query)
		if err == nil {
			return state, nil
		}
		readErrors = errors.Join(readErrors, fmt.Errorf("%s: %w", redirect.ControlURL, err))
	}
	return nil, fmt.Errorf("reading cluster authority through all compute peers: %w", readErrors)
}

// refreshPeers learns the current compute endpoints through a linearizable
// authority read. The gateway itself remains absent from the authority membership and
// never advertises a listener. Existing clients are reused so an in-flight
// stream is unaffected by a concurrent refresh.
func (c *controlOnlyCluster) refreshPeers(ctx context.Context) error {
	current := c.peerSnapshot()
	nodes := cluster.AuthorityQuery{Nodes: cluster.All[string]()}
	state, err := c.authorityState(ctx, nodes)
	if (err != nil || len(current) == 0) && c.sharedRoot != "" {
		// Every compute node it knew is gone, or it knew none: find the
		// others in the shared root.
		if found, findErr := c.probePeers(sharedRootComputePeers(c.sharedRoot)); findErr == nil {
			c.peersMu.Lock()
			c.peers = found
			c.peersMu.Unlock()
			current = c.peerSnapshot()
			state, err = c.authorityState(ctx, nodes)
		}
	}
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

// sharedRootComputePeers returns, as peers to probe, the daemons the shared
// root names: the cluster's authority first, which is this login node's
// control holder while no compute daemon runs, then its compute members.
func sharedRootComputePeers(sharedRoot string) []string {
	shared := cluster.NewSharedCluster(sharedRoot)
	var peers []string
	add := func(nodeID, role, controlURL string) {
		if role == cluster.RoleCompute && controlURL != "" {
			peers = append(peers, nodeID+"="+strings.TrimPrefix(controlURL, "http://"))
		}
	}
	if record, ok, err := shared.ReadAuthority(); err == nil && ok && record.ControlURL != "" {
		peers = append(peers, record.NodeID+"="+strings.TrimPrefix(record.ControlURL, "http://"))
	}
	members, _ := shared.ReadMembers()
	ids := make([]string, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		add(id, members[id].Role, members[id].ControlURL)
	}
	return peers
}

func prepareControlOnlyCluster(opts daemonClusterOptions, sharedRoot string) (*controlOnlyCluster, error) {
	if opts.AuthToken == "" {
		return nil, errors.New("a control-only daemon needs its cluster's token")
	}
	control := &controlOnlyCluster{nodeID: opts.NodeID, repositoryID: opts.RepositoryID, authToken: opts.AuthToken, sharedRoot: sharedRoot}
	if sharedRoot != "" {
		control.shared = cluster.NewSharedCluster(sharedRoot).KeepFilesOpen()
	}
	peers := opts.Peers
	if len(peers) == 0 {
		peers = sharedRootComputePeers(sharedRoot)
	}
	found, err := control.probePeers(peers)
	if err != nil && len(opts.Peers) != 0 {
		return nil, fmt.Errorf("no compute node of the cluster is reachable and compatible with %q: %w", sharedRoot, err)
	}
	// A gateway may start before any compute node does: it finds them in
	// the shared root as they start.
	control.peers = found
	// Discovery is best-effort during startup: a compatible seed is sufficient
	// to start, and the background refresher will retry a temporarily
	// unavailable authority read.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = control.refreshPeers(ctx)
	cancel()
	return control, nil
}

// probePeers returns the peers, given as NODE=HOST:PORT, that answer and
// serve this gateway's repository.
func (c *controlOnlyCluster) probePeers(peers []string) ([]controlOnlyPeer, error) {
	var found []controlOnlyPeer
	seen := make(map[string]struct{})
	var probeErrors error
	for _, peerSpec := range peers {
		_, baseURL, _, err := parseClusterPeer(peerSpec)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[baseURL]; ok {
			continue
		}
		seen[baseURL] = struct{}{}
		client, err := cluster.NewPlainClient(baseURL, c.nodeID, c.authToken, 2*time.Minute)
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
		if status.Cluster.Capabilities.RepositoryID != c.repositoryID {
			probeErrors = errors.Join(probeErrors, fmt.Errorf("%s: repository %q does not match TTC_ROOT repository %q", baseURL, status.Cluster.Capabilities.RepositoryID, c.repositoryID))
			continue
		}
		found = append(found, controlOnlyPeer{nodeID: status.Cluster.Capabilities.NodeID, url: baseURL, client: client})
	}
	if len(found) == 0 {
		if probeErrors == nil {
			probeErrors = errors.New("the shared root names none")
		}
		return nil, probeErrors
	}
	return found, nil
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
// cluster node incarnation which the authority has forgotten. A
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
	authority   *cluster.AuthorityCatalog
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
	// handoffReinstalls has, by transaction, when its graph handoff, which
	// awaits an inherited load, was last reinstalled.
	handoffReinstalls sync.Map
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
		// A node that does not hold the authority has no applied state: read
		// the transaction through the holder.
	}
	return r.store.Load(id)
}

func (r *daemonTargetRuntime) monitorSourceAvailable(id string) bool {
	if r.authority != nil {
		node, ok := r.authority.AppliedNode(id)
		// Lease expiry is interpreted and committed by the authority's holder.
		// Comparing the holder's wall-clock deadline on another host would be
		// unsafe; an applied non-offline record remains observable.
		if ok {
			return node.State != cluster.NodeOffline
		}
		// Without applied state, ask the holder.
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
	ft := newForkTimer("target-reserve", tx.Identity.TransactionID)
	defer ft.mark("return")
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
		// that the authority currently places on this node; otherwise create an isolated
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
	// incarnation blocks the other and authority placement selects the current one.
	prepared, err := r.backend.containerStore().PrepareDistributedFork(ctx, ttcbundle.DistributedForkOptions{
		SourceContainer: tx.Identity.SourceContainer, TargetName: targetName,
		SourcePhysicalBranch: tx.SourcePhysicalBranch,
		TargetBranch:         tx.Identity.TargetBranch, VisibleBranch: tx.Identity.VisibleTargetBranch(),
		ParentBranch:  tx.Identity.SourceBranch,
		TransactionID: tx.Identity.TransactionID, NodeID: r.nodeID,
		SourceNode: tx.Identity.SourceNodeID, Migration: tx.Identity.Migration,
		AuthoritativeMigration: authoritativeMigration,
		Source:                 &source, Spec: &spec, TargetID: targetID,
	})
	if err != nil {
		return err
	}
	// Prepare first unpacks the root filesystem of the family's image on a
	// node that has not run it: 0.3 s from the node's own disk, 7 s from
	// NFS. Begin now, while the source captures the branch and its pages
	// travel; Prepare's own call waits for this one under the image roots'
	// lock and finds the root unpacked, and repeats it if this one failed.
	go func(id string) {
		if _, err := r.backend.containerStore().RefreshContainerImageRoot(context.WithoutCancel(ctx), id); err != nil {
			fmt.Fprintf(os.Stderr, "ttcd: unpacking the image root of %q ahead of its fork: %v\n", id, err)
		}
	}(prepared.Container.ID)
	return nil
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

// peerRuntimeReplaceWait bounds how long an import waits for the lifecycle
// lock to replace the runtime it was reserved into; see replacePeerRuntime.
var peerRuntimeReplaceWait = time.Minute

// replacePeerRuntime makes the runtime with this ID one that the import of
// branch can be restored into, as replaceRuntimeForImport does, and reports
// whether create made it. making ends the import's mark on the runtime,
// which goes first: a command holding the lifecycle lock may be waiting for
// the import's branch to be made. It is replaced by the mark taken under the
// lock. The lock may be long in coming, and its holder may be a remote fork
// from this node waiting for a target whose import waits for this one's: the
// import gives up after peerRuntimeReplaceWait, or once ctx is done, unless
// the replacement has begun.
func (r *daemonTargetRuntime) replacePeerRuntime(ctx context.Context, id, branch string, create func() error, making *func()) (bool, error) {
	(*making)()
	*making = func() {}
	type replacement struct {
		created bool
		making  func()
		err     error
	}
	handover := &importReplacement{}
	replaced := make(chan replacement, 1)
	go func() {
		created, making, err := r.backend.forBackground().replaceRuntimeForImport(id, branch, create, handover)
		replaced <- replacement{created: created, making: making, err: err}
	}()
	timer := time.NewTimer(peerRuntimeReplaceWait)
	defer timer.Stop()
	var result replacement
	select {
	case result = <-replaced:
	case <-timer.C:
		if handover.giveUp() {
			return false, fmt.Errorf("the node's lifecycle lock was not free in %v", peerRuntimeReplaceWait)
		}
		result = <-replaced
	case <-ctx.Done():
		if handover.giveUp() {
			return false, ctx.Err()
		}
		result = <-replaced
	}
	*making = result.making
	return result.created, result.err
}

// restoreLostItsPeer reports whether err is a peer restore's refusal for want
// of a live branch to restore beside, which its runtime had when the import
// looked.
func restoreLostItsPeer(err error) bool {
	message := err.Error()
	return strings.Contains(message, "no active peer is available") ||
		strings.Contains(message, "not available for distributed peer import") ||
		strings.Contains(message, "dehydration restore peer ") && strings.Contains(message, " became ")
}

func (r *daemonTargetRuntime) Prepare(ctx context.Context, tx *cluster.Transaction, session cluster.RemoteSessionDescriptor) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	ft := newForkTimer("target", tx.Identity.TransactionID)
	defer ft.mark("return")
	ft.mark("lock")
	info, targetBranch, peer, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	// The import takes no lifecycle lock: keep a release of the runtime from
	// deleting it from under the branch until the runtime lists the branch,
	// once the restore below returns; see familyOperations.releasing. A
	// replacement of the runtime gives the mark up and takes it again; see
	// replacePeerRuntime.
	doneMaking := r.backend.operations.beginMaking(info.ID, targetBranch.ID)
	defer func() { doneMaking() }()
	info, err = r.backend.containerStore().RefreshContainerImageRoot(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("materializing target image cache: %w", err)
	}
	ft.mark("image-root")
	portableMounts := make([]ttcbundle.PortableInternalMount, len(session.InternalMounts))
	for i, mount := range session.InternalMounts {
		portableMounts[i] = ttcbundle.PortableInternalMount{
			Destination: mount.Destination, Contents: append([]byte(nil), mount.Contents...), Mode: os.FileMode(mount.Mode),
		}
	}
	if err := r.backend.containerStore().InstallPortableInternalMountsOf(info, portableMounts); err != nil {
		return fmt.Errorf("installing target-local internal mounts: %w", err)
	}
	ft.mark("internal-mounts")
	filesystemConnections, filesystemAliases, err := r.backend.materializeDistributedFilesystemRoots(info, targetBranch.FilesystemBranch, session.FilesystemRoots, peer, false)
	if err != nil {
		return fmt.Errorf("materializing distributed filesystem metadata: %w", err)
	}
	ft.mark("filesystem-roots")
	// materializeDistributedFilesystemRoots installs one-shot restore aliases in
	// the active repository map. A later restore failure must not leave those
	// foreign identities attached to an otherwise healthy peer runtime.
	aliasesPending := len(session.FilesystemRoots) != 0
	defer func() {
		if aliasesPending {
			_ = r.backend.containerStore().CommitActiveRepositoryRestoreIDsOf(info)
		}
	}()
	memoryDependencyBytes := session.StreamBytes
	if session.GraphArtifact != "" {
		memoryDependencyBytes = 0
	}
	if err := r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TargetContainer, tx.Identity.TransactionID, memoryDependencyBytes, session.FilesystemBytes); err != nil {
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
	// The node's map of active repositories is the family's on the node, which
	// a start or a cold restore there commits whole under the lifecycle lock
	// this import does not hold: one that committed after the import wrote
	// its identities took them back, and the restore found its repositories
	// under none it knew, "TTC volume restore resource ... was not provided".
	// The identities the import installed are its own to restore under.
	for current, restoreID := range filesystemAliases {
		restoreArgs.FilesystemRestoreIDs[current] = restoreID
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
		if err := r.backend.containerStore().UpdateDistributedForkMemoryGraph(tx.Identity.TargetContainer, tx.Identity.TransactionID, session.GraphSnapshot, session.GraphRootObjects, tx.Identity.TransactionID); err != nil {
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
	configured := r.backend.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC, info.TTCNetworkInterface())
	configured, err = r.backend.withRuntimeDiagnostics(configured, info.ID)
	if err != nil {
		return err
	}
	var cleanups []func()
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()
	// create makes the target's runtime, the import's branch its loader.
	create := func() error {
		// A new physical Sentry must bootstrap the exact visible branch and
		// filesystem identities regardless of whether this transaction was
		// reserved as a peer import. In particular, the first remote child of a
		// family on a node is represented by a transaction-scoped container, so
		// peer is false even though its visible branch is not "main".
		bundleDir, cleanup, err := r.backend.containerStore().PrepareDistributedBranchRestoreBundle(info.ID, targetBranch.ID, targetBranch.FilesystemBranch)
		if err != nil {
			return fmt.Errorf("preparing distributed target bundle: %w", err)
		}
		cleanups = append(cleanups, cleanup)
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
		return nil
	}
	ft.mark("bookkeeping")
	runtimeCreated := false
	var runtimeState struct {
		Status string `json:"status"`
	}
	stateOutput, stateErr := r.backend.command("state", info.ID)
	if stateErr == nil {
		if err := json.Unmarshal(stateOutput, &runtimeState); err != nil {
			return fmt.Errorf("decoding target runtime state: %w", err)
		}
	}
	// The runtime's state is its loader branch's, which may have exited while
	// another branch runs there: the import goes beside that branch. One that
	// runs no branch has none to restore beside, and the family's runtime on
	// the node is made and released under the lifecycle lock: the import
	// replaces it under the lock, as it makes one the family has no more.
	// Such a runtime was one whose last live branch a stop or a dehydration
	// took as the fork went on, keeping the runtime for the import, which was
	// refused, "distributed peer target runtime is "stopped", want running".
	switch {
	case peer && (stateErr != nil || runtimeState.Status != "running" && runtimeState.Status != "created" && !r.backend.runtimeRunsLiveBranch(info.ID)):
		created, err := r.replacePeerRuntime(ctx, info.ID, targetBranch.ID, create, &doneMaking)
		if err != nil {
			return fmt.Errorf("replacing distributed peer target runtime, which runs no branch: %w", err)
		}
		if created {
			runtimeState.Status = "created"
			runtimeCreated = true
		}
	case stateErr != nil:
		if err := create(); err != nil {
			return err
		}
		runtimeState.Status = "created"
		runtimeCreated = true
	}
	if runtimeState.Status == "created" || peer {
		restore := func(restoreArgs boot.DistributedForkRestoreArgs) error {
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
				return err
			}
			keepDurableTargets = true
			return nil
		}
		// A metadata peer restored into a newly created empty Sentry is its
		// physical loader branch, not an import into an already-running family.
		ft.mark("runtime")
		restoreArgs.Peer = peer && !runtimeCreated
		err := restore(restoreArgs)
		ft.mark("sentry-restore")
		for attempt := 0; err != nil && restoreArgs.Peer && attempt < 3 && (restoreLostItsPeer(err) || !r.backend.runtimeRunsLiveBranch(info.ID)); attempt++ {
			// The runtime's last live branch went after the import looked, as
			// the restore went on beside it or before: the import replaces
			// the runtime. Or the runtime was still restoring its branch, as a
			// rehydration or another import making it does, "sandbox is
			// restoringUnstarted, not available for distributed peer import":
			// the replacement waits that out and finds the runtime live, and
			// the import goes beside its branch then. It failed so before.
			created, replaceErr := r.replacePeerRuntime(ctx, info.ID, targetBranch.ID, create, &doneMaking)
			if replaceErr != nil {
				break
			}
			if created {
				runtimeCreated = true
				restoreArgs.Peer = false
			}
			err = restore(restoreArgs)
		}
		if err != nil {
			return fmt.Errorf("%w (filesystem restore aliases: %v)", err, restoreArgs.FilesystemRestoreIDs)
		}
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
		if err := r.backend.containerStore().CommitActiveRepositoryRestoreIDsOf(info); err != nil {
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
	return r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TargetContainer, tx.Identity.TransactionID, "paused")
}

func (r *daemonTargetRuntime) Publish(_ context.Context, tx *cluster.Transaction, paused bool) error {
	ft := newForkTimer("target-publish", tx.Identity.TransactionID)
	defer ft.mark("return")
	defer r.lockTarget(tx.Identity.TargetContainer)()
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
	if err != nil {
		return err
	}
	if !paused && branch.State != "running" {
		if _, err := r.backend.command("timetravel", info.ID, "resume", branch.ID); err != nil {
			return fmt.Errorf("resuming distributed target: %w", err)
		}
		if err := r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TargetContainer, tx.Identity.TransactionID, "running"); err != nil {
			return err
		}
	}
	if !tx.Identity.Migration {
		info, err = r.backend.containerStore().PublishDistributedForkTarget(tx.Identity.TargetContainer, tx.Identity.TransactionID)
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
	// after the authority placement is visible.
	r.monitorDependency(tx.Identity.TransactionID, info.ID)
	return nil
}

func (r *daemonTargetRuntime) PublishedTarget(tx *cluster.Transaction) (family, branch, physicalContainer, physicalBranch, incarnation string, err error) {
	if r == nil || tx == nil {
		err = errors.New("distributed target runtime is unavailable")
		return
	}
	info, targetBranch, _, loadErr := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
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
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
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
	info, err = r.backend.containerStore().PublishDistributedForkTarget(tx.Identity.TargetContainer, tx.Identity.TransactionID)
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
	if err := r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TargetContainer, tx.Identity.TransactionID, targetState); err != nil {
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
		if _, err := r.backend.ensureExecConsoleDrain(selector{container: info.ID, branch: branch.ID}, session, false); err != nil {
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
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
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
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
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
	if err := r.backend.containerStore().UpdateDistributedForkMemoryGraph(tx.Identity.TargetContainer, tx.Identity.TransactionID, graph.GraphSnapshot, objects, handoffReferenceOperation); err != nil {
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
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
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

// graphHandoffInheritedLoadNotDurable is the error of a Sentry's graph
// handoff that found a load its branch inherited from another transaction not
// yet durable.
const graphHandoffInheritedLoadNotDurable = "inherited distributed memory load is not durable yet"

// graphHandoffReinstallInterval bounds how often a graph handoff that awaits
// an inherited load is reinstalled: each attempt starts a relay.
const graphHandoffReinstallInterval = time.Second

// graphHandoffAwaitsInheritedLoad reports whether a graph handoff that failed
// with message only awaits an inherited load, whose own handoff makes it
// durable: reinstalled then, the handoff succeeds.
func graphHandoffAwaitsInheritedLoad(message string) bool {
	return strings.Contains(message, graphHandoffInheritedLoadNotDurable)
}

// reinstallGraphHandoffEventually reinstalls tx's graph handoff unless it was
// reinstalled less than graphHandoffReinstallInterval ago. The caller holds
// the target's lock.
func (r *daemonTargetRuntime) reinstallGraphHandoffEventually(ctx context.Context, tx *cluster.Transaction) error {
	now := time.Now()
	if last, ok := r.handoffReinstalls.Load(tx.Identity.TransactionID); ok && now.Sub(last.(time.Time)) < graphHandoffReinstallInterval {
		return nil
	}
	r.handoffReinstalls.Store(tx.Identity.TransactionID, now)
	return r.handoffLocked(ctx, tx, *tx.GraphHandoff)
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
		_, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
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
		// the authority. Treating the historical handoff as live would make daemon restart
		// depend on an object that the data plane has already proved unnecessary.
		return branch.DependentMemoryBytes != 0 || branch.SnapshotID != 0 || len(branch.MemoryRootObjects) != 0
	default:
		return false
	}
}

func (r *daemonTargetRuntime) Cancel(_ context.Context, tx *cluster.Transaction) error {
	defer r.lockTarget(tx.Identity.TargetContainer)()
	var familyID string
	info, branch, peer, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
	if err == nil {
		familyID = info.ID
		if tx.Identity.Migration && branch.DistributedMigration {
			if _, pending := info.DistributedForkImports[tx.Identity.TransactionID]; !pending {
				return fmt.Errorf("published migration %q cannot be canceled", tx.Identity.TransactionID)
			}
		}
		if peer {
			// Preparation can fail before a target Sentry exists. The
			// runtime-only release helpers are idempotent in that case. They
			// also handle a pending import that is the only physical branch in
			// this node's Sentry, where DestroyBranch deliberately rejects
			// removal of the last branch.
			release := r.backend.releaseMigratedSourceRuntime
			if !slices.ContainsFunc(info.Branches, func(recorded ttcbundle.BranchInfo) bool { return recorded.ID == branch.ID }) {
				release = r.backend.releaseCanceledImport
			}
			if destroyErr := release(info.ID, branch.ID); destroyErr != nil {
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
	if err := r.backend.containerStore().RemoveDistributedForkTarget(tx.Identity.TargetContainer, tx.Identity.TransactionID); err != nil {
		return err
	}
	// The family's node-local state goes with its last runtime on the node;
	// while one still runs there, the state stays until the node reclaims it
	// as orphaned, and the cancel, done, does not fail for it.
	if familyID != "" {
		if err := r.backend.removeLocalFamilyStateIfUnowned(familyID); err != nil && !errors.Is(err, errRuntimeFamilyLive) {
			return err
		}
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
	info, branch, _, err := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, tx.Identity.TransactionID)
	if err != nil {
		return r.forwardedDependencyError(tx, err)
	}
	if tx.GraphHandoff != nil {
		status, err := r.graphHandoffStatus(tx)
		if err != nil {
			// The authority may contain a bound handoff intent whose first runtime attempt
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
			if !graphHandoffAwaitsInheritedLoad(status.Error) {
				return fmt.Errorf("graph memory handoff failed: %s", status.Error)
			}
			// The handoff found that a load the branch inherited from another
			// transaction was not yet durable, which that transaction's own
			// handoff makes it. Reinstall it until it succeeds.
			if err := r.reinstallGraphHandoffEventually(ctx, tx); err != nil {
				return fmt.Errorf("reinstalling graph memory handoff that awaits an inherited load: %w", err)
			}
			return errors.New("graph memory handoff awaits an inherited load and is still in progress")
		}
		r.handoffReinstalls.Delete(tx.Identity.TransactionID)
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
	return r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TargetContainer, tx.Identity.TransactionID, 0, 0)
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
				_ = r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TargetContainer, transactionID, 0, 0)
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
					if info, loadErr := r.backend.containerStore().LoadDistributedForkTarget(tx.Identity.TargetContainer, transactionID); loadErr == nil {
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
			_, physicalBranch, _, branchErr := r.backend.containerStore().DistributedForkTargetBranch(tx.Identity.TargetContainer, transactionID)
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
				runtimeState := "unavailable"
				if stateOutput, stateErr := r.backend.command("state", containerID); stateErr == nil {
					state, decodeErr := decodeRuntimeStateStatus(stateOutput)
					if decodeErr != nil {
						// A transiently unreadable state record is not evidence that a
						// published target died. Keep polling until runsc reports a
						// coherent lifecycle state or the control RPC recovers.
						continue
					}
					runtimeState = state
					if state == "running" {
						continue
					}
				}
				failure := fmt.Sprintf("dependent target exited before remote memory drain completed (runtime state %q)", runtimeState)
				_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionFailed, failure, time.Now())
				// runsc state succeeds for a dead sandbox and reports "stopped".
				// Treating any successful state command as liveness previously left
				// the transaction published forever and the logical branch marked
				// running, which fenced every later operation behind a dead target.
				_ = r.backend.containerStore().UpdateDistributedForkTargetState(tx.Identity.TargetContainer, transactionID, "exited")
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
			info, infoErr := r.backend.containerStore().LoadDistributedForkTarget(tx.Identity.TargetContainer, transactionID)
			var filesystemBytes uint64
			if infoErr == nil {
				if branch, branchErr := persistentBranchInfo(info, targetBranch); branchErr == nil {
					filesystemBytes = branch.DependentFilesystemBytes
				}
			}
			if !status.Done {
				_ = r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TargetContainer, transactionID, status.BytesOutstanding, filesystemBytes)
				continue
			}
			if status.Error != "" {
				_, _ = r.store.Advance(transactionID, tx.Revision, cluster.TransactionFailed,
					"remote memory loader failed: "+status.Error, time.Now())
				return
			}
			_ = r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TargetContainer, transactionID, 0, filesystemBytes)
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
			_ = r.backend.containerStore().UpdateDistributedForkDependencies(tx.Identity.TargetContainer, transactionID, 0, 0)
			// The graph reference remains branch-scoped until the branch itself
			// is replaced or removed; becoming independent only removes its
			// volatile provider dependency.
			return
		}
	}()
}

func decodeRuntimeStateStatus(encoded []byte) (string, error) {
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(encoded, &state); err != nil {
		return "", fmt.Errorf("decoding runtime state: %w", err)
	}
	state.Status = strings.TrimSpace(strings.ToLower(state.Status))
	if state.Status == "" {
		return "", errors.New("runtime state omitted status")
	}
	return state.Status, nil
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
	familyObserved := s.familyConsoleObserved(key)
	s.sidecarMu.Lock()
	observed := s.sidecars[key] != nil || familyObserved
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
		// A family's console follower lives as long as the family: a Wait
		// alone would hold a thread for it all that time.
		awaitExit(command.Process.Pid)
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
	backend.resolvedFamilies = &familySet{}
	// Releasing the family's last live branch makes its resident
	// dehydrations durable, which gives the lifecycle lock up meanwhile.
	unlock := backend.holdFamilyLifecycle(backend.familyID(target.container))
	defer unlock()
	changed, err := backend.finalizeObservedInitExit(target)
	if err == nil && changed {
		// The exit changed only the family of the branch.
		err = backend.reconcilePlacementCatalogOf(backend.resolvedFamilies.list()...)
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

// daemonSocketFromEnvironment returns the socket of the daemon whose root the
// environment names, or "" if it names none.
func daemonSocketFromEnvironment() string {
	root, err := protocol.DaemonRoot(os.Getenv)
	if err != nil {
		return ""
	}
	return protocol.DaemonSocket(root)
}

func daemonIsolationMode(unsafeNoHostIsolation bool) string {
	if unsafeNoHostIsolation {
		return "unsafe-no-host-isolation"
	}
	return daemonHostIsolationMode
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
		cpuTopology:           opts.cpuTopology,
		unsafeNoHostIsolation: opts.unsafeNoHostIsolation,
		disableHostGSO:        opts.disableHostGSO,
		clientTERM:            os.Getenv("TERM"),
		progressTerminal:      progressTerminal,
		progressWidth:         progressWidth,
		stdin:                 stdin,
		stdout:                stdout,
		stderr:                stderr,
		explicitInitStops:     new(atomic.Int32),
		// A cluster whose nodes need no CPUID faulting runs them so.
		disableSyscallPatching: opts.disableSyscallPatching,
		requester:              opts.requester,
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
	backend.gfx = s.gfx
	backend.nvproxy = s.nvproxy
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
	s.configureFamilyLocks(backend)
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
// lost target reply as a failure; absence is itself proof that the authority admitted
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

// distributedTargetGoneWait bounds how long a source's drain waits for the
// transaction of a remote fork whose target's branch is gone to be canceled,
// as a removal of the fork does next.
const distributedTargetGoneWait = 30 * time.Second

// distributedTargetGone reports whether err is a target node's refusal to
// drain a remote fork's target for want of the target's branch.
func distributedTargetGone(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "target branch is unavailable") ||
		strings.Contains(message, "file does not exist") || strings.Contains(message, "no such file or directory")
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
		return transactionSourcedFrom(tx, containerID, branchID)
	})
}

// transactionSourcedFrom reports whether tx was taken from branchID of the
// container with containerID, "" matching any: of the logical family, as the
// transaction names its source, or of the physical runtime, as the commands
// that act on a branch name it once they resolve it. A remote fork's target
// runs in a runtime of its own, apart from its family: its dehydration, which
// matched only the family, drained none of the forks taken from it, and
// released the runtime they read their files through, "FetchSpec failed:
// loading container: file does not exist", the forks unable to read any file
// they had not read yet.
func transactionSourcedFrom(tx cluster.Transaction, containerID, branchID string) bool {
	matches := func(container, branch string) bool {
		return (containerID == "" || container == containerID) && (branchID == "" || branch == branchID)
	}
	physicalContainer, physicalBranch := migrationSourceRuntime(tx)
	return matches(tx.Identity.SourceContainer, tx.Identity.SourceBranch) || matches(physicalContainer, physicalBranch)
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
	// A removal of a remote fork cancels its transaction and takes its
	// target, in either order. No branch reads the source for the fork then,
	// and the source's command goes on, as had the removal come first: a
	// dehydration of the source beside the removal failed, "draining
	// distributed target ... failed permanently while transaction remained
	// published", the target's branch gone and its transaction not yet
	// canceled. A canceled migration keeps its source; see
	// monitorSparseMigrationCleanup.
	sourceRuntime, _ := migrationSourceRuntime(tx)
	canceledFork := func(tx cluster.Transaction) bool {
		if tx.State != cluster.TransactionCanceled || tx.Identity.Migration {
			return false
		}
		_, _ = backend.command("timetravel", sourceRuntime, distributedSourceTerminalCommand(&tx), tx.Identity.TransactionID)
		return true
	}
	var targetGone time.Time
	for tx.State != cluster.TransactionIndependent {
		var (
			current *cluster.Transaction
			stepErr error
		)
		previousState, previousRevision := tx.State, tx.Revision
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		switch tx.State {
		case cluster.TransactionPublished:
			current, stepErr = peer.Drain(ctx, tx.Identity.TransactionID, tx.Revision)
		case cluster.TransactionDraining:
			current, stepErr = peer.MarkIndependent(ctx, tx.Identity.TransactionID, tx.Revision)
		case cluster.TransactionCanceled, cluster.TransactionFailed:
			cancel()
			if canceledFork(tx) {
				return nil
			}
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
				if canceledFork(tx) {
					return nil
				}
				return fmt.Errorf("distributed target %q became %s while draining: %s", tx.Identity.TargetContainer, tx.State, tx.Failure)
			}
			// The transaction changed meanwhile without advancing, and the
			// step met a revision it no longer has: a dehydration failed so,
			// "transaction is published revision 7, want published revision
			// 6". The step goes again from the revision read.
			if distributedDrainStepAdvanced(previousState, tx.State) || tx.Revision != previousRevision {
				continue
			}
			if !distributedDrainRetryable(stepErr) {
				// The target's branch went before its transaction was
				// canceled, which comes next; see canceledFork.
				if !tx.Identity.Migration && distributedTargetGone(stepErr) && (targetGone.IsZero() || time.Since(targetGone) < distributedTargetGoneWait) {
					if targetGone.IsZero() {
						targetGone = time.Now()
					}
					time.Sleep(100 * time.Millisecond)
					continue
				}
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
// against the cluster authority and this node's source authority before any work is performed.
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
// those bytes immediately afterward. The target cleanup and the authority cancellation
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
			if err := b.retireTransactionWithoutBranch(store, *tx); err != nil {
				return fmt.Errorf("retiring removable distributed target %q: %w", transactionID, err)
			}
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
			b.distributedSource.abortExport(transactionID)
			if err := b.retireTransactionWithoutBranch(store, *canceled); err != nil {
				return fmt.Errorf("retiring removable distributed target %q: %w", transactionID, err)
			}
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

// retireTransactionWithoutBranch retires a finished remote fork whose target
// branch is gone: one canceled after the removal of its target, which a
// migration had moved on and which removing the placement could not retire.
func (b *runtimeBackend) retireTransactionWithoutBranch(store cluster.TransactionCatalog, tx cluster.Transaction) error {
	if tx.Identity.Migration || !tx.Terminal() {
		return nil
	}
	placements, err := b.placementCatalog()
	if err != nil {
		return err
	}
	records, err := placements.List()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.OperationID == tx.Identity.TransactionID || record.LastOperationID == tx.Identity.TransactionID || transactionTargetsRemovedPlacement(tx, record) {
			return nil
		}
	}
	if err := store.Delete(tx.Identity.TransactionID, tx.Revision); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// cancelRemoteForkTarget retries optimistic cancellation across transaction
// progress that races a family removal. Cancellation is fenced in the authority before
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
// exist. Removing the authority's placement also performs the user-visible lineage splice.
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
		// Its target reads the source no more: publishing the source's
		// memory for it would only delay this.
		b.distributedSource.abortExport(transactionID)
		unlockSource := b.distributedSource.lockSourceFamily(sourceContainer)
		defer unlockSource()
		_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
		if err := b.releaseDistributedGraphReferences(graphTransferReferenceOperation(transactionID)); err != nil {
			return fmt.Errorf("releasing removed migration roots %q: %w", transactionID, err)
		}
		if err := b.releaseMigratedSourceRuntimeDiscard(sourceContainer, sourceBranch); err != nil {
			return fmt.Errorf("discarding removed migration source %s@%s: %w", tx.Identity.SourceContainer, tx.Identity.SourceBranch, err)
		}
		// While a runtime of the family still runs on the node, its state
		// stays until the node reclaims it as orphaned: the removal, whose
		// source is gone, failed for it, "reclaiming removed migration source
		// family ...: runtime family ... still exists: its runtime still runs".
		if err := b.removeLocalFamilyStateIfUnowned(tx.Identity.SourceContainer); err != nil && !errors.Is(err, errRuntimeFamilyLive) {
			return fmt.Errorf("reclaiming removed migration source family %s: %w", tx.Identity.SourceContainer, err)
		}
		return nil
	}
	// A live coordinator may retire the metadata for a source incarnation that
	// the authority has already fenced. Its node-local runtime and graph-reference intent
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

// migrationSourceIncarnationFenced reports whether the authority proves that a
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

// removeMovedBranchEntry removes the entry of branchID from the record of
// the family familyID, which a migration of the branch left there, the
// family's catalog entry of a branch that ran elsewhere, once the branch is
// removed. Left, it named a branch that no longer existed: a fork could not
// take its name, "branch ... already exists", and a reconciliation on the
// node it named placed it again. A physical runtime's record is not a
// family's catalog.
func (b *runtimeBackend) removeMovedBranchEntry(familyID, branchID string) error {
	info, err := b.containerStore().LoadRuntimeContainer(familyID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDistributedPhysicalRuntime() || info.DistributedFamilyID != "" {
		return nil
	}
	if _, err := persistentBranchInfo(info, branchID); err != nil {
		return nil
	}
	return b.containerStore().RemoveBranchRecord(info.ID, branchID)
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
		if !tx.Identity.Migration || (tx.State != cluster.TransactionIndependent && !abortedMigration(*tx)) {
			return fmt.Errorf("transaction %q is not an independent migration", transactionID)
		}
		if _, err := placements.Lookup(tx.Identity.SourceContainer, tx.Identity.SourceBranch); err == nil {
			return fmt.Errorf("cannot retire migration source %q while placement %s@%s still exists", transactionID, tx.Identity.SourceContainer, tx.Identity.SourceBranch)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checking removed migration placement %q: %w", transactionID, err)
		}
		if tx.State != cluster.TransactionIndependent {
			// The branch was removed before its target was independent, and
			// its target with it: nothing reads the source any more.
			if tx, err = cancelRemovedMigration(store, *tx); err != nil {
				return err
			}
		}
		if err := b.cleanupRemovedMigrationSource(*tx); err != nil {
			return err
		}
		if err := b.removeMovedBranchEntry(tx.Identity.SourceContainer, tx.Identity.SourceBranch); err != nil {
			return fmt.Errorf("removing removed migration's family entry %s@%s: %w", tx.Identity.SourceContainer, tx.Identity.SourceBranch, err)
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

// abortedMigration reports whether tx is a migration whose target became
// its branch but not yet independent: removing the branch cancels it.
func abortedMigration(tx cluster.Transaction) bool {
	if !tx.Identity.Migration || !tx.OwnershipCommitted {
		return false
	}
	switch tx.State {
	case cluster.TransactionPublished, cluster.TransactionDraining, cluster.TransactionCanceled:
		return true
	}
	return false
}

// cancelRemovedMigration cancels the migration of a removed branch, which
// its target may be advancing meanwhile. The authority refuses while the branch
// exists.
func cancelRemovedMigration(store cluster.TransactionCatalog, tx cluster.Transaction) (*cluster.Transaction, error) {
	for tx.State != cluster.TransactionCanceled && tx.State != cluster.TransactionIndependent {
		canceled, err := store.Advance(tx.Identity.TransactionID, tx.Revision, cluster.TransactionCanceled, "", time.Now())
		if err == nil {
			return canceled, nil
		}
		current, loadErr := store.Load(tx.Identity.TransactionID)
		if loadErr != nil || current.Revision == tx.Revision {
			return nil, fmt.Errorf("canceling the migration %q of a removed branch: %w", tx.Identity.TransactionID, errors.Join(err, loadErr))
		}
		tx = *current
	}
	return &tx, nil
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
	var records []cluster.PlacementRecord
	var ids []string
	for _, tx := range transactions {
		if !tx.Identity.Migration || (tx.State != cluster.TransactionIndependent && !abortedMigration(tx)) || tx.Identity.SourceNodeID != b.nodeID {
			continue
		}
		if _, err := placements.Lookup(tx.Identity.SourceContainer, tx.Identity.SourceBranch); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if tx.State != cluster.TransactionIndependent {
			// A renamed branch keeps the migration that moved it.
			if records == nil {
				if records, err = placements.List(); err != nil {
					return err
				}
			}
			if slices.ContainsFunc(records, func(record cluster.PlacementRecord) bool {
				return record.OperationID == tx.Identity.TransactionID || record.LastOperationID == tx.Identity.TransactionID
			}) {
				continue
			}
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
	opts.unsafeNoHostIsolation = runscNoHostIsolation(s.unsafeNoHostIsolation)
	opts.disableHostGSO = s.disableHostGSO
	opts.disableSyscallPatching = s.noCPUIDFaulting
	opts.runtimePlatform = s.runtimePlatform
	opts.cpuFeatures = s.cpuFeatures
	opts.cpuTopology = s.cpuTopology
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
	return normalizeHostRuntimePlatform(strings.ToLower(strings.TrimSpace(value)))
}

func runtimePlatformFromEnvironment() string {
	value := os.Getenv("TTC_RUNTIME_PLATFORM")
	if normalized, err := normalizeRuntimePlatform(value); err == nil {
		return normalized
	}
	return defaultRuntimePlatform
}

// RunDaemon serves the versioned TTC local API from the ttcd binary.
func RunDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == daemonRootlessProbeArg {
		return 0
	}
	socket := ""
	runtimePlatform := defaultRuntimePlatform
	unsafeNoHostIsolation := false
	allowNetworkRoot := false
	accessGroup := ""
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
		case args[0] == "--allow-network-filesystem-root":
			allowNetworkRoot = true
			args = args[1:]
		case args[0] == "--platform":
			if len(args) < 2 || args[1] == "" {
				fmt.Fprintf(stderr, "ttcd: --platform requires %s\n", runtimePlatformFlagUsage())
				return 2
			}
			runtimePlatform = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--platform="):
			runtimePlatform = strings.TrimPrefix(args[0], "--platform=")
			args = args[1:]
		default:
			if consumed, err := consumeDaemonAccessOption(args, &accessGroup); consumed != 0 || err != nil {
				if err != nil {
					fmt.Fprintf(stderr, "ttcd: %v\n", err)
					return 2
				}
				args = args[consumed:]
				continue
			}
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
	if err := errors.Join(validateNoCPUIDFaultingOption(clusterOpts), validateCPUTopologyOption(clusterOpts)); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 2
	}
	if accessGroup != "" && clusterOpts.ControlHolder {
		fmt.Fprintf(stderr, "ttcd: %s applies to a daemon that clients connect to, not to a control holder\n", unsafeAccessHostGroupOption)
		return 2
	}
	access, err := resolveDaemonAccess(accessGroup)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 2
	}
	roots, err := daemonRootsFromEnvironment(clusterOpts.Configured)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 2
	}
	// The daemon's root, where it keeps all of its own files, identifies it.
	rootVariable := "TTC_ROOT"
	if clusterOpts.Configured {
		rootVariable = "TTC_LOCAL_ROOT"
	}
	layout := newDaemonLayout(roots.local)
	if socket != "" && filepath.Clean(socket) != layout.socket {
		fmt.Fprintf(stderr, "ttcd: --socket %q is not the socket of the daemon's root %s, %q\n", socket, rootVariable, layout.socket)
		return 2
	}
	socket = layout.socket
	checked := []daemonLayout{layout}
	if clusterOpts.ControlOnly {
		// The gateway's control holder is a daemon whose root is in the
		// gateway's.
		checked = append(checked, newDaemonLayout(controlHolderRoot(controlHolderDir(layout.root))))
	}
	for _, layout := range checked {
		if err := checkDaemonSocketPaths(layout); err != nil {
			fmt.Fprintf(stderr, "ttcd: %v; choose a shorter %s\n", err, rootVariable)
			return 2
		}
	}
	if name, err := checkDaemonRootFilesystem(rootVariable, layout.root, allowNetworkRoot); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	} else if name != "" {
		fmt.Fprintf(stderr, "ttcd: warning: %s %q is on a network filesystem (%s); its sockets, locks, and process state may not work there\n", rootVariable, layout.root, name)
	}
	// GPU renderers need the GPU, which the daemon's isolation bars its
	// children from: settle how they start first.
	gfx := startGFXHost(unsafeNoHostIsolation, stderr)
	defer gfx.Close()
	// Isolate the daemon before it touches its roots.
	if err := enterDaemonHostIsolation(unsafeNoHostIsolation); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
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
	if os.Getenv("TTC_CLUSTER_TOKEN") != "" {
		// Earlier daemons took their cluster's token from here; none leaks
		// to the processes the daemon starts.
		fmt.Fprintln(stderr, "ttcd: warning: TTC_CLUSTER_TOKEN is ignored: a cluster's daemons authenticate with the token in its TTC_ROOT")
		os.Unsetenv("TTC_CLUSTER_TOKEN")
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
		token, err := clusterAuthToken(roots.shared)
		if err != nil {
			fmt.Fprintf(stderr, "ttcd: %v\n", err)
			return 1
		}
		clusterOpts.AuthToken = token
		control, err := prepareControlOnlyCluster(clusterOpts, roots.shared)
		if err != nil {
			fmt.Fprintf(stderr, "ttcd: configuring control-only cluster access: %v\n", err)
			return 1
		}
		if err := prepareDaemonLayout(layout, ""); err != nil {
			fmt.Fprintf(stderr, "ttcd: preparing TTC_LOCAL_ROOT: %v\n", err)
			return 1
		}
		if err := serveControlOnlyDaemon(socket, roots.shared, control, access, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "ttcd: %v\n", err)
			return 1
		}
		return 0
	}
	daemonDataRoot := roots.shared
	daemonRoot := roots.runtime
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
	// The client starts the sibling ttcd directly. Validate host requirements
	// on the installed executable here so a missing macOS Hypervisor.framework
	// entitlement fails startup instead of the first container launch.
	if executable, err := os.Executable(); err != nil {
		fmt.Fprintf(stderr, "ttcd: finding executable: %v\n", err)
		return 1
	} else if err := preflightDaemonExecutable(executable); err != nil {
		fmt.Fprintf(stderr, "ttcd: validating executable %q: %v\n", executable, err)
		return 1
	}
	kernelRelease, err := daemonKernelRelease()
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: detecting host Linux kernel release: %v\n", err)
		return 1
	}
	kernelCompatibility, err := evaluateCurrentHostCompatibility(kernelRelease)
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	if warning := currentHostCompatibilityWarning(kernelCompatibility); warning != "" {
		fmt.Fprintf(stderr, "ttcd: warning: %s\n", warning)
	}
	if unsafeNoHostIsolation {
		fmt.Fprintln(stderr, "ttcd: warning: "+daemonNoHostIsolationWarning)
	} else if clusterOpts.ControlHolder {
		// A control holder runs no container.
	} else if err := preflightDaemonIsolation(false); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		return 1
	}
	if err := prepareDaemonStorageRoot(roots.shared, sharedRole, clusterOpts.RepositoryID, ""); err != nil {
		// Another daemon of a new cluster published its repository first.
		marker, markerErr := readClusterSharedRootMarker(roots.shared, clusterSharedRootVisibilityTimeout)
		if !clusterOpts.RepositoryGenerated || markerErr != nil || marker.RepositoryID == clusterOpts.RepositoryID {
			fmt.Fprintf(stderr, "ttcd: preparing TTC_ROOT: %v\n", err)
			return 1
		}
		clusterOpts.RepositoryID, clusterOpts.RepositoryGenerated = marker.RepositoryID, false
		if err := prepareDaemonStorageRoot(roots.shared, sharedRole, clusterOpts.RepositoryID, ""); err != nil {
			fmt.Fprintf(stderr, "ttcd: preparing TTC_ROOT: %v\n", err)
			return 1
		}
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
	if err := prepareDaemonLayout(layout, daemonRoot); err != nil {
		fmt.Fprintf(stderr, "ttcd: preparing %s: %v\n", rootVariable, err)
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
	// A standalone daemon's guests see the topology it is started with; a
	// cluster node's, the one its cluster records.
	topologyMode, topologyChosen, topologyFingerprint := clusterOpts.CPUTopology, clusterOpts.CPUTopologySet, ""
	if clusterRuntime != nil {
		topologyMode, topologyFingerprint = cpuProfile.cpuTopologyMode(), cpuProfile.TopologyFingerprint
		topologyChosen = topologyChosen || cpuProfile.Topology != ""
	}
	cpuTopology, err := resolveDaemonCPUTopology(topologyMode, topologyFingerprint)
	if err == nil && cpuTopology.Mode == cputopology.Host && runtimePlatform != "systrap" && !clusterOpts.ControlHolder && !clusterOpts.ControlOnly {
		err = fmt.Errorf("--cpu-topology host needs --platform systrap, whose application threads are host threads, not %s", runtimePlatform)
	}
	if err != nil {
		fmt.Fprintf(stderr, "ttcd: configuring CPU topology: %v\n", err)
		return 1
	}
	if cpuTopology.Mode == cputopology.Single && !topologyChosen && !clusterOpts.ControlHolder && !clusterOpts.ControlOnly && cputopology.AllowedNodeCount() > 1 {
		fmt.Fprintln(stderr, "ttcd: warning: "+daemonCPUTopologyWarning)
	}
	if warning := cpuProfileWithoutFaultingWarning(cpuProfile, runtimePlatform, hostSupportsCPUIDFaulting()); warning != "" && !clusterOpts.ControlHolder && !clusterOpts.ControlOnly {
		fmt.Fprintln(stderr, "ttcd: warning: "+warning)
	}
	if err := serveDaemonWithCluster(socket, runtimePlatform, cpuProfile, cpuTopology, access, unsafeNoHostIsolation, kernelCompatibility, daemonDataRoot, roots.local, daemonRoot, daemonRunsc, manager.Capability(), clusterRuntime, gfx, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "ttcd: %v\n", err)
		if errors.Is(err, errLeaseSelfFenced) {
			fmt.Fprintln(stderr, "ttcd: restarting to wait for the cluster; this node's sandboxes stay stopped until it answers")
			return DaemonRestartExitCode
		}
		if errors.Is(err, errClusterAuthorityChanged) {
			if clusterOpts.ControlHolder {
				// It held the authority only while no compute daemon ran.
				return 0
			}
			fmt.Fprintln(stderr, "ttcd: restarting for the cluster's new authority; this node's sandboxes stay stopped until it answers")
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
		// listener nor a control address.
		opts.PageListen = ""
		return nil
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
		// A daemon that finds no repository in the shared root makes one up;
		// if another daemon of a new cluster published its own first, this one
		// adopts it as it prepares the shared root: see RunDaemon.
		if marker, err := readClusterSharedRootMarker(roots.shared, 0); err == nil {
			opts.RepositoryID = marker.RepositoryID
		} else if errors.Is(err, os.ErrNotExist) {
			id, err := newDaemonRequestID()
			if err != nil {
				return err
			}
			opts.RepositoryID = "repo-" + strings.TrimPrefix(id, "req-")
			opts.RepositoryGenerated = true
		} else {
			return err
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
	if !clustered && strings.TrimSpace(os.Getenv("TTC_LOCAL_ROOT")) != "" {
		// A client would look for this daemon in TTC_LOCAL_ROOT.
		return daemonRoots{}, errors.New("TTC_LOCAL_ROOT is set, but only a daemon of a cluster has one; unset it to start a standalone daemon, whose root is TTC_ROOT")
	}
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

func freshDaemonRootEntryAllowed(entry os.DirEntry, startupLog os.FileInfo) bool {
	if entry.Name() == "lost+found" || strings.HasPrefix(entry.Name(), daemonRootMarkerTemporaryPrefix) && entry.Type().IsRegular() {
		return true
	}
	// The thin client opens the daemon's ttcd.log, in its root, before exec so
	// startup diagnostics survive a daemon failure; that log is necessarily
	// visible before the daemon can publish the format marker. Admit only the
	// exact regular file currently attached to stderr; a stale or user-created
	// ttcd.log must still make an unmarked root fail closed.
	if startupLog == nil || entry.Name() != protocol.LogName || !entry.Type().IsRegular() {
		return false
	}
	info, err := entry.Info()
	return err == nil && os.SameFile(info, startupLog)
}

func daemonStartupLogInfo() os.FileInfo {
	info, err := os.Stderr.Stat()
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
// Daemon startup performs this before platform probes, worker creation, cluster
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
		if entry.Name() == ".ttc-root-format" {
			// Published since the read above, as by another daemon of a new
			// cluster starting at the same time; it is linked into place
			// whole, so read it as if it had been there.
			return inspectDaemonStorageRoot(root, role)
		}
		if !freshDaemonRootEntryAllowed(entry, startupLog) {
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
		if !freshDaemonRootEntryAllowed(entry, startupLog) {
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

// localNodeAuthorityReset asks the first seed that answers whether this
// incarnation must discard its node-local authority; see
// localIncarnationReset.
func localNodeAuthorityReset(nodeID, incarnation, authToken string, clients map[string]*cluster.Client) (bool, error) {
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
		return localIncarnationReset(state, nodeID, incarnation)
	}
	return false, nil
}

// localIncarnationReset reports whether the authority no longer knows the
// node: `node rm` gives up what the node held, and forgets its record, so
// its absence from a linearizable read authorizes discarding what the node
// kept locally. An incarnation whose lease expired while it was away keeps
// its placements, volatile dependencies and unpublished origins.
func localIncarnationReset(state *cluster.AuthorityState, nodeID, incarnation string) (bool, error) {
	if state == nil {
		return false, errors.New("cluster authority state is nil")
	}
	if record, ok := state.Nodes[nodeID]; ok {
		if record.Incarnation != incarnation {
			return false, fmt.Errorf("cluster node %q already belongs to incarnation %q; local scratch belongs to %q", nodeID, record.Incarnation, incarnation)
		}
		return false, nil
	}
	return true, nil
}

func authorityStateThroughSeed(ctx context.Context, seed *cluster.Client, nodeID, authToken string) (*cluster.AuthorityState, error) {
	peer := seed
	seen := make(map[string]bool)
	for redirects := 0; redirects < 5; redirects++ {
		state, err := peer.AuthorityState(ctx)
		if err == nil {
			return state, nil
		}
		var redirect *cluster.AuthorityNotHolderError
		if !errors.As(err, &redirect) || redirect.ControlURL == "" {
			return nil, err
		}
		key := redirect.HolderID + "\x00" + redirect.ControlURL
		if seen[key] {
			return nil, fmt.Errorf("authority redirect loop through %q", redirect.HolderID)
		}
		seen[key] = true
		var clientErr error
		peer, clientErr = cluster.NewPlainClient(redirect.ControlURL, nodeID, authToken, 2*time.Minute)
		if clientErr != nil {
			return nil, fmt.Errorf("configuring the redirected authority %q: %w", redirect.HolderID, clientErr)
		}
	}
	return nil, errors.New("too many authority redirects while reading cluster authority")
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
	for _, removed := range []string{"--cluster-init", "--cluster-raft-bootstrap", "--cluster-join", "--cluster-peer"} {
		if args[0] == removed || strings.HasPrefix(args[0], removed+"=") {
			return 0, fmt.Errorf("%s is gone: every daemon of a cluster starts alike, and finds the cluster in its shared TTC_ROOT", removed)
		}
	}
	if args[0] == "--control-only" {
		opts.ControlOnly = true
		opts.Configured = true
		return 1, nil
	}
	if args[0] == "--control-holder" {
		opts.ControlHolder = true
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
	// A control holder creates none, and runs no guest on the cluster's CPU
	// profile: whatever platform it names, it holds a cluster of KVM nodes on
	// a host without CPUID faulting, or with a profile only KVM provides.
	// A cluster of identical CPUs may waive CPUID faulting when it is created;
	// validateDaemonCPUProfile then holds each node to the CPU it records.
	if !opts.ControlHolder {
		waived, err := clusterWaivesCPUIDFaulting(dataRoot, opts)
		if err != nil {
			return nil, err
		}
		if err := validateClusterCPUEnforcement(runtimePlatform, hostSupportsCPUIDFaulting(), waived); err != nil {
			return nil, err
		}
	}
	if opts.LeaseDuration == 0 {
		opts.LeaseDuration = defaultClusterLeaseDuration
	}
	if opts.Listen == "" || opts.Advertise == "" || opts.NodeID == "" || opts.RepositoryID == "" {
		return nil, errors.New("cluster mode requires listen/advertise addresses, node ID, and repository ID")
	}
	token, err := clusterAuthToken(dataRoot)
	if err != nil {
		return nil, err
	}
	opts.AuthToken = token
	resolvedCPU, err := resolveDaemonCPUProfile(opts.CPUProfileMode, opts.CPUFeatures, runtimePlatform)
	if err != nil {
		return nil, fmt.Errorf("resolving cluster CPU profile: %w", err)
	}
	if opts.NoCPUIDFaulting {
		if resolvedCPU, err = waiveCPUIDFaulting(resolvedCPU, runtimePlatform); err != nil {
			return nil, err
		}
	}
	switch opts.CPUTopology {
	case cputopology.Host:
		host, err := resolveDaemonCPUTopology(cputopology.Host, "")
		if err != nil {
			return nil, err
		}
		resolvedCPU.Topology, resolvedCPU.TopologyFingerprint = cputopology.Host, host.Fingerprint()
	case cputopology.Single:
		resolvedCPU.Topology = cputopology.Single
	}
	opts.ResolvedCPU = resolvedCPU
	pendingClusterPolicy, err := applyImmutableClusterConfig(dataRoot, &opts)
	if err != nil {
		return nil, err
	}
	if !opts.ControlHolder {
		if err := validateDaemonCPUProfile(opts.ResolvedCPU, runtimePlatform); err != nil {
			return nil, fmt.Errorf("validating cluster CPU profile: %w", err)
		}
	}
	clusterID, err := prepareClusterIdentity(dataRoot, localRoot, opts.RepositoryID, opts.NodeID)
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
	if opts.Advertise != "" && opts.PageAdvertise == "" {
		return nil, errors.New("cluster nodes require --cluster-page-advertise HOST:0")
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
	clients := make(map[string]*cluster.Client)
	incarnation, err := localNodeIncarnation(localRoot, opts.NodeID)
	if err != nil {
		return nil, err
	}
	controlListener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("listening for cluster control on %s: %w", opts.Listen, err)
	}
	closeListener := true
	defer func() {
		if closeListener {
			_ = controlListener.Close()
		}
	}()
	advertiseURL, err = resolveClusterAdvertise(advertiseURL, controlListener.Addr().String())
	if err != nil {
		return nil, err
	}
	parsedAdvertise, err := url.Parse(advertiseURL)
	if err != nil || parsedAdvertise.Host == "" {
		return nil, fmt.Errorf("resolving shared cluster advertise address %q", advertiseURL)
	}
	// The cluster is anchored in the shared root, which says which node holds
	// its authority, and keeps the authority's state: see
	// cluster.SharedCluster. This daemon holds it, or registers with the node
	// that does, and sends its reads and proposals there.
	shared := cluster.NewSharedCluster(dataRoot).KeepFilesOpen()
	role := cluster.RoleCompute
	if opts.ControlHolder {
		role = cluster.RoleControl
	}
	self := cluster.AuthorityRecord{
		NodeID: opts.NodeID, Incarnation: incarnation, Role: role,
		ControlURL: advertiseURL, ControlServerName: advertiseServerName,
	}
	heartbeat, err := cluster.StartHeartbeat(shared, cluster.MemberRecord{
		NodeID: self.NodeID, Incarnation: self.Incarnation, Role: self.Role,
		ControlURL: self.ControlURL, ControlServerName: self.ControlServerName,
	})
	if err != nil {
		return nil, fmt.Errorf("starting this node's cluster heartbeat: %w", err)
	}
	closeHeartbeat := true
	defer func() {
		if closeHeartbeat {
			heartbeat.Stop()
			shared.CloseFiles()
		}
	}()
	liveness := &cluster.Liveness{}
	if opts.ControlHolder {
		if err := armControlHolderParentDeath(); err != nil {
			return nil, fmt.Errorf("watching the gateway that started this control holder: %w", err)
		}
		var stillness []cluster.Stillness
		if encoded := os.Getenv(controlHolderStillnessEnv); encoded != "" {
			if err := json.Unmarshal([]byte(encoded), &stillness); err != nil {
				return nil, fmt.Errorf("decoding what the gateway saw of the cluster: %w", err)
			}
		}
		for _, seen := range stillness {
			liveness.Assume(seen)
		}
	}
	// Requests go to the holder the shared root names, as last read at most
	// a second before, or again once one sent there failed.
	holderView := &cluster.HolderView{Cluster: shared}
	authorityNode := cluster.NewAuthorityNode(opts.NodeID, 0, func() (cluster.AuthorityRecord, bool, error) {
		return holderView.Record(time.Second)
	})
	closeAuthority := true
	defer func() {
		if closeAuthority {
			_ = authorityNode.Close()
		}
	}()
	var authority cluster.AuthorityRecord
	// A node whose Sentries a fence stopped keeps trying, however long the
	// cluster is away; see registerClusterNodeWithRetry.
	wait := holdsFencedRuntimes(localRoot)
	giveUp := time.Now().Add(2 * cluster.AuthorityTakeover)
	recoveryChecked := false
	for report := time.Now().Add(clusterWaitReport); ; {
		decision, err := shared.DecideAuthority(context.Background(), self, liveness)
		if err != nil {
			return nil, err
		}
		authority = decision.Record
		if opts.ControlHolder && !decision.Hold {
			return nil, fmt.Errorf("node %s holds the cluster's authority, which a control holder holds only while no other daemon does", authority.NodeID)
		}
		if decision.Hold {
			if err := holdClusterAuthority(shared, authorityNode, heartbeat, liveness, authority); err != nil {
				return nil, err
			}
			break
		}
		holderClient, err := cluster.NewPlainClient(authority.ControlURL, opts.NodeID, opts.AuthToken, 2*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("configuring the cluster authority %q: %w", authority.NodeID, err)
		}
		clients = map[string]*cluster.Client{authority.NodeID: holderClient}
		// Once the authority proves the node absent, as `node rm` leaves it,
		// discard its node-local authority and rotate the execution epoch.
		// Immutable image cache and shared snapshot authority remain.
		if !recoveryChecked {
			reset, err := localNodeAuthorityReset(opts.NodeID, incarnation, opts.AuthToken, clients)
			if err != nil {
				return nil, err
			}
			recoveryChecked = true
			if reset {
				// An absent incarnation has no remaining execution or storage
				// authority, and its local journals must not be replayed under
				// the replacement incarnation. Reap the guardian-stopped runtime
				// before deleting the fence record and runsc sockets needed to
				// identify and clean those processes.
				if err := terminateRetiredElasticRuntimeRoot(filepath.Join(localRoot, "runtime", "runsc")); err != nil {
					return nil, fmt.Errorf("reaping retired local runtimes: %w", err)
				}
				if err := discardRetiredLocalAuthority(localRoot); err != nil {
					return nil, err
				}
				if incarnation, err = localNodeIncarnation(localRoot, opts.NodeID); err != nil {
					return nil, err
				}
				self.Incarnation = incarnation
				if err := heartbeat.Update(func(record *cluster.MemberRecord) { record.Incarnation = incarnation }, true); err != nil {
					return nil, err
				}
				if _, err := prepareClusterIdentity(dataRoot, localRoot, opts.RepositoryID, opts.NodeID); err != nil {
					return nil, err
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		joinErr := joinAuthorityThroughSeed(ctx, holderClient, cluster.AuthorityJoinRequest{
			NodeID: opts.NodeID, Incarnation: incarnation,
			ControlURL: advertiseURL, ControlServerName: advertiseServerName, ClusterID: clusterID,
			Capabilities: capabilities, MemoryTargetBytes: opts.MemoryTarget,
		}, opts.NodeID, opts.AuthToken)
		cancel()
		if joinErr == nil {
			break
		}
		if !wait && time.Now().After(giveUp) {
			return nil, fmt.Errorf("joining the cluster authority, node %s: %w", authority.NodeID, joinErr)
		}
		if !time.Now().Before(report) {
			fmt.Fprintf(stderr, "ttcd: waiting for the cluster authority, node %s: %v\n", authority.NodeID, joinErr)
			report = time.Now().Add(clusterWaitReport)
		}
		// Decide again: the holder may have ended.
		time.Sleep(time.Second)
	}
	if authority.Holds(self) {
		base := immutableClusterConfigFromOptions(opts)
		policy, err := reconcileClusterRuntimePolicy(authorityNode, dataRoot, base, pendingClusterPolicy)
		if err != nil {
			return nil, err
		}
		applyClusterRuntimePolicy(&opts, policy)
		capabilities.PagingPolicy = opts.PagingPolicy
		if err := capabilities.Validate(); err != nil {
			return nil, err
		}
	} else {
		// A member that is not the authority adopts the policy the shared
		// root records, which applyImmutableClusterConfig loaded.
		capabilities.PagingPolicy = opts.PagingPolicy
		if err := capabilities.Validate(); err != nil {
			return nil, err
		}
	}
	catalog, err := cluster.NewAuthorityCatalog(authorityNode)
	if err != nil {
		return nil, err
	}
	nodeRecord := cluster.NodeRecord{
		NodeID: opts.NodeID, Incarnation: incarnation,
		ControlURL: advertiseURL, ControlServerName: advertiseServerName,
		Capabilities: capabilities, MemoryTargetBytes: opts.MemoryTarget,
	}
	if opts.ControlHolder {
		nodeRecord.Labels = map[string]string{cluster.NodeRoleLabel: cluster.RoleControl}
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
	var holderClients sync.Map
	catalog.SetHolderClientResolver(func(retry bool) (*cluster.Client, error) {
		maxAge := time.Second
		if retry {
			maxAge = 0
		}
		record, ok, err := holderView.Record(maxAge)
		if err != nil {
			return nil, err
		}
		if !ok || record.Holds(self) {
			return nil, errors.New("the cluster authority has no other holder to forward to")
		}
		if client, ok := holderClients.Load(record.ControlURL); ok {
			return client.(*cluster.Client), nil
		}
		client, err := cluster.NewPlainClient(record.ControlURL, opts.NodeID, opts.AuthToken, 2*time.Minute)
		if err != nil {
			return nil, err
		}
		actual, _ := holderClients.LoadOrStore(record.ControlURL, client)
		return actual.(*cluster.Client), nil
	})
	catalog.SetLeaseRenewal(func() error {
		_, err := renewExecutionLease(catalog, opts.NodeID)
		return err
	})
	closeAuthority = false
	closeListener = false
	closeHeartbeat = false
	return &daemonClusterRuntime{
		listen: controlListener.Addr().String(), authToken: opts.AuthToken,
		controlListener: controlListener,
		controlPlane: &cluster.ControlPlane{
			Capabilities: capabilities, Store: transactions, MaxTransactions: opts.MaxTransactions, Now: time.Now,
			Authority:     &cluster.AuthorityService{Node: authorityNode, Capabilities: capabilities, ClusterID: clusterID},
			TopologyState: catalog.LinearizableState, AuthToken: opts.AuthToken,
		},
		source: source, directory: catalog, placements: placements, transactions: transactions,
		authority: authorityNode, catalog: catalog, nodeRecord: nodeRecord, lease: opts.LeaseDuration, clusterID: clusterID,
		shared: shared, heartbeat: heartbeat, liveness: liveness, authorityRecord: authority, self: self,
		autoRebalance: opts.AutoRebalance, capacitySink: capacitySink, cpuProfile: opts.ResolvedCPU,
	}, nil
}

const daemonImmutableClusterConfigVersion = 2

// daemonImmutableClusterConfigNoCPUIDFaultingVersion is the version of the
// configuration of a cluster whose systrap nodes need no CPUID faulting. A
// build that predates the waiver refuses it, rather than run such a cluster's
// nodes with syscall patching and without comparing their CPUs.
const daemonImmutableClusterConfigNoCPUIDFaultingVersion = 3

// daemonImmutableClusterConfigHostTopologyVersion is the version of the
// configuration of a cluster whose guests see the host's CPU topology, with or
// without CPUID faulting. A build that predates the choice refuses it, rather
// than show such a cluster's guests the single topology.
const daemonImmutableClusterConfigHostTopologyVersion = 4

func immutableClusterConfigVersion(profile daemonCPUProfile) int {
	if profile.Topology == cputopology.Host {
		return daemonImmutableClusterConfigHostTopologyVersion
	}
	if profile.NoCPUIDFaulting {
		return daemonImmutableClusterConfigNoCPUIDFaultingVersion
	}
	return daemonImmutableClusterConfigVersion
}

// clusterWaivesCPUIDFaulting reports whether the cluster of opts runs its
// systrap nodes without CPUID faulting: opts asks for that, or the cluster
// its shared root records was created so. It creates nothing, so that a node
// refused for want of CPUID faulting does not create its cluster.
func clusterWaivesCPUIDFaulting(sharedRoot string, opts daemonClusterOptions) (bool, error) {
	if opts.NoCPUIDFaulting {
		return true, nil
	}
	data, err := os.ReadFile(filepath.Join(sharedRoot, ".ttc-cluster-config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var config daemonImmutableClusterConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return false, fmt.Errorf("decoding immutable cluster configuration: %w", err)
	}
	return config.CPUProfile.NoCPUIDFaulting, nil
}

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
		Version: immutableClusterConfigVersion(opts.ResolvedCPU), PagingPolicy: opts.PagingPolicy,
		LeaseDuration: opts.LeaseDuration, MaxTransactions: opts.MaxTransactions,
		AutoRebalance: opts.AutoRebalance,
		CPUProfile:    opts.ResolvedCPU,
	}
}

func equalImmutableClusterConfig(a, b daemonImmutableClusterConfig) bool {
	return a.Version == b.Version && a.PagingPolicy == b.PagingPolicy &&
		a.LeaseDuration == b.LeaseDuration && a.MaxTransactions == b.MaxTransactions &&
		a.AutoRebalance == b.AutoRebalance && a.CPUProfile.Version == b.CPUProfile.Version &&
		a.CPUProfile.Mode == b.CPUProfile.Mode && slices.Equal(a.CPUProfile.Features, b.CPUProfile.Features) && a.CPUProfile.XsaveABI == b.CPUProfile.XsaveABI &&
		a.CPUProfile.NoCPUIDFaulting == b.CPUProfile.NoCPUIDFaulting && a.CPUProfile.CPUID == b.CPUProfile.CPUID &&
		a.CPUProfile.Topology == b.CPUProfile.Topology && a.CPUProfile.TopologyFingerprint == b.CPUProfile.TopologyFingerprint
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

// applyImmutableClusterConfig applies to opts the cluster-wide configuration
// the shared root records, which the cluster's first daemon records. It
// returns the configuration opts asks for instead, if it differs: the
// authority makes it the cluster's, when it holds it.
func applyImmutableClusterConfig(sharedRoot string, opts *daemonClusterOptions) (*daemonImmutableClusterConfig, error) {
	path := filepath.Join(sharedRoot, ".ttc-cluster-config.json")
	want := immutableClusterConfigFromOptions(*opts)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		encoded, marshalErr := json.Marshal(want)
		if marshalErr != nil {
			return nil, marshalErr
		}
		// Another daemon of a new cluster may record it first.
		if data, err = publishOnce(path, append(encoded, '\n')); err != nil {
			return nil, fmt.Errorf("creating immutable cluster configuration: %w", err)
		}
	}
	if err != nil {
		return nil, err
	}
	var got daemonImmutableClusterConfig
	if err := json.Unmarshal(data, &got); err != nil {
		return nil, fmt.Errorf("decoding immutable cluster configuration: %w", err)
	}
	if got.Version != immutableClusterConfigVersion(got.CPUProfile) {
		return nil, fmt.Errorf("unsupported immutable cluster configuration version %d", got.Version)
	}
	if opts.CPUProfileSet && (got.CPUProfile.Mode != want.CPUProfile.Mode || !slices.Equal(got.CPUProfile.Features, want.CPUProfile.Features)) {
		return nil, fmt.Errorf("requested CPU profile differs from immutable cluster profile %q", got.CPUProfile.Mode)
	}
	if opts.NoCPUIDFaulting && !got.CPUProfile.NoCPUIDFaulting {
		return nil, errors.New("--experimental-no-cpuid-faulting applies when a cluster is created; this cluster's systrap nodes require CPUID faulting")
	}
	if opts.CPUTopologySet && got.CPUProfile.cpuTopologyMode() != opts.CPUTopology {
		return nil, fmt.Errorf("--cpu-topology %s differs from this cluster's CPU topology, %s, which its first daemon recorded", opts.CPUTopology, got.CPUProfile.cpuTopologyMode())
	}
	compareWant := want
	if !opts.CPUProfileSet {
		compareWant.CPUProfile = got.CPUProfile
	}
	// Whether the cluster waives CPUID faulting a node adopts; whether the
	// node's CPU is the one the cluster records, validateDaemonCPUProfile
	// judges.
	compareWant.CPUProfile.NoCPUIDFaulting, compareWant.CPUProfile.CPUID = got.CPUProfile.NoCPUIDFaulting, got.CPUProfile.CPUID
	// A node adopts its cluster's CPU topology; whether the host's layout is
	// the one the cluster records, resolveDaemonCPUTopology judges.
	compareWant.CPUProfile.Topology, compareWant.CPUProfile.TopologyFingerprint = got.CPUProfile.Topology, got.CPUProfile.TopologyFingerprint
	compareWant.Version = immutableClusterConfigVersion(compareWant.CPUProfile)
	var pending *daemonImmutableClusterConfig
	if !equalImmutableClusterConfig(got, compareWant) {
		pending = &compareWant
	}
	opts.PagingPolicy = got.PagingPolicy
	opts.LeaseDuration = got.LeaseDuration
	opts.MaxTransactions = got.MaxTransactions
	opts.AutoRebalance = got.AutoRebalance
	opts.ResolvedCPU = got.CPUProfile
	return pending, nil
}

// clusterTokenFile, in a cluster's shared root, holds the token with which
// the cluster's daemons authenticate one another. Whoever can read the shared
// root administers the cluster: the token needs no other secret, and only the
// root's owner can read it.
const clusterTokenFile = ".ttc-cluster-token"

// minClusterTokenLength is the shortest token a cluster takes.
const minClusterTokenLength = 32

// clusterAuthToken returns the token of the cluster sharedRoot anchors, which
// the cluster's first daemon makes: 32 random bytes, in hexadecimal.
func clusterAuthToken(sharedRoot string) (string, error) {
	path := filepath.Join(sharedRoot, clusterTokenFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return "", fmt.Errorf("making the cluster's token: %w", err)
		}
		// Another daemon of a new cluster may make it first.
		data, err = publishOnce(path, []byte(hex.EncodeToString(secret[:])+"\n"))
	}
	if err != nil {
		return "", fmt.Errorf("reading the cluster's token: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("reading the cluster's token: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the cluster's token %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return "", fmt.Errorf("making the cluster's token readable by its owner alone: %w", err)
		}
	}
	token := strings.TrimSpace(string(data))
	if len(token) < minClusterTokenLength {
		return "", fmt.Errorf("the cluster's token in %s is shorter than %d characters", path, minClusterTokenLength)
	}
	return token, nil
}

// publishOnce makes path hold data, unless another daemon made it first, and
// returns what path holds. No reader sees it partly written: data is written
// to a file of its own, which is then linked to path, as link(2) does only if
// path does not exist.
func publishOnce(path string, data []byte) ([]byte, error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(data)
	if err = errors.Join(err, temp.Sync(), temp.Close()); err != nil {
		return nil, err
	}
	if err := os.Link(temp.Name(), path); err == nil {
		return data, syncDaemonRootDirectory(dir)
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return os.ReadFile(path)
}

func persistClusterRuntimePolicy(sharedRoot string, base daemonImmutableClusterConfig, policy cluster.ClusterRuntimePolicy) error {
	base.PagingPolicy = policy.PagingPolicy
	base.LeaseDuration = policy.LeaseDuration
	base.MaxTransactions = policy.MaxTransactions
	base.AutoRebalance = policy.AutoRebalance
	return writeDurableJSON(filepath.Join(sharedRoot, ".ttc-cluster-config.json"), base)
}

func reconcileClusterRuntimePolicy(node *cluster.AuthorityNode, sharedRoot string, base daemonImmutableClusterConfig, pending *daemonImmutableClusterConfig) (cluster.ClusterRuntimePolicy, error) {
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
	if effective.Revision == 0 || (changeRequested && !equalClusterRuntimePolicy(effective, desired)) {
		catalog, err := cluster.NewAuthorityCatalog(node)
		if err != nil {
			return cluster.ClusterRuntimePolicy{}, err
		}
		configured, err := catalog.ConfigureClusterPolicy(effective.Revision, desired)
		if err != nil {
			return cluster.ClusterRuntimePolicy{}, fmt.Errorf("configuring cluster operational policy: %w", err)
		}
		effective = *configured
	}
	if err := effective.Validate(); err != nil {
		return cluster.ClusterRuntimePolicy{}, fmt.Errorf("the authority's cluster operational policy: %w", err)
	}
	if err := persistClusterRuntimePolicy(sharedRoot, base, effective); err != nil {
		return cluster.ClusterRuntimePolicy{}, fmt.Errorf("persisting the cluster operational policy: %w", err)
	}
	return effective, nil
}

// joinAuthorityThroughSeed registers this node with the authority's holder,
// seed, following its redirects should another node hold it by then.
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
			// Whole-cluster restart commonly launches members concurrently: the
			// holder may not have opened its listener yet. Retry it within the
			// bounded context.
			peer = seed
			seen = make(map[string]bool)
			redirects = 0
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("waiting for the cluster authority's holder to answer: %w", ctx.Err())
			case <-timer.C:
			}
			continue
		}
		var redirect *cluster.AuthorityNotHolderError
		if !errors.As(err, &redirect) {
			return err
		}
		if redirect.ControlURL == "" {
			// The authority is between holders: ask again.
			peer = seed
			seen = make(map[string]bool)
			redirects = 0
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("waiting for the cluster authority's holder: %w", ctx.Err())
			case <-timer.C:
			}
			continue
		}
		redirects++
		if redirects > 5 {
			return errors.New("too many authority redirects while joining cluster")
		}
		key := redirect.HolderID + "\x00" + redirect.ControlURL
		if seen[key] {
			return fmt.Errorf("authority redirect loop through %q", redirect.HolderID)
		}
		seen[key] = true
		var clientErr error
		peer, clientErr = cluster.NewPlainClient(redirect.ControlURL, nodeID, authToken, 2*time.Minute)
		if clientErr != nil {
			return fmt.Errorf("configuring the redirected authority %q: %w", redirect.HolderID, clientErr)
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

type daemonClusterIdentity struct {
	Version      int       `json:"version"`
	ClusterID    string    `json:"cluster_id"`
	RepositoryID string    `json:"repository_id"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

// prepareClusterIdentity returns the ID of the cluster anchored in
// sharedRoot, which its first daemon records with its authority, and binds
// localRoot to it.
func prepareClusterIdentity(sharedRoot, localRoot, repositoryID, nodeID string) (string, error) {
	var identity daemonClusterIdentity
	path := filepath.Join(sharedRoot, "cluster", "authority", "cluster.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		clusterID, idErr := newDaemonRequestID()
		if idErr != nil {
			return "", idErr
		}
		identity = daemonClusterIdentity{Version: 1, ClusterID: clusterID, RepositoryID: repositoryID, CreatedBy: nodeID, CreatedAt: time.Now().UTC()}
		encoded, marshalErr := json.Marshal(identity)
		if marshalErr != nil {
			return "", marshalErr
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", err
		}
		// Another daemon of a new cluster may create it first.
		if data, err = publishOnce(path, encoded); err != nil {
			return "", fmt.Errorf("creating the cluster's identity: %w", err)
		}
	}
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		return "", fmt.Errorf("decoding the cluster's identity: %w", err)
	}
	if identity.Version != 1 || !transactionIDPatternForDaemon(identity.ClusterID) || identity.RepositoryID != repositoryID {
		return "", errors.New("shared TTC_ROOT has an incompatible cluster identity")
	}
	localDir := filepath.Join(localRoot, "cluster")
	if err := os.MkdirAll(localDir, 0700); err != nil {
		return "", err
	}
	localPath := filepath.Join(localDir, "cluster-id")
	if local, err := os.ReadFile(localPath); err == nil {
		if strings.TrimSpace(string(local)) != identity.ClusterID {
			return "", errors.New("TTC_LOCAL_ROOT belongs to another cluster")
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
	// access says who may use the daemon besides its owner.
	access daemonAccess
}

// serveControlOnlyDaemon exposes only the local Unix API. It never opens a TCP
// listener and never registers a cluster node, so compute nodes cannot select
// it for placement or initiate a connection to it. While no compute daemon
// runs, it runs a control holder, which listens only on loopback.
func serveControlOnlyDaemon(socket, sharedRoot string, control *controlOnlyCluster, access daemonAccess, stdout, stderr io.Writer) error {
	if control == nil {
		return errors.New("control-only cluster is unavailable")
	}
	dir := filepath.Dir(socket)
	if err := secureDaemonDirectory(dir, access, stderr); err != nil {
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
	// No daemon serves this root: remove what outlived the processes it
	// served, among them the journals of a gateway that ended abruptly.
	sweepDaemonFiles(newDaemonLayout(dir), "", nil, stderr)
	if journals, err := filepath.Glob(filepath.Join(dir, ".ttc-control-mutations-*")); err == nil {
		for _, journal := range journals {
			_ = os.RemoveAll(journal)
		}
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
	if err := secureDaemonSocket(socket, access); err != nil {
		return fmt.Errorf("securing daemon socket: %w", err)
	}
	if err := syscall.Flock(int(startLock.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlocking daemon startup: %w", err)
	}
	server := &controlOnlyDaemonServer{listener: listener, cluster: control, root: sharedRoot, localRoot: dir, access: access}
	mutationRoot, err := os.MkdirTemp(dir, ".ttc-control-mutations-")
	if err != nil {
		return fmt.Errorf("creating control-only mutation journal: %w", err)
	}
	defer os.RemoveAll(mutationRoot)
	server.mutationRoot = mutationRoot
	server.portIngress = newRoutedPortIngressManager(controlOnlyPortIngressRouter{cluster: control, root: sharedRoot})
	defer server.portIngress.Close()
	holderDir := controlHolderDir(dir)
	if err := os.MkdirAll(holderDir, 0700); err != nil {
		return fmt.Errorf("creating the control holder's directory: %w", err)
	}
	control.holder = &controlHolder{dir: holderDir, sharedRoot: sharedRoot, nodeID: control.nodeID, stderr: stderr}
	defer control.holder.stop()
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
	go func() {
		ticker := time.NewTicker(cluster.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-discoveryDone:
				return
			case <-ticker.C:
				control.observePeers()
			}
		}
	}()
	if stdout != nil {
		seed := "none"
		if urls := control.seedURLs(); len(urls) != 0 {
			seed = urls[0]
		}
		fmt.Fprintf(stdout, "ttcd control-only gateway listening on %s (repository=%s, seed=%s)\n", socket, control.repositoryID, seed)
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

// readDaemonRequestHeader reads a request's header line from conn, and the
// files the client passed with it, which come with its first bytes.
func readDaemonRequestHeader(conn net.Conn) (*bufio.Reader, string, []*os.File, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		reader := bufio.NewReader(conn)
		header, err := reader.ReadString('\n')
		return reader, header, nil, err
	}
	first := make([]byte, 4096)
	// Room for one file: the kernel closes any more, and says so.
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, flags, _, err := unixConn.ReadMsgUnix(first, oob)
	if err != nil {
		return nil, "", nil, err
	}
	var files []*os.File
	if oobn != 0 {
		messages, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return nil, "", nil, err
		}
		for index := range messages {
			fds, err := unix.ParseUnixRights(&messages[index])
			if err != nil {
				continue
			}
			for _, fd := range fds {
				files = append(files, os.NewFile(uintptr(fd), "client-archive"))
			}
		}
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return nil, "", files, errors.New("the client passed more files than a request takes")
	}
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(first[:n]), conn))
	header, err := reader.ReadString('\n')
	return reader, header, files, err
}

// daemonArchiveFileCommand reports whether a command takes an archive file in
// place of its stream, and whether it reads the archive: a restore and an
// image load read theirs, and a checkpoint and an image save write theirs.
func daemonArchiveFileCommand(args []string) (takes, reads bool) {
	if len(args) > 1 && args[0] == "image" {
		args = args[1:]
	}
	if len(args) == 0 {
		return false, false
	}
	switch args[0] {
	case "restore", "load":
		return true, true
	case "checkpoint", "save":
		return true, false
	}
	return false, false
}

// daemonRequestArchiveFile returns the archive file a request passed: one
// regular file, for a command that takes one and says it passes it, and none
// otherwise.
func daemonRequestArchiveFile(request daemonRequest, files []*os.File) (*os.File, error) {
	if !request.ArchiveFile {
		if len(files) != 0 {
			return nil, errors.New("the request passed a file it does not name")
		}
		return nil, nil
	}
	if takes, _ := daemonArchiveFileCommand(request.Args); request.Action != "command" || !takes {
		return nil, errors.New("only a checkpoint, a restore, an image load or an image save passes an archive file")
	}
	if len(files) != 1 {
		return nil, fmt.Errorf("the request passed %d files, not its archive", len(files))
	}
	info, err := files[0].Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("the request's archive is not a regular file")
	}
	return files[0], nil
}

// proxyDaemonArchiveFile runs on peer a request that passed its archive file,
// streaming the archive, which a peer cannot be passed: a restore's or a
// load's is read from the file as the command's input, and a checkpoint's or
// a save's written to it from the command's output, a checkpoint's emptied
// again if it fails. The client hears the rest of what the peer says.
func proxyDaemonArchiveFile(peer *cluster.Client, request daemonRequest, archive *os.File, conn net.Conn, writeMu *sync.Mutex) error {
	request.ArchiveFile = false
	var stdin io.Reader = strings.NewReader("")
	var stdout io.Writer = io.NewOffsetWriter(archive, 0)
	if _, reads := daemonArchiveFileCommand(request.Args); reads {
		info, err := archive.Stat()
		if err != nil {
			return err
		}
		request.Stdin = true
		stdin = io.NewSectionReader(archive, 0, info.Size())
		stdout = daemonFrameWriter{conn: conn, mu: writeMu, typ: daemonFrameStdout}
	}
	local, remote := net.Pipe()
	proxied := make(chan error, 1)
	go func() {
		proxied <- peer.ProxyDaemon(context.Background(), remote, remote)
		_ = remote.Close()
	}()
	status, err := exchangeDaemon(local, request, stdin, stdout, daemonFrameWriter{conn: conn, mu: writeMu, typ: daemonFrameStderr})
	_ = local.Close()
	proxyErr := <-proxied
	if request.Args[0] == "checkpoint" && (err != nil || status != 0) {
		err = errors.Join(err, archive.Truncate(0))
	}
	if err != nil {
		return errors.Join(err, proxyErr)
	}
	return writeDaemonStatus(conn, writeMu, status)
}

func (s *controlOnlyDaemonServer) handle(conn net.Conn, serverErrors io.Writer) {
	defer conn.Close()
	socketPeer, err := checkDaemonPeer(conn, s.access)
	if err != nil {
		if serverErrors != nil {
			fmt.Fprintf(serverErrors, "ttcd: rejecting peer: %v\n", err)
		}
		return
	}
	reader, header, files, err := readDaemonRequestHeader(conn)
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
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
	// The compute daemons this daemon forwards the request to take who asked
	// for it from the request, which they hear from their own user: what a
	// member's request says of it, the header this daemon forwards does not.
	requester := requesterOf(socketPeer, request.Requester)
	request.Requester = &requester
	if encoded, err := json.Marshal(&request); err == nil {
		header = string(append(encoded, '\n'))
	} else {
		writeError(err)
		return
	}
	if err := ownerOnlyRefusal(request, requester); err != nil {
		writeError(err)
		return
	}
	if request.Action == "command" {
		if err := memberHostPathRefusal(request.Args, requester); err != nil {
			writeError(err)
			return
		}
	}
	archiveFile, err := daemonRequestArchiveFile(request, files)
	if err != nil {
		writeError(err)
		return
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
			"shared_root":  s.root,
			"access_group": s.access.status(),
			"cluster":      map[string]any{"control_only": true, "repository_id": s.cluster.repositoryID, "seeds": seeds},
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
	peers := s.cluster.responsivePeers()
	if len(peers) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.cluster.refreshPeers(ctx)
		cancel()
		peers = s.cluster.responsivePeers()
	}
	if len(peers) == 0 {
		writeError(errors.New("no compute node of the cluster is running: a compute node that starts takes the cluster over from the shared root"))
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
	if archiveFile != nil {
		if err := proxyDaemonArchiveFile(client, request, archiveFile, conn, &writeMu); err != nil {
			s.cluster.active.Add(1)
			writeError(fmt.Errorf("proxying through compute seed %s: %w", peer.url, err))
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
			refreshed := s.cluster.responsivePeers()
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

// ownerLossWait bounds how long a routed command waits, its node having lost
// it, for the node to answer again: a node restarts within seconds once
// another has taken the cluster's authority over.
const ownerLossWait = 30 * time.Second

// ownerLoss is what a routed command's node losing it has cost so far.
type ownerLoss struct {
	deadline time.Time
	delay    time.Duration
}

// ownerLossRetry reports whether a routed command whose node ended its
// connection with err before answering it goes to its node again, and waits
// before it. A command with a request ID does, which the owner's journal runs
// once, without streamed input, until ownerLossWait passes; but not one whose
// node, chosen for it, refused the connection, which goes to another node.
func ownerLossRetry(request daemonRequest, automaticPlacement bool, err error, loss *ownerLoss) bool {
	if request.RequestID == "" || request.streamsClientFrames() {
		return false
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, syscall.ECONNRESET) &&
		!errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.EPIPE) {
		return false
	}
	if automaticPlacement && errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	now := time.Now()
	if loss.deadline.IsZero() {
		loss.deadline, loss.delay = now.Add(ownerLossWait), 100*time.Millisecond
	}
	if now.After(loss.deadline) {
		return false
	}
	time.Sleep(loss.delay)
	loss.delay = min(2*loss.delay, time.Second)
	return true
}

// staleRouteFilter holds back the first frame of a routed command's answer
// until it knows whether the answer is the owner's refusal of a stale route:
// that refusal, and the status after it, it keeps from the client, and
// reports; any other answer it passes on whole.
type staleRouteFilter struct {
	next    io.Writer
	held    []byte
	passing bool
	refused bool
}

func (f *staleRouteFilter) Write(p []byte) (int, error) {
	if f.passing {
		return f.next.Write(p)
	}
	if f.refused {
		return len(p), nil
	}
	f.held = append(f.held, p...)
	refusal, decided := staleRouteFrames(f.held)
	if !decided {
		return len(p), nil
	}
	if refusal {
		f.refused = true
		return len(p), nil
	}
	f.passing = true
	held := f.held
	f.held = nil
	if err := writeDaemonBytes(f.next, held); err != nil {
		return 0, err
	}
	return len(p), nil
}

// release passes on what the filter holds, the refusal it reported among it,
// which the routed command's answer ended with.
func (f *staleRouteFilter) release() error {
	held := f.held
	f.held, f.passing, f.refused = nil, true, false
	return writeDaemonBytes(f.next, held)
}

// staleRouteRetry reports whether a routed command that its owner refused as
// routed stale goes again, and waits before it: at once the first time, the
// branch's placement having changed, and then less often, as while another
// command claims the branch, until protocol.StaleRouteWait passes.
func staleRouteRetry(retry *ownerLoss) bool {
	now := time.Now()
	if retry.deadline.IsZero() {
		retry.deadline, retry.delay = now.Add(protocol.StaleRouteWait), 25*time.Millisecond
		return true
	}
	if now.After(retry.deadline) {
		return false
	}
	time.Sleep(retry.delay)
	retry.delay = min(2*retry.delay, time.Second)
	return true
}

// staleRouteFrames reports whether data, the start of a routed command's
// answer, is the owner's refusal of a stale route and the status that ends
// it, and whether it has enough of the answer to tell.
func staleRouteFrames(data []byte) (refusal, decided bool) {
	const header = 5
	if len(data) < header {
		return false, false
	}
	length := int(binary.BigEndian.Uint32(data[1:header]))
	prefix := protocol.StaleRouteRefusal
	if data[0] != daemonFrameStderr || length < len(prefix) || length > 4096 {
		return false, true
	}
	if len(data) < header+len(prefix) {
		return false, false
	}
	if string(data[header:header+len(prefix)]) != prefix {
		return false, true
	}
	status := header + length
	if len(data) < status+header+4 {
		return false, false
	}
	if data[status] != daemonFrameStatus || binary.BigEndian.Uint32(data[status+1:status+header]) != 4 ||
		binary.BigEndian.Uint32(data[status+header:status+header+4]) == 0 {
		return false, true
	}
	return true, true
}

func controlOnlyProxyInput(header string, stdin io.Reader, streaming bool) io.Reader {
	if !streaming {
		return strings.NewReader(header)
	}
	return io.MultiReader(strings.NewReader(header), stdin)
}

// deadOrigins tells which snapshot origins the cluster's authority calls
// dead, retired or abandoned, asking it again after deadOriginRecheck for an
// origin it last called alive: a dead origin stays dead.
type deadOrigins struct {
	authority *cluster.AuthorityCatalog

	mu    sync.Mutex
	alive map[ttcsnapshot.SnapshotID]time.Time
	gone  map[ttcsnapshot.SnapshotID]struct{}
}

// deadOriginRecheck is how long deadOrigins takes an origin it found alive
// for alive.
const deadOriginRecheck = time.Minute

// dead reports whether origin is dead. An origin it cannot ask about is not.
func (d *deadOrigins) dead(origin ttcsnapshot.SnapshotID) bool {
	if d == nil || d.authority == nil {
		return false
	}
	d.mu.Lock()
	if _, ok := d.gone[origin]; ok {
		d.mu.Unlock()
		return true
	}
	if at, ok := d.alive[origin]; ok && time.Since(at) < deadOriginRecheck {
		d.mu.Unlock()
		return false
	}
	d.mu.Unlock()
	state, err := d.authority.AuthorityStateOf(cluster.OriginsQuery(uint64(origin)))
	if err != nil {
		return false
	}
	dead := state.OriginDead(uint64(origin))
	d.mu.Lock()
	defer d.mu.Unlock()
	if dead {
		if d.gone == nil {
			d.gone = make(map[ttcsnapshot.SnapshotID]struct{})
		}
		d.gone[origin] = struct{}{}
		delete(d.alive, origin)
	} else {
		if d.alive == nil {
			d.alive = make(map[ttcsnapshot.SnapshotID]time.Time)
		}
		d.alive[origin] = time.Now()
	}
	return dead
}

func serveDaemon(socket, runtimePlatform string, unsafeNoHostIsolation bool, kernelCompatibility hostKernelCompatibility, dataRoot, root, runsc string, volumeCapability ttcvolume.Capability, stdout, stderr io.Writer) error {
	profile, err := applyStandaloneCPUProfile(dataRoot, "portable", "", runtimePlatform, false)
	if err != nil {
		return err
	}
	return serveDaemonWithCluster(socket, runtimePlatform, profile, cputopology.Topology{Mode: cputopology.Single}, daemonAccess{}, unsafeNoHostIsolation, kernelCompatibility, dataRoot, dataRoot, root, runsc, volumeCapability, nil, gfxHost{unconfined: unsafeNoHostIsolation}, stdout, stderr)
}

func serveDaemonWithCluster(socket, runtimePlatform string, cpuProfile daemonCPUProfile, cpuTopology cputopology.Topology, access daemonAccess, unsafeNoHostIsolation bool, kernelCompatibility hostKernelCompatibility, dataRoot, localRoot, root, runsc string, volumeCapability ttcvolume.Capability, clusterRuntime *daemonClusterRuntime, gfx gfxHost, stdout, stderr io.Writer) (err error) {
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
		// The node keeps the memory chunks of images the shared store has, as
		// it keeps their image layers, until shared GC retires them.
		snapshotData.EnableMemoryChunkCache((&deadOrigins{authority: clusterRuntime.catalog}).dead)
	}
	if err := validatePersistedMemoryGraph(ttcbundle.Store{Root: dataRoot, ActiveRoot: localRoot}); err != nil {
		return fmt.Errorf("validating snapshot-graph branch records: %w", err)
	}
	if clusterRuntime != nil && clusterRuntime.authority != nil {
		defer clusterRuntime.authority.Close()
	}
	dir := filepath.Dir(socket)
	if err := secureDaemonDirectory(dir, access, stderr); err != nil {
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
	// No daemon serves this root: remove what outlived the processes it
	// served.
	sweepDaemonFiles(newDaemonLayout(dir), root, func(id string) error {
		backend := &runtimeBackend{root: root, runsc: runsc, unsafeNoHostIsolation: runscNoHostIsolation(unsafeNoHostIsolation)}
		_, err := backend.command("delete", "--force", id)
		return err
	}, stderr)
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socket, err)
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := secureDaemonSocket(socket, access); err != nil {
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
		cpuTopologyMode:       cpuTopology.Mode,
		cpuTopology:           cpuTopologyAnnotation(cpuTopology),
		noCPUIDFaulting:       cpuProfile.NoCPUIDFaulting,
		access:                access,
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
		mutationLocks:         make(map[string]*sync.RWMutex),
	}
	server.portIngress = newPortIngressManager(server)
	defer server.portIngress.Close()
	run := daemonRunDir()
	if run == "" {
		run = filepath.Dir(socket)
	}
	server.gfx = newGFXService(run, filepath.Join(localRoot, "gfx"), gfx, stderr)
	defer server.gfx.Close()
	server.nvproxy = &nvproxyService{platform: runtimePlatform, dir: filepath.Join(localRoot, "nvproxy")}
	if err := compactDaemonMutations(server.mutationRoot(), daemonMutationRetention); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "ttcd: warning: compacting daemon mutation history: %v\n", err)
	}
	var sharedReferences *sharedReferenceService
	if clusterRuntime != nil {
		sharedReferences = newSharedReferenceService(dataRoot, localRoot, clusterRuntime.nodeRecord.NodeID, clusterRuntime.nodeRecord.Incarnation, clusterRuntime.catalog, sharedObjects, snapshotGraph, clusterRuntime.source)
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
		publisher.authority = clusterRuntime.catalog
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
	recoveryStore := ttcbundle.Store{Root: dataRoot, ActiveRoot: localRoot}
	_, recoveryErr := recoveryStore.RecoverContainerRemovals()
	recoveryErr = errors.Join(recoveryErr, recoveryStore.SweepRemovedActiveContainerState())
	unlockRecovery()
	// Removals from now on delete what they move away after they return; the
	// sweep above finds what a stopped daemon had not deleted.
	ttcbundle.RemoveInBackground()
	if recoveryErr != nil && stderr != nil {
		fmt.Fprintf(stderr, "ttcd: warning: recovering removed containers: %v\n", recoveryErr)
	}
	maintenanceDone := make(chan struct{})
	defer close(maintenanceDone)
	var maintenanceTransactions cluster.TransactionCatalog
	if clusterRuntime != nil {
		maintenanceTransactions = clusterRuntime.transactions
	}
	var maintenanceAuthority *cluster.AuthorityCatalog
	if clusterRuntime != nil {
		maintenanceAuthority = clusterRuntime.catalog
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
			if err := retireDaemonNode(clusterRuntime, offline.NodeID, offline.Incarnation); err != nil {
				result = errors.Join(result, fmt.Errorf("retiring the drained node: %w", err))
			} else {
				if err := removeLocalNodeIncarnation(localRoot, offline.NodeID, offline.Incarnation); err != nil {
					result = errors.Join(result, fmt.Errorf("rotating retired node incarnation: %w", err))
				}
			}
		}
		if err := removeCompletedElasticDrainIntent(dataRoot, record.NodeID); err != nil {
			result = errors.Join(result, fmt.Errorf("consuming completed drain intent: %w", err))
		}
		if result == nil {
			// The node's identity is gone. Suppress duplicate work in the
			// shutdown defer.
			elasticRecord = nil
		}
		return result
	}
	defer func() {
		// Relinquishing shared-data mutation authority is an authority operation. Stop
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
		if clusterRuntime != nil && clusterRuntime.heartbeat != nil {
			// A node that leaves gives the authority up; one that restarts
			// keeps it, unless another takes it meanwhile.
			if server.cleanStop.Load() {
				releaseClusterAuthority(clusterRuntime)
				if err := clusterRuntime.heartbeat.Leave(); err != nil && stderr != nil {
					fmt.Fprintf(stderr, "ttcd: warning: recording that this node stopped: %v\n", err)
				}
			} else {
				clusterRuntime.heartbeat.Stop()
			}
			clusterRuntime.shared.CloseFiles()
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
	// An authority that changed hands restarts the daemon too; see
	// watchClusterAuthority.
	var authorityChanged atomic.Pointer[error]
	defer func() {
		if cause := authorityChanged.Load(); cause != nil {
			err = errors.Join(*cause, err)
		}
	}()
	if clusterRuntime != nil {
		graphProvider := &graphObjectProvider{
			graph: snapshotGraph, data: snapshotData, authority: clusterRuntime.catalog,
			transactions: clusterRuntime.transactions, nodeID: clusterRuntime.nodeRecord.NodeID,
			incarnation: clusterRuntime.nodeRecord.Incarnation, shared: sharedReferences,
		}
		clusterRuntime.controlPlane.ObjectProvider = graphProvider
		clusterRuntime.controlPlane.SnapshotProvider = graphProvider
		clusterRuntime.controlPlane.SnapshotReferences = graphProvider
		clusterRuntime.controlPlane.SharedReferences = sharedReferences
		clusterRuntime.controlPlane.ReferenceInventory = sharedReferences
		resolver, err := ttcsnapshot.NewResolver(snapshotGraph, snapshotData,
			authorityOriginLocator{authority: clusterRuntime.catalog},
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
			cpuTopology:           server.cpuTopology,
			unsafeNoHostIsolation: runscNoHostIsolation(server.unsafeNoHostIsolation),
			disableHostGSO:        server.disableHostGSO,
			// A cluster whose nodes need no CPUID faulting runs them so.
			disableSyscallPatching: server.noCPUIDFaulting,
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
				authority: clusterRuntime.catalog, graph: targetBackend.snapshotGraph,
				graphData: targetBackend.snapshotData, graphShared: targetBackend.snapshotShared,
				sharedRefs: targetBackend.sharedReferences, peers: targetBackend.distributedSource,
			}
			return publisher.publishVolatileGraphOrigin(ctx, snapshot)
		}
		targetRuntime := &daemonTargetRuntime{
			backend: targetBackend, nodeID: clusterRuntime.controlPlane.Capabilities.NodeID,
			incarnation: clusterRuntime.nodeRecord.Incarnation, directory: clusterRuntime.directory,
			store: clusterRuntime.controlPlane.Store, authority: clusterRuntime.catalog, done: clusterDone,
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
			registered, err := registerClusterNodeWithRetry(clusterRuntime.directory, clusterRuntime.nodeRecord, clusterRuntime.lease, holdsFencedRuntimes(localRoot), stderr)
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
				clusterRuntime.directory, clusterRuntime.catalog, *registered, clusterRuntime.lease,
				stderr, clusterDone, targetBackend, leaseGuardian, fence)
			if err != nil {
				clusterListener.Close()
				return fmt.Errorf("starting elastic cluster heartbeat: %w", err)
			}
			localCacheCleanupStopped = maintainOrphanLocalCacheCleanup(
				clusterRuntime.catalog, targetBackend, clusterRuntime.lease/4, stderr, clusterDone,
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
				maintainSharedGCCoordinator(clusterRuntime.catalog, sharedObjects, sharedReferences, registered.NodeID, registered.Incarnation, stderr, sharedGCDone)
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
			// PID/start-time set against the authority's placement catalog before
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
			// drain and lease expiry own its lifecycle.
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
		go watchClusterAuthority(clusterRuntime, func(cause error) {
			wrapped := fmt.Errorf("%w: %w", errClusterAuthorityChanged, cause)
			authorityChanged.Store(&wrapped)
			_ = listener.Close()
			_ = clusterServer.Close()
		}, func() {
			server.cleanStop.Store(true)
			server.stopOnce.Do(func() { _ = server.listener.Close() })
		}, stderr, clusterDone)
		if elasticRecord != nil {
			go maintainAuthorityExpirations(clusterRuntime.authority, clusterRuntime.catalog, clusterRuntime.lease, stderr, clusterDone)
		}
		resumeDistributedForkMonitors(targetRuntime, targetBackend, clusterRuntime)
		resumeFrozenOriginPublications(targetBackend)
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
// committed from a stale partial view. Nothing dials a control daemon's node,
// which listens on its host's loopback alone: it proves nothing, and nothing
// is proved of it.
func validateClusterTopology(runtime *daemonClusterRuntime, self cluster.NodeRecord) error {
	if runtime == nil || runtime.directory == nil || runtime.source == nil {
		return errors.New("cluster topology services are unavailable")
	}
	if self.Labels[cluster.NodeRoleLabel] == cluster.RoleControl {
		return nil
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
			if record.NodeID == self.NodeID || before[record.NodeID] == "" || record.Labels[cluster.NodeRoleLabel] == cluster.RoleControl {
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

// sharedManifestIdle is how long a daemon keeps a shared manifest cached that
// it does not read: a restore or a publication reads one for a moment, and
// shared GC reads those it counts on each pass.
const sharedManifestIdle = 2 * time.Minute

// cachedGraphStateIdle is how long a daemon keeps the state of a closed
// graph cached that nothing opens again: an operation on a repository opens
// its graph several times in a row, and a dehydrated branch's not for hours.
const cachedGraphStateIdle = 2 * time.Minute

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
		if backend.snapshotShared != nil && backend.snapshotShared.ExpireManifests(sharedManifestIdle) != 0 {
			debug.FreeOSMemory()
		}
		if backend.snapshotData != nil && backend.snapshotGraph != nil {
			if _, err := backend.snapshotData.EvictDeadCachedChunks(backend.snapshotGraph); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: warning: evicting cached chunks of retired origins: %v\n", err)
			}
		}
		err = errors.Join(err, backend.reconcileProviderClaims())
		if err != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: reconciling distributed graph references: %v\n", err)
		}
		if backingErr != nil && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: reconciling consumed runtime memory backing: %v\n", backingErr)
		}
	}
}

// reconcileDistributedGraphReferencesAtStartup waits for the cluster
// authority's holder, volatile providers, and shared-reference coordinator to become
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

// waitDaemonWorker keeps an unavailable authority holder from turning an ordinary
// startup failure or daemon stop into an immortal half-started process. Some
// catalog interfaces predate context-aware reads, so a worker may be blocked
// inside a linearizable barrier even after clusterDone closes. The process is
// already committed to exit at this point; bounding the join lets subsequent
// deferred cleanup close the authority and local stores.
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
func registerClusterNodeWithRetry(directory cluster.NodeCatalog, record cluster.NodeRecord, lease time.Duration, wait bool, stderr io.Writer) (*cluster.NodeRecord, error) {
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
		time.Sleep(100 * time.Millisecond)
	}
}

// retireDaemonNode removes the record of the node nodeID, whose
// incarnation committed itself offline, from the authority.
func retireDaemonNode(runtime *daemonClusterRuntime, nodeID, incarnation string) error {
	if runtime == nil || runtime.catalog == nil {
		return errors.New("node retirement is unavailable")
	}
	record, err := runtime.directory.Lookup(nodeID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading retiring node authority: %w", err)
	}
	if record.Incarnation != incarnation || record.State != cluster.NodeOffline {
		return errors.New("node must commit its exact incarnation offline before identity retirement")
	}
	if _, err := runtime.catalog.RemoveNode(nodeID, incarnation, record.Revision); err != nil {
		return fmt.Errorf("removing retired node identity: %w", err)
	}
	return nil
}

func maintainRepositories(dataRoot, localRoot, runtimeRoot, runsc string, publications *ttcstorage.Queue, transactions cluster.TransactionCatalog, authority *cluster.AuthorityCatalog, snapshotGraph *ttcsnapshot.Store, snapshotData *ttcsnapshot.LocalDataStore, standalone bool, stderr io.Writer, done <-chan struct{}, lock func() func(), busy func() bool) {
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
		if ttcsnapshot.ExpireGraphStates(cachedGraphStateIdle) != 0 {
			// What the cache held is given back at once: the heap it grew
			// to is left for the runtime to return slowly otherwise.
			debug.FreeOSMemory()
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
				authorityState, authorityErr = authority.AuthorityStateOf(coldRootsQuery)
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

func (s *daemonServer) handle(conn net.Conn, serverErrors io.Writer) {
	defer conn.Close()
	socketPeer, err := checkDaemonPeer(conn, s.access)
	if err != nil {
		if serverErrors != nil {
			fmt.Fprintf(serverErrors, "ttcd: rejecting peer: %v\n", err)
		}
		return
	}
	reader, header, files, err := readDaemonRequestHeader(conn)
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
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
	// What the daemons this daemon forwards the request to know of who asked
	// for it, they take from the request.
	requester := requesterOf(socketPeer, request.Requester)
	request.Requester = &requester
	if err := ownerOnlyRefusal(request, requester); err != nil {
		writeError(err)
		return
	}
	if request.Action == "command" {
		if err := memberHostPathRefusal(request.Args, requester); err != nil {
			writeError(err)
			return
		}
	}
	archiveFile, err := daemonRequestArchiveFile(request, files)
	if err != nil {
		writeError(err)
		return
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
			"cpu_topology":             s.cpuTopologyMode,
			"isolation_mode":           daemonIsolationMode(s.unsafeNoHostIsolation),
			"unsafe_no_host_isolation": s.unsafeNoHostIsolation,
			"host_kernel_release":      s.hostKernelRelease,
			"host_gso_enabled":         !s.disableHostGSO,
			"host_cpuid_faulting":      hostSupportsCPUIDFaulting(),
			"access_group":             s.access.status(),
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
			clusterStatus["experimental_no_cpuid_faulting"] = s.noCPUIDFaulting
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
			opts.requester = &requester
			backend := directBackend(opts, strings.NewReader(""), io.Discard,
				daemonFrameWriter{conn: conn, mu: &writeMu, typ: daemonFrameStderr})
			s.configureCommandBackend(backend)
			// As a node command does, the stop takes the lock once no long
			// operation is in progress: one that holds the snapshot handoff
			// gate takes the lock back before it lets the gate go, and the
			// stop's own captures would wait for the gate holding the lock.
			unlock := s.lockDataRootIdle(opts.dataRoot)
			var err error
			if s.clusterCapabilities != nil {
				// A distributed daemon stop is also a node retirement. Reuse the
				// node-stop state machine so the node is cordoned, all authoritative
				// state and peer dependencies are drained, its offline record is
				// committed, and deferred shutdown can give up the authority if it
				// holds it. Merely applying the branch policy here would leave its
				// record ready, and the rest of the cluster waiting for its lease
				// to expire.
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
		request.Args = mainBranchOperands(request.Args)
		handled, status, stdout, stderr, err := s.executeDistributedEachTarget(request)
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
		var lost, stale ownerLoss
		routing := s.routingPlacements()
		claimsRouted := len(request.PlacementClaims) == 0
		if claimsRouted {
			placements := cluster.PlacementCatalog(s.clusterPlacements)
			if routing != nil {
				placements = routing
			}
			owner, request.PlacementClaims, err = placementAfterClaims(s.dataRoot, placements, request.Args)
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
			// Wait outside every lifecycle lock for its atomic authority command, then
			// redo routing so the destination can claim the unplaced branch. This
			// is required even when the requested destination is the former owner:
			// otherwise start can carry a stale running-placement claim across the
			// publication commit and fail at the destination as soon as the authority
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
				if request.AutomaticPlacement && owner == s.clusterCapabilities.NodeID {
					// The entry daemon chose this node by the records of the nodes
					// it last read. If this node has stopped accepting work since,
					// its memory having passed its target, say, another that
					// accepts it takes the command, as the entry daemon would have
					// chosen it.
					if local, lookupErr := s.clusterDirectory.Lookup(owner); lookupErr == nil {
						if refusal := nodeAdmissionRefusal(*local, time.Now()); refusal != nil {
							failedAdmissionNodes[owner] = struct{}{}
							for _, node := range request.Route {
								failedAdmissionNodes[node] = struct{}{}
							}
							records, listErr := s.clusterDirectory.List()
							if listErr != nil {
								writeError(fmt.Errorf("%w; reselecting cluster placement: %w", refusal, listErr))
								return
							}
							selected, selectErr := selectAdmissionNodePreferring(records, *s.clusterCapabilities, time.Now(), failedAdmissionNodes, s.familyRuntimeNodes(request.Args))
							if selectErr != nil {
								writeError(fmt.Errorf("%w; reselecting cluster placement: %w", refusal, selectErr))
								return
							}
							owner = selected.NodeID
							request.PlacementNode = owner
							automaticPlacement = true
						}
					}
				}
			} else {
				records, listErr := s.clusterDirectory.List()
				if listErr != nil {
					writeError(fmt.Errorf("selecting cluster placement: %w", listErr))
					return
				}
				selected, selectErr := selectAdmissionNodePreferring(records, *s.clusterCapabilities, time.Now(), nil, s.familyRuntimeNodes(request.Args))
				if selectErr != nil {
					writeError(selectErr)
					return
				}
				owner = selected.NodeID
				request.PlacementNode = owner
				request.AutomaticPlacement = true
				automaticPlacement = true
			}
		}
		for owner != "" && owner != s.clusterCapabilities.NodeID {
			if slices.Contains(request.Route, owner) || slices.Contains(request.Route, s.clusterCapabilities.NodeID) {
				writeError(fmt.Errorf("cluster command routing loop through %v to node %q", request.Route, owner))
				return
			}
			peer, err := s.routedPeer(owner, routing)
			if err != nil {
				writeError(err)
				return
			}
			proxiedRequest := request
			proxiedRequest.Route = append(append([]string(nil), request.Route...), s.clusterCapabilities.NodeID)
			if archiveFile != nil {
				if err := proxyDaemonArchiveFile(peer, proxiedRequest, archiveFile, conn, &writeMu); err != nil {
					writeError(fmt.Errorf("proxying command to node %q: %w", owner, err))
				}
				return
			}
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
			counted := &countingDaemonWriter{writer: conn}
			output := io.Writer(counted)
			var tracked *daemonStatusWriter
			if preparedIngress != nil {
				tracked = &daemonStatusWriter{writer: counted}
				output = tracked
			}
			var staleRoute *staleRouteFilter
			if !request.streamsClientFrames() {
				staleRoute = &staleRouteFilter{next: output}
				output = staleRoute
			}
			err = peer.ProxyDaemon(context.Background(), input, output)
			if err == nil && staleRoute != nil && staleRoute.refused {
				// The owner refused the command, before it did anything, as
				// routed by a placement that another command has changed
				// since: it moved the branch, unplaced it, or is claiming it.
				// The command goes as the branch is placed now, and waits out
				// a claim under way, rather than failing for the other's sake.
				// One that admits an unplaced branch to a node has the entry
				// choose the node, which it no longer does here.
				placements := cluster.PlacementCatalog(s.clusterPlacements)
				if routing != nil {
					placements = routing
				}
				rerouted, claims, routeErr := placementAfterClaims(s.dataRoot, placements, request.Args)
				if routeErr == nil && (rerouted != "" || !daemonCommandNeedsPlacementAdmission(request.Args)) && staleRouteRetry(&stale) {
					owner, request.PlacementClaims = rerouted, claims
					continue
				}
			}
			// What the filter holds goes on before the answer is judged: a
			// node that lost the command after it began answering, which the
			// count of what reached the client tells, is not asked again.
			if staleRoute != nil {
				if releaseErr := staleRoute.release(); err == nil {
					err = releaseErr
				}
			}
			if err == nil {
				if tracked != nil {
					ingressCommitted = tracked.statusSeen && tracked.status == 0
				}
				return
			}
			if counted.bytes == 0 && ownerLossRetry(request, automaticPlacement, err, &lost) {
				// The node lost the command before it answered, as a node does
				// that restarts once another has taken the cluster's authority
				// over. The command goes again, routed anew, under its request
				// ID, which the owner's journal runs once, or answers that it
				// began before the node stopped.
				if s.stderr != nil {
					fmt.Fprintf(s.stderr, "ttcd: warning: node %q lost %v before answering it: %v; routing it again\n", owner, request.Args, err)
				}
				if claimsRouted {
					placements := cluster.PlacementCatalog(s.clusterPlacements)
					if routing != nil {
						placements = routing
					}
					if rerouted, claims, routeErr := daemonCommandPlacementWithCatalog(s.dataRoot, placements, request.Args); routeErr == nil {
						owner, request.PlacementClaims = rerouted, claims
					}
				}
				continue
			}
			if !automaticPlacement || request.Stdin || !errors.Is(err, syscall.ECONNREFUSED) {
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
			selected, selectErr := selectAdmissionNodePreferring(records, *s.clusterCapabilities, time.Now(), failedAdmissionNodes, s.familyRuntimeNodes(request.Args))
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
	opts.requester = &requester
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
	backend.archiveFile = archiveFile
	backend.clientTERM = request.ClientTERM
	backend.progressTerminal = request.StderrTerminal
	backend.progressWidth = request.StderrWidth
	clientCanceled := make(chan struct{})
	var cancelClientOnce sync.Once
	backend.clientCanceled = clientCanceled
	backend.clientGone = terminalInput.clientGone()
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
	listing := daemonCommandListsInventory(request.Args)
	// The locks of the families the command names come first, and the
	// lifecycle lock, shared by a command that changes only its family,
	// after them.
	var families []string
	shared := false
	if !listing {
		families, shared = s.requestFamilies(backend, request.Args, request.PlacementClaims)
	}
	lockLifecycle := func() func() { return s.lockDataRoot(opts.dataRoot) }
	if shared {
		lockLifecycle = func() func() { return s.lockDataRootShared(opts.dataRoot) }
	}
	releaseFamilies := func() {}
	lockFamilies := func() bool {
		if len(families) == 0 {
			return true
		}
		release, ok := s.lockFamilies(opts.dataRoot, families, terminalInput.clientGone())
		releaseFamilies = release
		return ok
	}
	if !lockFamilies() {
		stdinReader.Close()
		return
	}
	backend.heldFamilies = families
	backend.resolvedFamilies = &familySet{}
	if listing {
		// A listing neither waits for the lifecycle lock, which a mutation
		// such as a durable dehydration may hold for minutes, nor holds it
		// while it lists, which would make every mutation wait for it. It
		// reads the records and asks the runtimes without the lock, and
		// repairs nothing; below, it lists again holding the lock only if
		// there is a repair to make and the lock is free.
		holdsLifecycleLock = false
		backend.inventoryReadOnly = true
		rawUnlock = func() {}
	} else if holdsLifecycleLock {
		rawUnlock = lockLifecycle()
	} else {
		rawUnlock = func() {}
	}
	backend.lifecycleLockHeld = holdsLifecycleLock
	backend.mutating = daemonCommandNeedsMutationLock(request.Args)
	backend.commandName = daemonCommandName(request.Args)
	hold := &lifecycleHold{acquire: lockLifecycle, release: rawUnlock}
	unlock := func() {
		hold.unlock()
		releaseFamilies()
	}
	if shared && len(families) == 0 && daemonCommandMakesFamily(request.Args) {
		backend.makingFamily = &familyMaking{
			server: s, dataRoot: opts.dataRoot, hold: hold,
			exclusive: func() func() { return s.lockDataRoot(opts.dataRoot) },
			holding: func(release func()) {
				previous := releaseFamilies
				releaseFamilies = func() {
					previous()
					release()
				}
			},
		}
	}
	if daemonCommandReleasesLifecycleAfterStart(request.Args) {
		backend.lifecycleReady = unlock
	}
	if !daemonCommandKeepsLifecycleLock(request.Args) {
		s.configureLongOperations(backend, hold)
	}
	// A retry after a lost response must consult the durable result before
	// mutable-state validation. The committed command may have removed its own
	// runtime or authority placement, in which case revalidating the original claim
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
	// A command on a family another request's long operation is working on
	// waits, without the lock, for it to finish, as it waited for the lock,
	// unless its client leaves; a node command waits for every one.
	for {
		operation, busy := s.operations.target(request.Args)
		if !busy {
			operation, busy = s.claimedRuntimeOperation(backend, request)
		}
		allIdle := !daemonCommandKeepsLifecycleLock(request.Args) || !s.operations.any()
		if !busy && allIdle {
			break
		}
		unlock()
		idle := true
		if busy {
			idle = s.operations.waitIdleUnless(operation.id, terminalInput.clientGone())
		} else {
			s.operations.waitAllIdle()
		}
		if !idle || !lockFamilies() {
			stdinReader.Close()
			return
		}
		hold.relock()
		if !backend.inventoryReadOnly {
			s.reconcile(backend)
		}
	}
	placements, placementErr := backend.placementCatalog()
	if placementErr != nil && len(request.PlacementClaims) != 0 {
		unlock()
		stdinReader.Close()
		writeError(placementErr)
		return
	}
	claimed, err := s.validateRoutedClaims(opts.dataRoot, backend.nodeID, request.Args, request.PlacementClaims, placements)
	if err != nil {
		unlock()
		stdinReader.Close()
		writeError(err)
		return
	}
	backend.claimedAuthority = claimed
	if err := s.claimUnplacedLifecycleTargets(backend, request.Args); err != nil {
		unlock()
		stdinReader.Close()
		// Another node claimed the branch since the command was routed here,
		// as a start or a rehydration there does: the command, which has done
		// nothing and is journaled nowhere, is refused as routed stale, and
		// its router sends it there. A rehydration failed so, "branch ...
		// belongs to cluster node ...", beside a rehydration on another node.
		var moved *remoteBranchOwnershipError
		if errors.As(err, &moved) {
			err = fmt.Errorf("stale routed command: %w", err)
		}
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
		mutationBackend.clientGone = backend.clientGone
		mutationBackend.archiveFile = backend.archiveFile
		mutationBackend.heldFamilies = families
		mutationBackend.resolvedFamilies = backend.resolvedFamilies
		mutationBackend.claimedAuthority = backend.claimedAuthority
		mutationBackend.makingFamily = backend.makingFamily
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
		s.configureFamilyLocks(mutationBackend)
		if movableDaemonCommand(request, archiveFile != nil) {
			mutationBackend.moves = &commandMoves{}
		}
		status := runCommand(request.Args, mutationBackend.stdout, mutationBackend.stderr, mutationBackend)
		// What the command changed, it resolves anew after.
		mutationBackend.claimedAuthority.discard()
		unlocked, forwarded := false, false
		if owner := mutationBackend.moves.movedTo(); status != 0 && owner != "" && s.clusterCapabilities != nil && owner != s.clusterCapabilities.NodeID {
			// The command's branch went to another node after the command
			// was routed here, as a claim of the branch there or a migration
			// of it takes it, and the command, which failed resolving the
			// branch, did nothing: it goes to the branch's node, whose answer
			// is the command's, journaled here too. Commands failed so,
			// "branch ... belongs to cluster node ...", beside a start of
			// their cold branch on another node.
			unlock()
			unlocked = true
			if forwardedStatus, stdout, stderr, forwardErr := s.forwardMovedCommand(request); forwardErr == nil {
				mutationStdout.Reset()
				mutationStdout.WriteString(stdout)
				mutationStderr.Reset()
				mutationStderr.WriteString(stderr)
				status, forwarded = forwardedStatus, true
			} else {
				fmt.Fprintf(&mutationStderr, "ttcd: sending the command to node %q, where its branch is now: %v\n", owner, forwardErr)
			}
		}
		if !forwarded {
			s.recordCommandEvents(opts.dataRoot, request.Args, status, mutationBackend.resolvedFamilies, request.Requester)
		}
		if status == 0 && !forwarded {
			// Publication runs outside the lifecycle lock. It may unplace a
			// dehydration after the pre-command claim but before rehydrate
			// consumes the durable image. Repeat the idempotent claim at the
			// successful execution boundary before catalog reconciliation.
			if err := s.reclaimCompletedLifecycleTargets(mutationBackend, request.Args); err != nil {
				fmt.Fprintf(&mutationStderr, "ttcd: claiming completed rehydration placement: %v\n", err)
				status = 1
			} else if daemonCommandNeedsMutationLock(request.Args) {
				if err := s.activateMaterializedLifecycleTargets(mutationBackend, request.Args); err != nil {
					fmt.Fprintf(&mutationStderr, "ttcd: warning: activating materialized placement: %v\n", err)
				}
				if families, reconcile := commandReconciledFamilies(request.Args, mutationBackend); reconcile {
					if err := mutationBackend.reconcilePlacementCatalogOf(families...); err != nil {
						fmt.Fprintf(&mutationStderr, "ttcd: warning: reconciling placement catalog: %v\n", err)
					}
				}
			}
		}
		if status == 0 && mutationBackend.daemonStopRequested && !forwarded {
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
		if !unlocked {
			unlock()
		}
		stdinReader.Close()
		if err == nil {
			ingressCommitted = preparedIngress != nil && record.Status == 0
			writeDaemonBufferedResult(conn, &writeMu, record.Stdout, record.Stderr, record.Status)
		} else {
			writeDaemonBufferedResult(conn, &writeMu, mutationStdout.Bytes(), mutationStderr.Bytes(), status)
		}
		if status == 0 && mutationBackend.daemonStopRequested && !forwarded {
			s.stopAfterReply(conn)
		}
		return
	}
	var status int
	if listing {
		status = s.runListing(opts.dataRoot, request.Args, backend)
	} else {
		// A command a router sent here that its first resolution finds
		// the branch of on another node, as an exec whose branch a
		// rehydration there took meanwhile, has done nothing: it is refused
		// as routed stale, before any output, and its router sends it on.
		// An exec failed so, "branch ... belongs to cluster node ...".
		if len(request.Route) != 0 && !request.streamsClientFrames() && archiveFile == nil && branchSelectorCount(request.Args) == 1 {
			backend.moves = &commandMoves{refuse: true}
		}
		status = runCommand(request.Args, backend.stdout, backend.stderr, backend)
	}
	// What the command changed, it resolves anew after.
	backend.claimedAuthority.discard()
	s.recordCommandEvents(opts.dataRoot, request.Args, status, backend.resolvedFamilies, request.Requester)
	if status == 0 {
		if err := s.reclaimCompletedLifecycleTargets(backend, request.Args); err != nil {
			fmt.Fprintf(backend.stderr, "ttcd: claiming completed rehydration placement: %v\n", err)
			status = 1
		} else if daemonCommandNeedsMutationLock(request.Args) {
			if err := s.activateMaterializedLifecycleTargets(backend, request.Args); err != nil {
				fmt.Fprintf(backend.stderr, "ttcd: warning: activating materialized placement: %v\n", err)
			}
			if families, reconcile := commandReconciledFamilies(request.Args, backend); reconcile {
				if err := backend.reconcilePlacementCatalogOf(families...); err != nil {
					fmt.Fprintf(backend.stderr, "ttcd: warning: reconciling placement catalog: %v\n", err)
				}
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

// runListing runs a listing command without the lifecycle lock. If that
// finds a family whose runtime is gone, or the daemon has yet to reconcile,
// and the lock is free, it lists again holding the lock, which reconciles
// and records what it finds; otherwise it gives the first listing.
func (s *daemonServer) runListing(dataRoot string, args []string, backend *runtimeBackend) int {
	var stdout, stderr bytes.Buffer
	status := runCommand(args, &stdout, &stderr, backend)
	if backend.inventoryRepairWanted || !s.reconciledRoots(backend) {
		if unlock, locked := s.tryLockDataRoot(dataRoot); locked {
			backend.inventoryReadOnly, backend.inventoryRepairWanted, backend.lifecycleLockHeld = false, false, true
			s.reconcile(backend)
			stdout.Reset()
			stderr.Reset()
			status = runCommand(args, &stdout, &stderr, backend)
			backend.lifecycleLockHeld = false
			unlock()
		}
	}
	_, _ = backend.stdout.Write(stdout.Bytes())
	_, _ = backend.stderr.Write(stderr.Bytes())
	return status
}

// movableDaemonCommand reports whether request, a journaled command, may go
// to another node once its first resolution of a branch finds the branch
// there (forwardMovedCommand): a lifecycle command of one branch, which takes
// no input and no file from its client.
func movableDaemonCommand(request daemonRequest, archive bool) bool {
	if archive || request.Stdin || request.streamsClientFrames() || len(daemonCommandFamilyTargets(request.Args)) != 1 {
		return false
	}
	args := request.Args
	if args[0] == "container" {
		args = args[1:]
	}
	if args[0] == "cp" || args[0] == "port" {
		return false
	}
	return branchSelectorCount(request.Args) == 1
}

// branchSelectorCount counts the CONTAINER@BRANCH operands of a command.
func branchSelectorCount(args []string) int {
	if len(args) != 0 && args[0] == "container" {
		args = args[1:]
	}
	selectors := 0
	for i, arg := range args {
		if i != 0 && !strings.HasPrefix(arg, "-") && strings.Contains(arg, "@") {
			selectors++
		}
	}
	return selectors
}

// forwardMovedCommand sends request, whose branch its first resolution here
// found on another node, to the node the branch's placement names now, and
// returns that node's answer. It routes the command again if the node
// refuses it as routed stale, as one does if the branch moves on again.
func (s *daemonServer) forwardMovedCommand(request daemonRequest) (int, string, string, error) {
	routing := s.routingPlacements()
	placements := cluster.PlacementCatalog(s.clusterPlacements)
	if routing != nil {
		placements = routing
	}
	lastErr := errors.New("the command was refused as routed stale")
	for attempt := 0; attempt < 3; attempt++ {
		owner, claims, err := placementAfterClaims(s.dataRoot, placements, request.Args)
		if err != nil {
			return 0, "", "", err
		}
		if owner == "" || owner == s.clusterCapabilities.NodeID {
			return 0, "", "", fmt.Errorf("the branch's placement names node %q now", owner)
		}
		if slices.Contains(request.Route, owner) {
			return 0, "", "", fmt.Errorf("cluster command routing loop through %v to node %q", request.Route, owner)
		}
		peer, err := s.routedPeer(owner, routing)
		if err != nil {
			return 0, "", "", err
		}
		forwarded := request
		forwarded.Route = append(append([]string(nil), request.Route...), s.clusterCapabilities.NodeID)
		forwarded.PlacementClaims = claims
		status, stdout, stderr, err := executeRemoteDaemonRequest(context.Background(), peer, forwarded)
		if err != nil {
			return 0, "", "", err
		}
		if status != 0 && strings.HasPrefix(stderr, protocol.StaleRouteRefusal) {
			lastErr = errors.New(strings.TrimSpace(stderr))
			continue
		}
		return status, stdout, stderr, nil
	}
	return 0, "", "", lastErr
}

// claimedRuntimeOperation returns a long operation of another request working
// on the physical runtime of a branch that request's placement claims name,
// where that runtime is apart from the branch's family, as a migration's
// source or a remote fork's container is: the command would wait for the
// operation once it resolved the branch (resolveSelectorWaiting), journaled
// by then, and a migration that moved the branch meanwhile failed it, "branch
// ... belongs to cluster node ...", 10 commands of 6,552 in ten runs of the
// four-client probe with migrations. Waited for before the claims are
// validated, the move has the command refused as routed stale, and its router
// sends it to the branch's new owner.
func (s *daemonServer) claimedRuntimeOperation(backend *runtimeBackend, request daemonRequest) (familyOperation, bool) {
	if len(request.PlacementClaims) == 0 || !daemonCommandNeedsMutationLock(request.Args) || !s.operations.any() {
		return familyOperation{}, false
	}
	placements, err := backend.placementCatalog()
	if err != nil {
		return familyOperation{}, false
	}
	command := daemonCommandName(request.Args)
	for _, claim := range request.PlacementClaims {
		record, err := placements.Lookup(claim.FamilyID, claim.BranchID)
		if err != nil || record.PhysicalContainer == "" || record.PhysicalContainer == claim.FamilyID {
			continue
		}
		if op, ok := s.operations.lookup(record.PhysicalContainer); ok && !op.admits(command) {
			return op, true
		}
	}
	return familyOperation{}, false
}

// validateRoutedClaims validates the placement claims a command was routed
// here with. A claim of one branch is validated against what the authority
// says of the branch when it is resolved, which the command's resolution of
// the branch then reads rather than asking again: one round trip to the
// authority's holder fewer for each command routed to a branch, but for a
// command that may claim the branch first.
func (s *daemonServer) validateRoutedClaims(dataRoot, node string, args []string, claims []daemonPlacementClaim, placements cluster.PlacementCatalog) (*claimedBranchAuthority, error) {
	if len(claims) != 1 || s.distributedSource == nil || s.distributedSource.authority == nil || daemonCommandClaimsUnplaced(args) {
		return nil, validateDaemonPlacementClaimsWithCatalog(dataRoot, node, args, claims, placements)
	}
	claim := claims[0]
	state, err := s.distributedSource.authority.AuthorityStateOf(cluster.ResolutionQuery(claim.FamilyID, claim.FamilyID, claim.BranchID))
	if err != nil {
		return nil, fmt.Errorf("loading placement authority: %w", err)
	}
	if err := validateDaemonPlacementClaimsWithCatalog(dataRoot, node, args, claims, authorityStatePlacements{state: state}); err != nil {
		return nil, err
	}
	return &claimedBranchAuthority{family: claim.FamilyID, branch: claim.BranchID, state: state}, nil
}

// claimedBranchAuthority is what the authority said of a branch of a logical
// family, the placement and authorities that resolving the branch reads, when
// a claim of it was validated.
type claimedBranchAuthority struct {
	family, branch string
	state          *cluster.AuthorityState
	// used is set once a resolution has taken it, or none may.
	used atomic.Bool
}

// take returns what the authority said of family's branch, if c is of that
// branch and nothing has taken it, and leaves it to nothing after: the first
// resolution of a command takes it, and any later one may follow what the
// command changed.
func (c *claimedBranchAuthority) take(family, branch string) *cluster.AuthorityState {
	if c == nil || c.used.Swap(true) || c.family != family || c.branch != branch {
		return nil
	}
	return c.state
}

// discard leaves c to nothing.
func (c *claimedBranchAuthority) discard() {
	if c != nil {
		c.used.Store(true)
	}
}

// authorityStatePlacements looks placements up in an authority state.
type authorityStatePlacements struct {
	cluster.PlacementCatalog
	state *cluster.AuthorityState
}

func (p authorityStatePlacements) Lookup(family, branch string) (*cluster.PlacementRecord, error) {
	return p.state.PlacementOf(family, branch)
}

// routingPlacements is the placement catalog a command is routed to its
// branch's owner by. A lookup of the branch's placement reads with it the node
// record of the node that owned the family's branch the command before was
// routed to, which the peer the command is sent to is made from if it is the
// owner still: reading the owner's record after was another round trip to the
// authority's holder. Every node's record would be as many more bytes of each
// read as the cluster has nodes.
type routingPlacements struct {
	cluster.PlacementCatalog
	server *daemonServer
	nodes  map[string]cluster.NodeRecord
}

// routeHints has, by family, the node the last command routed to a branch of
// it went to.
type routeHints struct {
	mu    sync.Mutex
	owner map[string]string
}

// maxRouteHints bounds routeHints: once it holds that many, it is forgotten.
const maxRouteHints = 4096

func (h *routeHints) get(family string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.owner[family]
}

func (h *routeHints) set(family, owner string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.owner == nil || len(h.owner) >= maxRouteHints {
		h.owner = make(map[string]string)
	}
	h.owner[family] = owner
}

// familyRuntimeNodes is familyRuntimeNodes of the catalog that routes the
// server's commands: a start or rehydration placed automatically goes to one
// of the nodes it returns, if one accepts it.
func (s *daemonServer) familyRuntimeNodes(args []string) map[string]struct{} {
	placements := cluster.PlacementCatalog(s.clusterPlacements)
	if routing := s.routingPlacements(); routing != nil {
		placements = routing
	}
	return familyRuntimeNodes(s.dataRoot, placements, args)
}

// routingPlacements returns the catalog to route a command by, or nil on a
// node whose placements are not the authority's.
func (s *daemonServer) routingPlacements() *routingPlacements {
	if s.distributedSource == nil || s.distributedSource.authority == nil || s.clusterPlacements == nil {
		return nil
	}
	return &routingPlacements{PlacementCatalog: s.clusterPlacements, server: s}
}

func (p *routingPlacements) Lookup(family, branch string) (*cluster.PlacementRecord, error) {
	query := cluster.ResolutionQuery(family, family, branch)
	if hint := p.server.routeHints.get(family); hint != "" {
		query.Nodes = cluster.Only(hint)
	}
	state, err := p.server.distributedSource.authority.AuthorityStateOf(query)
	if err != nil {
		return nil, err
	}
	record, err := state.PlacementOf(family, branch)
	if err != nil {
		return nil, err
	}
	if record.OwnerNode != "" {
		p.server.routeHints.set(family, record.OwnerNode)
	}
	for id, node := range state.Nodes {
		if p.nodes == nil {
			p.nodes = make(map[string]cluster.NodeRecord)
		}
		p.nodes[id] = node
	}
	return record, nil
}

// Branch is the catalog's authority of family's branch, which routing asks
// whether a command may take a branch being made: a created one, or one whose
// rehydration failed, which rm may. Without it, as the catalog it wraps
// offers it but an embedded interface does not, every such command was
// refused, and a family whose rehydration failed could not be removed.
func (p *routingPlacements) Branch(family, branch string) (*cluster.BranchAuthority, error) {
	authority, ok := p.PlacementCatalog.(interface {
		Branch(family, branch string) (*cluster.BranchAuthority, error)
	})
	if !ok {
		return nil, fmt.Errorf("placements without branch authorities: %w", errors.ErrUnsupported)
	}
	return authority.Branch(family, branch)
}

// routedPeer returns the peer to send a command routed by routing, if not
// nil, to owner.
func (s *daemonServer) routedPeer(owner string, routing *routingPlacements) (*cluster.Client, error) {
	if routing != nil {
		if record, ok := routing.nodes[owner]; ok {
			return s.distributedSource.peerOfRecord(record, false)
		}
	}
	return s.distributedSource.peer(owner, false)
}

// reconciledRoots reports whether reconcile has completed for backend's
// roots.
func (s *daemonServer) reconciledRoots(backend *runtimeBackend) bool {
	if backend.dataRoot == "" {
		return true
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	return s.reconciled[backend.dataRoot+"\x00"+backend.root]
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

// commandReconciledFamilies returns the families whose placements the
// reconciliation after a successful command must bring in line with this
// node's runtimes: those the command resolved, if it changes only the
// families it names, or, as run and restore, the family it creates; or none,
// which stands for every family. It also reports whether the command calls
// for a reconciliation at all: an image command changes no placement, and
// every family's reconciliation read the record of each on the node.
func commandReconciledFamilies(args []string, b *runtimeBackend) ([]string, bool) {
	switch name := daemonCommandName(args); {
	case imageCommand(args):
		return nil, false
	case familyScopedCommands[name], name == "migrate", name == "run", name == "create", name == "restore":
		return b.resolvedFamilies.list(), true
	case name == "_auto-remove-now", name == "checkpoint", name == "commit":
		// They change only the family whose branch they resolve, a run
		// --rm's when its branch exits among them: every family's
		// reconciliation read the record of each on the node.
		return b.resolvedFamilies.list(), true
	case name == "_remove-family-record" && len(args) > 1:
		// The family whose record it removed, which it names last: as a
		// removal of the family's branches, it changed no other. Every
		// family's reconciliation read the record of each on the node.
		return args[len(args)-1:], true
	default:
		return nil, true
	}
}

// imageCommand reports whether args are an image command's, which changes
// the image store and no family.
func imageCommand(args []string) bool {
	if len(args) != 0 && args[0] == "container" {
		args = args[1:]
	}
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "pull", "load", "save", "rmi":
		return true
	case "image":
		return len(args) > 1 && (args[1] == "pull" || args[1] == "load" || args[1] == "save" || args[1] == "rm" || args[1] == "remove")
	}
	return false
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
	case "ls", "inspect", "top", "_processes-local":
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
	case "ps", "ls", "inspect", "top", "_processes-local", "_rootfs-sizes-local", "stats", "diff":
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

func (s *daemonServer) dataRootLock(dataRoot string) *sync.RWMutex {
	key := dataRootKey(dataRoot)
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if s.mutationLocks == nil {
		s.mutationLocks = make(map[string]*sync.RWMutex)
	}
	lock := s.mutationLocks[key]
	if lock == nil {
		lock = &sync.RWMutex{}
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
			if terminal != nil {
				terminal.markGone()
			}
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
					_, _ = backend.ensureExecConsoleDrain(target, session, false)
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

// writeDaemonFrame writes a frame, its header and payload in one write where
// writer takes several buffers at once.
func writeDaemonFrame(writer io.Writer, mu *sync.Mutex, typ byte, payload []byte) error {
	if len(payload) > daemonMaxFrame {
		return fmt.Errorf("daemon frame is too large: %d", len(payload))
	}
	header := [5]byte{typ}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	mu.Lock()
	defer mu.Unlock()
	buffers := net.Buffers{header[:], payload}
	_, err := buffers.WriteTo(writer)
	return err
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
	var stale ownerLoss
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
			// The node that owned the branch when the command was routed
			// refused it, before it did anything or journaled it, as another
			// command had moved, unplaced or claimed the branch: a node that
			// routed it to itself has no hop to route it again. The command
			// goes again, under its request ID, waiting out the other.
			if status != 0 && bufferedStdout.Len() == 0 && protocol.StaleRouteAnswer(bufferedStderr.String()) && staleRouteRetry(&stale) {
				attempt--
				continue
			}
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
			buffer := make([]byte, 256<<10)
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

func startDaemonProcess(socket, runtimePlatform string, unsafeNoHostIsolation bool, accessGroup string, clusterOpts daemonClusterOptions) (string, <-chan error, error) {
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
	if accessGroup != "" {
		commandArgs = append(commandArgs, unsafeAccessHostGroupOption, accessGroup)
	}
	if !clusterOpts.ControlOnly {
		commandArgs = append(commandArgs, "--platform", runtimePlatform, "--cpu-profile", clusterOpts.CPUProfileMode)
		if clusterOpts.CPUFeatures != "" {
			commandArgs = append(commandArgs, "--cpu-features", clusterOpts.CPUFeatures)
		}
		if clusterOpts.NoCPUIDFaulting {
			commandArgs = append(commandArgs, "--experimental-no-cpuid-faulting")
		}
		if clusterOpts.CPUTopologySet {
			commandArgs = append(commandArgs, "--cpu-topology", clusterOpts.CPUTopology)
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
		} else {
			commandArgs = append(commandArgs,
				"--cluster-listen", clusterOpts.Listen,
				"--cluster-node-id", clusterOpts.NodeID,
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
			// ttcd makes up a repository for a new shared root itself, and
			// adopts one another daemon publishes first.
			if !clusterOpts.RepositoryGenerated {
				commandArgs = append(commandArgs, "--cluster-repository-id", clusterOpts.RepositoryID)
			}
			if clusterOpts.Advertise != "" {
				commandArgs = append(commandArgs, "--cluster-advertise", clusterOpts.Advertise)
			}
			if clusterOpts.MemoryTarget != 0 {
				commandArgs = append(commandArgs, "--cluster-memory-target", strconv.FormatUint(clusterOpts.MemoryTarget, 10))
			}
			if clusterOpts.AutoRebalance {
				commandArgs = append(commandArgs, "--cluster-auto-rebalance")
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
		}
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return "", nil, err
	}
	defer devNull.Close()
	logPath := protocol.DaemonLog(filepath.Dir(socket))
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
		if strings.Contains(line, "disabling host GSO") || strings.Contains(line, daemonCPUTopologyWarning) || strings.Contains(line, "this host has no CPUID faulting") {
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
	accessGroup := ""
	if len(args) != 0 && args[0] == "start" {
		startArgs := args[1:]
		for len(startArgs) != 0 {
			if consumed, err := consumeDaemonAccessOption(startArgs, &accessGroup); consumed != 0 || err != nil {
				if err != nil {
					fmt.Fprintf(stderr, "ttc: %v\n", err)
					return 2
				}
				startArgs = startArgs[consumed:]
				continue
			}
			switch {
			case startArgs[0] == "--platform":
				if len(startArgs) < 2 || startArgs[1] == "" {
					fmt.Fprintf(stderr, "ttc: --platform requires %s\n", runtimePlatformFlagUsage())
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
		if err := errors.Join(validateNoCPUIDFaultingOption(clusterOpts), validateCPUTopologyOption(clusterOpts)); err != nil {
			fmt.Fprintf(stderr, "ttc: %v\n", err)
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
		fmt.Fprintf(stderr, "ttc: usage: ttc daemon start [--platform %s] [isolation/volume/cluster options]|status|stop|requests|request inspect|request resolve\n", runtimePlatformFlagUsage())
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
			fmt.Fprintln(stderr, "ttc: warning: "+daemonNoHostIsolationWarning)
		}
		logPath, processDone, err := startDaemonProcess(opts.daemonSocket, opts.runtimePlatform, opts.unsafeNoHostIsolation, accessGroup, clusterOpts)
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
