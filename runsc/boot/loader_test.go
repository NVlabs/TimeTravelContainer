// Copyright 2018 The gVisor Authors.
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

package boot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/moby/sys/capability"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/control/server"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/eventfd"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/pipefs"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/signalfd"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/timerfd"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/kernel/shm"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/seccheck"
	unixsocket "gvisor.dev/gvisor/pkg/sentry/socket/unix"
	unixtransport "gvisor.dev/gvisor/pkg/sentry/socket/unix/transport"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/fsgofer"
	"gvisor.dev/gvisor/runsc/specutils"
)

func init() {
	log.SetLevel(log.Debug)
	if err := fsgofer.OpenProcSelfFD("/proc/self/fd"); err != nil {
		panic(err)
	}
}

func testConfig() *config.Config {
	testFlags := flag.NewFlagSet("test", flag.ContinueOnError)
	config.RegisterFlags(testFlags)
	conf, err := config.NewFromFlags(testFlags)
	if err != nil {
		panic(err)
	}
	// Change test defaults.
	conf.RootDir = "unused_root_dir"
	conf.Network = config.NetworkNone
	conf.DisableSeccomp = true
	return conf
}

// testSpec returns a simple spec that can be used in tests.
func testSpec() *specs.Spec {
	return &specs.Spec{
		// The host filesystem root is the sandbox root.
		Root: &specs.Root{
			Path:     "/",
			Readonly: true,
		},
		Process: &specs.Process{
			Args: []string{"/bin/true"},
		},
	}
}

// startGofer starts a new gofer routine serving 'root' path. It returns the
// sandbox side of the connection, and a function that when called will stop the
// gofer.
func startGofer(root string, conf *config.Config) (int, func(), error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, nil, err
	}
	sandboxEnd, goferEnd := fds[0], fds[1]

	socket, err := unet.NewSocket(goferEnd)
	if err != nil {
		unix.Close(sandboxEnd)
		unix.Close(goferEnd)
		return 0, nil, fmt.Errorf("error creating server on FD %d: %v", goferEnd, err)
	}
	server := lisafs.NewServer()
	connImpl := fsgofer.NewConnectionImpl(&fsgofer.Config{
		HostUDS:            conf.GetHostUDS(),
		HostFifo:           conf.HostFifo,
		DonateMountPointFD: conf.DirectFS,
	})
	c, err := server.CreateConnection(socket, root, fsgofer.ConnectionOpts(true), connImpl)
	if err != nil {
		return 0, nil, err
	}
	server.StartConnection(c)
	// Closing the gofer socket will stop the gofer and exit goroutine above.
	cleanup := func() {
		if err := socket.Close(); err != nil {
			log.Warningf("Error closing gofer socket: %v", err)
		}
		server.Wait()
		server.Destroy()
	}
	return sandboxEnd, cleanup, nil
}

func createLoader(conf *config.Config, spec *specs.Spec) (*Loader, func(), error) {
	return createLoaderWithGoferMountConf(conf, spec, specutils.GoferMountConf{Lower: specutils.Lisafs, Upper: specutils.NoOverlay})
}

func createLoaderWithGoferMountConf(conf *config.Config, spec *specs.Spec, mountConf specutils.GoferMountConf) (*Loader, func(), error) {
	sock := fmt.Sprintf("\x00loader-test.%010d", rand.Int())
	fd, err := server.CreateSocket(sock)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create socket: %w", err)
	}
	sandEnd, cleanup, err := startGofer(spec.Root.Path, conf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to start gofer: %w", err)
	}

	// Loader takes ownership of stdio.
	var stdio []int
	for _, f := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		fd := int(f.Fd())
		newFd, err := unix.Dup(fd)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to dup FD %d: %w", fd, err)
		}
		stdio = append(stdio, newFd)
	}

	args := Args{
		ID:               "foo",
		Spec:             spec,
		Conf:             conf,
		ControllerFD:     fd,
		GoferFDs:         []int{sandEnd},
		DevGoferFD:       -1,
		StdioFDs:         stdio,
		GoferMountConfs:  []specutils.GoferMountConf{mountConf},
		PodInitConfigFD:  -1,
		ExecFD:           -1,
		RootfsUpperTarFD: -1,
	}
	l, err := New(args)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("boot.New: %w", err)
	}
	return l, cleanup, nil
}

// TestRun runs a simple application in a sandbox and checks that it succeeds.
func TestRun(t *testing.T) {
	l, cleanup, err := createLoader(testConfig(), testSpec())
	if err != nil {
		t.Fatalf("error creating loader: %v", err)
	}

	defer l.Destroy()
	defer cleanup()

	// Start a goroutine to read the start chan result, otherwise Run will
	// block forever.
	var resultChanErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		resultChanErr = <-l.ctrl.manager.startResultChan
		wg.Done()
	}()

	// Run the container.
	if err := l.Run(); err != nil {
		t.Errorf("error running container: %v", err)
	}

	// We should have not gotten an error on the startResultChan.
	wg.Wait()
	if resultChanErr != nil {
		t.Errorf("error on startResultChan: %v", resultChanErr)
	}

	// Wait for the application to exit.  It should succeed.
	if status := l.WaitExit(); !status.Exited() || status.ExitStatus() != 0 {
		t.Errorf("application exited with %s, want exit status 0", status)
	}
}

// TestRunLiveForkNetworkNone forks a live loader with no host-facing network.
func TestRunLiveForkNetworkNone(t *testing.T) {
	spec := testSpec()
	spec.Process.Args = []string{"/bin/sleep", "30"}
	spec.Root.Readonly = false
	conf := testConfig()
	conf.DirectFS = false
	l, cleanup, err := createLoaderWithGoferMountConf(conf, spec, specutils.GoferMountConf{Lower: specutils.Lisafs, Upper: specutils.MemoryOverlay})
	if err != nil {
		t.Fatalf("error creating loader: %v", err)
	}
	defer cleanup()
	defer l.Destroy()

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- <-l.ctrl.manager.startResultChan
	}()
	if err := l.Run(); err != nil {
		t.Fatalf("running container: %v", err)
	}
	if err := <-resultCh; err != nil {
		t.Fatalf("start result: %v", err)
	}

	// Standard OCI stdio is host-facing and therefore ineligible. Close it at
	// the fork boundary so this fixture is a self-contained container.
	l.k.Pause()
	ctx := l.k.SupervisorContext()
	for fd := int32(0); fd < 3; fd++ {
		if file := l.k.GlobalInit().Leader().FDTable().Remove(ctx, fd); file != nil {
			file.DecRef(ctx)
		}
	}
	hostEventFD, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatalf("creating host eventfd: %v", err)
	}
	hostEventFile, err := eventfd.NewFromHost(ctx, l.k.VFS(), hostEventFD, linux.O_RDWR|linux.O_NONBLOCK)
	if err != nil {
		unix.Close(hostEventFD)
		t.Fatalf("importing host eventfd: %v", err)
	}
	const rejectedFD = int32(9)
	if _, err := l.k.GlobalInit().Leader().FDTable().NewFDAt(ctx, rejectedFD, hostEventFile, kernel.FDFlags{}); err != nil {
		hostEventFile.DecRef(ctx)
		t.Fatalf("installing host eventfd: %v", err)
	}
	hostEventFile.DecRef(ctx)
	if _, err := l.family.Fork(&ForkBranchArgs{ID: "host-eventfd"}); err == nil {
		t.Fatal("live fork unexpectedly accepted a host-backed eventfd")
	} else {
		var resourceErr *kernel.LiveForkResourceError
		if !errors.As(err, &resourceErr) {
			t.Fatalf("host eventfd rejection is not a resource report: %v", err)
		}
		if len(resourceErr.Issues) != 1 || resourceErr.Issues[0].PID != 1 || resourceErr.Issues[0].FD != rejectedFD {
			t.Fatalf("host eventfd rejection issues: got %+v, want pid 1 fd %d", resourceErr.Issues, rejectedFD)
		}
	}
	rejectedEventFile := l.k.GlobalInit().Leader().FDTable().Remove(ctx, rejectedFD)
	if rejectedEventFile == nil {
		t.Fatalf("host eventfd %d disappeared", rejectedFD)
	}
	rejectionEpollFile, err := l.k.VFS().NewEpollInstanceFD(ctx)
	if err != nil {
		rejectedEventFile.DecRef(ctx)
		t.Fatalf("creating rejection epoll: %v", err)
	}
	rejectionEpoll := rejectionEpollFile.Impl().(*vfs.EpollInstance)
	if err := rejectionEpoll.AddInterest(rejectedEventFile, rejectedFD, linux.EpollEvent{Events: linux.EPOLLIN}); err != nil {
		rejectionEpollFile.DecRef(ctx)
		rejectedEventFile.DecRef(ctx)
		t.Fatalf("registering host eventfd with rejection epoll: %v", err)
	}
	const rejectionEpollFD = int32(8)
	if _, err := l.k.GlobalInit().Leader().FDTable().NewFDAt(ctx, rejectionEpollFD, rejectionEpollFile, kernel.FDFlags{}); err != nil {
		rejectionEpollFile.DecRef(ctx)
		rejectedEventFile.DecRef(ctx)
		t.Fatalf("installing rejection epoll: %v", err)
	}
	rejectionEpollFile.DecRef(ctx)
	if _, err := l.family.Fork(&ForkBranchArgs{ID: "epoll-host-eventfd"}); err == nil {
		t.Fatal("live fork unexpectedly accepted an epoll-reachable host eventfd")
	} else {
		var resourceErr *kernel.LiveForkResourceError
		if !errors.As(err, &resourceErr) {
			t.Fatalf("epoll host eventfd rejection is not a resource report: %v", err)
		}
		if len(resourceErr.Issues) != 1 || resourceErr.Issues[0].FD != rejectionEpollFD {
			t.Fatalf("epoll host eventfd rejection issues: got %+v, want root fd %d", resourceErr.Issues, rejectionEpollFD)
		}
	}
	if file := l.k.GlobalInit().Leader().FDTable().Remove(ctx, rejectionEpollFD); file != nil {
		file.DecRef(ctx)
	}
	rejectedEventFile.DecRef(ctx)
	pipeReader, pipeWriter, err := pipefs.NewConnectedPipeFDs(ctx, l.k.PipeMount(), linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating internal pipe: %v", err)
	}
	pipeFDs, err := l.k.GlobalInit().Leader().FDTable().NewFDs(ctx, 10, []*vfs.FileDescription{pipeReader, pipeWriter}, kernel.FDFlags{})
	pipeReader.DecRef(ctx)
	pipeWriter.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing internal pipe: %v", err)
	}
	parentReader := liveForkTestFD(t, l.k, pipeFDs[0])
	parentWriter := liveForkTestFD(t, l.k, pipeFDs[1])
	liveForkTestWrite(t, ctx, parentWriter, "seed")
	eventFile, err := eventfd.New(ctx, l.k.VFS(), 7, false, linux.O_RDWR|linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating internal eventfd: %v", err)
	}
	eventFD, err := l.k.GlobalInit().Leader().FDTable().NewFD(ctx, 12, eventFile, kernel.FDFlags{})
	eventFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing internal eventfd: %v", err)
	}
	parentEvent := liveForkTestFD(t, l.k, eventFD)
	epollFile, err := l.k.VFS().NewEpollInstanceFD(ctx)
	if err != nil {
		t.Fatalf("creating epoll instance: %v", err)
	}
	parentEpoll := epollFile.Impl().(*vfs.EpollInstance)
	const epollData = int32(0x544f53)
	if err := parentEpoll.AddInterest(parentReader, pipeFDs[0], linux.EpollEvent{
		Events: linux.EPOLLIN,
		Data:   [2]int32{epollData, 0},
	}); err != nil {
		t.Fatalf("registering pipe with epoll: %v", err)
	}
	epollFD, err := l.k.GlobalInit().Leader().FDTable().NewFD(ctx, 13, epollFile, kernel.FDFlags{})
	epollFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing epoll instance: %v", err)
	}
	parentEpollFile := liveForkTestFD(t, l.k, epollFD)
	parentEpoll = parentEpollFile.Impl().(*vfs.EpollInstance)
	timerFile, err := timerfd.New(ctx, l.k.VFS(), l.k.MonotonicClock(), linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating timerfd: %v", err)
	}
	parentTimer := timerFile.Impl().(*timerfd.TimerFileDescription)
	parentTimer.NotifyTimer(2)
	timerFD, err := l.k.GlobalInit().Leader().FDTable().NewFD(ctx, 14, timerFile, kernel.FDFlags{})
	timerFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing timerfd: %v", err)
	}
	parentTimerFile := liveForkTestFD(t, l.k, timerFD)
	parentTimer = parentTimerFile.Impl().(*timerfd.TimerFileDescription)
	parentTask := l.k.GlobalInit().Leader()
	shmSegment, err := parentTask.IPCNamespace().ShmRegistry().FindOrCreate(parentTask, 1, 0, hostarch.PageSize, 0600, true, true, true)
	if err != nil {
		t.Fatalf("creating SysV SHM segment: %v", err)
	}
	shmOpts, err := shmSegment.ConfigureAttach(parentTask, 0, shm.AttachOpts{})
	if err != nil {
		shmSegment.DecRef(parentTask)
		t.Fatalf("configuring first SysV SHM mapping: %v", err)
	}
	shmAddr1, err := parentTask.MemoryManager().MMap(parentTask, shmOpts)
	if err != nil {
		shmSegment.DecRef(parentTask)
		t.Fatalf("mapping first SysV SHM alias: %v", err)
	}
	shmOpts, err = shmSegment.ConfigureAttach(parentTask, 0, shm.AttachOpts{})
	if err != nil {
		shmSegment.DecRef(parentTask)
		t.Fatalf("configuring second SysV SHM mapping: %v", err)
	}
	shmAddr2, err := parentTask.MemoryManager().MMap(parentTask, shmOpts)
	if err != nil {
		shmSegment.DecRef(parentTask)
		t.Fatalf("mapping second SysV SHM alias: %v", err)
	}
	shmSegment.DecRef(parentTask)
	liveForkTestMMWrite(t, parentTask, shmAddr1, "seed")
	liveForkTestMMRead(t, parentTask, shmAddr2, "seed")
	signalMask := linux.SignalSetOf(linux.SIGUSR1)
	parentTask.SetSignalMask(parentTask.SignalMask() | signalMask)
	signalFile, err := signalfd.New(l.k.VFS(), parentTask, signalMask, linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating signalfd: %v", err)
	}
	signalFD, err := parentTask.FDTable().NewFD(ctx, 15, signalFile, kernel.FDFlags{})
	signalFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing signalfd: %v", err)
	}
	parentSignalFile := liveForkTestFD(t, l.k, signalFD)
	if err := parentTask.SendSignal(kernel.SignalInfoPriv(linux.SIGUSR1)); err != nil {
		t.Fatalf("queuing parent signal before fork: %v", err)
	}
	parentRoot := parentTask.MountNamespace().Root(ctx)
	inotifyFile, err := vfs.NewInotifyFD(ctx, l.k.VFS(), linux.O_NONBLOCK)
	if err != nil {
		t.Fatalf("creating inotify instance: %v", err)
	}
	parentInotify := inotifyFile.Impl().(*vfs.Inotify)
	watchID := parentInotify.AddWatch(parentRoot.Dentry(), linux.IN_ATTRIB)
	inotifyFD, err := parentTask.FDTable().NewFD(ctx, 16, inotifyFile, kernel.FDFlags{})
	inotifyFile.DecRef(ctx)
	if err != nil {
		t.Fatalf("installing inotify instance: %v", err)
	}
	parentInotifyFile := liveForkTestFD(t, l.k, inotifyFD)
	parentRoot.Dentry().Watches().Notify(ctx, "", linux.IN_ATTRIB, 0, vfs.InodeEvent, false)
	unixEP1, unixEP2 := unixtransport.NewPair(parentTask, linux.SOCK_SEQPACKET, l.k)
	unixFile1, unixErr := unixsocket.NewSockfsFile(parentTask, unixEP1, linux.SOCK_SEQPACKET)
	if unixErr != nil {
		t.Fatalf("creating first internal Unix socket: %v", unixErr)
	}
	unixFile2, unixErr := unixsocket.NewSockfsFile(parentTask, unixEP2, linux.SOCK_SEQPACKET)
	if unixErr != nil {
		unixFile1.DecRef(parentTask)
		t.Fatalf("creating second internal Unix socket: %v", unixErr)
	}
	unixFDs, err := parentTask.FDTable().NewFDs(parentTask, 17, []*vfs.FileDescription{unixFile1, unixFile2}, kernel.FDFlags{})
	unixFile1.DecRef(parentTask)
	unixFile2.DecRef(parentTask)
	if err != nil {
		t.Fatalf("installing internal Unix socketpair: %v", err)
	}
	parentUnix1 := liveForkTestFD(t, l.k, unixFDs[0])
	parentUnix2 := liveForkTestFD(t, l.k, unixFDs[1])
	liveForkTestWrite(t, parentTask, parentUnix1, "seed")
	memfdFile, err := tmpfs.NewMemfd(parentTask, parentTask.Credentials(), l.k.ShmMount(), false, "live-fork")
	if err != nil {
		t.Fatalf("creating memfd: %v", err)
	}
	const memfdFD = int32(19)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, memfdFD, memfdFile, kernel.FDFlags{}); err != nil {
		memfdFile.DecRef(parentTask)
		t.Fatalf("installing memfd: %v", err)
	}
	memfdFile.DecRef(parentTask)
	parentMemfd := liveForkTestFD(t, l.k, memfdFD)
	liveForkTestPWrite(t, parentTask, parentMemfd, 0, "seed")
	const overlayPath = "timetravel-overlay-state"
	overlayFile := liveForkTestCreateFile(t, parentTask, overlayPath)
	const overlayFD = int32(20)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, overlayFD, overlayFile, kernel.FDFlags{}); err != nil {
		overlayFile.DecRef(parentTask)
		t.Fatalf("installing overlay file: %v", err)
	}
	overlayFile.DecRef(parentTask)
	parentOverlayFile := liveForkTestFD(t, l.k, overlayFD)
	liveForkTestPWrite(t, parentTask, parentOverlayFile, 0, "seed")
	hardlinkFile := liveForkTestCreateFile(t, parentTask, "hardlink-source")
	liveForkTestPWrite(t, parentTask, hardlinkFile, 0, "seed")
	hardlinkFile.DecRef(parentTask)
	liveForkTestLink(t, parentTask, "hardlink-source", "hardlink-alias")
	truncateFile := liveForkTestCreateFile(t, parentTask, "truncate-state")
	liveForkTestPWrite(t, parentTask, truncateFile, 0, "seed")
	const truncateFD = int32(21)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, truncateFD, truncateFile, kernel.FDFlags{}); err != nil {
		truncateFile.DecRef(parentTask)
		t.Fatalf("installing truncate file: %v", err)
	}
	truncateFile.DecRef(parentTask)
	parentTruncateFile := liveForkTestFD(t, l.k, truncateFD)

	childInfo, err := l.family.Fork(&ForkBranchArgs{ID: "child"})
	if err != nil {
		t.Fatalf("forking live loader: %v", err)
	}
	if got, want := childInfo.State, BranchRunning; got != want {
		t.Errorf("child state after fork: got %q, want %q", got, want)
	}
	child, err := l.family.branch(childInfo.ID)
	if err != nil {
		t.Fatalf("looking up child: %v", err)
	}
	if l.k.RootNetworkNamespace().Stack() == child.k.RootNetworkNamespace().Stack() {
		t.Error("parent and child share a network stack object")
	}
	parentPID := l.k.RootPIDNamespace().IDOfThreadGroup(l.k.GlobalInit())
	childPID := child.k.RootPIDNamespace().IDOfThreadGroup(child.k.GlobalInit())
	if parentPID != childPID {
		t.Errorf("init PID changed: parent=%d child=%d", parentPID, childPID)
	}
	childCtx := child.k.SupervisorContext()
	childReader := liveForkTestFD(t, child.k, pipeFDs[0])
	childWriter := liveForkTestFD(t, child.k, pipeFDs[1])
	childEvent := liveForkTestFD(t, child.k, eventFD)
	childEpollFile := liveForkTestFD(t, child.k, epollFD)
	childEpoll := childEpollFile.Impl().(*vfs.EpollInstance)
	childTimerFile := liveForkTestFD(t, child.k, timerFD)
	childTimer := childTimerFile.Impl().(*timerfd.TimerFileDescription)
	childTask := child.k.GlobalInit().Leader()
	liveForkTestMMRead(t, childTask, shmAddr1, "seed")
	liveForkTestMMRead(t, childTask, shmAddr2, "seed")
	liveForkTestMMWrite(t, childTask, shmAddr1, "chld")
	liveForkTestMMRead(t, childTask, shmAddr2, "chld")
	liveForkTestMMRead(t, parentTask, shmAddr1, "seed")
	liveForkTestMMWrite(t, parentTask, shmAddr2, "prnt")
	liveForkTestMMRead(t, parentTask, shmAddr1, "prnt")
	liveForkTestMMRead(t, childTask, shmAddr2, "chld")
	childSignalFile := liveForkTestFD(t, child.k, signalFD)
	childRoot := childTask.MountNamespace().Root(childCtx)
	childInotifyFile := liveForkTestFD(t, child.k, inotifyFD)
	childInotify := childInotifyFile.Impl().(*vfs.Inotify)
	childUnix1 := liveForkTestFD(t, child.k, unixFDs[0])
	childUnix2 := liveForkTestFD(t, child.k, unixFDs[1])
	childMemfd := liveForkTestFD(t, child.k, memfdFD)
	childOverlayFile := liveForkTestFD(t, child.k, overlayFD)
	childTruncateFile := liveForkTestFD(t, child.k, truncateFD)

	// Pipe endpoints still alias one queue inside a branch, but queues and
	// readiness are duplicated across branches.
	liveForkTestRead(t, childCtx, childReader, "seed")
	if got := childReader.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("child still readable after consuming seed: %#x", got)
	}
	if got := parentReader.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child read consumed parent's seed: parent readiness %#x", got)
	}
	liveForkEpollEvents(t, childEpoll, nil)
	liveForkEpollEvents(t, parentEpoll, []linux.EpollEvent{{
		Events: linux.EPOLLIN,
		Data:   [2]int32{epollData, 0},
	}})
	liveForkTestRead(t, ctx, parentReader, "seed")
	liveForkEpollEvents(t, parentEpoll, nil)
	liveForkTestWrite(t, ctx, parentWriter, "parent")
	if got := childReader.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent write made child readable: %#x", got)
	}
	liveForkEpollEvents(t, parentEpoll, []linux.EpollEvent{{
		Events: linux.EPOLLIN,
		Data:   [2]int32{epollData, 0},
	}})
	liveForkEpollEvents(t, childEpoll, nil)
	if err := childEpoll.DeleteInterest(childReader, pipeFDs[0]); err != nil {
		t.Fatalf("deleting child epoll interest: %v", err)
	}
	liveForkTestWrite(t, childCtx, childWriter, "child")
	liveForkEpollEvents(t, childEpoll, nil)
	liveForkTestRead(t, ctx, parentReader, "parent")
	liveForkTestRead(t, childCtx, childReader, "child")
	liveForkTestWouldBlock(t, ctx, parentReader)
	liveForkTestWouldBlock(t, childCtx, childReader)

	// Internal eventfd counters and readiness follow the same branch-local
	// semantics as pipe queues.
	liveForkEventRead(t, childCtx, childEvent, 7)
	if got := parentEvent.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child eventfd read consumed parent counter: %#x", got)
	}
	liveForkEventRead(t, ctx, parentEvent, 7)
	liveForkEventWrite(t, ctx, parentEvent, 3)
	if got := childEvent.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent eventfd write made child readable: %#x", got)
	}
	liveForkEventWrite(t, childCtx, childEvent, 4)
	liveForkEventRead(t, ctx, parentEvent, 3)
	liveForkEventRead(t, childCtx, childEvent, 4)
	liveForkEventWouldBlock(t, ctx, parentEvent)
	liveForkEventWouldBlock(t, childCtx, childEvent)

	// Timerfd expiration counters and notifications are branch-local, while
	// Resume above reconstructs each cloned timer's runtime-only host fields.
	liveForkCounterRead(t, childCtx, childTimerFile, 2, "child timerfd")
	if got := parentTimerFile.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child timerfd read consumed parent expirations: %#x", got)
	}
	liveForkCounterRead(t, ctx, parentTimerFile, 2, "parent timerfd")
	parentTimer.NotifyTimer(3)
	if got := childTimerFile.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent timerfd notification made child readable: %#x", got)
	}
	childTimer.NotifyTimer(4)
	liveForkCounterRead(t, ctx, parentTimerFile, 3, "parent timerfd")
	liveForkCounterRead(t, childCtx, childTimerFile, 4, "child timerfd")
	liveForkEventWouldBlock(t, ctx, parentTimerFile)
	liveForkEventWouldBlock(t, childCtx, childTimerFile)
	entry, timerReady := waiter.NewChannelEntry(waiter.EventIn)
	if err := childTimerFile.EventRegister(&entry); err != nil {
		t.Fatalf("registering child timerfd waiter: %v", err)
	}
	now, _ := childTimer.GetTime()
	childTimer.SetTime(ktime.Setting{Enabled: true, Next: now.Add(20 * time.Millisecond)})
	select {
	case <-timerReady:
	case <-time.After(2 * time.Second):
		t.Fatal("cloned child timerfd did not expire")
	}
	childTimerFile.EventUnregister(&entry)
	liveForkCounterRead(t, childCtx, childTimerFile, 1, "armed child timerfd")
	if got := parentTimerFile.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("child timerfd expiration made parent readable: %#x", got)
	}

	// Signalfd target-task pointers, pending queues, and waiter registrations
	// are reconstructed within each branch.
	liveForkSignalRead(t, childCtx, childSignalFile, linux.SIGUSR1)
	if got := parentSignalFile.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child signalfd read consumed parent pending signal: %#x", got)
	}
	liveForkSignalRead(t, ctx, parentSignalFile, linux.SIGUSR1)
	if err := parentTask.SendSignal(kernel.SignalInfoPriv(linux.SIGUSR1)); err != nil {
		t.Fatalf("queuing parent-only signal: %v", err)
	}
	if got := childSignalFile.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent signal made child signalfd readable: %#x", got)
	}
	if err := childTask.SendSignal(kernel.SignalInfoPriv(linux.SIGUSR1)); err != nil {
		t.Fatalf("queuing child-only signal: %v", err)
	}
	liveForkSignalRead(t, ctx, parentSignalFile, linux.SIGUSR1)
	liveForkSignalRead(t, childCtx, childSignalFile, linux.SIGUSR1)
	liveForkSignalWouldBlock(t, ctx, parentSignalFile)
	liveForkSignalWouldBlock(t, childCtx, childSignalFile)

	// Inotify watch targets, queued events, and watch-list mutations are local
	// to each cloned VFS graph.
	liveForkInotifyRead(t, childCtx, childInotifyFile, watchID, linux.IN_ATTRIB)
	if got := parentInotifyFile.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child inotify read consumed parent event: %#x", got)
	}
	liveForkInotifyRead(t, ctx, parentInotifyFile, watchID, linux.IN_ATTRIB)
	parentRoot.Dentry().Watches().Notify(ctx, "", linux.IN_ATTRIB, 0, vfs.InodeEvent, false)
	if got := childInotifyFile.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent inotify event made child readable: %#x", got)
	}
	childRoot.Dentry().Watches().Notify(childCtx, "", linux.IN_ATTRIB, 0, vfs.InodeEvent, false)
	liveForkInotifyRead(t, ctx, parentInotifyFile, watchID, linux.IN_ATTRIB)
	liveForkInotifyRead(t, childCtx, childInotifyFile, watchID, linux.IN_ATTRIB)
	if err := childInotify.RmWatch(childCtx, watchID); err != nil {
		t.Fatalf("removing child inotify watch: %v", err)
	}
	liveForkInotifyRead(t, childCtx, childInotifyFile, watchID, linux.IN_IGNORED)
	parentRoot.Dentry().Watches().Notify(ctx, "", linux.IN_ATTRIB, 0, vfs.InodeEvent, false)
	liveForkInotifyRead(t, ctx, parentInotifyFile, watchID, linux.IN_ATTRIB)
	liveForkInotifyWouldBlock(t, childCtx, childInotifyFile)

	// A connected internal Unix socketpair preserves peer topology inside each
	// branch, but receive queues and readiness are duplicated at the fork.
	liveForkTestRead(t, childTask, childUnix2, "seed")
	if got := parentUnix2.Readiness(waiter.EventIn); got&waiter.EventIn == 0 {
		t.Errorf("child Unix read consumed parent queued packet: %#x", got)
	}
	liveForkTestRead(t, parentTask, parentUnix2, "seed")
	liveForkTestWrite(t, parentTask, parentUnix1, "parent")
	if got := childUnix2.Readiness(waiter.EventIn); got&waiter.EventIn != 0 {
		t.Errorf("parent Unix write made child peer readable: %#x", got)
	}
	liveForkTestWrite(t, childTask, childUnix1, "child")
	liveForkTestRead(t, parentTask, parentUnix2, "parent")
	liveForkTestRead(t, childTask, childUnix2, "child")
	liveForkTestWouldBlock(t, parentTask, parentUnix2)
	liveForkTestWouldBlock(t, childTask, childUnix2)

	// Tmpfs syscall writes and mapped shared writes detach MemoryFile pages
	// without copying file contents at the fork boundary.
	liveForkTestPRead(t, parentTask, parentMemfd, 0, "seed")
	liveForkTestPRead(t, childTask, childMemfd, 0, "seed")
	liveForkTestPWrite(t, parentTask, parentMemfd, 0, "prnt")
	liveForkTestPRead(t, parentTask, parentMemfd, 0, "prnt")
	liveForkTestPRead(t, childTask, childMemfd, 0, "seed")
	liveForkTestMappedWrite(t, childTask, childMemfd, "chld")
	liveForkTestPRead(t, childTask, childMemfd, 0, "chld")
	liveForkTestPRead(t, parentTask, parentMemfd, 0, "prnt")

	// The writable root uses a branch-local tmpfs OverlayFS upper over the
	// shared immutable gofer lower. Contents, open-unlinked files, and directory
	// topology diverge independently.
	liveForkTestPRead(t, parentTask, parentOverlayFile, 0, "seed")
	liveForkTestPRead(t, childTask, childOverlayFile, 0, "seed")
	liveForkTestPWrite(t, parentTask, parentOverlayFile, 0, "prnt")
	liveForkTestMappedWrite(t, childTask, childOverlayFile, "chld")
	liveForkTestPRead(t, parentTask, parentOverlayFile, 0, "prnt")
	liveForkTestPRead(t, childTask, childOverlayFile, 0, "chld")
	if err := parentOverlayFile.SetStat(ctx, vfs.SetStatOptions{Stat: linux.Statx{Mask: linux.STATX_MODE, Mode: uint16(linux.ModeRegular | 0600)}}); err != nil {
		t.Fatalf("changing parent overlay mode: %v", err)
	}
	liveForkTestMode(t, ctx, parentOverlayFile, 0600)
	liveForkTestMode(t, childCtx, childOverlayFile, 0644)
	liveForkTestUnlink(t, childTask, overlayPath)
	liveForkTestPathMissing(t, childTask, overlayPath)
	liveForkTestPathRead(t, parentTask, overlayPath, "prnt")
	parentOnly := liveForkTestCreateFile(t, parentTask, "parent-only")
	parentOnly.DecRef(parentTask)
	liveForkTestPathMissing(t, childTask, "parent-only")
	liveForkTestPRead(t, childTask, childOverlayFile, 0, "chld")
	parentHardlink := liveForkTestOpenPath(t, parentTask, "hardlink-alias", linux.O_RDWR)
	childHardlink := liveForkTestOpenPath(t, childTask, "hardlink-alias", linux.O_RDWR)
	liveForkTestPWrite(t, ctx, parentHardlink, 0, "PARN")
	liveForkTestPathRead(t, parentTask, "hardlink-source", "PARN")
	liveForkTestPRead(t, childCtx, childHardlink, 0, "seed")
	liveForkTestUnlink(t, childTask, "hardlink-alias")
	liveForkTestPathMissing(t, childTask, "hardlink-alias")
	liveForkTestPathRead(t, childTask, "hardlink-source", "seed")
	liveForkTestPathRead(t, parentTask, "hardlink-alias", "PARN")
	parentHardlink.DecRef(ctx)
	childHardlink.DecRef(childCtx)
	if err := childTruncateFile.SetStat(childCtx, vfs.SetStatOptions{Stat: linux.Statx{Mask: linux.STATX_SIZE, Size: 2}}); err != nil {
		t.Fatalf("truncating child upper file: %v", err)
	}
	liveForkTestPRead(t, parentTask, parentTruncateFile, 0, "seed")
	liveForkTestPRead(t, childTask, childTruncateFile, 0, "se")
	liveForkTestSize(t, ctx, parentTruncateFile, 4)
	liveForkTestSize(t, childCtx, childTruncateFile, 2)
	if err := parentTruncateFile.Allocate(ctx, 0, hostarch.PageSize, hostarch.PageSize); err != nil {
		t.Fatalf("allocating sparse parent upper range: %v", err)
	}
	liveForkTestSize(t, ctx, parentTruncateFile, 2*hostarch.PageSize)
	liveForkTestSize(t, childCtx, childTruncateFile, 2)
	liveForkTestPRead(t, childTask, childTruncateFile, 0, "se")
	parentLowerFile := liveForkTestOpenPath(t, parentTask, "etc/hostname", linux.O_RDONLY)
	originalLower := liveForkTestPReadString(t, parentTask.Kernel().SupervisorContext(), parentLowerFile, 0, 4)
	childCopiedUpFile := liveForkTestOpenPath(t, childTask, "etc/hostname", linux.O_RDWR)
	liveForkTestPWrite(t, childCtx, childCopiedUpFile, 0, "CHLD")
	liveForkTestPRead(t, childCtx, childCopiedUpFile, 0, "CHLD")
	liveForkTestPRead(t, ctx, parentLowerFile, 0, originalLower)
	liveForkTestUnlink(t, childTask, "etc/hostname")
	liveForkTestPathMissing(t, childTask, "etc/hostname")
	liveForkTestPRead(t, childCtx, childCopiedUpFile, 0, "CHLD")
	liveForkTestPRead(t, ctx, parentLowerFile, 0, originalLower)
	parentLowerFile.DecRef(ctx)
	childCopiedUpFile.DecRef(childCtx)

	grandchildInfo, err := l.family.Fork(&ForkBranchArgs{Source: childInfo.ID, ID: "grandchild", Paused: true})
	if err != nil {
		t.Fatalf("recursively forking child: %v", err)
	}
	grandchild, err := l.family.branch(grandchildInfo.ID)
	if err != nil {
		t.Fatalf("looking up grandchild: %v", err)
	}
	grandchildTask := grandchild.k.GlobalInit().Leader()
	liveForkTestMMRead(t, grandchildTask, shmAddr1, "chld")
	liveForkTestMMWrite(t, grandchildTask, shmAddr2, "grnd")
	liveForkTestMMRead(t, grandchildTask, shmAddr1, "grnd")
	liveForkTestMMRead(t, childTask, shmAddr1, "chld")
	liveForkTestMMRead(t, parentTask, shmAddr1, "prnt")
	grandchildMemfd := liveForkTestFD(t, grandchild.k, memfdFD)
	grandchildOverlayFile := liveForkTestFD(t, grandchild.k, overlayFD)
	liveForkTestPRead(t, grandchildTask, grandchildMemfd, 0, "chld")
	liveForkTestPRead(t, grandchildTask, grandchildOverlayFile, 0, "chld")
	liveForkTestPathMissing(t, grandchildTask, overlayPath)
	liveForkTestPathMissing(t, grandchildTask, "parent-only")
	liveForkTestPWrite(t, grandchildTask, grandchildMemfd, 0, "grnd")
	liveForkTestPWrite(t, grandchildTask, grandchildOverlayFile, 0, "grnd")
	liveForkTestPRead(t, childTask, childMemfd, 0, "chld")
	liveForkTestPRead(t, childTask, childOverlayFile, 0, "chld")
	grandchildMemfd.DecRef(grandchildTask)
	grandchildOverlayFile.DecRef(grandchildTask)
	if err := l.family.Destroy(grandchildInfo.ID); err != nil {
		t.Fatalf("destroying grandchild: %v", err)
	}
	parentReader.DecRef(ctx)
	parentWriter.DecRef(ctx)
	parentEvent.DecRef(ctx)
	parentEpollFile.DecRef(ctx)
	parentTimerFile.DecRef(ctx)
	parentSignalFile.DecRef(ctx)
	parentInotifyFile.DecRef(ctx)
	parentRoot.DecRef(ctx)
	parentUnix1.DecRef(parentTask)
	parentUnix2.DecRef(parentTask)
	parentMemfd.DecRef(parentTask)
	parentOverlayFile.DecRef(parentTask)
	parentTruncateFile.DecRef(parentTask)
	childReader.DecRef(childCtx)
	childWriter.DecRef(childCtx)
	childEvent.DecRef(childCtx)
	childEpollFile.DecRef(childCtx)
	childTimerFile.DecRef(childCtx)
	childSignalFile.DecRef(childCtx)
	childInotifyFile.DecRef(childCtx)
	childRoot.DecRef(childCtx)
	childUnix1.DecRef(childTask)
	childUnix2.DecRef(childTask)
	childMemfd.DecRef(childTask)
	childOverlayFile.DecRef(childTask)
	childTruncateFile.DecRef(childTask)
	if err := l.family.Pause(childInfo.ID); err != nil {
		t.Fatalf("pausing child: %v", err)
	}
	if err := l.family.Resume(childInfo.ID); err != nil {
		t.Fatalf("resuming child: %v", err)
	}
	var childProcesses []*control.Process
	if err := l.family.Processes(childInfo.ID, l.root.cid, &childProcesses); err != nil {
		t.Fatalf("listing child processes: %v", err)
	}
	if len(childProcesses) != 1 || childProcesses[0].PID != 1 {
		t.Fatalf("child processes: got %+v, want virtual PID 1", childProcesses)
	}
	if err := l.family.Signal(childInfo.ID, l.root.cid, 0, int32(linux.SIGKILL), DeliverToProcess); err != nil {
		t.Fatalf("signaling child init: %v", err)
	}
	status, err := l.family.Wait(childInfo.ID, l.root.cid, 0)
	if err != nil {
		t.Fatalf("waiting for child init: %v", err)
	}
	ws := linux.WaitStatus(status)
	if !ws.Signaled() || ws.TerminationSignal() != linux.SIGKILL {
		t.Errorf("child wait status: got %v, want SIGKILL", ws)
	}
	if got := l.k.GlobalInit().Leader().ExitState(); got != kernel.TaskExitNone {
		t.Errorf("parent init changed after signaling child: got exit state %v", got)
	}
	if err := l.family.Destroy(childInfo.ID); err != nil {
		t.Fatalf("destroying child: %v", err)
	}
	parentMemfd = liveForkTestFD(t, l.k, memfdFD)
	liveForkTestPRead(t, parentTask, parentMemfd, 0, "prnt")
	parentMemfd.DecRef(parentTask)

	l.k.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	l.k.Unpause()
	l.k.WaitExited()
}

func liveForkTestFD(t *testing.T, k *kernel.Kernel, fd int32) *vfs.FileDescription {
	t.Helper()
	file, _ := k.GlobalInit().Leader().FDTable().Get(fd)
	if file == nil {
		t.Fatalf("FD %d is missing", fd)
	}
	return file
}

func liveForkTestWrite(t *testing.T, ctx context.Context, file *vfs.FileDescription, value string) {
	t.Helper()
	n, err := file.Write(ctx, usermem.BytesIOSequence([]byte(value)), vfs.WriteOptions{})
	if err != nil || n != int64(len(value)) {
		t.Fatalf("Write(%q): got (%d, %v)", value, n, err)
	}
}

func liveForkTestRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, want string) {
	t.Helper()
	buf := make([]byte, len(want))
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if err != nil || n != int64(len(want)) || !bytes.Equal(buf, []byte(want)) {
		t.Fatalf("Read: got (%q, %d, %v), want %q", buf, n, err, want)
	}
}

func liveForkTestWouldBlock(t *testing.T, ctx context.Context, file *vfs.FileDescription) {
	t.Helper()
	buf := make([]byte, 1)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if n != 0 || !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		t.Fatalf("empty nonblocking Read: got (%d, %v), want (0, EWOULDBLOCK)", n, err)
	}
}

func liveForkEventWrite(t *testing.T, ctx context.Context, file *vfs.FileDescription, value uint64) {
	t.Helper()
	var buf [8]byte
	binary.NativeEndian.PutUint64(buf[:], value)
	n, err := file.Write(ctx, usermem.BytesIOSequence(buf[:]), vfs.WriteOptions{})
	if err != nil || n != int64(len(buf)) {
		t.Fatalf("eventfd Write(%d): got (%d, %v)", value, n, err)
	}
}

func liveForkEventRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, want uint64) {
	t.Helper()
	var buf [8]byte
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf[:]), vfs.ReadOptions{})
	got := binary.NativeEndian.Uint64(buf[:])
	if err != nil || n != int64(len(buf)) || got != want {
		t.Fatalf("eventfd Read: got (%d, %d, %v), want %d", got, n, err, want)
	}
}

func liveForkCounterRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, want uint64, kind string) {
	t.Helper()
	var buf [8]byte
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf[:]), vfs.ReadOptions{})
	got := binary.NativeEndian.Uint64(buf[:])
	if err != nil || n != int64(len(buf)) || got != want {
		t.Fatalf("%s Read: got (%d, %d, %v), want %d", kind, got, n, err, want)
	}
}

func liveForkEventWouldBlock(t *testing.T, ctx context.Context, file *vfs.FileDescription) {
	t.Helper()
	var buf [8]byte
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf[:]), vfs.ReadOptions{})
	if n != 0 || !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		t.Fatalf("empty eventfd Read: got (%d, %v), want (0, EWOULDBLOCK)", n, err)
	}
}

func liveForkEpollEvents(t *testing.T, epoll *vfs.EpollInstance, want []linux.EpollEvent) {
	t.Helper()
	got := epoll.ReadEvents(nil, 16)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("epoll events mismatch (-want +got):\n%s", diff)
	}
}

func liveForkSignalRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, want linux.Signal) {
	t.Helper()
	buf := make([]byte, 128)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	got := linux.Signal(binary.NativeEndian.Uint32(buf[:4]))
	if err != nil || n != int64(len(buf)) || got != want {
		t.Fatalf("signalfd Read: got (signal %d, %d, %v), want signal %d", got, n, err, want)
	}
}

func liveForkSignalWouldBlock(t *testing.T, ctx context.Context, file *vfs.FileDescription) {
	t.Helper()
	buf := make([]byte, 128)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if n != 0 || !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		t.Fatalf("empty signalfd Read: got (%d, %v), want (0, EWOULDBLOCK)", n, err)
	}
}

func liveForkInotifyRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, wantWD int32, wantMask uint32) {
	t.Helper()
	buf := make([]byte, 128)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	gotWD := int32(binary.NativeEndian.Uint32(buf[0:4]))
	gotMask := binary.NativeEndian.Uint32(buf[4:8])
	if err != nil || n != 16 || gotWD != wantWD || gotMask != wantMask {
		t.Fatalf("inotify Read: got (wd %d, mask %#x, %d, %v), want (wd %d, mask %#x, 16, nil)", gotWD, gotMask, n, err, wantWD, wantMask)
	}
}

func liveForkInotifyWouldBlock(t *testing.T, ctx context.Context, file *vfs.FileDescription) {
	t.Helper()
	buf := make([]byte, 128)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if n != 0 || !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		t.Fatalf("empty inotify Read: got (%d, %v), want (0, EWOULDBLOCK)", n, err)
	}
}

func liveForkTestPWrite(t *testing.T, ctx context.Context, file *vfs.FileDescription, offset int64, value string) {
	t.Helper()
	n, err := file.PWrite(ctx, usermem.BytesIOSequence([]byte(value)), offset, vfs.WriteOptions{})
	if err != nil || n != int64(len(value)) {
		t.Fatalf("PWrite(%q, %d): got (%d, %v)", value, offset, n, err)
	}
}

func liveForkTestPRead(t *testing.T, ctx context.Context, file *vfs.FileDescription, offset int64, want string) {
	t.Helper()
	buf := make([]byte, len(want))
	n, err := file.PRead(ctx, usermem.BytesIOSequence(buf), offset, vfs.ReadOptions{})
	if err != nil || n != int64(len(buf)) || string(buf) != want {
		t.Fatalf("PRead(%d): got (%q, %d, %v), want %q", offset, buf, n, err, want)
	}
}

func liveForkTestCreateFile(t *testing.T, task *kernel.Task, path string) *vfs.FileDescription {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	file, err := task.Kernel().VFS().OpenAt(ctx, task.Credentials(), &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(path),
	}, &vfs.OpenOptions{
		Flags: linux.O_RDWR | linux.O_CREAT | linux.O_EXCL,
		Mode:  linux.ModeRegular | 0644,
	})
	if err != nil {
		t.Fatalf("creating %q: %v", path, err)
	}
	return file
}

func liveForkTestUnlink(t *testing.T, task *kernel.Task, path string) {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	if err := task.Kernel().VFS().UnlinkAt(ctx, task.Credentials(), &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(path),
	}); err != nil {
		t.Fatalf("unlinking %q: %v", path, err)
	}
}

func liveForkTestLink(t *testing.T, task *kernel.Task, oldPath, newPath string) {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	oldPop := &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(oldPath)}
	newPop := &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(newPath)}
	if err := task.Kernel().VFS().LinkAt(ctx, task.Credentials(), oldPop, newPop); err != nil {
		t.Fatalf("linking %q to %q: %v", newPath, oldPath, err)
	}
}

func liveForkTestPathMissing(t *testing.T, task *kernel.Task, path string) {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	file, err := task.Kernel().VFS().OpenAt(ctx, task.Credentials(), &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(path),
	}, &vfs.OpenOptions{Flags: linux.O_RDONLY})
	if file != nil {
		file.DecRef(ctx)
	}
	if !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Fatalf("opening missing path %q: got %v, want ENOENT", path, err)
	}
}

func liveForkTestPathRead(t *testing.T, task *kernel.Task, path, want string) {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	file := liveForkTestOpenPath(t, task, path, linux.O_RDONLY)
	defer file.DecRef(ctx)
	liveForkTestPRead(t, ctx, file, 0, want)
}

func liveForkTestOpenPath(t *testing.T, task *kernel.Task, path string, flags uint32) *vfs.FileDescription {
	t.Helper()
	ctx := task.Kernel().SupervisorContext()
	root := task.MountNamespace().Root(ctx)
	defer root.DecRef(ctx)
	file, err := task.Kernel().VFS().OpenAt(ctx, task.Credentials(), &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(path),
	}, &vfs.OpenOptions{Flags: flags})
	if err != nil {
		t.Fatalf("opening %q: %v", path, err)
	}
	return file
}

func liveForkTestPReadString(t *testing.T, ctx context.Context, file *vfs.FileDescription, offset int64, length int) string {
	t.Helper()
	buf := make([]byte, length)
	n, err := file.PRead(ctx, usermem.BytesIOSequence(buf), offset, vfs.ReadOptions{})
	if err != nil || n != int64(length) {
		t.Fatalf("PRead(%d, %d): got (%q, %d, %v)", offset, length, buf, n, err)
	}
	return string(buf)
}

func liveForkTestMode(t *testing.T, ctx context.Context, file *vfs.FileDescription, want linux.FileMode) {
	t.Helper()
	stat, err := file.Stat(ctx, vfs.StatOptions{Mask: linux.STATX_MODE})
	if err != nil {
		t.Fatalf("Stat mode: %v", err)
	}
	if got := linux.FileMode(stat.Mode) & 0777; got != want {
		t.Fatalf("mode: got %#o, want %#o", got, want)
	}
}

func liveForkTestSize(t *testing.T, ctx context.Context, file *vfs.FileDescription, want uint64) {
	t.Helper()
	stat, err := file.Stat(ctx, vfs.StatOptions{Mask: linux.STATX_SIZE})
	if err != nil {
		t.Fatalf("Stat size: %v", err)
	}
	if stat.Size != want {
		t.Fatalf("size: got %d, want %d", stat.Size, want)
	}
}

func liveForkTestMMWrite(t *testing.T, task *kernel.Task, addr hostarch.Addr, value string) {
	t.Helper()
	if n, err := task.MemoryManager().CopyOut(task, addr, []byte(value), usermem.IOOpts{}); err != nil || n != len(value) {
		t.Fatalf("MemoryManager.CopyOut(%#x, %q): got (%d, %v)", addr, value, n, err)
	}
}

func liveForkTestMMRead(t *testing.T, task *kernel.Task, addr hostarch.Addr, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if n, err := task.MemoryManager().CopyIn(task, addr, got, usermem.IOOpts{}); err != nil || n != len(got) {
		t.Fatalf("MemoryManager.CopyIn(%#x): got (%q, %d, %v)", addr, got, n, err)
	}
	if string(got) != want {
		t.Fatalf("memory at %#x: got %q, want %q", addr, got, want)
	}
}

func liveForkTestMappedWrite(t *testing.T, ctx context.Context, file *vfs.FileDescription, value string) {
	t.Helper()
	opts := memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.ReadWrite,
	}
	if err := file.ConfigureMMap(ctx, &opts); err != nil {
		t.Fatalf("ConfigureMMap: %v", err)
	}
	defer opts.MappingIdentity.DecRef(ctx)
	mr := memmap.MappableRange{Start: 0, End: hostarch.PageSize}
	translations, err := opts.Mappable.Translate(ctx, mr, mr, hostarch.Write)
	if err != nil || len(translations) != 1 {
		t.Fatalf("Translate: got (%d translations, %v), want (1, nil)", len(translations), err)
	}
	translation := translations[0]
	fr := memmap.FileRange{Start: translation.Offset, End: translation.Offset + uint64(len(value))}
	blocks, err := translation.File.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	n, err := safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice([]byte(value))))
	if err != nil || n != uint64(len(value)) {
		t.Fatalf("mapped write %q: got (%d, %v)", value, n, err)
	}
}

// TestStartSignal tests that the controller Start message will cause
// WaitForStartSignal to return.
func TestStartSignal(t *testing.T) {
	l, cleanup, err := createLoader(testConfig(), testSpec())
	if err != nil {
		t.Fatalf("error creating loader: %v", err)
	}
	defer l.Destroy()
	defer cleanup()

	// We aren't going to wait on this application, so the control server
	// needs to be shut down manually.
	defer l.ctrl.srv.Stop(time.Hour)

	// Start a goroutine that calls WaitForStartSignal and writes to a
	// channel when it returns.
	waitFinished := make(chan struct{})
	go func() {
		l.WaitForStartSignal()
		// Pretend that Run() executed and returned no error.
		l.ctrl.manager.startResultChan <- nil
		waitFinished <- struct{}{}
	}()

	// Nothing has been written to the channel, so waitFinished should not
	// return.  Give it a little bit of time to make sure the goroutine has
	// started.
	select {
	case <-waitFinished:
		t.Errorf("WaitForStartSignal completed but it should not have")
	case <-time.After(50 * time.Millisecond):
		// OK.
	}

	// Trigger the control server StartRoot method.
	cid := "foo"
	if err := l.ctrl.manager.StartRoot(&cid, nil); err != nil {
		t.Errorf("error calling StartRoot: %v", err)
	}

	// Now WaitForStartSignal should return (within a short amount of
	// time).
	select {
	case <-waitFinished:
		// OK.
	case <-time.After(50 * time.Millisecond):
		t.Errorf("WaitForStartSignal did not complete but it should have")
	}
}

// Test that network=host with raw sockets enabled requires CAP_NET_RAW on the
// host.
func TestHostnetWithRawSockets(t *testing.T) {
	// Drop CAP_NET_RAW from effective capabilities, if we have it.
	pid := os.Getpid()
	caps, err := capability.NewPid2(0)
	if err != nil {
		t.Fatalf("error getting capabilities for pid %d: %v", pid, err)
	}
	if err := caps.Load(); err != nil {
		t.Fatalf("error loading capabilities: %v", err)
	}
	if caps.Get(capability.EFFECTIVE, capability.CAP_NET_RAW) {
		caps.Unset(capability.EFFECTIVE, capability.CAP_NET_RAW)
		if err := caps.Apply(capability.EFFECTIVE); err != nil {
			t.Fatalf("error applying capabilities")
		}
		// Be nice and add it back when we are done.
		defer func() {
			caps.Set(capability.EFFECTIVE, capability.CAP_NET_RAW)
			if err := caps.Apply(capability.EFFECTIVE); err != nil {
				t.Fatalf("error restoring capabilities")
			}
		}()
	}

	// Configure host network with raw sockets.
	conf := testConfig()
	conf.Network = config.NetworkHost
	conf.EnableRaw = true

	// Creating loader should fail.
	l, err := New(Args{
		ID:              "should-fail",
		Spec:            testSpec(),
		Conf:            conf,
		DevGoferFD:      -1,
		PodInitConfigFD: -1,
		ExecFD:          -1,
	})
	if err == nil {
		l.Destroy()
		t.Fatalf("expected loader.New() to fail but it did not")
	}
	// Error message must be about CAP_NET_RAW.
	if !strings.Contains(err.Error(), "CAP_NET_RAW") {
		t.Errorf("expected error to contain CAP_NET_RAW but got %q", err)
	}
}

type CreateMountTestcase struct {
	name string
	// Spec that will be used to create the mount manager.  Note
	// that we can't mount procfs without a kernel, so each spec
	// MUST contain something other than procfs mounted at /proc.
	spec specs.Spec
	// Paths that are expected to exist in the resulting fs.
	expectedPaths []string
}

func createMountTestcases() []*CreateMountTestcase {
	testCases := []*CreateMountTestcase{
		{
			// Only proc.
			name: "only proc mount",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
				},
			},
			// /proc, /dev, and /sys should always be mounted.
			expectedPaths: []string{"/proc", "/dev", "/sys"},
		},
		{
			// Mount at a deep path, with many components that do
			// not exist in the root.
			name: "deep mount path",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/some/very/very/deep/path",
						Type:        "tmpfs",
					},
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
				},
			},
			// /some/deep/path should be mounted, along with /proc, /dev, and /sys.
			expectedPaths: []string{"/some/very/very/deep/path", "/proc", "/dev", "/sys"},
		},
		{
			// Mounts are nested inside each other.
			name: "nested mounts",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
					{
						Destination: "/foo",
						Type:        "tmpfs",
					},
					{
						Destination: "/foo/qux",
						Type:        "tmpfs",
					},
					{
						// File mounts with the same prefix.
						Destination: "/foo/qux-quz",
						Type:        "tmpfs",
					},
					{
						Destination: "/foo/bar",
						Type:        "tmpfs",
					},
					{
						Destination: "/foo/bar/baz",
						Type:        "tmpfs",
					},
					{
						// A deep path that is in foo but not the other mounts.
						Destination: "/foo/some/very/very/deep/path",
						Type:        "tmpfs",
					},
				},
			},
			expectedPaths: []string{"/foo", "/foo/bar", "/foo/bar/baz", "/foo/qux",
				"/foo/qux-quz", "/foo/some/very/very/deep/path", "/proc", "/dev", "/sys"},
		},
		{
			name: "mount inside /dev",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
					{
						Destination: "/dev",
						Type:        "tmpfs",
					},
					{
						// Mount with the same prefix.
						Destination: "/dev/fd-foo",
						Type:        "tmpfs",
					},
					{
						// Unsupported fs type.
						Destination: "/dev/mqueue",
						Type:        "mqueue",
					},
					{
						Destination: "/dev/foo",
						Type:        "tmpfs",
					},
					{
						Destination: "/dev/bar",
						Type:        "tmpfs",
					},
				},
			},
			expectedPaths: []string{"/proc", "/dev", "/dev/fd-foo", "/dev/foo", "/dev/bar", "/sys"},
		},
		{
			name: "mounts inside mandatory mounts",
			spec: specs.Spec{
				Root: &specs.Root{
					Path:     os.TempDir(),
					Readonly: true,
				},
				Mounts: []specs.Mount{
					{
						Destination: "/proc",
						Type:        "tmpfs",
					},
					{
						Destination: "/sys/bar",
						Type:        "tmpfs",
					},
					{
						Destination: "/tmp/baz",
						Type:        "tmpfs",
					},
					{
						Destination: "/dev/goo",
						Type:        "tmpfs",
					},
				},
			},
			expectedPaths: []string{"/proc", "/sys", "/sys/bar", "/tmp", "/tmp/baz", "/dev/goo"},
		},
	}

	return testCases
}

// Test that MountNamespace can be created with various specs.
func TestCreateMountNamespace(t *testing.T) {
	for _, tc := range createMountTestcases() {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			spec.Mounts = tc.spec.Mounts
			spec.Root = tc.spec.Root

			t.Logf("Using root: %q", spec.Root.Path)
			l, loaderCleanup, err := createLoader(testConfig(), spec)
			if err != nil {
				t.Fatalf("failed to create loader: %v", err)
			}
			defer l.Destroy()
			defer loaderCleanup()

			l.mu.Lock()
			defer l.mu.Unlock()
			mntr := l.newContainerMounter(&l.root)
			ctx := l.k.SupervisorContext()
			creds := auth.NewRootCredentials(l.root.procArgs.Credentials.UserNamespace)
			mns, err := mntr.mountAll(ctx, creds, l.root.spec, l.root.conf, &l.root.procArgs)
			if err != nil {
				t.Fatalf("mountAll: %v", err)
			}

			root := mns.Root(ctx)
			defer root.DecRef(ctx)
			for _, p := range tc.expectedPaths {
				target := &vfs.PathOperation{
					Root:  root,
					Start: root,
					Path:  fspath.Parse(p),
				}

				if d, err := l.k.VFS().GetDentryAt(ctx, l.root.procArgs.Credentials, target, &vfs.GetDentryOptions{}); err != nil {
					t.Errorf("expected path %v to exist with spec %v, but got error %v", p, tc.spec, err)
				} else {
					d.DecRef(ctx)
				}
			}
		})
	}
}

type createMountPointTestCase struct {
	name string
	path string
	mode linux.FileMode
	fail bool
}

func TestCreateMountPoint(t *testing.T) {
	createMountPointTestCases := []createMountPointTestCase{
		{
			name: "File",
			path: "/test/test-file",
			mode: linux.ModeRegular,
		},
		{
			name: "Directory",
			path: "/test/test-dir",
			mode: linux.ModeDirectory,
		},
		{
			name: "FileWithIntDirs",
			path: "/test/a/b/c/test-file",
			mode: linux.ModeRegular,
		},
		{
			name: "DirectoryWithIntDirs",
			path: "/test/d/e/f/g/test-dir",
			mode: linux.ModeDirectory,
		},
		{
			name: "ExistingFile",
			path: "/test/test-file",
			mode: linux.ModeRegular,
		},
		{
			name: "ExistingDirectory",
			path: "/test/test-dir",
			mode: linux.ModeDirectory,
		},
		{
			name: "DirVSFile",
			path: "/test/test-file",
			mode: linux.ModeDirectory,
			fail: true,
		},
		{
			name: "FileVSDir",
			path: "/test/test-dir",
			mode: linux.ModeRegular,
			fail: true,
		},
	}

	spec := testSpec()
	spec.Root = &specs.Root{
		Path: os.TempDir(),
	}
	spec.Mounts = append(spec.Mounts, specs.Mount{
		Destination: "/test",
		Type:        "tmpfs",
	})

	t.Logf("Using root: %q", spec.Root.Path)
	l, loaderCleanup, err := createLoader(testConfig(), spec)
	if err != nil {
		t.Fatalf("failed to create loader: %v", err)
	}
	defer l.Destroy()
	defer loaderCleanup()

	l.mu.Lock()
	defer l.mu.Unlock()
	mntr := l.newContainerMounter(&l.root)
	ctx := l.k.SupervisorContext()
	creds := auth.NewRootCredentials(l.root.procArgs.Credentials.UserNamespace)
	mns, err := mntr.mountAll(ctx, creds, l.root.spec, l.root.conf, &l.root.procArgs)
	if err != nil {
		t.Fatalf("mountAll: %v", err)
	}

	root := mns.Root(ctx)
	defer root.DecRef(ctx)

	for _, tc := range createMountPointTestCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := mntr.makeMountPoint(ctx, creds, mns, tc.path, tc.mode); err != nil {
				if tc.fail {
					return
				}
				t.Fatalf("makeMountPoint failed: %v", err)
			} else {
				if tc.fail {
					t.Fatalf("makeMountPoint doesn't fail")
				}
			}
			target := &vfs.PathOperation{
				Root:  root,
				Start: root,
				Path:  fspath.Parse(tc.path),
			}

			if mode, err := mntr.getPathMode(ctx, creds, target); err != nil {
				t.Errorf("expected path %v, but got error %v", tc.path, err)
			} else {
				if mode.IsDir() != tc.mode.IsDir() {
					t.Errorf("path mode %v doesn't match the mount mode %v", mode, tc.mode)
				}
			}
		})
	}
}

func TestMain(m *testing.M) {
	cpuid.Initialize()
	seccheck.Initialize()
	os.Exit(m.Run())
}

func TestNetworkConfig(t *testing.T) {
	l, cleanup, err := createLoader(testConfig(), testSpec())
	if err != nil {
		t.Fatalf("error creating loader: %v", err)
	}
	defer l.Destroy()
	defer cleanup()

	// We aren't going to wait on this application, so the control server
	// needs to be shut down manually.
	defer l.ctrl.srv.Stop(time.Hour)

	go func() {
		l.WaitForStartSignal()
		l.ctrl.manager.startResultChan <- nil
	}()

	args := &CreateLinksAndRoutesArgs{
		LoopbackLinks: []LoopbackLink{
			{
				Name: "lo",
				Addresses: []IPWithPrefix{
					{Address: net.IP("\x7f\x00\x00\x01"), PrefixLen: 8},
				},
				Routes: []Route{
					{
						Destination: net.IPNet{
							IP:   net.IP{127, 0, 0, 0},
							Mask: net.IPMask{255, 0, 0, 0},
						},
					},
				},
			},
		},
	}
	if err := l.ctrl.manager.SetNetworkArgs(args, nil); err != nil {
		t.Errorf("error calling SetNetworkConfig: %v", err)
	}
	var networkArgs CreateLinksAndRoutesArgs
	if err := l.ctrl.manager.GetNetworkConfig(nil, &networkArgs); err != nil {
		t.Errorf("error calling NetworkConfig: %v", err)
	}
	if diff := cmp.Diff(networkArgs.LoopbackLinks, args.LoopbackLinks); diff != "" {
		t.Errorf("Network config content mismatch (-want +got):\n%s", diff)
	}
}
