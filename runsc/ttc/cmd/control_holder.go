// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/runsc/ttc/cluster"
	"gvisor.dev/gvisor/runsc/ttc/protocol"
)

// A login node's gateway relays commands to the cluster's compute daemons.
// While none runs, and no daemon holds the cluster's authority, it runs a
// control holder: a daemon of the login node that holds the authority, so
// that the cluster answers what needs no compute daemon, and that stops,
// giving the authority up, once a compute daemon runs. Nothing dials the
// login node: the holder listens on loopback, the gateway relays to it, and
// a compute daemon that starts takes the authority from the shared root.

// controlHolderSuffix makes the node ID of a gateway's control holder from
// the gateway's own.
const controlHolderSuffix = "-holder"

// controlHolderStillnessEnv passes a control holder what its gateway saw of
// the cluster's counters, so that the holder need not watch a daemon that
// died stand still as long again.
const controlHolderStillnessEnv = "TTC_CONTROL_HOLDER_STILLNESS"

// controlHolder runs a gateway's control holder, in dir.
type controlHolder struct {
	dir        string
	sharedRoot string
	nodeID     string
	stderr     io.Writer

	mu sync.Mutex
	// watchedSince is when the gateway first looked at the shared root.
	watchedSince time.Time
	// stopped is set once the gateway stops, which starts no holder since.
	stopped bool
	process *os.Process
	exited  chan struct{}
	started time.Time
	// retryAt is when the holder may start again after it ended, backoff
	// later, doubling while each ends soon after it starts; ended is set
	// once the gateway noticed the last one end.
	retryAt time.Time
	backoff time.Duration
	ended   bool
}

// controlHolderDir returns the directory of the holder of the gateway whose
// root is root.
func controlHolderDir(root string) string { return filepath.Join(root, "holder") }

// controlHolderRoot returns the root of the holder, a daemon, whose directory
// is dir.
func controlHolderRoot(dir string) string { return filepath.Join(dir, "local") }

func (h *controlHolder) socket() string { return protocol.DaemonSocket(controlHolderRoot(h.dir)) }

// running reports whether the holder's process runs.
func (h *controlHolder) running() bool {
	if h.process == nil {
		return false
	}
	select {
	case <-h.exited:
		return false
	default:
		return true
	}
}

// consider starts the holder if the cluster needs one: the gateway has
// watched the shared root for AuthorityTakeover, and has seen no compute
// daemon's counter advance, nor that of a daemon that holds the authority.
func (h *controlHolder) consider(members map[string]cluster.MemberRecord, liveness *cluster.Liveness) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.watchedSince.IsZero() {
		h.watchedSince = time.Now()
	}
	running := false
	for _, member := range members {
		if member.Role == cluster.RoleCompute && liveness.Advancing(member) {
			running = true
		}
	}
	record, recorded, err := cluster.NewSharedCluster(h.sharedRoot).ReadAuthority()
	if err != nil {
		return
	}
	if recorded {
		if member, ok := members[record.NodeID]; ok && member.Incarnation == record.Incarnation &&
			member.Released < record.Epoch && liveness.Advancing(member) {
			running = true
		}
	}
	if h.process != nil && !h.running() && !h.ended {
		h.ended = true
		if time.Since(h.started) < 2*cluster.AuthorityTakeover {
			h.backoff = min(max(2*h.backoff, time.Second), time.Minute)
		} else {
			h.backoff = 0
		}
		h.retryAt = time.Now().Add(h.backoff)
	}
	if running || h.stopped || h.running() || time.Since(h.watchedSince) < cluster.AuthorityTakeover || time.Now().Before(h.retryAt) {
		return
	}
	stillness := make([]cluster.Stillness, 0, len(members))
	for _, member := range members {
		stillness = append(stillness, liveness.Stillness(member))
	}
	if err := h.start(stillness); err != nil && h.stderr != nil {
		fmt.Fprintf(h.stderr, "ttcd: starting this login node's control holder: %v\n", err)
	}
}

// start starts the holder, afresh: it keeps nothing of its own between runs,
// but for what the gateway saw of the cluster's counters.
func (h *controlHolder) start(stillness []cluster.Stillness) error {
	local := controlHolderRoot(h.dir)
	if err := os.RemoveAll(local); err != nil {
		return err
	}
	if err := os.MkdirAll(local, 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(h.dir, "ttcd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	executable, err := selfExecutable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "--socket", h.socket(),
		"--platform", defaultRuntimePlatform, "--control-holder",
		"--cluster-node-id", h.nodeID+controlHolderSuffix,
		"--cluster-listen", "127.0.0.1:0", "--cluster-advertise", "127.0.0.1:0",
		"--cluster-page-listen", "127.0.0.1:0")
	// It is this daemon's binary, and named as it is.
	command.Args[0] = os.Args[0]
	encoded, err := json.Marshal(stillness)
	if err != nil {
		return err
	}
	command.Env = append(os.Environ(), "TTC_ROOT="+h.sharedRoot, "TTC_LOCAL_ROOT="+local, controlHolderStillnessEnv+"="+string(encoded))
	command.Stdout, command.Stderr = log, log
	// It ends with the gateway, which relays to it.
	setControlHolderProcess(command)
	if err := command.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	h.process, h.exited, h.started, h.ended = command.Process, exited, time.Now(), false
	go func() {
		_ = command.Wait()
		close(exited)
	}()
	if h.stderr != nil {
		fmt.Fprintf(h.stderr, "ttcd: no compute daemon runs: starting this login node's control holder, node %s, to hold the cluster's authority until one does\n", h.nodeID+controlHolderSuffix)
	}
	return nil
}

// stop stops the holder, as a daemon stop does, so that a compute daemon
// takes the authority at once, and waits for it to end. A holder still
// starting answers once it has started.
func (h *controlHolder) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	if !h.running() {
		return
	}
	deadline := time.After(time.Minute)
	for {
		if conn, err := connectDaemon(h.socket()); err == nil {
			request := daemonRequest{Version: daemonAPIVersion, Action: "stop", ManageStop: true}
			_, _ = exchangeDaemon(conn, request, strings.NewReader(""), io.Discard, io.Discard)
			conn.Close()
			break
		}
		select {
		case <-h.exited:
			return
		case <-deadline:
			_ = h.process.Kill()
			<-h.exited
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	select {
	case <-h.exited:
	case <-deadline:
		_ = h.process.Kill()
		<-h.exited
	}
}
