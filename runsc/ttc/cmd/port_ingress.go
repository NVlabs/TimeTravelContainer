// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0

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

var errIngressPortNotFound = errors.New("ephemeral ingress port not found")

// portIngressManager owns node-local, intentionally ephemeral published
// ports. Each accepted connection resolves current Raft placement, so the
// listening endpoint remains valid after migration without being recreated on
// the destination node.
type portIngressManager struct {
	server *daemonServer
	mu     sync.Mutex
	ports  map[string]*portIngressBinding
}

func newPortIngressManager(server *daemonServer) *portIngressManager {
	return &portIngressManager{server: server, ports: make(map[string]*portIngressBinding)}
}

func ingressPortKey(binding ttcbundle.PortBinding) string {
	return net.JoinHostPort(binding.HostIP, strconv.Itoa(int(binding.HostPort))) + "/" + binding.Protocol
}

func (m *portIngressManager) Publish(target selector, binding ttcbundle.PortBinding) error {
	if binding.Protocol != "tcp" {
		return fmt.Errorf("unsupported published-port protocol %q", binding.Protocol)
	}
	if m.server.clusterPlacements != nil {
		family, err := resolvePlacementFamily(m.server.clusterPlacements, target.container)
		if err != nil {
			return fmt.Errorf("resolving placement family %q: %w", target.container, err)
		}
		placement, err := m.server.clusterPlacements.Lookup(family, target.branch)
		if err != nil {
			return err
		}
		if placement.State != cluster.PlacementRunning && placement.State != cluster.PlacementPaused {
			return fmt.Errorf("branch %q is %s", target.branch, placement.State)
		}
		target.container = family
	} else {
		info, err := m.serverStore().LoadContainer(target.container)
		if err != nil {
			return err
		}
		branch, err := persistentBranchInfo(info, target.branch)
		if err != nil {
			return err
		}
		if branch.State != "running" && branch.State != "paused" {
			return fmt.Errorf("branch %q is %s", target.branch, branch.State)
		}
		if branch.Quarantined {
			return fmt.Errorf("branch %q is quarantined", target.branch)
		}
		if info.NetworkMode != "ttc" {
			return fmt.Errorf("container %q uses network mode %q, not ttc", info.Name, info.NetworkMode)
		}
		target.container = info.ID
	}
	return m.publishResolved(target, binding)
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

func (m *portIngressManager) serverStore() ttcbundle.Store {
	return ttcbundle.Store{Root: m.server.dataRoot, ActiveRoot: m.server.localRoot, CPUFeatures: m.server.cpuFeatures}
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
	owner := ""
	if m.server.clusterPlacements != nil && m.server.clusterCapabilities != nil {
		placement, err := m.server.clusterPlacements.Lookup(target.container, target.branch)
		if err != nil {
			return err
		}
		if placement.State != cluster.PlacementRunning && placement.State != cluster.PlacementPaused {
			return fmt.Errorf("branch %s is not connectable (placement=%s)", target, placement.State)
		}
		owner = placement.OwnerNode
	}
	if owner == "" || m.server.clusterCapabilities == nil || owner == m.server.clusterCapabilities.NodeID {
		backend := directBackend(m.server.optionsFromRequest(), nil, io.Discard, io.Discard)
		m.server.configureVolumeBackend(backend)
		backend.nodeID = owner
		backend.distributedSource = m.server.distributedSource
		guest, err := backend.openGuestPortStream(ctx, target, port)
		if err != nil {
			return err
		}
		defer guest.Close()
		return bridgePortStreams(ctx, conn, guest)
	}
	peer, err := m.server.distributedSource.peer(owner, false)
	if err != nil {
		return err
	}
	return peer.ProxyPort(ctx, target.container, target.branch, port, conn, conn)
}

func (m *portIngressManager) List(container string) ([]ttcbundle.PortBinding, error) {
	family := ""
	if m.server.clusterPlacements != nil {
		resolved, err := resolvePlacementFamily(m.server.clusterPlacements, container)
		if err != nil {
			return nil, err
		}
		family = resolved
	} else {
		info, err := m.serverStore().LoadContainer(container)
		if err != nil {
			return nil, err
		}
		family = info.ID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []ttcbundle.PortBinding
	for _, entry := range m.ports {
		if entry.target.container == family {
			result = append(result, entry.binding)
		}
	}
	return result, nil
}

func (m *portIngressManager) Unpublish(target selector, binding ttcbundle.PortBinding) error {
	family := ""
	if m.server.clusterPlacements != nil {
		resolved, err := resolvePlacementFamily(m.server.clusterPlacements, target.container)
		if err != nil {
			return err
		}
		family = resolved
	} else {
		info, err := m.serverStore().LoadContainer(target.container)
		if err != nil {
			return err
		}
		family = info.ID
	}
	binding.BranchID = target.branch
	key := ingressPortKey(binding)
	m.mu.Lock()
	entry := m.ports[key]
	if entry == nil || entry.target.container != family || entry.target.branch != target.branch || entry.binding.ContainerPort != binding.ContainerPort {
		m.mu.Unlock()
		return errIngressPortNotFound
	}
	delete(m.ports, key)
	m.mu.Unlock()
	entry.cancel()
	return entry.listener.Close()
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
	// Unix socket paths are limited to roughly 108 bytes on Linux. TTC roots
	// are commonly long NFS/Lustre paths, so keep this short-lived rendezvous
	// socket in the host temporary directory rather than below TTC_LOCAL_ROOT.
	file, err := os.CreateTemp("", "ttc-port-stream-")
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
