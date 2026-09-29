// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

type portIngressBinding struct {
	target   selector
	binding  ttcbundle.PortBinding
	listener net.Listener
	cancel   context.CancelFunc
}

type preparedRunIngress struct {
	target   selector
	bindings []ttcbundle.PortBinding
}

var errIngressPortNotFound = errors.New("ephemeral ingress port not found")

// portIngressManager owns node-local, intentionally ephemeral published
// ports. Each accepted connection resolves current Raft placement, so the
// listening endpoint remains valid after migration without being recreated on
// the destination node.
type portIngressManager struct {
	router portIngressRouter
	mu     sync.Mutex
	ports  map[string]*portIngressBinding
}

// portIngressRouter separates the lifetime of a node-local listening socket
// from the runtime that currently owns its target. Compute daemons resolve the
// route from their local Raft catalog; outbound-only control daemons resolve it
// through a linearizable authority read. Keeping this boundary here prevents
// the command proxy from accidentally moving an ingress socket to the branch
// owner.
type portIngressRouter interface {
	Resolve(selector) (selector, error)
	ResolveFamily(string) (string, error)
	Forward(context.Context, selector, uint16, net.Conn) error
	Clustered() bool
}

type daemonPortIngressRouter struct{ server *daemonServer }

type controlOnlyPortIngressRouter struct {
	cluster *controlOnlyCluster
	root    string
}

func newPortIngressManager(server *daemonServer) *portIngressManager {
	return newRoutedPortIngressManager(daemonPortIngressRouter{server: server})
}

func newRoutedPortIngressManager(router portIngressRouter) *portIngressManager {
	return &portIngressManager{router: router, ports: make(map[string]*portIngressBinding)}
}

func ingressPortKey(binding ttcbundle.PortBinding) string {
	return net.JoinHostPort(binding.HostIP, strconv.Itoa(int(binding.HostPort))) + "/" + binding.Protocol
}

func (m *portIngressManager) Publish(target selector, binding ttcbundle.PortBinding) error {
	if binding.Protocol != "tcp" {
		return fmt.Errorf("unsupported published-port protocol %q", binding.Protocol)
	}
	if m == nil || m.router == nil {
		return errors.New("published-port ingress is unavailable")
	}
	resolved, err := m.router.Resolve(target)
	if err != nil {
		return err
	}
	return m.publishResolved(resolved, binding)
}

func (r daemonPortIngressRouter) Clustered() bool {
	return r.server != nil && r.server.clusterPlacements != nil
}

func (r daemonPortIngressRouter) Resolve(target selector) (selector, error) {
	if r.server == nil {
		return selector{}, errors.New("published-port daemon is unavailable")
	}
	if r.server.clusterPlacements != nil {
		family, err := resolvePlacementFamily(r.server.clusterPlacements, target.container)
		if err != nil {
			return selector{}, fmt.Errorf("resolving placement family %q: %w", target.container, err)
		}
		placement, err := r.server.clusterPlacements.Lookup(family, target.branch)
		if err != nil {
			return selector{}, err
		}
		if placement.State != cluster.PlacementRunning && placement.State != cluster.PlacementPaused {
			return selector{}, fmt.Errorf("branch %q is %s", target.branch, placement.State)
		}
		if placement.Quarantined {
			return selector{}, fmt.Errorf("branch %q is quarantined", target.branch)
		}
		if info, loadErr := r.serverStore().LoadContainer(family); loadErr == nil {
			if info.NetworkMode != "ttc" {
				return selector{}, fmt.Errorf("container %q uses network mode %q, not ttc", info.Name, info.NetworkMode)
			}
		} else if !errors.Is(loadErr, os.ErrNotExist) && r.server.dataRoot != "" {
			return selector{}, fmt.Errorf("loading published-port family metadata: %w", loadErr)
		}
		return selector{container: family, branch: target.branch}, nil
	}
	info, err := r.serverStore().LoadContainer(target.container)
	if err != nil {
		return selector{}, err
	}
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil {
		return selector{}, err
	}
	if branch.State != "running" && branch.State != "paused" {
		return selector{}, fmt.Errorf("branch %q is %s", target.branch, branch.State)
	}
	if branch.Quarantined {
		return selector{}, fmt.Errorf("branch %q is quarantined", target.branch)
	}
	if info.NetworkMode != "ttc" {
		return selector{}, fmt.Errorf("container %q uses network mode %q, not ttc", info.Name, info.NetworkMode)
	}
	return selector{container: info.ID, branch: target.branch}, nil
}

func (r daemonPortIngressRouter) serverStore() ttcbundle.Store {
	return ttcbundle.Store{Root: r.server.dataRoot, ActiveRoot: r.server.localRoot, CPUFeatures: r.server.cpuFeatures}
}

func (r daemonPortIngressRouter) ResolveFamily(container string) (string, error) {
	if r.server.clusterPlacements != nil {
		return resolvePlacementFamily(r.server.clusterPlacements, container)
	}
	info, err := r.serverStore().LoadContainer(container)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

// PublishPrepared installs a node-local ingress listener for a container that
// this daemon has prepared locally. A new clustered run reaches this point
// before its first Raft placement record exists, so Publish cannot resolve it
// through the placement catalog yet. Connections accepted after publication
// still resolve placement on every dial and therefore follow later migration.
func (m *portIngressManager) PublishPrepared(info ttcbundle.ContainerInfo, branch string, binding ttcbundle.PortBinding) error {
	if info.NetworkMode != "ttc" {
		return fmt.Errorf("container %q uses network mode %q, not ttc", info.Name, info.NetworkMode)
	}
	record, err := persistentBranchInfo(info, branch)
	if err != nil {
		return err
	}
	if record.Quarantined {
		return fmt.Errorf("branch %q is quarantined", branch)
	}
	return m.publishResolved(selector{container: logicalFamilyID(info), branch: branch}, binding)
}

func (m *portIngressManager) publishResolved(target selector, binding ttcbundle.PortBinding) error {
	binding.BranchID = target.branch
	key := ingressPortKey(binding)
	m.mu.Lock()
	if _, ok := m.ports[key]; ok {
		m.mu.Unlock()
		return fmt.Errorf("published host endpoint %s is already active on this node", key)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(binding.HostIP, strconv.Itoa(int(binding.HostPort))))
	if err != nil {
		m.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := &portIngressBinding{target: target, binding: binding, listener: listener, cancel: cancel}
	m.ports[key] = entry
	m.mu.Unlock()
	go m.serve(ctx, entry)
	return nil
}

// PublishPending reserves a node-local endpoint before a remotely placed run
// exists in Raft. The logical container name is resolved on every accepted
// connection, so the listener is useful as soon as the target commits its
// placement and continues to follow subsequent migrations.
func (m *portIngressManager) PublishPending(target selector, binding ttcbundle.PortBinding) error {
	if !m.Clustered() {
		return errors.New("pending ingress requires clustered TTC")
	}
	if binding.Protocol != "tcp" {
		return fmt.Errorf("unsupported published-port protocol %q", binding.Protocol)
	}
	return m.publishResolved(target, binding)
}

func (m *portIngressManager) removePending(target selector, binding ttcbundle.PortBinding) error {
	key := ingressPortKey(binding)
	m.mu.Lock()
	entry := m.ports[key]
	if entry == nil || entry.target != target || entry.binding.ContainerPort != binding.ContainerPort {
		m.mu.Unlock()
		return errIngressPortNotFound
	}
	delete(m.ports, key)
	m.mu.Unlock()
	entry.cancel()
	return entry.listener.Close()
}

func (m *portIngressManager) rollbackPrepared(prepared *preparedRunIngress) {
	if prepared == nil {
		return
	}
	for _, binding := range prepared.bindings {
		_ = m.removePending(prepared.target, binding)
	}
}

func (m *portIngressManager) serve(ctx context.Context, entry *portIngressBinding) {
	for {
		conn, err := entry.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = m.forward(ctx, entry.target, uint16(entry.binding.ContainerPort), conn)
		}()
	}
}

func (m *portIngressManager) forward(ctx context.Context, target selector, port uint16, conn net.Conn) error {
	resolved, err := m.router.Resolve(target)
	if err != nil {
		return err
	}
	return m.router.Forward(ctx, resolved, port, conn)
}

func (r daemonPortIngressRouter) Forward(ctx context.Context, target selector, port uint16, conn net.Conn) error {
	server := r.server
	owner := ""
	if server.clusterPlacements != nil && server.clusterCapabilities != nil {
		placement, err := server.clusterPlacements.Lookup(target.container, target.branch)
		if err != nil {
			return err
		}
		if placement.State != cluster.PlacementRunning && placement.State != cluster.PlacementPaused {
			return fmt.Errorf("branch %s is not connectable (placement=%s)", target, placement.State)
		}
		owner = placement.OwnerNode
	}
	if owner == "" || server.clusterCapabilities == nil || owner == server.clusterCapabilities.NodeID {
		backend := directBackend(server.optionsFromRequest(), nil, io.Discard, io.Discard)
		server.configureVolumeBackend(backend)
		backend.nodeID = owner
		backend.distributedSource = server.distributedSource
		guest, err := backend.openGuestPortStream(ctx, target, port)
		if err != nil {
			return err
		}
		defer guest.Close()
		return bridgePortStreams(ctx, conn, guest)
	}
	peer, err := server.distributedSource.peer(owner, false)
	if err != nil {
		return err
	}
	return peer.ProxyPort(ctx, target.container, target.branch, port, conn, conn)
}

func (r controlOnlyPortIngressRouter) Clustered() bool { return true }

// placements reads the placements of the authority state.
func (r controlOnlyPortIngressRouter) placements(ctx context.Context) (*cluster.AuthorityState, error) {
	if r.cluster == nil {
		return nil, errors.New("control-only cluster is unavailable")
	}
	return r.cluster.authorityState(ctx, cluster.AuthorityQuery{Placements: cluster.All[string]()})
}

func controlOnlyPlacement(state *cluster.AuthorityState, target selector) (cluster.PlacementRecord, error) {
	if state == nil {
		return cluster.PlacementRecord{}, errors.New("cluster authority is unavailable")
	}
	var found *cluster.PlacementRecord
	for _, record := range state.Placements {
		if record.BranchID != target.branch || (record.FamilyID != target.container && record.FamilyName != target.container) {
			continue
		}
		if found != nil && found.FamilyID != record.FamilyID {
			return cluster.PlacementRecord{}, fmt.Errorf("container name %q is ambiguous", target.container)
		}
		copy := record
		found = &copy
	}
	if found == nil {
		return cluster.PlacementRecord{}, fmt.Errorf("branch %s does not exist", target)
	}
	return *found, nil
}

func (r controlOnlyPortIngressRouter) Resolve(target selector) (selector, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state, err := r.placements(ctx)
	if err != nil {
		return selector{}, err
	}
	placement, err := controlOnlyPlacement(state, target)
	if err != nil {
		return selector{}, err
	}
	if placement.State != cluster.PlacementRunning && placement.State != cluster.PlacementPaused {
		return selector{}, fmt.Errorf("branch %q is %s", target.branch, placement.State)
	}
	if placement.Quarantined {
		return selector{}, fmt.Errorf("branch %q is quarantined", target.branch)
	}
	info, err := (ttcbundle.Store{Root: r.root}).LoadContainer(placement.FamilyID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return selector{}, fmt.Errorf("loading published-port family metadata: %w", err)
	}
	if err == nil && info.NetworkMode != "ttc" {
		return selector{}, fmt.Errorf("container %q uses network mode %q, not ttc", info.Name, info.NetworkMode)
	}
	return selector{container: placement.FamilyID, branch: placement.BranchID}, nil
}

func (r controlOnlyPortIngressRouter) ResolveFamily(container string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state, err := r.placements(ctx)
	if err != nil {
		return "", err
	}
	for _, record := range state.Placements {
		if record.FamilyID == container || record.FamilyName == container {
			return record.FamilyID, nil
		}
	}
	return "", fmt.Errorf("container %q does not exist", container)
}

func (r controlOnlyPortIngressRouter) Forward(ctx context.Context, target selector, port uint16, conn net.Conn) error {
	state, err := r.placements(ctx)
	if err != nil {
		return err
	}
	placement, err := controlOnlyPlacement(state, target)
	if err != nil {
		return err
	}
	if placement.State != cluster.PlacementRunning && placement.State != cluster.PlacementPaused {
		return fmt.Errorf("branch %s is not connectable (placement=%s)", target, placement.State)
	}
	for _, peer := range r.cluster.peerSnapshot() {
		if peer.nodeID == placement.OwnerNode {
			return peer.client.ProxyPort(ctx, placement.FamilyID, placement.BranchID, port, conn, conn)
		}
	}
	// Membership may have changed since the gateway's periodic refresh. Refresh
	// once before declaring a currently authoritative owner unreachable.
	refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = r.cluster.refreshPeers(refreshCtx)
	cancel()
	for _, peer := range r.cluster.peerSnapshot() {
		if peer.nodeID == placement.OwnerNode {
			return peer.client.ProxyPort(ctx, placement.FamilyID, placement.BranchID, port, conn, conn)
		}
	}
	return fmt.Errorf("branch owner %q has no reachable control endpoint", placement.OwnerNode)
}

func (m *portIngressManager) List(container string) ([]ttcbundle.PortBinding, error) {
	family, err := m.router.ResolveFamily(container)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []ttcbundle.PortBinding
	for _, entry := range m.ports {
		entryFamily := entry.target.container
		if entryFamily != family {
			entryFamily, _ = m.router.ResolveFamily(entryFamily)
		}
		if entryFamily == family {
			result = append(result, entry.binding)
		}
	}
	return result, nil
}

func (m *portIngressManager) Unpublish(target selector, binding ttcbundle.PortBinding) error {
	family, err := m.router.ResolveFamily(target.container)
	if err != nil {
		return err
	}
	binding.BranchID = target.branch
	key := ingressPortKey(binding)
	m.mu.Lock()
	entry := m.ports[key]
	entryFamily := ""
	if entry != nil {
		entryFamily = entry.target.container
		if entryFamily != family {
			entryFamily, _ = m.router.ResolveFamily(entryFamily)
		}
	}
	if entry == nil || entryFamily != family || entry.target.branch != target.branch || entry.binding.ContainerPort != binding.ContainerPort {
		m.mu.Unlock()
		return errIngressPortNotFound
	}
	delete(m.ports, key)
	m.mu.Unlock()
	entry.cancel()
	return entry.listener.Close()
}

func (m *portIngressManager) Clustered() bool {
	return m != nil && m.router != nil && m.router.Clustered()
}

// prepareRunIngress removes published-port options from a clustered run and
// returns the node-local listeners that the entry daemon must reserve. It also
// makes TTC's generated name explicit so a listener can refer to the logical
// container before the remote owner creates it.
func prepareRunIngress(args []string) ([]string, *preparedRunIngress, error) {
	if len(args) == 0 {
		return args, nil, nil
	}
	prefix := 1
	if args[0] == "container" {
		if len(args) < 2 || args[1] != "run" {
			return args, nil, nil
		}
		prefix = 2
	} else if args[0] != "run" {
		return args, nil, nil
	}
	clean := append([]string(nil), args[:prefix]...)
	name := ""
	networkMode := "none"
	var ports []string
	for i := prefix; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || arg == "-" || arg == "" || arg[0] != '-' {
			clean = append(clean, args[i:]...)
			break
		}
		switch {
		case arg == "--name":
			if i+1 >= len(args) {
				return nil, nil, errors.New("--name requires a container name")
			}
			name = args[i+1]
			clean = append(clean, arg, name)
			i++
			continue
		case strings.HasPrefix(arg, "--name="):
			name = strings.TrimPrefix(arg, "--name=")
			clean = append(clean, arg)
			continue
		case arg == "--network" || arg == "--net":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s requires a network mode", arg)
			}
			networkMode = args[i+1]
			clean = append(clean, arg, networkMode)
			i++
			continue
		case strings.HasPrefix(arg, "--network="):
			networkMode = strings.TrimPrefix(arg, "--network=")
			clean = append(clean, arg)
			continue
		case strings.HasPrefix(arg, "--net="):
			networkMode = strings.TrimPrefix(arg, "--net=")
			clean = append(clean, arg)
			continue
		case arg == "-p" || arg == "--publish":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s requires a port binding", arg)
			}
			ports = append(ports, args[i+1])
			i++
			continue
		case strings.HasPrefix(arg, "--publish="):
			ports = append(ports, strings.TrimPrefix(arg, "--publish="))
			continue
		case strings.HasPrefix(arg, "-p="):
			ports = append(ports, strings.TrimPrefix(arg, "-p="))
			continue
		}
		clean = append(clean, arg)
		if placementOptionConsumesValue("run", arg) && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			clean = append(clean, args[i])
		}
	}
	if len(ports) == 0 {
		return args, nil, nil
	}
	mode, err := parseNetworkMode(networkMode)
	if err != nil {
		return nil, nil, err
	}
	if mode != "ttc" {
		return nil, nil, errors.New("--publish requires --network=ttc")
	}
	bindings, err := parsePublishedPorts(ports)
	if err != nil {
		return nil, nil, err
	}
	if name == "" {
		name = "ttc-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		withName := make([]string, 0, len(clean)+2)
		withName = append(withName, clean[:prefix]...)
		withName = append(withName, "--name", name)
		clean = append(withName, clean[prefix:]...)
	}
	return clean, &preparedRunIngress{target: selector{container: name, branch: defaultBranchID}, bindings: bindings}, nil
}

func (m *portIngressManager) prepareRun(args []string) ([]string, *preparedRunIngress, error) {
	clean, prepared, err := prepareRunIngress(args)
	if err != nil || prepared == nil {
		return clean, prepared, err
	}
	for i, binding := range prepared.bindings {
		if err := m.PublishPending(prepared.target, binding); err != nil {
			for _, installed := range prepared.bindings[:i] {
				_ = m.removePending(prepared.target, installed)
			}
			return nil, nil, fmt.Errorf("publishing ingress port: %w", err)
		}
	}
	return clean, prepared, nil
}

func (m *portIngressManager) Close() error {
	m.mu.Lock()
	entries := make([]*portIngressBinding, 0, len(m.ports))
	for _, entry := range m.ports {
		entries = append(entries, entry)
	}
	m.ports = make(map[string]*portIngressBinding)
	m.mu.Unlock()
	var result error
	for _, entry := range entries {
		entry.cancel()
		result = errors.Join(result, entry.listener.Close())
	}
	return result
}

func (b *runtimeBackend) openGuestPortStream(ctx context.Context, target selector, port uint16) (net.Conn, error) {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return nil, err
	}
	state, err := b.branchRuntimeState(resolved)
	if err != nil {
		return nil, err
	}
	if state != "running" && state != "paused" {
		return nil, fmt.Errorf("branch %q is %s", resolved.branch, state)
	}
	// Keep this short-lived rendezvous socket with the daemon's other sockets,
	// whose short names fit the Unix socket path limit.
	file, err := os.CreateTemp(daemonRunDir(), "p")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	file.Close()
	os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	defer func() {
		listener.Close()
		os.Remove(path)
	}()
	args := b.configuredRunscArgsForNetworkConfig(info.NetworkMode, info.NetworkNAT, info.NetworkIP, info.NetworkMAC)
	args = append(args, "port-forward", "--ttc-allow-stopped-sandbox", "--branch="+resolved.branch, "--stream="+path, info.ID, strconv.Itoa(int(port)))
	command := runscCommand(b.runsc, args...)
	var diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &diagnostic, &diagnostic
	if err := command.Start(); err != nil {
		return nil, err
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		_ = unixListener.SetDeadline(time.Now().Add(5 * time.Second))
	}
	guest, acceptErr := listener.Accept()
	waitErr := command.Wait()
	if acceptErr != nil {
		return nil, fmt.Errorf("accepting guest port stream: %w: %s", acceptErr, diagnostic.String())
	}
	if waitErr != nil {
		guest.Close()
		return nil, fmt.Errorf("opening guest port stream: %w: %s", waitErr, diagnostic.String())
	}
	return guest, nil
}

func bridgePortStreams(ctx context.Context, a, b net.Conn) error {
	done := make(chan error, 2)
	copyOne := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if closer, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- err
	}
	go copyOne(a, b)
	go copyOne(b, a)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type daemonPortProxy struct{ backend *runtimeBackend }

func (p daemonPortProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/vnd.ttc.port-stream" {
		writePortProxyError(w, http.StatusUnsupportedMediaType, "invalid port stream content type")
		return
	}
	portValue, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 16)
	if err != nil || portValue == 0 {
		writePortProxyError(w, http.StatusBadRequest, "invalid container port")
		return
	}
	target := selector{container: r.URL.Query().Get("family"), branch: r.URL.Query().Get("branch")}
	if target.container == "" || target.branch == "" {
		writePortProxyError(w, http.StatusBadRequest, "invalid port target")
		return
	}
	guest, err := p.backend.openGuestPortStream(r.Context(), target, uint16(portValue))
	if err != nil {
		writePortProxyError(w, http.StatusConflict, err.Error())
		return
	}
	defer guest.Close()
	controller := http.NewResponseController(w)
	if err := controller.EnableFullDuplex(); err != nil {
		writePortProxyError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/vnd.ttc.port-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	uploadDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(guest, r.Body)
		if closer, ok := guest.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		close(uploadDone)
	}()
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := guest.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func writePortProxyError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
