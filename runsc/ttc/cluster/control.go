// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

const (
	controlReservePath         = "/v1/forks/reserve"
	controlForkPrefix          = "/v1/forks/"
	controlProxyPath           = "/v1/daemon"
	controlObjectPrefix        = "/v1/objects/"
	controlSnapshotPrefix      = "/v1/snapshots/"
	controlSharedLedgerPrefix  = "/v1/shared-ledger/"
	controlReferenceClaimsPath = "/v1/reference-claims"
	controlTopologyProbePath   = "/v1/topology/probe"
	controlTopologyEchoPath    = "/v1/topology/echo"
	controlExecutionRenewPath  = "/v1/execution/renew"
	controlPortProxyPath       = "/v1/ports/connect"
	maxControlBody             = 1 << 20
	controlNodeHeader          = "X-TTC-Node"
	controlObjectLengthHeader  = "X-TTC-Object-Length"
)

// LoadServerTLSConfig constructs a TLS 1.3 configuration that requires a
// client certificate chaining to clientCAFile.
func LoadServerTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	certificate, roots, err := loadPeerTLSMaterial(certFile, keyFile, clientCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
	}, nil
}

// LoadClientTLSConfig constructs a TLS 1.3 configuration with a client
// certificate and strict server-name validation.
func LoadClientTLSConfig(certFile, keyFile, serverCAFile, serverName string) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("cluster TLS server name is empty")
	}
	certificate, roots, err := loadPeerTLSMaterial(certFile, keyFile, serverCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		RootCAs: roots, ServerName: serverName,
	}, nil
}

func loadPeerTLSMaterial(certFile, keyFile, caFile string) (tls.Certificate, *x509.CertPool, error) {
	if certFile == "" || keyFile == "" || caFile == "" {
		return tls.Certificate{}, nil, errors.New("cluster TLS certificate, key, and CA files are required")
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("loading cluster TLS key pair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("reading cluster CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("cluster CA file contains no certificates")
	}
	return certificate, roots, nil
}

// ReserveRequest asks the target to create a hidden transaction. The target
// independently negotiates capabilities rather than trusting a client result.
type ReserveRequest struct {
	SourceCapabilities Capabilities           `json:"source_capabilities"`
	Identity           ForkIdentity           `json:"identity"`
	Family             RemoteFamilyDescriptor `json:"family"`
	// SourcePhysical* identifies the exact node-local provider selected by the
	// source daemon. This is distinct from Identity's stable logical branch:
	// an unplaced cold branch may be materialized into a transaction-scoped
	// cache before it serves a remote fork. The authority retains this identity until
	// SourceNeeded becomes false, fencing local cache reclamation meanwhile.
	// Migrations omit these fields because their source is derived atomically
	// from the placement transition by the authority state machine.
	SourcePhysicalContainer string `json:"source_physical_container,omitempty"`
	SourcePhysicalBranch    string `json:"source_physical_branch,omitempty"`
}

// RemoteFamilyDescriptor carries the small immutable configuration needed to
// reserve a target runtime. It deliberately travels over the peer control
// connection: requiring a target to discover newly-created metadata through a
// shared filesystem makes correctness depend on NFS/Lustre cache convergence.
type RemoteFamilyDescriptor struct {
	Container json.RawMessage `json:"container"`
	Spec      json.RawMessage `json:"spec"`
}

func (d RemoteFamilyDescriptor) validate() error {
	const maxDescriptorBytes = 4 << 20
	if len(d.Container) == 0 || len(d.Spec) == 0 {
		return errors.New("remote family descriptor is incomplete")
	}
	if len(d.Container) > maxDescriptorBytes || len(d.Spec) > maxDescriptorBytes {
		return errors.New("remote family descriptor is too large")
	}
	if !json.Valid(d.Container) || !json.Valid(d.Spec) {
		return errors.New("remote family descriptor contains invalid JSON")
	}
	return nil
}

// CancelRequest uses optimistic revision checking.
type CancelRequest struct {
	Revision uint64 `json:"revision"`
}

// RemoteSessionDescriptor is the secret-bearing handoff from the source
// Sentry page server to the target daemon. It is sent only over the trusted
// cluster control channel and is never stored in the durable transaction record.
type RemoteSessionDescriptor struct {
	Address          string `json:"address"`
	SnapshotToken    string `json:"snapshot_token"`
	MetadataBytes    uint64 `json:"metadata_bytes"`
	StreamBytes      uint64 `json:"stream_bytes"`
	MaxTransferBytes uint64 `json:"max_transfer_bytes"`
	// FilesystemRoots carries one bounded immutable graph root per repository.
	// It does not require writing a shared filesystem object before target
	// execution can begin.
	FilesystemRoots []FilesystemRootDescriptor `json:"filesystem_roots,omitempty"`
	FilesystemBytes uint64                     `json:"filesystem_bytes,omitempty"`
	// MetadataArtifact names an immutable, transaction-scoped kernel metadata
	// object served by Address. Keeping this handoff on the authenticated peer
	// channel avoids relying on cross-client NFS rename visibility before the
	// target can execute.
	MetadataArtifact string `json:"metadata_artifact,omitempty"`
	// DurableDehydration declares that MetadataArtifact is a TTC dehydration
	// envelope rather than a live-fork SessionMetadata envelope.
	DurableDehydration bool `json:"durable_dehydration,omitempty"`
	// GraphArtifact names the transaction-scoped dense restore view served by
	// Address and derived from immutable snapshot-graph roots.
	// Bulk objects remain in their authoritative provider or shared pack and
	// are populated into the target cache only when faulted.
	GraphArtifact      string `json:"graph_artifact,omitempty"`
	GraphArtifactBytes uint64 `json:"graph_artifact_bytes,omitempty"`
	// GraphSnapshot and GraphRootObjects preserve immutable backing identity.
	// GraphArtifact selects a graph-native sparse restore; without a manifest,
	// the roots protect immutable files still referenced by a live page-stream
	// restore after all remote bytes have become resident. Root names are
	// target-local handles reconstructed from the filesystem-branch identity.
	GraphSnapshot    uint64   `json:"graph_snapshot,omitempty"`
	GraphRootObjects []string `json:"graph_root_objects,omitempty"`
	// InternalMounts carries the small TTC-generated files that are bind-mounted
	// into the guest (currently hostname, hosts, and resolv.conf). Their source
	// paths are node-local and therefore cannot be reused by a remote gofer.
	InternalMounts []InternalMountDescriptor `json:"internal_mounts,omitempty"`
	// ExecSessions names managed exec task groups contained in the captured
	// Kernel. The processes themselves remain part of Kernel state.
	ExecSessions []RemoteExecSessionDescriptor `json:"exec_sessions,omitempty"`
	// ExecConsoleLogs names retained host-side output rings that must move with
	// their managed exec sessions. Bytes are streamed from Address rather than
	// embedded in this control descriptor.
	ExecConsoleLogs []RemoteConsoleLogDescriptor `json:"exec_console_logs,omitempty"`
}

// RemoteExecSessionDescriptor is the transport-neutral identity of a managed
// exec task group. PID is a guest virtual PID and is stable across a fork.
type RemoteExecSessionDescriptor struct {
	ID         string    `json:"id"`
	PID        int32     `json:"pid"`
	ConsoleID  string    `json:"console_id"`
	Terminal   bool      `json:"terminal"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	ExitStatus *uint32   `json:"exit_status,omitempty"`
}

// RemoteConsoleLogDescriptor describes one immutable-at-capture console log.
// ExecID identifies the target-side managed exec session; Size fences the
// streamed response and prevents a peer from writing unbounded local data.
type RemoteConsoleLogDescriptor struct {
	ExecID string `json:"exec_id"`
	Size   uint64 `json:"size"`
}

// InternalMountDescriptor is portable content for one TTC-generated mount.
// User bind mounts are deliberately excluded; those follow their own mount
// policy and are never smuggled through the control plane.
type InternalMountDescriptor struct {
	Destination string `json:"destination"`
	Contents    []byte `json:"contents"`
	Mode        uint32 `json:"mode"`
}

// FilesystemRootDescriptor is the path-free filesystem portion of a remote
// handoff. Tree and file objects are fetched from the fenced provider by their
// immutable IDs; the descriptor itself is O(number of mounted repositories).
type FilesystemRootDescriptor struct {
	Source string `json:"source"`
	// RepositoryID identifies the repository catalog that provides Root.
	RepositoryID string `json:"repository_id"`
	// RestoreID is the repository identity encoded in the source Sentry state.
	// A successful restore canonicalizes that state to the target-local
	// repository, so every later handoff advertises its current physical ID
	// rather than extending an alias chain across nodes.
	RestoreID    string `json:"restore_id"`
	BranchID     string `json:"branch_id"`
	Root         string `json:"root"`
	BaseRoot     string `json:"base_root,omitempty"`
	NextInode    uint64 `json:"next_inode"`
	LogicalBytes uint64 `json:"logical_bytes,omitempty"`
}

func (s RemoteSessionDescriptor) validate(negotiated NegotiatedCapabilities) error {
	if len(s.ExecSessions) > 4096 {
		return errors.New("remote session has too many exec sessions")
	}
	seenExecSessions := make(map[string]struct{}, len(s.ExecSessions))
	for _, session := range s.ExecSessions {
		if session.ID == "" || session.PID <= 0 || (session.State != "running" && session.State != "exited") {
			return fmt.Errorf("remote session has invalid exec session %q", session.ID)
		}
		if session.State == "exited" && session.ExitStatus == nil {
			return fmt.Errorf("remote session has exited exec session %q without status", session.ID)
		}
		if _, ok := seenExecSessions[session.ID]; ok {
			return fmt.Errorf("remote session repeats exec session %q", session.ID)
		}
		seenExecSessions[session.ID] = struct{}{}
	}
	if len(s.ExecConsoleLogs) > len(s.ExecSessions) {
		return errors.New("remote session has more exec console logs than exec sessions")
	}
	seenConsoleLogs := make(map[string]struct{}, len(s.ExecConsoleLogs))
	for _, log := range s.ExecConsoleLogs {
		if log.ExecID == "" || log.Size == 0 || log.Size > math.MaxInt64 {
			return fmt.Errorf("remote session has invalid exec console log %q", log.ExecID)
		}
		if _, ok := seenExecSessions[log.ExecID]; !ok {
			return fmt.Errorf("remote session has console log for unknown exec session %q", log.ExecID)
		}
		if _, ok := seenConsoleLogs[log.ExecID]; ok {
			return fmt.Errorf("remote session repeats exec console log %q", log.ExecID)
		}
		seenConsoleLogs[log.ExecID] = struct{}{}
	}
	if len(s.InternalMounts) > 64 {
		return errors.New("remote session has too many internal mounts")
	}
	seenInternalMounts := make(map[string]struct{}, len(s.InternalMounts))
	internalMountBytes := 0
	for _, mount := range s.InternalMounts {
		clean := filepath.Clean(mount.Destination)
		if !filepath.IsAbs(mount.Destination) || clean != mount.Destination || clean == "/" {
			return fmt.Errorf("invalid internal mount destination %q", mount.Destination)
		}
		if _, ok := seenInternalMounts[clean]; ok {
			return fmt.Errorf("remote session repeats internal mount %q", clean)
		}
		seenInternalMounts[clean] = struct{}{}
		if mount.Mode&^0777 != 0 {
			return fmt.Errorf("internal mount %q has invalid mode %#o", clean, mount.Mode)
		}
		internalMountBytes += len(mount.Contents)
		if internalMountBytes > 1<<20 {
			return errors.New("remote session internal mounts exceed 1 MiB")
		}
	}
	host, port, err := net.SplitHostPort(s.Address)
	portNumber, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || host == "" || portErr != nil || portNumber == 0 {
		return fmt.Errorf("invalid remote page address %q", s.Address)
	}
	token, err := hex.DecodeString(s.SnapshotToken)
	if err != nil || len(token) != 16 {
		return errors.New("snapshot token must be 16 bytes encoded as hexadecimal")
	}
	for label, artifact := range map[string]string{"metadata": s.MetadataArtifact, "graph": s.GraphArtifact} {
		if artifact == "" {
			continue
		}
		if len(artifact) > 4096 || strings.ContainsAny(artifact, "/\\") || artifact == "." || artifact == ".." {
			return fmt.Errorf("invalid remote %s artifact name %q", label, artifact)
		}
	}
	if s.MetadataBytes == 0 || s.StreamBytes == 0 {
		return errors.New("remote session has an empty metadata or page stream")
	}
	if s.StreamBytes%negotiated.BasePageSize != 0 {
		return fmt.Errorf("remote page stream size %d is not base-page aligned", s.StreamBytes)
	}
	if s.MaxTransferBytes < negotiated.BasePageSize || s.MaxTransferBytes > negotiated.MaxTransferSize || s.MaxTransferBytes%negotiated.BasePageSize != 0 {
		return fmt.Errorf("remote transfer size %d is outside negotiated bounds", s.MaxTransferBytes)
	}
	if len(s.FilesystemRoots) > 4096 {
		return errors.New("filesystem handoff has too many repository roots")
	}
	seenFilesystemSources := make(map[string]struct{}, len(s.FilesystemRoots))
	var filesystemBytes uint64
	for _, root := range s.FilesystemRoots {
		if root.Source == "" || root.RepositoryID == "" || root.RestoreID == "" || root.BranchID == "" || root.NextInode <= 1 {
			return errors.New("filesystem handoff has an incomplete graph root")
		}
		for label, id := range map[string]string{"repository": root.RepositoryID, "restore": root.RestoreID} {
			decoded, err := hex.DecodeString(id)
			if err != nil || len(decoded) != 16 {
				return fmt.Errorf("filesystem handoff source %q has invalid %s identity %q", root.Source, label, id)
			}
		}
		if _, exists := seenFilesystemSources[root.Source]; exists {
			return fmt.Errorf("filesystem handoff repeats source %q", root.Source)
		}
		seenFilesystemSources[root.Source] = struct{}{}
		object, err := ttcsnapshot.ParseObjectID(root.Root)
		if err != nil || object.Kind != ttcsnapshot.ObjectKindMetadata {
			return fmt.Errorf("filesystem handoff source %q has invalid root %q", root.Source, root.Root)
		}
		if root.BaseRoot != "" {
			base, err := ttcsnapshot.ParseObjectID(root.BaseRoot)
			if err != nil || base.Kind != ttcsnapshot.ObjectKindMetadata {
				return fmt.Errorf("filesystem handoff source %q has invalid base root %q", root.Source, root.BaseRoot)
			}
		}
		if root.LogicalBytes > ^uint64(0)-filesystemBytes {
			return errors.New("filesystem handoff byte count overflows")
		}
		filesystemBytes += root.LogicalBytes
	}
	if len(s.FilesystemRoots) != 0 {
		if s.Address == "" || s.SnapshotToken == "" {
			return errors.New("volatile filesystem graph roots require a peer address and token")
		}
		host, port, err := net.SplitHostPort(s.Address)
		portNumber, portErr := strconv.ParseUint(port, 10, 16)
		if err != nil || host == "" || portErr != nil || portNumber == 0 {
			return fmt.Errorf("invalid filesystem graph address %q", s.Address)
		}
		token, err := hex.DecodeString(s.SnapshotToken)
		if err != nil || len(token) != 16 {
			return errors.New("filesystem graph token must be 16 bytes encoded as hexadecimal")
		}
		if filesystemBytes != s.FilesystemBytes {
			return fmt.Errorf("filesystem handoff reports %d bytes, roots contain %d", s.FilesystemBytes, filesystemBytes)
		}
	}
	if s.GraphArtifact != "" {
		if s.MetadataArtifact == "" || !s.DurableDehydration {
			return errors.New("graph memory restore requires durable peer metadata")
		}
		if s.GraphSnapshot == 0 || len(s.GraphRootObjects) == 0 || len(s.GraphRootObjects) > 4096 {
			return errors.New("graph memory restore requires bounded immutable root identities")
		}
		if s.GraphArtifactBytes == 0 {
			return errors.New("graph memory restore requires a non-empty artifact")
		}
	} else if s.GraphSnapshot != 0 || len(s.GraphRootObjects) != 0 {
		// A live page-stream restore may still carry immutable graph identity.
		// The target retains these roots while pages are fetched from the source,
		// preserving CoW ancestry across a later migration. Only process-free
		// sparse restore requires a shared graph manifest.
		if s.GraphSnapshot == 0 || len(s.GraphRootObjects) == 0 || len(s.GraphRootObjects) > 4096 {
			return errors.New("live graph memory identity requires bounded immutable root identities")
		}
	}
	if s.GraphArtifact == "" && s.GraphArtifactBytes != 0 {
		return errors.New("graph memory artifact size has no artifact")
	}
	if s.MetadataArtifact != "" && s.GraphArtifact == "" {
		return errors.New("peer distributed metadata requires graph memory")
	}
	if s.DurableDehydration && s.MetadataArtifact == "" {
		return errors.New("durable dehydration restore requires peer metadata")
	}
	return nil
}

func (s RemoteSessionDescriptor) digest() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// PrepareRequest asks the target to restore an unpublished, paused container.
type PrepareRequest struct {
	Revision uint64                  `json:"revision"`
	Session  RemoteSessionDescriptor `json:"session"`
}

// GraphHandoffDescriptor identifies immutable graph memory that can replace a
// target's live source connection.
type GraphHandoffDescriptor struct {
	GraphManifest    string `json:"graph_manifest,omitempty"`
	GraphSnapshot    uint64 `json:"graph_snapshot"`
	GraphRootObjects string `json:"graph_root_objects,omitempty"`
}

func (d GraphHandoffDescriptor) validate() error {
	clean := filepath.Clean(d.GraphManifest)
	if filepath.IsAbs(d.GraphManifest) || clean != d.GraphManifest || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid graph handoff manifest path %q", d.GraphManifest)
	}
	if d.GraphSnapshot == 0 {
		return errors.New("graph handoff has no snapshot identity")
	}
	objects := strings.Split(d.GraphRootObjects, ",")
	if len(objects) == 0 || len(objects) > 4096 {
		return errors.New("graph handoff has an invalid root count")
	}
	for _, object := range objects {
		if strings.TrimSpace(object) != object || object == "" {
			return errors.New("graph handoff has an invalid root object")
		}
		id, err := ttcsnapshot.ParseObjectID(object)
		if err != nil || uint64(id.Snapshot) != d.GraphSnapshot {
			return fmt.Errorf("graph handoff root %q does not belong to snapshot %d", object, d.GraphSnapshot)
		}
	}
	return nil
}

// HandoffRequest durably binds and installs a graph replacement reader.
type HandoffRequest struct {
	Revision uint64                 `json:"revision"`
	Graph    GraphHandoffDescriptor `json:"graph"`
}

// RevisionRequest advances a transaction without carrying new secret state.
type RevisionRequest struct {
	Revision uint64           `json:"revision"`
	Metrics  *TransferMetrics `json:"metrics,omitempty"`
}

// PublishRequest controls the initial externally visible target state.
type PublishRequest struct {
	Revision uint64 `json:"revision"`
	Paused   bool   `json:"paused"`
}

// TargetRuntime implements idempotent target-local container operations. Each
// method is keyed by tx.Identity.TransactionID and may be called again after a
// lost response or daemon restart.
type TargetRuntime interface {
	Reserve(context.Context, *Transaction, RemoteFamilyDescriptor) error
	Prepare(context.Context, *Transaction, RemoteSessionDescriptor) error
	Publish(context.Context, *Transaction, bool) error
	Activate(context.Context, *Transaction) error
	Drain(context.Context, *Transaction) error
	Handoff(context.Context, *Transaction, GraphHandoffDescriptor) error
	Cancel(context.Context, *Transaction) error
	MarkIndependent(context.Context, *Transaction) error
}

// PublishedTargetIdentity reports the immutable physical identity created by
// a target runtime. Logical target names are insufficient because TTC assigns
// a random family ID when a remote fork creates a new family.
type PublishedTargetIdentity interface {
	PublishedTarget(*Transaction) (family, branch, physicalContainer, physicalBranch, incarnation string, err error)
}

type controlError struct {
	Error            string `json:"error"`
	NotHolder        bool   `json:"not_holder,omitempty"`
	HolderID         string `json:"holder_id,omitempty"`
	HolderControlURL string `json:"holder_control_url,omitempty"`
	HolderServerName string `json:"holder_server_name,omitempty"`
}

const (
	controlAuthorityProposePath = "/v1/authority/propose"
	controlAuthorityStatePath   = "/v1/authority/state"
	controlAuthorityQueryPath   = "/v1/authority/query"
	controlAuthorityJoinPath    = "/v1/authority/join"
	controlAuthorityRetirePath  = "/v1/authority/retire"
)

// AuthorityProposal deliberately omits a timestamp. The authority's holder
// adds its time immediately before appending the command.
type AuthorityProposal struct {
	RequestID string               `json:"request_id"`
	Kind      AuthorityCommandKind `json:"kind"`
	Payload   json.RawMessage      `json:"payload"`
}

// AuthorityJoinRequest registers a daemon's execution incarnation with the
// authority.
type AuthorityJoinRequest struct {
	NodeID            string       `json:"node_id"`
	Incarnation       string       `json:"incarnation"`
	ControlURL        string       `json:"control_url"`
	ControlServerName string       `json:"control_server_name,omitempty"`
	ClusterID         string       `json:"cluster_id"`
	Capabilities      Capabilities `json:"capabilities"`
	MemoryTargetBytes uint64       `json:"memory_target_bytes,omitempty"`
}

// AuthorityRetireRequest removes the record of a node whose daemon stopped
// cleanly, once it committed its incarnation offline.
type AuthorityRetireRequest struct {
	NodeID      string `json:"node_id"`
	Incarnation string `json:"incarnation"`
}

// TopologyProbeRequest asks an existing member to connect back to a joining
// member. The authenticated request proves joining->existing reachability;
// the nonce-bearing callback proves existing->joining reachability.
type TopologyProbeRequest struct {
	NodeID      string `json:"node_id"`
	Incarnation string `json:"incarnation"`
	ControlURL  string `json:"control_url"`
	Nonce       string `json:"nonce"`
}

type topologyEchoRequest struct {
	ExpectedPeer string `json:"expected_peer"`
	Nonce        string `json:"nonce"`
}

// AuthorityService exposes the authority's proposals, reads and node
// registration over the token-authenticated cluster HTTP endpoint on the
// trusted private cluster network.
type AuthorityService struct {
	Node         *AuthorityNode
	Capabilities Capabilities
	ClusterID    string
}

// serveQuery answers a linearizable read of the records query names.
func (a *AuthorityService) serveQuery(w http.ResponseWriter, r *http.Request, query AuthorityQuery, notHolder func(error) bool) {
	data, err := a.Node.EncodeLinearizable(r.Context(), query)
	if err != nil {
		if notHolder(err) {
			return
		}
		writeControlError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, json.RawMessage(data))
}

func (a *AuthorityService) serveHTTP(w http.ResponseWriter, r *http.Request, peer string) {
	if a == nil || a.Node == nil {
		writeControlError(w, http.StatusServiceUnavailable, "the cluster authority is unavailable")
		return
	}
	notHolder := func(err error) bool {
		var redirect *AuthorityNotHolderError
		if !errors.As(err, &redirect) {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(controlError{
			Error: err.Error(), NotHolder: true, HolderID: redirect.HolderID,
			HolderControlURL: redirect.ControlURL, HolderServerName: redirect.ControlServerName,
		})
		return true
	}
	switch r.URL.Path {
	case controlAuthorityProposePath:
		if r.Method != http.MethodPost {
			writeControlError(w, http.StatusMethodNotAllowed, "authority proposal requires POST")
			return
		}
		var proposal AuthorityProposal
		if err := decodeControlJSON(r.Body, &proposal); err != nil {
			writeControlError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !transactionIDPattern.MatchString(proposal.RequestID) || len(proposal.Payload) > maxControlBody {
			writeControlError(w, http.StatusBadRequest, "invalid authority proposal")
			return
		}
		var payload any = json.RawMessage(proposal.Payload)
		result, err := a.Node.Propose(r.Context(), proposal.RequestID, proposal.Kind, payload)
		if err != nil {
			if notHolder(err) {
				return
			}
			writeControlError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeControlJSON(w, http.StatusOK, result)
	case controlAuthorityStatePath:
		if r.Method != http.MethodGet {
			writeControlError(w, http.StatusMethodNotAllowed, "authority state requires GET")
			return
		}
		a.serveQuery(w, r, WholeAuthority, notHolder)
	case controlAuthorityQueryPath:
		if r.Method != http.MethodPost {
			writeControlError(w, http.StatusMethodNotAllowed, "authority query requires POST")
			return
		}
		var query AuthorityQuery
		if err := decodeControlJSON(r.Body, &query); err != nil {
			writeControlError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.serveQuery(w, r, query, notHolder)
	case controlAuthorityJoinPath:
		if r.Method != http.MethodPost {
			writeControlError(w, http.StatusMethodNotAllowed, "authority join requires POST")
			return
		}
		var request AuthorityJoinRequest
		if err := decodeControlJSON(r.Body, &request); err != nil {
			writeControlError(w, http.StatusBadRequest, err.Error())
			return
		}
		if request.NodeID != peer || request.Capabilities.NodeID != request.NodeID || !transactionIDPattern.MatchString(request.Incarnation) {
			writeControlError(w, http.StatusForbidden, "joining node ID does not match its client certificate")
			return
		}
		if request.ClusterID == "" || request.ClusterID != a.ClusterID {
			writeControlError(w, http.StatusConflict, "joining node belongs to another cluster")
			return
		}
		if _, err := Negotiate(a.Capabilities, request.Capabilities); err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
		state, err := a.Node.LinearizableState(r.Context())
		if err != nil {
			if notHolder(err) {
				return
			}
			writeControlError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if state.ClusterPolicy.LeaseDuration <= 0 {
			writeControlError(w, http.StatusServiceUnavailable, "cluster execution lease policy is unavailable")
			return
		}
		// A join registers the incarnation unless it is registered and live
		// already, as for a retry of the same join, or a member restarting that
		// asks to be admitted again: a registration makes the node Joining, and
		// a late one could demote it once Ready. An incarnation that expired
		// and returns registers again, told apart from its earlier joins by the
		// revision it expired at.
		current, known := state.Nodes[request.NodeID]
		if !known || current.Incarnation != request.Incarnation || current.State == NodeOffline {
			requestID := "join-register-" + request.Incarnation
			if known && current.Incarnation == request.Incarnation {
				requestID = fmt.Sprintf("%s-%d", requestID, current.Revision)
			}
			record := NodeRecord{
				NodeID: request.NodeID, Incarnation: request.Incarnation,
				ControlURL: request.ControlURL, ControlServerName: request.ControlServerName,
				Capabilities:      request.Capabilities,
				MemoryTargetBytes: request.MemoryTargetBytes,
			}
			result, err := a.Node.Propose(r.Context(), requestID, AuthorityNodeRegister, AuthorityNodeRegisterPayload{
				Record: record, LeaseDuration: state.ClusterPolicy.LeaseDuration,
			})
			if err != nil {
				if notHolder(err) {
					return
				}
				writeControlError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			if err := result.Err(); err != nil {
				writeControlError(w, http.StatusConflict, err.Error())
				return
			}
		}
		writeControlJSON(w, http.StatusOK, map[string]any{"node_id": request.NodeID})
	case controlAuthorityRetirePath:
		if r.Method != http.MethodPost {
			writeControlError(w, http.StatusMethodNotAllowed, "authority retirement requires POST")
			return
		}
		var request AuthorityRetireRequest
		if err := decodeControlJSON(r.Body, &request); err != nil {
			writeControlError(w, http.StatusBadRequest, err.Error())
			return
		}
		if request.NodeID != peer {
			writeControlError(w, http.StatusForbidden, "only a node may retire itself")
			return
		}
		state, err := a.Node.LinearizableState(r.Context())
		if err != nil {
			if notHolder(err) {
				return
			}
			writeControlError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if node, ok := state.Nodes[request.NodeID]; ok {
			if node.Incarnation != request.Incarnation || node.State != NodeOffline {
				writeControlError(w, http.StatusConflict, "node must commit its exact incarnation offline before retirement")
				return
			}
			catalog, err := NewAuthorityCatalog(a.Node)
			if err != nil {
				writeControlError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			if _, err := catalog.RemoveNode(node.NodeID, node.Incarnation, node.Revision); err != nil {
				writeControlError(w, http.StatusConflict, err.Error())
				return
			}
		}
		writeControlJSON(w, http.StatusOK, map[string]any{"node_id": request.NodeID})
	default:
		writeControlError(w, http.StatusNotFound, "unknown authority endpoint")
	}
}

// ControlPlane is the token-authenticated target-side HTTP handler on the
// trusted private cluster network.
type ControlPlane struct {
	Capabilities    Capabilities
	Store           TransactionCatalog
	MaxTransactions int
	Now             func() time.Time
	TargetRuntime   TargetRuntime
	Authority       *AuthorityService
	// TopologyState returns linearizable cluster membership. Topology probes
	// must not validate a newly committed joining identity against a stale
	// read.
	TopologyState func(context.Context) (*AuthorityState, error)
	// AuthToken authenticates peers on a trusted plaintext network; without
	// it, or a verified TLS client certificate, every request is refused. It
	// provides admission control only; it does not provide confidentiality.
	AuthToken string
	// CommandProxy forwards the TTC daemon wire protocol after peer
	// authentication. It is optional for non-elastic deployments.
	CommandProxy http.Handler
	// PortProxy opens one branch-local guest TCP connection and bridges it to
	// the authenticated full-duplex request stream.
	PortProxy http.Handler
	// RenewExecution synchronously renews this daemon's node lease in the
	// current epoch. Placement coordinators call it immediately before
	// reserving capacity, closing the window after a takeover in which a
	// target's periodic heartbeat has not run yet.
	RenewExecution func(context.Context) error
	// ObjectProvider serves immutable graph objects owned by this node. Reads
	// are fenced by the provider's current authority and transaction.
	ObjectProvider ImmutableObjectProvider
	// SnapshotProvider serves immutable graph metadata without transferring
	// reference-count authority to the requesting cache.
	SnapshotProvider ImmutableSnapshotProvider
	// SnapshotReferences applies bounded root-reference operations at the
	// unique volatile origin owner. Object and descendant counts never enter
	// the control plane or the authority.
	SnapshotReferences SnapshotReferenceProvider
	// SharedReferences is active only on the daemon holding the current
	// shared-GC lease. It imports a manifest-backed origin and applies the
	// provider's ordered handoff suffix before the authority changes object routing.
	SharedReferences SharedSnapshotReferenceProvider
	// ReferenceInventory exposes this node's durable active root claims so a
	// newly elected shared-GC coordinator can rebuild its disposable local
	// ledger. It carries bounded roots, never pages or file records.
	ReferenceInventory SnapshotReferenceInventoryProvider
	operationMu        sync.Mutex // serializes reservation/quota accounting
	transactionMu      sync.Map   // transaction ID -> *sync.Mutex
}

// ImmutableObjectProvider opens one complete immutable object after checking
// that this daemon still owns its snapshot origin.
type ImmutableObjectProvider interface {
	OpenObject(context.Context, string, string, string) (io.ReadCloser, uint64, error)
}

// ImmutableSnapshotProvider opens the exact metadata catalog for one origin.
type ImmutableSnapshotProvider interface {
	OpenSnapshot(context.Context, string, uint64, string) (io.ReadCloser, uint64, error)
	OpenSnapshotObject(context.Context, string, uint64, string, string) (io.ReadCloser, uint64, error)
}

// SnapshotRootBinding identifies one logical root slot and its immutable root
// object. Slot is stable within an operation, so retries have exactly the same
// provider-owned root name even when several roots select the same object.
type SnapshotRootBinding struct {
	Slot   uint32 `json:"slot"`
	Object string `json:"object"`
}

// SnapshotReferenceProvider owns reference counts for volatile origins.
// Implementations must make retain/release idempotent by operation identity.
type SnapshotReferenceProvider interface {
	RetainSnapshotRoots(context.Context, string, uint64, string, string, []SnapshotRootBinding) error
	ReleaseSnapshotRoots(context.Context, string, uint64, string, string, []SnapshotRootBinding) error
	ApplySnapshotReference(context.Context, string, uint64, string, string, string, bool) error
	PublishSnapshot(context.Context, string, uint64) error
}

// SharedReferenceMutation is one provider handoff record. Object uses TTC's
// stable textual object identity so the cluster package remains independent
// of the graph implementation.
type SharedReferenceMutation struct {
	Sequence  uint64 `json:"sequence"`
	Operation string `json:"operation"`
	Name      string `json:"name"`
	Object    string `json:"object"`
	Retain    bool   `json:"retain"`
}

// SharedSnapshotReferenceProvider owns exact reference state while its daemon
// holds the shared-GC lease.
type SharedSnapshotReferenceProvider interface {
	ImportSharedSnapshot(context.Context, string, uint64) error
	ApplySharedSnapshotReference(context.Context, string, uint64, string, string, string, bool) error
	ApplySharedSnapshotHandoff(context.Context, string, uint64, uint64, []SharedReferenceMutation) error
	FlushSharedSnapshot(context.Context, string, uint64) error
}

// SnapshotReferenceClaim is one currently active root batch retained by this
// node. Operation and slots are stable idempotency identities.
type SnapshotReferenceClaim struct {
	Origin    uint64                `json:"origin"`
	Operation string                `json:"operation"`
	Roots     []SnapshotRootBinding `json:"roots"`
}

// SnapshotReferenceInventory binds claims to the exact daemon incarnation
// that produced them. A node name alone is insufficient after restart: a
// delayed response from the previous incarnation must not authorize GC.
type SnapshotReferenceInventory struct {
	NodeID      string                   `json:"node_id"`
	Incarnation string                   `json:"incarnation"`
	Claims      []SnapshotReferenceClaim `json:"claims,omitempty"`
}

// SnapshotReferenceInventoryProvider reports local durable claims during rare
// coordinator rebuilds. Ordinary reads, writes, forks, and faults never call
// this interface.
type SnapshotReferenceInventoryProvider interface {
	SnapshotReferenceInventory(context.Context, string) (*SnapshotReferenceInventory, error)
}

type sharedHandoffRequest struct {
	From      uint64                    `json:"from"`
	Mutations []SharedReferenceMutation `json:"mutations"`
}

type snapshotReferenceRequest struct {
	Transaction string                `json:"transaction"`
	Operation   string                `json:"operation"`
	Action      string                `json:"action"`
	Roots       []SnapshotRootBinding `json:"roots"`
	Name        string                `json:"name,omitempty"`
	Object      string                `json:"object,omitempty"`
	Retain      bool                  `json:"retain,omitempty"`
}

func (c *ControlPlane) lockTransaction(id string) func() {
	value, _ := c.transactionMu.LoadOrStore(id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ServeHTTP serves the versioned reservation/status/cancel API. TTC's daemon
// uses plaintext HTTP on a trusted private network and requires AuthToken;
// TLS remains usable by embedders of this package.
func (c *ControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c.Store == nil || c.Now == nil || c.MaxTransactions <= 0 {
		writeControlError(w, http.StatusServiceUnavailable, "cluster control plane is not configured")
		return
	}
	peer, err := verifiedPeerNodeID(r, c.AuthToken)
	if err != nil {
		writeControlError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if r.URL.Path == controlTopologyEchoPath {
		if r.Method != http.MethodPost {
			writeControlError(w, http.StatusMethodNotAllowed, "topology echo requires POST")
			return
		}
		var request topologyEchoRequest
		if err := decodeControlJSON(r.Body, &request); err != nil {
			writeControlError(w, http.StatusBadRequest, err.Error())
			return
		}
		if request.ExpectedPeer != peer || !transactionIDPattern.MatchString(request.Nonce) {
			writeControlError(w, http.StatusForbidden, "invalid topology echo identity or nonce")
			return
		}
		writeControlJSON(w, http.StatusOK, map[string]string{"nonce": request.Nonce})
		return
	}
	if r.URL.Path == controlTopologyProbePath {
		c.serveTopologyProbe(w, r, peer)
		return
	}
	if r.URL.Path == controlExecutionRenewPath {
		if r.Method != http.MethodPost {
			writeControlError(w, http.StatusMethodNotAllowed, "execution authority renewal requires POST")
			return
		}
		if c.RenewExecution == nil {
			writeControlError(w, http.StatusServiceUnavailable, "execution authority renewal is unavailable")
			return
		}
		if err := c.RenewExecution(r.Context()); err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
		writeControlJSON(w, http.StatusOK, map[string]string{"node_id": c.Capabilities.NodeID})
		return
	}
	if r.URL.Path == controlPortProxyPath && r.Method == http.MethodPost {
		if c.PortProxy == nil {
			writeControlError(w, http.StatusNotImplemented, "port tunnel is unavailable")
			return
		}
		c.PortProxy.ServeHTTP(w, r)
		return
	}
	if r.URL.Path == controlProxyPath && r.Method == http.MethodPost {
		if c.CommandProxy == nil {
			writeControlError(w, http.StatusNotImplemented, "daemon command proxy is unavailable")
			return
		}
		c.CommandProxy.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, controlObjectPrefix) {
		c.serveObject(w, r, peer)
		return
	}
	if strings.HasPrefix(r.URL.Path, controlSnapshotPrefix) {
		c.serveSnapshot(w, r, peer)
		return
	}
	if strings.HasPrefix(r.URL.Path, controlSharedLedgerPrefix) {
		c.serveSharedLedger(w, r, peer)
		return
	}
	if r.URL.Path == controlReferenceClaimsPath {
		if r.Method != http.MethodGet {
			writeControlError(w, http.StatusMethodNotAllowed, "snapshot reference inventory requires GET")
			return
		}
		if c.ReferenceInventory == nil {
			writeControlError(w, http.StatusServiceUnavailable, "snapshot reference inventory is unavailable")
			return
		}
		inventory, err := c.ReferenceInventory.SnapshotReferenceInventory(r.Context(), peer)
		if err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
		writeControlJSON(w, http.StatusOK, inventory)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/authority/") {
		if c.Authority == nil {
			writeControlError(w, http.StatusNotImplemented, "the cluster authority is unavailable")
			return
		}
		c.Authority.serveHTTP(w, r, peer)
		return
	}
	if r.URL.Path == controlReservePath {
		c.reserve(w, r, peer)
		return
	}
	if !strings.HasPrefix(r.URL.Path, controlForkPrefix) {
		writeControlError(w, http.StatusNotFound, "unknown cluster endpoint")
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, controlForkPrefix)
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || !transactionIDPattern.MatchString(parts[0]) {
		writeControlError(w, http.StatusBadRequest, "invalid transaction ID")
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		c.status(w, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		c.cancel(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "prepare" && r.Method == http.MethodPost {
		c.prepare(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "publish" && r.Method == http.MethodPost {
		c.publish(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "activate" && r.Method == http.MethodPost {
		c.activate(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "independent" && r.Method == http.MethodPost {
		c.independent(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "drain" && r.Method == http.MethodPost {
		c.drain(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "handoff" && r.Method == http.MethodPost {
		c.handoff(w, r, id, peer)
		return
	}
	writeControlError(w, http.StatusNotFound, "unknown cluster endpoint")
}

func (c *ControlPlane) serveTopologyProbe(w http.ResponseWriter, r *http.Request, peer string) {
	if r.Method != http.MethodPost {
		writeControlError(w, http.StatusMethodNotAllowed, "topology probe requires POST")
		return
	}
	var request TopologyProbeRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.NodeID != peer || !transactionIDPattern.MatchString(request.Incarnation) || !transactionIDPattern.MatchString(request.Nonce) {
		writeControlError(w, http.StatusForbidden, "invalid topology probe identity")
		return
	}
	if c.Authority == nil || c.Authority.Node == nil {
		writeControlError(w, http.StatusServiceUnavailable, "topology authority is unavailable")
		return
	}
	var state *AuthorityState
	var err error
	if c.TopologyState != nil {
		state, err = c.TopologyState(r.Context())
	} else {
		// Retain a local fallback for small in-process control-plane tests. Real
		// clustered daemons always configure the linearizable reader.
		state, err = c.Authority.Node.AppliedState()
	}
	if err != nil {
		writeControlError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	record, ok := state.Nodes[request.NodeID]
	if !ok || record.Incarnation != request.Incarnation || record.ControlURL != request.ControlURL || record.State == NodeOffline {
		writeControlError(w, http.StatusConflict, "joining node topology identity is not committed")
		return
	}
	callback, err := NewPlainClient(request.ControlURL, c.Capabilities.NodeID, c.AuthToken, 5*time.Second)
	if err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	var response map[string]string
	if err := callback.call(r.Context(), http.MethodPost, controlTopologyEchoPath, topologyEchoRequest{ExpectedPeer: c.Capabilities.NodeID, Nonce: request.Nonce}, &response); err != nil {
		writeControlError(w, http.StatusBadGateway, "reverse topology probe failed: "+err.Error())
		return
	}
	if response["nonce"] != request.Nonce {
		writeControlError(w, http.StatusBadGateway, "reverse topology probe returned the wrong nonce")
		return
	}
	writeControlJSON(w, http.StatusOK, map[string]string{"node_id": c.Capabilities.NodeID, "nonce": request.Nonce})
}

func (c *ControlPlane) serveSharedLedger(w http.ResponseWriter, r *http.Request, peer string) {
	if r.Method != http.MethodPost {
		writeControlError(w, http.StatusMethodNotAllowed, "shared reference mutation requires POST")
		return
	}
	if c.SharedReferences == nil {
		writeControlError(w, http.StatusServiceUnavailable, "shared reference coordinator is unavailable")
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, controlSharedLedgerPrefix)
	text, action, ok := strings.Cut(remainder, "/")
	snapshot, err := strconv.ParseUint(text, 10, 64)
	if !ok || snapshot == 0 || err != nil || strconv.FormatUint(snapshot, 10) != text {
		writeControlError(w, http.StatusBadRequest, "invalid shared snapshot path")
		return
	}
	switch action {
	case "import":
		if err := c.SharedReferences.ImportSharedSnapshot(r.Context(), peer, snapshot); err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	case "reference":
		var request snapshotReferenceRequest
		if err := decodeControlJSON(r.Body, &request); err != nil {
			writeControlError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !transactionIDPattern.MatchString(request.Operation) || request.Transaction != "" || request.Action != "" || len(request.Roots) != 0 || request.Name == "" || request.Object == "" {
			writeControlError(w, http.StatusBadRequest, "invalid shared reference mutation")
			return
		}
		if err := c.SharedReferences.ApplySharedSnapshotReference(r.Context(), peer, snapshot, request.Operation, request.Name, request.Object, request.Retain); err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	case "catchup":
		var request sharedHandoffRequest
		if err := decodeControlJSON(r.Body, &request); err != nil {
			writeControlError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(request.Mutations) > 4096 {
			writeControlError(w, http.StatusBadRequest, "shared handoff batch exceeds limit")
			return
		}
		if err := c.SharedReferences.ApplySharedSnapshotHandoff(r.Context(), peer, snapshot, request.From, request.Mutations); err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	case "flush":
		if err := c.SharedReferences.FlushSharedSnapshot(r.Context(), peer, snapshot); err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	default:
		writeControlError(w, http.StatusNotFound, "unknown shared reference operation")
		return
	}
	writeControlJSON(w, http.StatusOK, map[string]any{"snapshot_id": snapshot, "action": action})
}

func (c *ControlPlane) serveObject(w http.ResponseWriter, r *http.Request, peer string) {
	if r.Method != http.MethodGet {
		writeControlError(w, http.StatusMethodNotAllowed, "immutable object fetch requires GET")
		return
	}
	if c.ObjectProvider == nil {
		writeControlError(w, http.StatusNotImplemented, "immutable object provider is unavailable")
		return
	}
	objectID, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, controlObjectPrefix))
	if err != nil || objectID == "" || strings.Contains(objectID, "/") {
		writeControlError(w, http.StatusBadRequest, "invalid immutable object ID")
		return
	}
	transaction := r.URL.Query().Get("transaction")
	reader, length, err := c.ObjectProvider.OpenObject(r.Context(), peer, objectID, transaction)
	if err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	defer reader.Close()
	w.Header().Set(controlObjectLengthHeader, strconv.FormatUint(length, 10))
	w.Header().Set("Content-Length", strconv.FormatUint(length, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	written, copyErr := io.CopyN(w, reader, int64(length))
	if copyErr == nil && uint64(written) != length {
		copyErr = io.ErrUnexpectedEOF
	}
	// At this point headers are committed. Closing the response early is the
	// protocol error signal; the target installs nothing until exact EOF.
	_ = copyErr
}

func (c *ControlPlane) serveSnapshot(w http.ResponseWriter, r *http.Request, peer string) {
	remainder := strings.TrimPrefix(r.URL.Path, controlSnapshotPrefix)
	text, suffix, ok := strings.Cut(remainder, "/")
	if !ok || (suffix != "catalog" && suffix != "references" && suffix != "publish" && !strings.HasPrefix(suffix, "objects/")) {
		writeControlError(w, http.StatusBadRequest, "invalid immutable snapshot path")
		return
	}
	snapshot, err := strconv.ParseUint(text, 10, 64)
	if err != nil || snapshot == 0 || strconv.FormatUint(snapshot, 10) != text {
		writeControlError(w, http.StatusBadRequest, "invalid immutable snapshot ID")
		return
	}
	if suffix == "references" {
		c.serveSnapshotReferences(w, r, peer, snapshot)
		return
	}
	if suffix == "publish" {
		if r.Method != http.MethodPost {
			writeControlError(w, http.StatusMethodNotAllowed, "snapshot publication requires POST")
			return
		}
		if c.SnapshotReferences == nil {
			writeControlError(w, http.StatusNotImplemented, "snapshot publication provider is unavailable")
			return
		}
		if err := c.SnapshotReferences.PublishSnapshot(r.Context(), peer, snapshot); err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
		writeControlJSON(w, http.StatusOK, map[string]any{"snapshot_id": snapshot, "published": true})
		return
	}
	if r.Method != http.MethodGet {
		writeControlError(w, http.StatusMethodNotAllowed, "immutable snapshot fetch requires GET")
		return
	}
	if c.SnapshotProvider == nil {
		writeControlError(w, http.StatusNotImplemented, "immutable snapshot provider is unavailable")
		return
	}
	transaction := r.URL.Query().Get("transaction")
	var reader io.ReadCloser
	var length uint64
	if strings.HasPrefix(suffix, "objects/") {
		object, unescapeErr := url.PathUnescape(strings.TrimPrefix(suffix, "objects/"))
		if unescapeErr != nil || object == "" || strings.Contains(object, "/") {
			writeControlError(w, http.StatusBadRequest, "invalid immutable snapshot object ID")
			return
		}
		reader, length, err = c.SnapshotProvider.OpenSnapshotObject(r.Context(), peer, snapshot, object, transaction)
	} else {
		reader, length, err = c.SnapshotProvider.OpenSnapshot(r.Context(), peer, snapshot, transaction)
	}
	if err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	defer reader.Close()
	w.Header().Set(controlObjectLengthHeader, strconv.FormatUint(length, 10))
	w.Header().Set("Content-Length", strconv.FormatUint(length, 10))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	written, copyErr := io.CopyN(w, reader, int64(length))
	if copyErr == nil && uint64(written) != length {
		copyErr = io.ErrUnexpectedEOF
	}
	_ = copyErr
}

func (c *ControlPlane) serveSnapshotReferences(w http.ResponseWriter, r *http.Request, peer string, snapshot uint64) {
	if r.Method != http.MethodPost {
		writeControlError(w, http.StatusMethodNotAllowed, "snapshot reference mutation requires POST")
		return
	}
	if c.SnapshotReferences == nil {
		writeControlError(w, http.StatusNotImplemented, "snapshot reference provider is unavailable")
		return
	}
	var request snapshotReferenceRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !transactionIDPattern.MatchString(request.Operation) || len(request.Roots) > 4096 {
		writeControlError(w, http.StatusBadRequest, "invalid snapshot reference operation")
		return
	}
	var err error
	switch request.Action {
	case "retain":
		if (request.Transaction != "" && !transactionIDPattern.MatchString(request.Transaction)) || len(request.Roots) == 0 || request.Name != "" || request.Object != "" || request.Retain {
			writeControlError(w, http.StatusBadRequest, "invalid snapshot root retain")
			return
		}
		err = c.SnapshotReferences.RetainSnapshotRoots(r.Context(), peer, snapshot, request.Transaction, request.Operation, request.Roots)
	case "release":
		if (request.Transaction != "" && !transactionIDPattern.MatchString(request.Transaction)) || len(request.Roots) == 0 || request.Name != "" || request.Object != "" || request.Retain {
			writeControlError(w, http.StatusBadRequest, "invalid snapshot root release")
			return
		}
		err = c.SnapshotReferences.ReleaseSnapshotRoots(r.Context(), peer, snapshot, request.Transaction, request.Operation, request.Roots)
	case "reference":
		if request.Transaction != "" || len(request.Roots) != 0 || request.Name == "" || request.Object == "" {
			writeControlError(w, http.StatusBadRequest, "invalid owner reference mutation")
			return
		}
		err = c.SnapshotReferences.ApplySnapshotReference(r.Context(), peer, snapshot, request.Operation, request.Name, request.Object, request.Retain)
	default:
		writeControlError(w, http.StatusBadRequest, "invalid snapshot reference action")
		return
	}
	if err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, map[string]any{"snapshot_id": snapshot, "operation": request.Operation, "action": request.Action})
}

func (c *ControlPlane) reserve(w http.ResponseWriter, r *http.Request, peer string) {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if r.Method != http.MethodPost {
		writeControlError(w, http.StatusMethodNotAllowed, "reserve requires POST")
		return
	}
	var request ReserveRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.SourceCapabilities.NodeID != peer || request.Identity.SourceNodeID != peer {
		writeControlError(w, http.StatusForbidden, "client certificate does not match source node ID")
		return
	}
	if request.Identity.TargetNodeID != c.Capabilities.NodeID || request.Identity.RepositoryID != c.Capabilities.RepositoryID {
		writeControlError(w, http.StatusConflict, "target node or repository identity does not match")
		return
	}
	negotiated, err := Negotiate(request.SourceCapabilities, c.Capabilities)
	if err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	if err := request.Family.validate(); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Identity.Migration && (request.SourcePhysicalContainer != "" || request.SourcePhysicalBranch != "") {
		writeControlError(w, http.StatusBadRequest, "migration physical source is selected by authority placement")
		return
	}
	if (request.SourcePhysicalContainer == "") != (request.SourcePhysicalBranch == "") ||
		(request.SourcePhysicalContainer != "" && (!transactionIDPattern.MatchString(request.SourcePhysicalContainer) || !transactionIDPattern.MatchString(request.SourcePhysicalBranch))) {
		writeControlError(w, http.StatusBadRequest, "invalid physical source identity")
		return
	}
	requestSourceMatches := func(tx *Transaction) bool {
		return tx.SourcePhysicalContainer == request.SourcePhysicalContainer && tx.SourcePhysicalBranch == request.SourcePhysicalBranch
	}
	advanceReserved := func(tx *Transaction) (*Transaction, error) {
		advanced, err := c.Store.Advance(tx.Identity.TransactionID, tx.Revision, TransactionTargetReserved, "", c.Now())
		if err == nil {
			return advanced, nil
		}
		// The local reservation is idempotent and may overlap recovery or a
		// replayed request at this final authority CAS. Resolve the authoritative
		// state rather than leaking a stale-revision implementation detail.
		current, loadErr := c.Store.Load(tx.Identity.TransactionID)
		if loadErr != nil {
			return nil, errors.Join(err, loadErr)
		}
		if !reflect.DeepEqual(current.Identity, tx.Identity) || !reflect.DeepEqual(current.Negotiated, tx.Negotiated) {
			return nil, errors.New("transaction changed identity while reserving target")
		}
		switch current.State {
		case TransactionPreparing:
			return c.Store.Advance(current.Identity.TransactionID, current.Revision, TransactionTargetReserved, "", c.Now())
		case TransactionTargetReserved, TransactionSourceCaptured, TransactionTargetRestoring,
			TransactionTargetPaused, TransactionPublished, TransactionDraining, TransactionIndependent:
			return current, nil
		case TransactionCanceled, TransactionFailed:
			return nil, fmt.Errorf("transaction became %s while reserving target: %s", current.State, current.Failure)
		default:
			return nil, err
		}
	}
	if existing, err := c.Store.Load(request.Identity.TransactionID); err == nil {
		if reflect.DeepEqual(existing.Identity, request.Identity) && reflect.DeepEqual(existing.Negotiated, negotiated) && requestSourceMatches(existing) {
			if existing.State == TransactionPreparing {
				if c.TargetRuntime == nil {
					writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
					return
				}
				if err := c.TargetRuntime.Reserve(r.Context(), existing, request.Family); err != nil {
					writeControlError(w, http.StatusConflict, "reserving target runtime: "+err.Error())
					return
				}
				existing, err = advanceReserved(existing)
				if err != nil {
					writeControlError(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
			writeControlJSON(w, http.StatusOK, existing)
			return
		}
		writeControlError(w, http.StatusConflict, "transaction ID is already bound to different fork parameters")
		return
	}
	records, err := c.Store.List()
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	active := 0
	for i := range records {
		if !records[i].Terminal() {
			active++
		}
	}
	if active >= c.MaxTransactions {
		writeControlError(w, http.StatusTooManyRequests, "target distributed-fork transaction quota is exhausted")
		return
	}
	tx, err := NewTransaction(request.Identity, negotiated, c.Now())
	if err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	tx.SourcePhysicalContainer = request.SourcePhysicalContainer
	tx.SourcePhysicalBranch = request.SourcePhysicalBranch
	if err := c.Store.Create(tx); err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	if err := c.TargetRuntime.Reserve(r.Context(), tx, request.Family); err != nil {
		_, _ = c.Store.Advance(tx.Identity.TransactionID, tx.Revision, TransactionFailed, err.Error(), c.Now())
		writeControlError(w, http.StatusConflict, "reserving target runtime: "+err.Error())
		return
	}
	tx, err = advanceReserved(tx)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeControlJSON(w, http.StatusCreated, tx)
}

func (c *ControlPlane) prepare(w http.ResponseWriter, r *http.Request, id, peer string) {
	unlock := c.lockTransaction(id)
	defer unlock()
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID {
		writeControlError(w, http.StatusForbidden, "only the source node may prepare the target")
		return
	}
	var request PrepareRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := request.Session.validate(tx.Negotiated); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	digest, err := request.Session.digest()
	if err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tx.SessionDigest != "" && tx.SessionDigest != digest {
		writeControlError(w, http.StatusConflict, "transaction is bound to another data-plane session")
		return
	}
	if tx.State == TransactionTargetReserved {
		tx, err = c.Store.BindSession(id, request.Revision, digest, c.Now())
		if err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if tx.State == TransactionSourceCaptured {
		tx, err = c.Store.Advance(id, tx.Revision, TransactionTargetRestoring, "", c.Now())
		if err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if tx.State == TransactionTargetRestoring {
		if err := c.TargetRuntime.Prepare(r.Context(), tx, request.Session); err != nil {
			failed, advanceErr := c.Store.Advance(id, tx.Revision, TransactionFailed, err.Error(), c.Now())
			if advanceErr != nil {
				writeControlError(w, http.StatusInternalServerError, fmt.Sprintf("target restore failed: %v; recording failure: %v", err, advanceErr))
				return
			}
			writeControlError(w, http.StatusConflict, "target restore failed: "+failed.Failure)
			return
		}
		tx, err = c.Store.Advance(id, tx.Revision, TransactionTargetPaused, "", c.Now())
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if tx.State != TransactionTargetPaused && tx.State != TransactionPublished && tx.State != TransactionDraining && tx.State != TransactionIndependent {
		writeControlError(w, http.StatusConflict, fmt.Sprintf("transaction is %s, not prepared", tx.State))
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) publish(w http.ResponseWriter, r *http.Request, id, peer string) {
	unlock := c.lockTransaction(id)
	defer unlock()
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID {
		writeControlError(w, http.StatusForbidden, "only the source node may publish the target")
		return
	}
	var request PublishRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if (tx.State == TransactionPublished || tx.State == TransactionDraining || tx.State == TransactionIndependent) && !tx.Activated {
		// A migration is published physically before its placement cutover. A
		// replayed Publish request may therefore observe TransactionPublished
		// while the source still owns execution authority. Only the explicit
		// Activate request, which checks OwnershipCommitted below, may resume it.
		if tx.Identity.Migration {
			writeControlJSON(w, http.StatusOK, tx)
			return
		}
		if err := c.TargetRuntime.Activate(r.Context(), tx); err != nil {
			writeControlError(w, http.StatusConflict, "target placement is committed but activation retry failed: "+err.Error())
			return
		}
		tx, err = c.Store.MarkActivated(id, tx.Revision, c.Now())
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "target resumed but activation recording retry failed: "+err.Error())
			return
		}
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	if tx.State == TransactionPublished || tx.State == TransactionDraining || tx.State == TransactionIndependent {
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	if tx.State != TransactionTargetPaused || tx.Revision != request.Revision {
		writeControlError(w, http.StatusConflict, fmt.Sprintf("transaction is %s revision %d, want %s revision %d", tx.State, tx.Revision, TransactionTargetPaused, request.Revision))
		return
	}
	_, atomicPublication := c.Store.(AtomicTargetPublisher)
	runtimePaused := request.Paused
	if tx.Identity.Migration || atomicPublication {
		// authority-backed targets never execute before the placement grant that
		// authorizes them. Publish the physical target paused, commit authority,
		// then activate it below.
		runtimePaused = true
	}
	if err := c.TargetRuntime.Publish(r.Context(), tx, runtimePaused); err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	if !tx.Identity.Migration {
		if publisher, ok := c.Store.(AtomicTargetPublisher); ok {
			identity, ok := c.TargetRuntime.(PublishedTargetIdentity)
			if !ok {
				writeControlError(w, http.StatusInternalServerError, "target runtime cannot report its published identity")
				return
			}
			family, branch, physicalContainer, physicalBranch, incarnation, identityErr := identity.PublishedTarget(tx)
			if identityErr != nil {
				writeControlError(w, http.StatusInternalServerError, identityErr.Error())
				return
			}
			tx, err = publisher.PublishTarget(id, tx.Revision, request.Paused, family, branch, physicalContainer, physicalBranch, incarnation)
		} else {
			tx, err = c.Store.Publish(id, tx.Revision, request.Paused, true, c.Now())
		}
	} else if publisher, ok := c.Store.(AtomicMigrationTargetPublisher); ok {
		identity, ok := c.TargetRuntime.(PublishedTargetIdentity)
		if !ok {
			writeControlError(w, http.StatusInternalServerError, "migration target runtime cannot report its physical identity")
			return
		}
		_, _, physicalContainer, physicalBranch, incarnation, identityErr := identity.PublishedTarget(tx)
		if identityErr != nil {
			writeControlError(w, http.StatusInternalServerError, identityErr.Error())
			return
		}
		tx, err = publisher.PublishMigrationTarget(id, tx.Revision, request.Paused, physicalContainer, physicalBranch, incarnation)
	} else {
		tx, err = c.Store.Publish(id, tx.Revision, request.Paused, false, c.Now())
	}
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if atomicPublication && !tx.Identity.Migration && !request.Paused && !tx.Activated {
		if err := c.TargetRuntime.Activate(r.Context(), tx); err != nil {
			// Placement is already committed. Leave the target physically paused
			// and let recovery retry activation; rolling authority back after the
			// commit would be less safe than rolling this step forward.
			writeControlError(w, http.StatusInternalServerError, "target placement committed but activation is pending recovery: "+err.Error())
			return
		}
		tx, err = c.Store.MarkActivated(id, tx.Revision, c.Now())
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "target resumed but activation recording is pending recovery: "+err.Error())
			return
		}
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) activate(w http.ResponseWriter, r *http.Request, id, peer string) {
	unlock := c.lockTransaction(id)
	defer unlock()
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID {
		writeControlError(w, http.StatusForbidden, "only the source node may activate the target")
		return
	}
	var request RevisionRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tx.Activated {
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	if _, atomicPublication := c.Store.(AtomicTargetPublisher); atomicPublication && tx.Identity.Migration && !tx.OwnershipCommitted {
		writeControlError(w, http.StatusConflict, "migration placement ownership has not committed")
		return
	}
	activatable := tx.State == TransactionPublished || tx.State == TransactionDraining || tx.State == TransactionIndependent
	if !activatable || tx.Revision != request.Revision {
		writeControlError(w, http.StatusConflict, fmt.Sprintf("transaction is not an activatable published target at revision %d", request.Revision))
		return
	}
	if err := c.TargetRuntime.Activate(r.Context(), tx); err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	tx, err = c.Store.MarkActivated(id, tx.Revision, c.Now())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) independent(w http.ResponseWriter, r *http.Request, id, peer string) {
	var operation func(context.Context, *Transaction) error
	if c.TargetRuntime != nil {
		operation = c.TargetRuntime.MarkIndependent
	}
	c.advanceRuntime(w, r, id, peer, TransactionPublished, TransactionIndependent, operation)
}

func (c *ControlPlane) drain(w http.ResponseWriter, r *http.Request, id, peer string) {
	var operation func(context.Context, *Transaction) error
	if c.TargetRuntime != nil {
		operation = func(ctx context.Context, tx *Transaction) error {
			// BindGraphHandoff commits intent before the target-local reader switch.
			// If that first attempt fails, a later eager drain must re-drive the
			// immutable handoff before advancing the transaction to Draining;
			// otherwise independence waits forever for status that was never
			// installed in the Sentry.
			if tx.GraphHandoff != nil {
				if err := c.TargetRuntime.Handoff(ctx, tx, *tx.GraphHandoff); err != nil {
					return err
				}
			}
			return c.TargetRuntime.Drain(ctx, tx)
		}
	}
	c.advanceRuntime(w, r, id, peer, TransactionPublished, TransactionDraining, operation)
}

func (c *ControlPlane) handoff(w http.ResponseWriter, r *http.Request, id, peer string) {
	unlock := c.lockTransaction(id)
	defer unlock()
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID {
		writeControlError(w, http.StatusForbidden, "only the source node may hand off target memory")
		return
	}
	if tx.Identity.Migration && !tx.Activated {
		writeControlError(w, http.StatusConflict, "migration target is not activated after placement cutover")
		return
	}
	var request HandoffRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := request.Graph.validate(); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tx.GraphHandoff != nil && *tx.GraphHandoff != request.Graph {
		writeControlError(w, http.StatusConflict, "transaction is bound to another graph handoff")
		return
	}
	if tx.State == TransactionIndependent || tx.State == TransactionDraining {
		if tx.GraphHandoff == nil {
			writeControlError(w, http.StatusConflict, "transaction advanced without a graph handoff")
			return
		}
		if tx.State == TransactionDraining {
			// The transaction record may outlive the target daemon process that
			// started the asynchronous Sentry reader switch. Re-drive the exact
			// immutable handoff before acknowledging a retry.
			if err := c.TargetRuntime.Handoff(r.Context(), tx, request.Graph); err != nil {
				writeControlError(w, http.StatusConflict, err.Error())
				return
			}
		}
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	if tx.State != TransactionPublished || tx.Revision != request.Revision {
		writeControlError(w, http.StatusConflict, fmt.Sprintf("transaction is %s revision %d, want %s revision %d", tx.State, tx.Revision, TransactionPublished, request.Revision))
		return
	}
	if tx.GraphHandoff == nil {
		tx, err = c.Store.BindGraphHandoff(id, tx.Revision, request.Graph, c.Now())
		if err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if err := c.TargetRuntime.Handoff(r.Context(), tx, request.Graph); err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	tx, err = c.Store.Advance(id, tx.Revision, TransactionDraining, "", c.Now())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) advanceRuntime(w http.ResponseWriter, r *http.Request, id, peer string, required, next TransactionState, operation func(context.Context, *Transaction) error) {
	unlock := c.lockTransaction(id)
	defer unlock()
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID {
		writeControlError(w, http.StatusForbidden, "only the source node may advance the target")
		return
	}
	var request RevisionRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tx.State == next || (next == TransactionIndependent && tx.State == TransactionIndependent) {
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	stateMatches := tx.State == required || (next == TransactionIndependent && tx.State == TransactionDraining)
	if !stateMatches || tx.Revision != request.Revision {
		writeControlError(w, http.StatusConflict, fmt.Sprintf("transaction is %s revision %d, want %s revision %d", tx.State, tx.Revision, required, request.Revision))
		return
	}
	if operation == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target runtime operation is unavailable")
		return
	}
	if err := operation(r.Context(), tx); err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	if next == TransactionIndependent && request.Metrics != nil {
		metrics := *request.Metrics
		metrics.TimeToIndependent = c.Now().Sub(tx.CreatedAt)
		tx, err = c.Store.AdvanceIndependent(id, tx.Revision, metrics, c.Now())
	} else {
		tx, err = c.Store.Advance(id, tx.Revision, next, "", c.Now())
	}
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) status(w http.ResponseWriter, id, peer string) {
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID && peer != tx.Identity.TargetNodeID {
		writeControlError(w, http.StatusForbidden, "peer does not own transaction")
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) cancel(w http.ResponseWriter, r *http.Request, id, peer string) {
	unlock := c.lockTransaction(id)
	defer unlock()
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID && peer != tx.Identity.TargetNodeID {
		writeControlError(w, http.StatusForbidden, "peer does not own transaction")
		return
	}
	var request CancelRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Cancellation is destructive: TargetRuntime.Cancel removes the physical
	// target. Fence a cancelable transaction in the authority before touching that
	// runtime; otherwise a concurrent Published -> Draining transition can win
	// after physical deletion and leave an authoritative independent placement
	// without a Sentry. Once draining has begun, return the observed state so
	// the removal coordinator can finish the independence barrier and use
	// ordinary branch deletion instead.
	if tx.State == TransactionDraining || tx.State == TransactionIndependent {
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	// Failed preparation is already terminal, but cancellation still has to
	// reclaim any partially created runtime and filesystem import. Canceled is
	// likewise retried through cleanup: the daemon may have crashed after the
	// durable fence and before TargetRuntime.Cancel completed.
	if tx.State != TransactionCanceled && tx.State != TransactionFailed {
		tx, err = c.Store.Advance(id, request.Revision, TransactionCanceled, "", c.Now())
		if err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if c.TargetRuntime != nil {
		if err := c.TargetRuntime.Cancel(r.Context(), tx); err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func verifiedPeerNodeID(r *http.Request, token string) (string, error) {
	if r.TLS != nil && len(r.TLS.VerifiedChains) != 0 && len(r.TLS.VerifiedChains[0]) != 0 {
		nodeID := r.TLS.VerifiedChains[0][0].Subject.CommonName
		if nodeID == "" {
			return "", errors.New("client certificate common name must contain the TTC node ID")
		}
		return nodeID, nil
	}
	// Without a token, no peer is authenticated: a caller names whichever
	// node it likes.
	if token == "" {
		return "", errors.New("cluster control plane has no authentication token")
	}
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(value), []byte(token)) != 1 {
		return "", errors.New("invalid cluster authentication token")
	}
	nodeID := r.Header.Get(controlNodeHeader)
	if nodeID == "" {
		return "", errors.New("cluster peer node identity is required")
	}
	return nodeID, nil
}

func decodeControlJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxControlBody+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decoding cluster request: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("cluster request contains trailing JSON")
	}
	return nil
}

func writeControlJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeControlError(w http.ResponseWriter, status int, message string) {
	writeControlJSON(w, status, controlError{Error: message})
}

// Client calls one target ttcd control plane over HTTP or HTTPS.
type Client struct {
	BaseURL    string
	HTTP       *http.Client
	streamHTTP *http.Client
	daemonHTTP *http.Client
	nodeID     string
	authToken  string
}

// NewPlainClient constructs a client for TTC's trusted private-network mode.
func NewPlainClient(baseURL, nodeID, authToken string, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" {
		return nil, fmt.Errorf("invalid plaintext cluster peer URL %q", baseURL)
	}
	if nodeID == "" || authToken == "" || timeout <= 0 {
		return nil, errors.New("cluster client requires a node ID, an authentication token and a positive timeout")
	}
	// Daemon tunnels are deliberately not pooled with bounded control calls or
	// immutable-object reads. A tunnel can remain open for an interactive exec
	// or a whole node drain; reusing a stale HTTP/1 connection can strand one
	// node retirement even though the peer's listener remains healthy.
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTP: &http.Client{
			Transport: &http.Transport{},
			Timeout:   timeout,
		},
		streamHTTP: &http.Client{Transport: &http.Transport{}},
		daemonHTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		nodeID:     nodeID,
		authToken:  authToken,
	}, nil
}

func (c *Client) authenticate(req *http.Request) {
	if c.nodeID != "" {
		req.Header.Set(controlNodeHeader, c.nodeID)
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
}

// FetchObject opens one immutable graph object as a streaming response. The
// caller must read exactly length bytes before atomically installing it.
func (c *Client) FetchObject(ctx context.Context, objectID, transaction string) (io.ReadCloser, uint64, error) {
	if objectID == "" || strings.Contains(objectID, "/") {
		return nil, 0, errors.New("invalid immutable object ID")
	}
	endpoint := c.BaseURL + controlObjectPrefix + url.PathEscape(objectID)
	if transaction != "" {
		endpoint += "?transaction=" + url.QueryEscape(transaction)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	c.authenticate(request)
	client := c.streamHTTP
	if client == nil {
		client = c.HTTP
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		var failure controlError
		if err := json.NewDecoder(io.LimitReader(response.Body, maxControlBody)).Decode(&failure); err != nil || failure.Error == "" {
			return nil, 0, fmt.Errorf("immutable object provider returned %s", response.Status)
		}
		return nil, 0, errors.New(failure.Error)
	}
	lengthText := response.Header.Get(controlObjectLengthHeader)
	length, err := strconv.ParseUint(lengthText, 10, 64)
	if err != nil || response.ContentLength < 0 || uint64(response.ContentLength) != length {
		response.Body.Close()
		return nil, 0, errors.New("immutable object provider returned an invalid exact length")
	}
	return response.Body, length, nil
}

// FetchSnapshot opens a provider-fenced immutable metadata catalog. The
// caller validates its schema and graph closure before using it.
func (c *Client) FetchSnapshot(ctx context.Context, snapshot uint64, transaction string) (io.ReadCloser, uint64, error) {
	if snapshot == 0 {
		return nil, 0, errors.New("invalid immutable snapshot ID")
	}
	endpoint := c.BaseURL + controlSnapshotPrefix + strconv.FormatUint(snapshot, 10) + "/catalog"
	if transaction != "" {
		endpoint += "?transaction=" + url.QueryEscape(transaction)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	c.authenticate(request)
	client := c.streamHTTP
	if client == nil {
		client = c.HTTP
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		var failure controlError
		if err := json.NewDecoder(io.LimitReader(response.Body, maxControlBody)).Decode(&failure); err != nil || failure.Error == "" {
			return nil, 0, fmt.Errorf("immutable snapshot provider returned %s", response.Status)
		}
		return nil, 0, errors.New(failure.Error)
	}
	lengthText := response.Header.Get(controlObjectLengthHeader)
	length, err := strconv.ParseUint(lengthText, 10, 64)
	if err != nil || length == 0 || length > maxAuthorityStateBytes || response.ContentLength < 0 || uint64(response.ContentLength) != length {
		response.Body.Close()
		return nil, 0, errors.New("immutable snapshot provider returned an invalid exact length")
	}
	return response.Body, length, nil
}

// FetchSnapshotObject fetches one immutable metadata record from its exact
// origin. Unlike FetchSnapshot, this operation is O(1) in the number of other
// objects in that origin and is suitable for demand-loaded filesystem walks.
func (c *Client) FetchSnapshotObject(ctx context.Context, snapshot uint64, objectID, transaction string) (io.ReadCloser, uint64, error) {
	if snapshot == 0 || objectID == "" || strings.Contains(objectID, "/") {
		return nil, 0, errors.New("invalid immutable snapshot object request")
	}
	endpoint := c.BaseURL + controlSnapshotPrefix + strconv.FormatUint(snapshot, 10) + "/objects/" + url.PathEscape(objectID)
	if transaction != "" {
		endpoint += "?transaction=" + url.QueryEscape(transaction)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	c.authenticate(request)
	client := c.streamHTTP
	if client == nil {
		client = c.HTTP
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		var failure controlError
		if err := json.NewDecoder(io.LimitReader(response.Body, maxControlBody)).Decode(&failure); err != nil || failure.Error == "" {
			return nil, 0, fmt.Errorf("immutable snapshot object provider returned %s", response.Status)
		}
		return nil, 0, errors.New(failure.Error)
	}
	lengthText := response.Header.Get(controlObjectLengthHeader)
	length, err := strconv.ParseUint(lengthText, 10, 64)
	if err != nil || length == 0 || length > maxAuthorityStateBytes || response.ContentLength < 0 || uint64(response.ContentLength) != length {
		response.Body.Close()
		return nil, 0, errors.New("immutable snapshot object provider returned an invalid exact length")
	}
	return response.Body, length, nil
}

// RetainSnapshotRoots durably activates a bounded logical-root batch at the
// unique provider for snapshot. Retrying the same operation is a no-op.
func (c *Client) RetainSnapshotRoots(ctx context.Context, snapshot uint64, transaction, operation string, roots []SnapshotRootBinding) error {
	return c.mutateSnapshotRoots(ctx, snapshot, snapshotReferenceRequest{Transaction: transaction, Operation: operation, Action: "retain", Roots: roots})
}

// ReleaseSnapshotRoots durably deactivates a previously retained root batch.
// Retrying a completed release is a no-op.
func (c *Client) ReleaseSnapshotRoots(ctx context.Context, snapshot uint64, transaction, operation string, roots []SnapshotRootBinding) error {
	return c.mutateSnapshotRoots(ctx, snapshot, snapshotReferenceRequest{Transaction: transaction, Operation: operation, Action: "release", Roots: roots})
}

func (c *Client) mutateSnapshotRoots(ctx context.Context, snapshot uint64, request snapshotReferenceRequest) error {
	if snapshot == 0 || (request.Transaction != "" && !transactionIDPattern.MatchString(request.Transaction)) || !transactionIDPattern.MatchString(request.Operation) || len(request.Roots) == 0 || len(request.Roots) > 4096 {
		return errors.New("invalid snapshot reference operation")
	}
	var response map[string]any
	return c.call(ctx, http.MethodPost, controlSnapshotPrefix+strconv.FormatUint(snapshot, 10)+"/references", request, &response)
}

// ApplySnapshotReference delivers one ordered cross-owner object edge to a
// volatile provider. The provider stores it as a bridge root until ownership
// changes or the edge is released.
func (c *Client) ApplySnapshotReference(ctx context.Context, snapshot uint64, operation, name, object string, retain bool) error {
	if snapshot == 0 || !transactionIDPattern.MatchString(operation) || name == "" || object == "" {
		return errors.New("invalid owner reference mutation")
	}
	var response map[string]any
	request := snapshotReferenceRequest{Operation: operation, Action: "reference", Name: name, Object: object, Retain: retain}
	return c.call(ctx, http.MethodPost, controlSnapshotPrefix+strconv.FormatUint(snapshot, 10)+"/references", request, &response)
}

// PublishSnapshot asks the exact volatile provider to make an immutable
// origin and all of its backing origins durable in shared storage. The
// provider serializes concurrent requests; a retry after a lost response is
// idempotent because shared authority is immutable.
func (c *Client) PublishSnapshot(ctx context.Context, snapshot uint64) error {
	if snapshot == 0 {
		return errors.New("invalid snapshot publication request")
	}
	var response map[string]any
	return c.call(ctx, http.MethodPost, controlSnapshotPrefix+strconv.FormatUint(snapshot, 10)+"/publish", struct{}{}, &response)
}

// ImportSharedSnapshot asks the current shared-GC coordinator to import the
// immutable manifest baseline. Retrying after a lost response is idempotent.
func (c *Client) ImportSharedSnapshot(ctx context.Context, snapshot uint64) error {
	if snapshot == 0 {
		return errors.New("invalid shared snapshot import")
	}
	var response map[string]any
	return c.call(ctx, http.MethodPost, controlSharedLedgerPrefix+strconv.FormatUint(snapshot, 10)+"/import", struct{}{}, &response)
}

// ApplySharedSnapshotReference delivers one idempotent bridge reference to
// the shared-GC coordinator while the caller still owns the volatile origin.
// This is intentionally distinct from ApplySnapshotReference, which targets
// the volatile provider rather than the shared ledger.
func (c *Client) ApplySharedSnapshotReference(ctx context.Context, snapshot uint64, operation, name, object string, retain bool) error {
	if snapshot == 0 || !transactionIDPattern.MatchString(operation) || name == "" || object == "" {
		return errors.New("invalid shared reference mutation")
	}
	var response map[string]any
	request := snapshotReferenceRequest{Operation: operation, Name: name, Object: object, Retain: retain}
	return c.call(ctx, http.MethodPost, controlSharedLedgerPrefix+strconv.FormatUint(snapshot, 10)+"/reference", request, &response)
}

// ApplySharedSnapshotHandoff advances the coordinator's durable exact cursor.
func (c *Client) ApplySharedSnapshotHandoff(ctx context.Context, snapshot, from uint64, mutations []SharedReferenceMutation) error {
	if snapshot == 0 || len(mutations) > 4096 {
		return errors.New("invalid shared snapshot handoff")
	}
	var response map[string]any
	request := sharedHandoffRequest{From: from, Mutations: append([]SharedReferenceMutation(nil), mutations...)}
	return c.call(ctx, http.MethodPost, controlSharedLedgerPrefix+strconv.FormatUint(snapshot, 10)+"/catchup", request, &response)
}

// FlushSharedSnapshot waits until every preceding cross-owner edge retain for
// the imported origin is durable at its destination provider.
func (c *Client) FlushSharedSnapshot(ctx context.Context, snapshot uint64) error {
	if snapshot == 0 {
		return errors.New("invalid shared snapshot flush")
	}
	var response map[string]any
	return c.call(ctx, http.MethodPost, controlSharedLedgerPrefix+strconv.FormatUint(snapshot, 10)+"/flush", struct{}{}, &response)
}

// NewClient constructs a client using tlsConfig for peer authentication.
func NewClient(baseURL string, tlsConfig *tls.Config, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" {
		return nil, fmt.Errorf("invalid cluster peer URL %q", baseURL)
	}
	if tlsConfig == nil || timeout <= 0 {
		return nil, errors.New("cluster client requires TLS and a positive timeout")
	}
	// Keep bounded transaction RPCs on a transport separate from full-duplex
	// daemon streams. Sharing an HTTP/2 connection lets a stalled or canceled
	// stream affect transaction progress and has produced TLS record corruption
	// under concurrent elastic routing. Control RPCs need no multiplexing;
	// daemon proxy streams retain HTTP/2 full duplex on their own connections.
	controlTransport := &http.Transport{TLSClientConfig: tlsConfig.Clone()}
	streamTransport := &http.Transport{TLSClientConfig: tlsConfig.Clone(), ForceAttemptHTTP2: true}
	daemonTransport := &http.Transport{TLSClientConfig: tlsConfig.Clone(), DisableKeepAlives: true}
	return &Client{
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		HTTP:       &http.Client{Transport: controlTransport, Timeout: timeout},
		streamHTTP: &http.Client{Transport: streamTransport},
		daemonHTTP: &http.Client{Transport: daemonTransport},
	}, nil
}

// ProxyDaemon carries one framed TTC daemon connection over the authenticated
// cluster transport. The caller controls cancellation through ctx.
func (c *Client) ProxyDaemon(ctx context.Context, input io.Reader, output io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+controlProxyPath, input)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/vnd.ttc.daemon-stream")
	c.authenticate(req)
	// Streaming exec sessions may legitimately outlive the timeout used for
	// individual transaction RPCs; their context is the lifetime bound.
	client := c.daemonHTTP
	if client == nil {
		client = c.streamHTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited := io.LimitReader(resp.Body, maxControlBody+1)
		var remote controlError
		if err := json.NewDecoder(limited).Decode(&remote); err != nil || remote.Error == "" {
			return fmt.Errorf("cluster peer returned HTTP %s", resp.Status)
		}
		return fmt.Errorf("cluster peer: %s", remote.Error)
	}
	_, err = io.Copy(output, resp.Body)
	return err
}

// ProxyPort bridges one byte stream to a TCP listener in the selected guest.
func (c *Client) ProxyPort(ctx context.Context, family, branch string, port uint16, input io.Reader, output io.Writer) error {
	if family == "" || branch == "" || port == 0 {
		return errors.New("invalid guest port tunnel target")
	}
	endpoint := c.BaseURL + controlPortProxyPath + "?family=" + url.QueryEscape(family) + "&branch=" + url.QueryEscape(branch) + "&port=" + strconv.Itoa(int(port))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, input)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/vnd.ttc.port-stream")
	c.authenticate(req)
	client := c.daemonHTTP
	if client == nil {
		client = c.streamHTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited := io.LimitReader(resp.Body, maxControlBody+1)
		var remote controlError
		if err := json.NewDecoder(limited).Decode(&remote); err != nil || remote.Error == "" {
			return fmt.Errorf("cluster peer returned HTTP %s", resp.Status)
		}
		return fmt.Errorf("cluster peer: %s", remote.Error)
	}
	_, err = io.Copy(output, resp.Body)
	return err
}

// ProposeAuthority forwards a deterministic mutation envelope to the
// authority's holder, which supplies the command timestamp.
func (c *Client) ProposeAuthority(ctx context.Context, proposal AuthorityProposal) (AuthorityResult, error) {
	var result AuthorityResult
	if err := c.call(ctx, http.MethodPost, controlAuthorityProposePath, proposal, &result); err != nil {
		return AuthorityResult{}, err
	}
	return result, nil
}

// AuthorityState returns a linearizable cluster control snapshot.
func (c *Client) AuthorityState(ctx context.Context) (*AuthorityState, error) {
	var state AuthorityState
	if err := c.callWithLimit(ctx, http.MethodGet, controlAuthorityStatePath, nil, &state, maxAuthorityStateBytes); err != nil {
		return nil, err
	}
	return &state, nil
}

// AuthorityQuery reads the records of the authority state that query names,
// linearizably, from the node that holds the authority.
func (c *Client) AuthorityQuery(ctx context.Context, query AuthorityQuery) (*AuthorityState, error) {
	var state AuthorityState
	if err := c.callWithLimit(ctx, http.MethodPost, controlAuthorityQueryPath, query, &state, maxAuthorityStateBytes); err != nil {
		return nil, err
	}
	state.initMaps()
	return &state, nil
}

// SnapshotReferenceInventory fetches the peer's bounded durable root inventory
// for shared reference-ledger reconstruction.
func (c *Client) SnapshotReferenceInventory(ctx context.Context) (*SnapshotReferenceInventory, error) {
	var inventory SnapshotReferenceInventory
	if err := c.callWithLimit(ctx, http.MethodGet, controlReferenceClaimsPath, nil, &inventory, maxAuthorityStateBytes); err != nil {
		return nil, err
	}
	return &inventory, nil
}

// JoinAuthority registers this client's authenticated node with the
// authority's holder, as a joining node until it renews as ready.
func (c *Client) JoinAuthority(ctx context.Context, request AuthorityJoinRequest) error {
	var response map[string]any
	return c.call(ctx, http.MethodPost, controlAuthorityJoinPath, request, &response)
}

// ProbeTopology proves bidirectional control reachability between this
// client's server and the joining node described by request.
func (c *Client) ProbeTopology(ctx context.Context, request TopologyProbeRequest) error {
	var response map[string]string
	if err := c.call(ctx, http.MethodPost, controlTopologyProbePath, request, &response); err != nil {
		return err
	}
	if response["nonce"] != request.Nonce {
		return errors.New("topology probe returned the wrong nonce")
	}
	return nil
}

// RenewExecutionAuthority asks the target daemon itself to renew its
// execution lease with the current authority. A different node must never
// fabricate a target heartbeat merely because its lease has not expired.
func (c *Client) RenewExecutionAuthority(ctx context.Context) error {
	var response map[string]string
	return c.call(ctx, http.MethodPost, controlExecutionRenewPath, nil, &response)
}

// RetireAuthority asks the authority to remove the record of this client's
// node, which committed its incarnation offline.
func (c *Client) RetireAuthority(ctx context.Context, request AuthorityRetireRequest) error {
	var response map[string]any
	return c.call(ctx, http.MethodPost, controlAuthorityRetirePath, request, &response)
}

// Reserve negotiates and reserves an unpublished target transaction.
func (c *Client) Reserve(ctx context.Context, request ReserveRequest) (*Transaction, error) {
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlReservePath, request, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Status reloads one target transaction after a disconnect or daemon restart.
func (c *Client) Status(ctx context.Context, id string) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodGet, controlForkPrefix+id, nil, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Cancel requests idempotent target cleanup at revision.
func (c *Client) Cancel(ctx context.Context, id string, revision uint64) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/cancel", CancelRequest{Revision: revision}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Prepare restores a hidden target from a source data-plane session. Retrying
// the identical descriptor is safe after a lost response.
func (c *Client) Prepare(ctx context.Context, id string, revision uint64, session RemoteSessionDescriptor) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/prepare", PrepareRequest{Revision: revision, Session: session}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Publish makes a successfully prepared paused target externally visible.
func (c *Client) Publish(ctx context.Context, id string, revision uint64, paused bool) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/publish", PublishRequest{Revision: revision, Paused: paused}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Activate resumes a migration target after placement ownership has moved.
func (c *Client) Activate(ctx context.Context, id string, revision uint64) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/activate", RevisionRequest{Revision: revision}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// MarkIndependent records that all remote pages are local and the source may
// be stopped without affecting the target.
func (c *Client) MarkIndependent(ctx context.Context, id string, revision uint64) (*Transaction, error) {
	return c.advance(ctx, id, "independent", revision)
}

// MarkIndependentWithMetrics records final source transfer accounting with the
// independence transition.
func (c *Client) MarkIndependentWithMetrics(ctx context.Context, id string, revision uint64, metrics TransferMetrics) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	if err := metrics.Validate(); err != nil {
		return nil, err
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/independent", RevisionRequest{Revision: revision, Metrics: &metrics}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Drain requests eager target materialization so the source may stop safely.
func (c *Client) Drain(ctx context.Context, id string, revision uint64) (*Transaction, error) {
	return c.advance(ctx, id, "drain", revision)
}

// Handoff replaces future live page pulls with an immutable graph snapshot.
func (c *Client) Handoff(ctx context.Context, id string, revision uint64, graph GraphHandoffDescriptor) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	if err := graph.validate(); err != nil {
		return nil, err
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/handoff", HandoffRequest{Revision: revision, Graph: graph}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

func (c *Client) advance(ctx context.Context, id, operation string, revision uint64) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/"+operation, RevisionRequest{Revision: revision}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

func (c *Client) call(ctx context.Context, method, path string, request, response any) error {
	return c.callWithLimit(ctx, method, path, request, response, maxControlBody)
}

func (c *Client) callWithLimit(ctx context.Context, method, path string, request, response any, responseLimit int64) error {
	var body io.Reader
	if request != nil {
		data, err := json.Marshal(request)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authenticate(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, responseLimit+1)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var remote controlError
		if err := json.NewDecoder(limited).Decode(&remote); err != nil || remote.Error == "" {
			return fmt.Errorf("cluster peer returned HTTP %s", resp.Status)
		}
		if remote.NotHolder {
			return &AuthorityNotHolderError{
				HolderID: remote.HolderID, ControlURL: remote.HolderControlURL, ControlServerName: remote.HolderServerName,
			}
		}
		return fmt.Errorf("cluster peer: %s", remote.Error)
	}
	if err := json.NewDecoder(limited).Decode(response); err != nil {
		return fmt.Errorf("decoding cluster response: %w", err)
	}
	return nil
}
