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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/processutil"
	"gvisor.dev/gvisor/runsc/ttc/protocol"
)

// A daemon keeps every file of its own in its root: the TTC_ROOT of a
// standalone daemon, and the TTC_LOCAL_ROOT of a daemon of a cluster.
//
//	ttcd.sock      its API socket; ttcd.sock.start.lock serializes its start
//	ttcd.log       its log, which the client that starts it opens
//	run/           the Unix sockets of its sandboxes and helpers
//	tmp/           the TMPDIR of the daemon and of every process it starts
//	runtime/runsc  runsc's state of its sandboxes
//
// What run/, tmp/ and runtime/runsc hold lives only as long as the processes
// it serves. A starting daemon removes what outlived them: everything, after
// the host restarted, and otherwise what serves a process that is gone.
const (
	daemonRunDirName = "run"
	daemonTmpDirName = "tmp"
	// daemonRunDirEnv names run/ to the processes a daemon starts. runsc
	// creates the control sockets of the sandboxes there.
	daemonRunDirEnv = "TTC_RUN_DIR"
	// daemonBootName is the file of run/ that names the host's boot whose
	// processes the files of run/, tmp/ and runtime/runsc serve.
	daemonBootName = "boot"
	// daemonTmpStaleAge is how long an entry of tmp/ stays unmodified before
	// a starting daemon removes it. The daemon's operations remove their own
	// temporary files, so one that stays that long was left by a crash.
	daemonTmpStaleAge = time.Hour
)

// daemonRunNameMax is the length of the longest name a daemon gives a socket
// in run/.
var daemonRunNameMax = len(daemonRunSocketName("c", ""))

// hostBootID returns an identifier of the host's current boot.
var hostBootID = readHostBootID

// daemonLayout is where a daemon keeps its files.
type daemonLayout struct {
	root   string
	socket string
	run    string
	tmp    string
}

func newDaemonLayout(root string) daemonLayout {
	return daemonLayout{
		root:   root,
		socket: protocol.DaemonSocket(root),
		run:    filepath.Join(root, daemonRunDirName),
		tmp:    filepath.Join(root, daemonTmpDirName),
	}
}

// daemonRunSocketName returns a name for a socket of run/ that serves key,
// short enough for any root checkDaemonSocketPaths admits.
func daemonRunSocketName(kind, key string) string {
	digest := sha256.Sum256([]byte(key))
	return kind + hex.EncodeToString(digest[:8]) + ".sock"
}

// daemonRunDir returns the run/ of the daemon this process runs for, or "" if
// it runs for none.
func daemonRunDir() string {
	return os.Getenv(daemonRunDirEnv)
}

// checkDaemonRootFilesystem refuses a daemon's root on a network filesystem,
// unless allow. The root holds live Unix sockets, locks, and the state of the
// processes of one host, which a network filesystem does not keep to the
// host; a standalone TTC_ROOT on one would also be shared with every host that
// mounts it. It returns the name of the network filesystem it allowed, if any.
func checkDaemonRootFilesystem(variable, root string, allow bool) (string, error) {
	// The root may not exist yet: check the filesystem it will be made on.
	path := root
	for {
		var statfs unix.Statfs_t
		err := daemonStatfs(path, &statfs)
		if err == nil {
			name := daemonStatfsNetworkFilesystem(&statfs)
			if name != "" && !allow {
				return "", fmt.Errorf("%s %q is on a network filesystem (%s); a daemon's root must be on a local filesystem (pass --allow-network-filesystem-root to start anyway)", variable, root, name)
			}
			return name, nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, os.ErrNotExist) || parent == path {
			return "", fmt.Errorf("checking the filesystem of %s %q: %w", variable, root, err)
		}
		path = parent
	}
}

// checkDaemonSocketPaths refuses a root too long for the paths of the Unix
// sockets a daemon makes in it.
func checkDaemonSocketPaths(layout daemonLayout) error {
	longest := filepath.Join(layout.run, strings.Repeat("x", daemonRunNameMax))
	for _, path := range []string{layout.socket, longest} {
		if len(path) >= daemonUnixPathMax {
			excess := len(path) - (daemonUnixPathMax - 1)
			return fmt.Errorf("the daemon's root %q is %d bytes too long for the path of its Unix socket %q, which must be shorter than %d bytes", layout.root, excess, path, daemonUnixPathMax)
		}
	}
	return nil
}

// prepareDaemonLayout makes the directories of a daemon's own files and gives
// the processes it starts, and itself, their places in them.
func prepareDaemonLayout(layout daemonLayout, runtimeRoot string) error {
	for _, dir := range []string{layout.run, layout.tmp, runtimeRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("creating %q: %w", dir, err)
		}
	}
	if err := os.Setenv("TMPDIR", layout.tmp); err != nil {
		return err
	}
	return os.Setenv(daemonRunDirEnv, layout.run)
}

// sweepDaemonFiles removes the files of run/, tmp/ and runtimeRoot, if any,
// that serve no process: all of them if the host restarted since they were
// made. deleteSandbox deletes the state of a sandbox that is gone, as runsc
// does, with the processes it leaves; runtimeRoot is that of runsc. It runs
// with the daemon's start lock held and no daemon serving.
func sweepDaemonFiles(layout daemonLayout, runtimeRoot string, deleteSandbox func(id string) error, stderr io.Writer) {
	warn := func(format string, args ...any) {
		if stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: "+format+"\n", args...)
		}
	}
	boot, err := hostBootID()
	if err != nil {
		warn("reading the host's boot ID: %v", err)
	}
	bootPath := filepath.Join(layout.run, daemonBootName)
	previous, _ := os.ReadFile(bootPath)
	if boot != "" && strings.TrimSpace(string(previous)) != boot {
		// Every process these files served is gone, and the PIDs that runsc
		// recorded may now be those of others: remove the files outright.
		for _, dir := range []string{layout.run, layout.tmp} {
			if err := removeDirectoryEntries(dir, nil); err != nil {
				warn("removing the files of an earlier boot from %q: %v", dir, err)
			}
		}
		if runtimeRoot != "" {
			if err := removeDirectoryEntries(runtimeRoot, isRunscRuntimeEntry); err != nil {
				warn("removing the sandbox state of an earlier boot from %q: %v", runtimeRoot, err)
			}
		}
		if err := os.WriteFile(bootPath, []byte(boot+"\n"), 0600); err != nil {
			warn("recording the host's boot: %v", err)
		}
		return
	}
	if runtimeRoot != "" {
		sweepRunscRuntime(runtimeRoot, deleteSandbox, warn)
	}
	sweepRunSockets(layout.run, warn)
	cutoff := time.Now().Add(-daemonTmpStaleAge)
	if err := removeDirectoryEntries(layout.tmp, func(entry os.DirEntry) bool {
		info, err := entry.Info()
		return err == nil && info.ModTime().Before(cutoff)
	}); err != nil {
		warn("removing stale temporary files from %q: %v", layout.tmp, err)
	}
}

// transientBundleDirName is the directory of runsc's root that holds the
// bundles for starting sandboxes.
const transientBundleDirName = "ttc-transient-bundles"

// runscStateSummary is what the sweep reads of runsc's state of a sandbox.
type runscStateSummary struct {
	id  string
	pid int
}

// readRunscStateSummary reads the sandbox ID and PID from runsc's state file
// at path.
func readRunscStateSummary(path string) (runscStateSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runscStateSummary{}, err
	}
	var state struct {
		ID      string `json:"id"`
		Sandbox struct {
			PID int `json:"pid"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return runscStateSummary{}, err
	}
	return runscStateSummary{id: state.ID, pid: state.Sandbox.PID}, nil
}

// isRunscRuntimeEntry reports whether entry, of runsc's root, is runsc's state
// of a sandbox or a bundle for starting one.
func isRunscRuntimeEntry(entry os.DirEntry) bool {
	name := entry.Name()
	return strings.HasSuffix(name, ".state") || strings.HasSuffix(name, ".lock") || name == transientBundleDirName
}

// sweepRunscRuntime deletes the sandboxes of runtimeRoot whose Sentry is gone
// and removes the locks runsc leaves of deleted ones.
func sweepRunscRuntime(runtimeRoot string, deleteSandbox func(id string) error, warn func(string, ...any)) {
	entries, err := os.ReadDir(runtimeRoot)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			warn("reading %q: %v", runtimeRoot, err)
		}
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".state") {
			continue
		}
		state, err := readRunscStateSummary(filepath.Join(runtimeRoot, name))
		if err != nil || state.id == "" || processutil.IsRunning(state.pid) {
			continue
		}
		if deleteSandbox == nil {
			continue
		}
		if err := deleteSandbox(state.id); err != nil {
			warn("deleting sandbox %q, whose Sentry is gone: %v", state.id, err)
		}
	}
	// runsc removes a sandbox's lock with its state, but recreates it when
	// asked about the sandbox afterwards. A lock that no process holds and
	// that no state goes with is stale.
	entries, err = os.ReadDir(runtimeRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".lock") {
			continue
		}
		base := strings.TrimSuffix(name, ".lock")
		if _, err := os.Stat(filepath.Join(runtimeRoot, base+".state")); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		removeUnheldLock(filepath.Join(runtimeRoot, name))
	}
}

// removeUnheldLock removes the lock file at path if no process holds it.
func removeUnheldLock(path string) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return
	}
	_ = os.Remove(path)
}

// sweepRunSockets removes the sockets of run/ that no process listens on.
func sweepRunSockets(run string, warn func(string, ...any)) {
	entries, err := os.ReadDir(run)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			warn("reading %q: %v", run, err)
		}
		return
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSocket == 0 {
			continue
		}
		path := filepath.Join(run, entry.Name())
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			conn.Close()
			continue
		}
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(path)
		}
	}
}

// removeDirectoryEntries removes the entries of dir that remove selects, or
// all of them if remove is nil.
func removeDirectoryEntries(dir string, remove func(os.DirEntry) bool) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if remove != nil && !remove(entry) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
