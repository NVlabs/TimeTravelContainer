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
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	sentryremote "gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcpack"
	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

type distributedSourceRuntime struct {
	capabilities  cluster.Capabilities
	incarnation   string
	peers         map[string]*cluster.Client
	directory     cluster.NodeCatalog
	placements    cluster.PlacementCatalog
	transactions  cluster.TransactionCatalog
	authority     *cluster.RaftCatalog
	authToken     string
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

func (b *runtimeBackend) placementCatalog() (cluster.PlacementCatalog, error) {
	if b != nil && b.distributedSource != nil && b.distributedSource.placements != nil {
		return b.distributedSource.placements, nil
	}
	if b == nil || b.dataRoot == "" {
		return nil, errors.New("placement authority is unavailable")
	}
	return cluster.OpenPlacementStore(filepath.Join(b.dataRoot, "cluster", "placements"))
}

func (b *runtimeBackend) transactionCatalog() (cluster.TransactionCatalog, error) {
	if b != nil && b.distributedSource != nil && b.distributedSource.transactions != nil {
		return b.distributedSource.transactions, nil
	}
	if b == nil || b.dataRoot == "" {
		return nil, errors.New("transaction authority is unavailable")
	}
	return cluster.OpenStore(filepath.Join(b.dataRoot, "cluster", "transactions"))
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
	if record.Capabilities.BuildID != r.capabilities.BuildID {
		return nil, fmt.Errorf("cluster target node %q uses TTC build %q, want exact build %q", nodeID, record.Capabilities.BuildID, r.capabilities.BuildID)
	}
	return r.peerForRecord(*record)
}

// peerForRecord constructs a control-plane client from an already-authorized
// Raft membership record. In particular, Raft leader redirection must use a
// locally applied record: resolving it through directory.Lookup would recurse
// into the same linearizable read that is being redirected.
func (r *distributedSourceRuntime) peerForRecord(record cluster.NodeRecord) (*cluster.Client, error) {
	nodeID := record.NodeID
	r.peerMu.Lock()
	defer r.peerMu.Unlock()
	if cached, ok := r.dynamicPeers[nodeID]; ok && cached.incarnation == record.Incarnation &&
		cached.controlURL == record.ControlURL && cached.serverName == record.ControlServerName {
		return cached.client, nil
	}
	client, err := cluster.NewPlainClient(record.ControlURL, r.capabilities.NodeID, r.authToken, 2*time.Minute)
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

// distributedForkPlacementBackend selects where a fork should execute. An
// empty requested node gives the scheduler authority to choose. Keeping a
// fork local while the source node accepts work preserves resident CoW
// sharing; a pressured or draining source is placed on the least-loaded
// compatible node instead.
type distributedForkPlacementBackend interface {
	selectForkNode(requested string) (node string, remote bool, err error)
}

func (b *runtimeBackend) selectForkNode(requested string) (string, bool, error) {
	if b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		if requested == "" {
			return "", false, nil
		}
		return "", false, errors.New("distributed node placement is not configured")
	}
	now := time.Now()
	if requested != "" {
		record, err := b.distributedSource.directory.Lookup(requested)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", false, fmt.Errorf("cluster node %q is not registered", requested)
			}
			return "", false, fmt.Errorf("looking up cluster node %q: %w", requested, err)
		}
		if !record.AcceptsPlacement(now) {
			return "", false, fmt.Errorf("cluster node %q is not accepting placement (state=%s)", requested, record.State)
		}
		if record.NodeID != b.nodeID {
			if _, err := cluster.Negotiate(b.distributedSource.capabilities, record.Capabilities); err != nil {
				return "", false, fmt.Errorf("cluster node %q is incompatible: %w", requested, err)
			}
		}
		return record.NodeID, record.NodeID != b.nodeID, nil
	}

	local, err := b.distributedSource.directory.Lookup(b.nodeID)
	if err == nil && local.AcceptsPlacement(now) {
		return local.NodeID, false, nil
	}
	records, listErr := b.distributedSource.directory.List()
	if listErr != nil {
		return "", false, fmt.Errorf("listing cluster nodes for fork placement: %w", listErr)
	}
	selected, selectErr := selectAdmissionNode(records, b.distributedSource.capabilities, now)
	if selectErr != nil {
		if err != nil {
			return "", false, fmt.Errorf("local node lookup failed (%v) and automatic fork placement failed: %w", err, selectErr)
		}
		return "", false, fmt.Errorf("automatic fork placement: %w", selectErr)
	}
	return selected.NodeID, selected.NodeID != b.nodeID, nil
}

type distributedPageRelay struct {
	listener             net.Listener
	backend              *runtimeBackend
	source               string
	session              string
	close                sync.Once
	mu                   sync.Mutex
	closed               bool
	commands             map[*exec.Cmd]struct{}
	connections          map[*net.TCPConn]struct{}
	filesystemConfigured bool
}

func newDistributedPageRelay(listener net.Listener, backend *runtimeBackend, source, session string) *distributedPageRelay {
	relay := &distributedPageRelay{
		listener: listener, backend: backend, source: source, session: session,
		commands: make(map[*exec.Cmd]struct{}), connections: make(map[*net.TCPConn]struct{}),
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
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			tcp.Close()
			continue
		}
		r.connections[tcp] = struct{}{}
		r.mu.Unlock()
		// Filesystem sources are donated before memory streams, but remain idle
		// until a guest actually reads an object. Classifying here synchronously
		// would stop Accept at that first idle stream and deadlock the metadata
		// connection behind it.
		go r.serveConnection(tcp)
	}
}

func (r *distributedPageRelay) serveConnection(tcp *net.TCPConn) {
	defer func() {
		r.mu.Lock()
		delete(r.connections, tcp)
		r.mu.Unlock()
	}()
	magic, err := peekDistributedMagic(tcp)
	if err != nil {
		tcp.Close()
		return
	}
	if magic == [4]byte{'T', 'T', 'C', 'F'} {
		r.mu.Lock()
		configured := r.filesystemConfigured
		closed := r.closed
		r.mu.Unlock()
		if closed || !configured {
			tcp.Close()
			return
		}
		file, err := tcp.File()
		tcp.Close()
		if err != nil {
			return
		}
		r.serveFilesystemFile(file)
		return
	}
	file, err := tcp.File()
	tcp.Close()
	if err != nil {
		return
	}
	r.serveFile(file)
}

func peekDistributedMagic(tcp *net.TCPConn) ([4]byte, error) {
	var magic [4]byte
	raw, err := tcp.SyscallConn()
	if err != nil {
		return magic, err
	}
	var socketErr error
	err = raw.Read(func(fd uintptr) bool {
		n, _, recvErr := unix.Recvfrom(int(fd), magic[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		if errors.Is(recvErr, unix.EAGAIN) || errors.Is(recvErr, unix.EWOULDBLOCK) {
			return false
		}
		if recvErr != nil {
			socketErr = recvErr
			return true
		}
		if n != len(magic) {
			socketErr = io.ErrUnexpectedEOF
		}
		return true
	})
	return magic, errors.Join(err, socketErr)
}

func (r *distributedPageRelay) ConfigureFilesystem() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.filesystemConfigured {
		return errors.New("distributed filesystem relay is closed or already configured")
	}
	r.filesystemConfigured = true
	return nil
}

func (r *distributedPageRelay) serveFile(file *os.File) {
	r.serveRunscFile(file, "distributed-serve")
}

func (r *distributedPageRelay) serveFilesystemFile(file *os.File) {
	r.serveRunscFile(file, "distributed-filesystem-serve")
}

func (r *distributedPageRelay) serveRunscFile(file *os.File, action string) {
	defer file.Close()
	timeout := unix.NsecToTimeval((30 * time.Second).Nanoseconds())
	if err := unix.SetsockoptTimeval(int(file.Fd()), unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		return
	}
	if err := unix.SetsockoptTimeval(int(file.Fd()), unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout); err != nil {
		return
	}
	command := runscCommand(r.backend.runsc,
		"--root", r.backend.root, "timetravel", "--distributed-connection-fd=3",
		r.source, action, r.session)
	command.ExtraFiles = []*os.File{file}
	command.Stdin = strings.NewReader("")
	command.Stdout = io.Discard
	var commandError bytes.Buffer
	command.Stderr = &commandError
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if err := command.Start(); err != nil {
		r.mu.Unlock()
		fmt.Fprintf(os.Stderr, "ttcd: distributed %s relay could not start: %v\n", action, err)
		return
	}
	r.commands[command] = struct{}{}
	r.mu.Unlock()
	waitErr := command.Wait()
	if waitErr != nil {
		fmt.Fprintf(os.Stderr, "ttcd: distributed %s relay failed: %v: %s\n", action, waitErr, strings.TrimSpace(commandError.String()))
	}
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
			connections := make([]*net.TCPConn, 0, len(r.connections))
			for connection := range r.connections {
				connections = append(connections, connection)
			}
			r.mu.Unlock()
			for _, connection := range connections {
				_ = connection.Close()
			}
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

// prepareDistributedFilesystem seals metadata for the already-forked volume
// generation into one immutable shared pack and configures the source relay to
// serve only its allowlisted objects. Active object data remains node-local.
func (b *runtimeBackend) prepareDistributedFilesystem(info ttcbundle.ContainerInfo, filesystemBranch, transactionID string, relay *distributedPageRelay) (string, uint64, error) {
	if filesystemBranch == "" {
		return "", 0, errors.New("distributed capture omitted its filesystem generation")
	}
	spec, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return "", 0, err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return "", 0, err
	}
	if len(configs) == 0 {
		return "", 0, nil
	}
	if err := beginColdPackIntent(b.dataRoot, transactionID); err != nil {
		return "", 0, fmt.Errorf("recording distributed filesystem pack intent: %w", err)
	}
	staging := filepath.Join(b.activeDataRoot(), "distributed-filesystem-staging", transactionID)
	if err := os.RemoveAll(staging); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(staging, 0700); err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(staging)
	var repositories []*ttcvolume.Repository
	var dependentBytes uint64
	countedObjects := make(map[string]struct{})
	closeRepositories := func() {
		for _, repository := range repositories {
			_ = repository.Close()
		}
	}
	for _, config := range configs {
		path := filepath.Clean(config.Path)
		repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			closeRepositories()
			return "", 0, err
		}
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(config.Source)))
		destination := filepath.Join(staging, hex.EncodeToString(key[:]))
		_, objects, err := repository.ExportRemoteSnapshot(filesystemBranch, destination)
		if err != nil {
			repository.Close()
			closeRepositories()
			return "", 0, err
		}
		for object, size := range objects {
			identity := repository.Info().ID + ":" + object
			if _, ok := countedObjects[identity]; !ok {
				dependentBytes += uint64(size)
				countedObjects[identity] = struct{}{}
			}
		}
		repositories = append(repositories, repository)
	}
	pack, err := ttcpack.WriteDirectory(staging, filepath.Join(b.dataRoot, "cold"))
	if err != nil {
		closeRepositories()
		return "", 0, err
	}
	if err := bindColdPackIntent(b.dataRoot, transactionID, pack); err != nil {
		closeRepositories()
		return "", 0, fmt.Errorf("binding distributed filesystem pack intent: %w", err)
	}
	// The manager opens repositories only to publish the immutable root
	// descriptors above. Runtime reads, including multihop reads inherited from
	// another node, must be served by the source Sentry: it owns the live
	// repositories and their transient upstream resolvers. Retaining duplicate
	// manager-side handles here both wastes descriptors and loses that resolver
	// state.
	closeRepositories()
	repositories = nil
	if err := relay.ConfigureFilesystem(); err != nil {
		return "", 0, err
	}
	return pack.String(), dependentBytes, nil
}

func (b *runtimeBackend) materializeDistributedFilesystemMetadata(info ttcbundle.ContainerInfo, filesystemBranch, transactionID, packText string, peer bool) (int, error) {
	if packText == "" {
		return 0, nil
	}
	pack, err := ttcpack.ParseID(packText)
	if err != nil {
		return 0, err
	}
	staging := filepath.Join(b.activeDataRoot(), "distributed-filesystem-metadata", transactionID)
	if err := os.RemoveAll(staging); err != nil {
		return 0, err
	}
	if err := ttcpack.Materialize(filepath.Join(b.dataRoot, "cold"), pack, staging); err != nil {
		return 0, err
	}
	defer os.RemoveAll(staging)
	store := b.containerStore()
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return 0, err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return 0, err
	}
	manager, err := ttcvolume.OpenManager(b.activeDataRoot())
	if err != nil {
		return 0, err
	}
	for index := range configs {
		config := &configs[index]
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(config.Source)))
		snapshot := filepath.Join(staging, hex.EncodeToString(key[:]))
		var snapshotInfo ttcvolume.SnapshotInfo
		encodedSnapshot, err := os.ReadFile(filepath.Join(snapshot, "snapshot.json"))
		if err != nil {
			return 0, fmt.Errorf("reading remote repository %q identity: %w", config.Source, err)
		}
		if err := json.Unmarshal(encodedSnapshot, &snapshotInfo); err != nil {
			return 0, fmt.Errorf("decoding remote repository %q identity: %w", config.Source, err)
		}
		if peer {
			var repository *ttcvolume.Repository
			if _, statErr := os.Stat(filepath.Join(config.Path, "volume.json")); statErr == nil {
				repository, err = ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
				if err == nil {
					_, err = repository.ImportRemoteBranchSnapshot(snapshot, filesystemBranch)
				}
			} else if errors.Is(statErr, os.ErrNotExist) {
				// A family can exist in the shared catalog without having any
				// active filesystem on this node. Materialize its first imported
				// branch directly into the node-local canonical repository path.
				repository, _, err = manager.ImportPrivateRemoteSnapshot(config.Path, "peer-"+fmt.Sprint(index), snapshot, filesystemBranch)
			} else {
				err = statErr
			}
			if err != nil {
				return 0, fmt.Errorf("importing peer repository %q metadata: %w", config.Source, err)
			}
			// Remote metadata is intentionally unresolved until the fenced
			// filesystem stream is donated to the Sentry. Verify only the durable
			// branch pointer here; enumerating BranchObjects would defeat lazy
			// startup and cannot succeed before the resolver is attached.
			if _, err := repository.Branch(filesystemBranch); err != nil {
				_ = repository.Close()
				return 0, fmt.Errorf("verifying imported peer repository %q branch %q: %w", config.Source, filesystemBranch, err)
			}
			config.Branch = filesystemBranch
			config.CopyMode = string(manager.Capability().Mode)
			config.RestoreID = snapshotInfo.RepositoryID
			config.Private = true
			closeErr := repository.Close()
			if closeErr != nil {
				return 0, closeErr
			}
			continue
		}
		pathKey := sha256.Sum256([]byte(filepath.Clean(config.Path)))
		destination := filepath.Join(b.activeDataRoot(), "containers", info.ID, "repositories", "remote", hex.EncodeToString(pathKey[:]))
		var repository *ttcvolume.Repository
		if _, err := os.Stat(destination); err == nil {
			repository, err = ttcvolume.OpenRepository(destination, manager.Capability().Mode)
		} else if errors.Is(err, os.ErrNotExist) {
			repository, _, err = manager.ImportPrivateRemoteSnapshot(destination, "remote-"+fmt.Sprint(index), snapshot, filesystemBranch)
		} else {
			return 0, err
		}
		if err != nil {
			return 0, fmt.Errorf("importing remote repository %q metadata: %w", config.Source, err)
		}
		config.Path = destination
		config.Branch = filesystemBranch
		config.CopyMode = string(manager.Capability().Mode)
		config.RestoreID = snapshotInfo.RepositoryID
		config.Private = true
		if err := repository.Close(); err != nil {
			return 0, err
		}
	}
	if err := store.WriteActiveRepositoryMap(info.ID, configs); err != nil {
		return 0, err
	}
	return len(configs), nil
}

// exportDistributedGraph publishes the fork-time memory retained by session
// into a graph-native snapshot origin. The Sentry writes a bounded capture
// stream directly to the daemon; no compatibility repository is created.
func (b *runtimeBackend) exportDistributedGraph(containerID, transactionID string) (_ cluster.GraphHandoffDescriptor, rootNames, rootObjects []string, retErr error) {
	if b.snapshotGraph == nil || b.snapshotData == nil || b.snapshotShared == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, errors.New("distributed graph publication is unavailable")
	}
	origin, sealState, err := b.allocateGraphSnapshotOrigin()
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	shared := false
	manifestRelative := filepath.Join("cluster", "memory-manifests", transactionID+".manifest")
	manifestPath := filepath.Join(b.dataRoot, manifestRelative)
	defer func() {
		if retErr == nil {
			return
		}
		_ = os.Remove(manifestPath)
		if shared {
			_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
		} else {
			_ = b.releaseGraphMemoryRoots(rootNames)
		}
		if err := b.retireFailedGraphSnapshot(origin); err != nil && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: retiring failed live handoff snapshot %d: %v\n", origin, err)
		}
	}()

	reader, writer, err := os.Pipe()
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	type importResult struct {
		capture ttcchunk.GraphImportResult
		err     error
	}
	imported := make(chan importResult, 1)
	prefix := fmt.Sprintf("remote/%d/%s", origin, transactionID)
	go func() {
		capture, importErr := ttcchunk.ImportGraphCapture(reader, b.snapshotGraph, b.snapshotData, origin, prefix)
		_ = reader.Close()
		imported <- importResult{capture: capture, err: importErr}
	}()
	_, exportErr := b.commandWithFiles([]*os.File{writer}, "timetravel", "--dehydration-graph-stream-fd=3", containerID, "distributed-export", transactionID)
	_ = writer.Close()
	outcome := <-imported
	if exportErr != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("streaming live graph memory: %w", exportErr)
	}
	if outcome.err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("importing live graph memory: %w", outcome.err)
	}
	for _, root := range outcome.capture.Roots {
		rootNames = append(rootNames, root.Name)
		rootObjects = append(rootObjects, root.Root.String())
	}
	if len(rootObjects) == 0 {
		return cluster.GraphHandoffDescriptor{}, nil, nil, errors.New("live graph capture returned no roots")
	}
	if err := b.snapshotGraph.Seal(origin, sealState); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("sealing live graph snapshot %d: %w", origin, err)
	}
	adapted, err := ttcchunk.BuildGraphManifest(b.snapshotGraph, outcome.capture.Roots)
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	encoded, _, err := ttcchunk.EncodeManifest(adapted.Manifest)
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	if err := writeDurableDistributedFile(manifestPath, encoded); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph manifest: %w", err)
	}
	if err := b.snapshotShared.PublishHandoff(context.Background(), b.snapshotGraph, origin, b.snapshotData); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph snapshot %d: %w", origin, err)
	}
	if _, _, err := b.snapshotGraph.FreezeSnapshotLedger(origin); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	changes, _, frozen, err := b.snapshotGraph.SnapshotLedgerHandoffChanges(origin, 0)
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	if !frozen {
		return cluster.GraphHandoffDescriptor{}, nil, nil, errors.New("live graph reference handoff did not freeze")
	}
	publisher := &branchPublisher{
		nodeID: b.nodeID, incarnation: b.distributedSource.incarnation,
		authority: b.distributedSource.authority, graphShared: b.snapshotShared,
		sharedRefs: b.sharedReferences, peers: b.distributedSource,
	}
	if err := publisher.publishSharedReferenceLedger(origin, changes); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph reference ledger: %w", err)
	}
	if err := b.distributedSource.authority.PublishSnapshotOrigin(b.nodeID, b.distributedSource.incarnation, uint64(origin)); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph authority: %w", err)
	}
	shared = true
	if err := b.snapshotGraph.MarkShared(origin); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	if err := b.snapshotGraph.UnfreezeSnapshotLedger(origin); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	if err := releasePublishedProviderGraphRoots(b.snapshotGraph, b.snapshotData, origin); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	return cluster.GraphHandoffDescriptor{
		GraphManifest: manifestRelative, GraphSnapshot: uint64(origin),
		GraphRootObjects: strings.Join(rootObjects, ","),
	}, rootNames, rootObjects, nil
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
	if state := normalizeLifecycleState(sourceBranch.State); state == "dehydrated" || state == "dehydrating" {
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
	filesystemPack, filesystemBytes, err := b.prepareDistributedFilesystem(sourceInfo, session.FilesystemBranch, transactionID, relay)
	if err != nil {
		return selector{}, fmt.Errorf("sealing distributed filesystem generation: %w", err)
	}
	descriptor := cluster.RemoteSessionDescriptor{
		Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
		MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
		MaxTransferBytes:       dataPlaneMax,
		FilesystemMetadataPack: filesystemPack, FilesystemBytes: filesystemBytes,
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
	sourceBranch, branchErr := persistentBranchInfo(sourceInfo, source.branch)
	if branchErr != nil {
		return selector{}, branchErr
	}
	if err := b.ensureLocalColdBranch(sourceInfo, sourceBranch); err != nil {
		return selector{}, fmt.Errorf("materializing dehydrated remote-fork source: %w", err)
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

	imageDir := existingMemoryBranchDir(b.activeDataRoot(), sourceInfo.ID, "dehydrated", sourceBranch)
	controlData, err := os.ReadFile(filepath.Join(imageDir, "control.json"))
	if err != nil {
		return selector{}, fmt.Errorf("reading dehydrated source control state: %w", err)
	}
	var capture boot.BranchDehydrateInfo
	if err := json.Unmarshal(controlData, &capture); err != nil {
		return selector{}, fmt.Errorf("decoding dehydrated source control state: %w", err)
	}
	if capture.FilesystemBranch == "" || capture.SnapshotID == 0 || len(capture.MemoryRoots) == 0 {
		return selector{}, errors.New("dehydrated source has no graph-native memory roots")
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
	graphRestore, err := b.openGraphMemoryRestore(capture)
	if err != nil {
		return selector{}, fmt.Errorf("opening dehydrated graph memory: %w", err)
	}
	graphManifest, err := io.ReadAll(graphRestore.manifest)
	graphRestore.close()
	if err != nil {
		return selector{}, fmt.Errorf("reading dehydrated graph manifest: %w", err)
	}
	decodedGraphManifest, err := ttcchunk.DecodeManifest(graphManifest, nil)
	if err != nil {
		return selector{}, fmt.Errorf("decoding dehydrated graph manifest: %w", err)
	}
	graphManifestRelative := filepath.Join("cluster", "memory-manifests", transactionID+".manifest")
	graphManifestPath := filepath.Join(b.dataRoot, graphManifestRelative)
	if err := writeDurableDistributedFile(graphManifestPath, graphManifest); err != nil {
		return selector{}, fmt.Errorf("publishing distributed graph manifest: %w", err)
	}
	defer os.Remove(graphManifestPath)

	childFilesystemBranch := ttcvolume.ScopedBranchID(sourceInfo.ID, transactionID)

	filesystemForks, err := b.forkDehydratedDistributedFilesystems(sourceInfo.ID, capture.FilesystemBranch, childFilesystemBranch)
	if err != nil {
		return selector{}, err
	}
	filesystemCommitted := false
	committedFilesystemForks := 0
	defer func() {
		if filesystemCommitted {
			if !committed {
				_ = b.removeDistributedFilesystemBranch(sourceInfo.ID, childFilesystemBranch)
			}
			return
		}
		for i := len(filesystemForks) - 1; i >= committedFilesystemForks; i-- {
			_ = filesystemForks[i].abort()
		}
		if committedFilesystemForks != 0 {
			_ = b.removeDistributedFilesystemBranch(sourceInfo.ID, childFilesystemBranch)
		}
	}()
	for _, fork := range filesystemForks {
		if err := fork.commit(); err != nil {
			return selector{}, fmt.Errorf("committing distributed filesystem fork: %w", err)
		}
		committedFilesystemForks++
	}
	filesystemCommitted = true
	listener, err := net.Listen("tcp", b.distributedSource.pageListen)
	if err != nil {
		return selector{}, fmt.Errorf("opening dehydrated filesystem range listener: %w", err)
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
	relay := newDistributedPageRelay(listener, &relayBackend, sourceInfo.ID, transactionID)
	relayOwned := true
	defer func() {
		if relayOwned {
			relay.Close()
		}
	}()
	var filesystemToken ttcvolume.RemoteToken
	if _, err := rand.Read(filesystemToken[:]); err != nil {
		return selector{}, err
	}
	filesystemPack, filesystemBytes, err := b.prepareDistributedFilesystem(sourceInfo, childFilesystemBranch, transactionID, relay)
	if err != nil {
		return selector{}, fmt.Errorf("preparing dehydrated filesystem ranges: %w", err)
	}
	var streamBytes uint64
	for _, run := range decodedGraphManifest.Runs {
		streamBytes += uint64(run.Pages) * 4096
	}
	rootObjects := make([]string, len(capture.MemoryRoots))
	for index, root := range capture.MemoryRoots {
		rootObjects[index] = root.Root
	}
	descriptor := cluster.RemoteSessionDescriptor{
		Address: advertise, SnapshotToken: hex.EncodeToString(filesystemToken[:]),
		MetadataBytes: uint64(len(encodedMetadata)), StreamBytes: streamBytes,
		MaxTransferBytes: tx.Negotiated.PagingPolicy.TransferBytes,
		SharedMetadata:   metadataRelative, GraphManifest: graphManifestRelative,
		GraphSnapshot: capture.SnapshotID, GraphRootObjects: rootObjects,
		DurableDehydration: true, FilesystemMetadataPack: filesystemPack, FilesystemBytes: filesystemBytes,
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
	committed = true
	relayOwned = false
	go b.completeDehydratedFilesystemTransfer(peer, transactionID, tx.Revision, sourceInfo.ID, childFilesystemBranch, relay)
	if committedTransaction != nil {
		*committedTransaction = transactionID
	}
	return target, nil
}

func (b *runtimeBackend) completeDehydratedFilesystemTransfer(peer *cluster.Client, transactionID string, revision uint64, containerID, filesystemBranch string, relay *distributedPageRelay) {
	defer relay.Close()
	defer b.removeDistributedFilesystemBranch(containerID, filesystemBranch)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tx, err := peer.Drain(ctx, transactionID, revision)
		cancel()
		if err == nil && tx != nil {
			revision = tx.Revision
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			_, err = peer.MarkIndependentWithMetrics(ctx, transactionID, revision, cluster.TransferMetrics{})
			cancel()
			if err == nil {
				return
			}
		}
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		status, statusErr := peer.Status(ctx, transactionID)
		cancel()
		if statusErr == nil {
			revision = status.Revision
			switch status.State {
			case cluster.TransactionIndependent, cluster.TransactionCanceled, cluster.TransactionFailed:
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (b *runtimeBackend) removeDistributedFilesystemBranch(containerID, branch string) error {
	spec, err := b.containerStore().LoadSpec(containerID)
	if err != nil {
		return err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(configs))
	var result error
	for _, config := range configs {
		if _, ok := seen[config.Path]; ok {
			continue
		}
		seen[config.Path] = struct{}{}
		repository, openErr := ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
		if openErr != nil {
			result = errors.Join(result, openErr)
			continue
		}
		removeErr := repository.RemoveBranch(branch)
		closeErr := repository.Close()
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, removeErr)
		}
		result = errors.Join(result, closeErr)
	}
	return result
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
	sourceBranch, err := persistentBranchInfo(sourceInfo, source.branch)
	if err != nil {
		return nil, err
	}
	if state := normalizeLifecycleState(sourceBranch.State); state == "dehydrated" || state == "dehydrating" {
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
		filesystemPack, filesystemBytes, err := b.prepareDistributedFilesystem(sourceInfo, session.FilesystemBranch, child.transaction, child.relay)
		if err != nil {
			return nil, fmt.Errorf("sealing filesystem generation for target %s: %w", child.target, err)
		}
		descriptors[i] = cluster.RemoteSessionDescriptor{
			Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
			MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
			MaxTransferBytes:       child.dataPlaneMax,
			FilesystemMetadataPack: filesystemPack, FilesystemBytes: filesystemBytes,
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
	handoff, rootNames, rootObjects, err := b.exportDistributedGraph(sourceContainer, transactionID)
	if err != nil {
		b.monitorDistributedFork(peer, sourceContainer, transactionID, revision, relay)
		return
	}
	var tx *cluster.Transaction
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tx, err = peer.Handoff(ctx, transactionID, revision, handoff)
		if err != nil {
			if current, statusErr := peer.Status(ctx, transactionID); statusErr == nil {
				tx = current
				revision = current.Revision
				switch current.State {
				case cluster.TransactionDraining, cluster.TransactionIndependent:
					if current.GraphHandoff != nil && *current.GraphHandoff == handoff {
						err = nil
					} else if current.State == cluster.TransactionDraining && current.GraphHandoff == nil {
						// A concurrent node drain may eagerly materialize the target
						// before this optional graph handoff is bound. Eager drain is
						// already a valid route to independence; do not retry handoff
						// forever merely because the faster strategy lost the race.
						err = nil
					} else if current.State == cluster.TransactionIndependent {
						// The target faulted every page from TCP before handoff.
						cancel()
						_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
						_ = os.Remove(filepath.Join(b.dataRoot, handoff.GraphManifest))
						_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
						relay.Close()
						return
					}
				case cluster.TransactionCanceled, cluster.TransactionFailed:
					cancel()
					_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
					_ = os.Remove(filepath.Join(b.dataRoot, handoff.GraphManifest))
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
	if tx.GraphHandoff == nil {
		// Eager drain won the race, so this source-side sparse publication was
		// never made authoritative and can be reclaimed immediately.
		_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
		_ = os.Remove(filepath.Join(b.dataRoot, handoff.GraphManifest))
	}
	if err := b.completeSharedDistributedFork(peer, sourceContainer, transactionID, tx.Revision, relay); err != nil {
		// Retrying status/independence is safe; the target now has no source
		// reads, while the source retains its snapshot until completion.
		b.monitorHandoffCompletion(peer, sourceContainer, transactionID, tx.Revision, relay)
		return
	}
	_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
	_ = os.Remove(filepath.Join(b.dataRoot, handoff.GraphManifest))
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
		draining, drainErr := peer.Drain(ctx, transactionID, revision)
		cancel()
		if drainErr != nil || draining == nil {
			continue
		}
		revision = draining.Revision
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
		// Independent is normally terminal, but older or interrupted activation
		// recording can leave an authorized target independent yet unactivated.
		// Roll that safe paused state forward before applying the terminal fast
		// path.
		if tx.Terminal() && !(tx.State == cluster.TransactionIndependent && !tx.Activated) {
			continue
		}
		if tx.Identity.TargetNodeID == localNode && (tx.State == cluster.TransactionPublished || tx.State == cluster.TransactionDraining || tx.State == cluster.TransactionIndependent) {
			if info, err := backend.containerStore().LoadDistributedForkTarget(tx.Identity.TransactionID); err == nil {
				if !tx.Activated {
					if placements, openErr := backend.placementCatalog(); openErr == nil {
						placementBranch := tx.Identity.VisibleTargetBranch()
						if tx.Identity.Migration {
							placementBranch = tx.Identity.SourceBranch
						}
						if placement, lookupErr := placements.Lookup(tx.Identity.SourceContainer, placementBranch); lookupErr == nil &&
							placement.OwnerNode == localNode && placement.OwnerIncarnation == targetRuntime.incarnation &&
							(!tx.Identity.Migration || (placement.LastOperationID == tx.Identity.TransactionID && placement.State != cluster.PlacementMigrating)) {
							txCopy := tx
							go recoverTargetActivation(targetRuntime, runtime.controlPlane, &txCopy)
						}
					}
				}
				if tx.State == cluster.TransactionPublished && tx.GraphHandoff != nil {
					txCopy := tx
					go recoverTargetGraphHandoff(targetRuntime, runtime.controlPlane, &txCopy)
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
		if tx.Identity.Migration && tx.State == cluster.TransactionPublished && tx.OwnershipCommitted && !tx.Activated {
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

func recoverTargetActivation(targetRuntime *daemonTargetRuntime, control *cluster.ControlPlane, tx *cluster.Transaction) {
	if targetRuntime == nil || control == nil || control.Store == nil || tx == nil || tx.Activated {
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

func recoverTargetGraphHandoff(targetRuntime *daemonTargetRuntime, control *cluster.ControlPlane, tx *cluster.Transaction) {
	if targetRuntime == nil || control == nil || control.Store == nil || tx == nil || tx.GraphHandoff == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := targetRuntime.Handoff(ctx, tx, *tx.GraphHandoff)
	cancel()
	if err != nil {
		return
	}
	_, _ = control.Store.Advance(tx.Identity.TransactionID, tx.Revision, cluster.TransactionDraining, "", control.Now())
}
