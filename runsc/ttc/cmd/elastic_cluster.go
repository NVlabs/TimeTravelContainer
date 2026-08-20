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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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
		if info.NodeID == "" {
			continue
		}
		if owner != "" && owner != info.NodeID {
			return "", fmt.Errorf("command spans cluster nodes %q and %q", owner, info.NodeID)
		}
		owner = info.NodeID
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
