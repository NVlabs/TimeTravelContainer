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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/ttcpack"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/cmd/sandboxsetup"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/fsgofer"
	"gvisor.dev/gvisor/runsc/fsgofer/extension"
	"gvisor.dev/gvisor/runsc/fsgofer/filter"
	"gvisor.dev/gvisor/runsc/profile"
	"gvisor.dev/gvisor/runsc/specutils"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

var caps = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_DAC_READ_SEARCH",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_SYS_CHROOT",
}

var udsOpenCaps = []string{
	"CAP_SETUID",
	"CAP_SETGID",
}

// goferCaps is the minimal set of capabilities needed by the Gofer to operate
// on files.
var goferCaps = &specs.LinuxCapabilities{
	Bounding:    caps,
	Effective:   caps,
	Permitted:   caps,
	Inheritable: caps,
	Ambient:     caps,
}

var goferUdsOpenCaps = &specs.LinuxCapabilities{
	Bounding:  udsOpenCaps,
	Effective: udsOpenCaps,
	Permitted: udsOpenCaps,
}

// goferSyncFDs contains file descriptors that are used for synchronization
// of the Gofer startup process against other processes.
type goferSyncFDs struct {
	// chrootFD is a file descriptor that is used to wait until container
	// filesystem related setup is done. This setup involves creating files and
	// mounts in the Gofer process's mount namespace and needs to be done before
	// the Gofer chroots.
	// If this is set, this FD is the first that the Gofer waits for.
	chrootFD int

	// usernsFD is a file descriptor that is used to wait until
	// user namespace ID mappings are established in the Gofer's userns.
	// If this is set, this FD is the second that the Gofer waits for.
	usernsFD int

	// procMountFD is a file descriptor that has to be closed when the
	// procfs mount isn't needed anymore. It is read by the procfs unmounter
	// process.
	// It is only set when re-execing is necessary.
	// If this is set, this FD is the last that the Gofer interacts with and
	// closes.
	procMountFD int
}

// Gofer implements subcommands.Command for the "gofer" command, which starts a
// filesystem gofer.  This command should not be called directly.
type Gofer struct {
	util.InternalSubCommand
	bundleDir  string
	ioFDs      sandboxsetup.IntFlags
	devIoFD    int
	applyCaps  bool
	setUpRoot  bool
	mountConfs specutils.GoferMountConfFlags

	// uid and gid are the user and group IDs to switch to after setting up the
	// user namespace.
	uid int
	gid int

	specFD            int
	rootfsOwnershipFD int
	lazyPackFD        int
	lazyRootFD        int
	lazyStateFD       int
	lazyPrefix        string
	mountsFD          int
	goferToHostRPCFD  int
	profileFDs        profile.FDArgs
	syncFDs           goferSyncFDs
	stopProfiling     func()
}

// Name implements subcommands.Command.
func (*Gofer) Name() string {
	return "gofer"
}

// Synopsis implements subcommands.Command.
func (g *Gofer) Synopsis() string {
	return "launch a gofer process that proxies access to container files"
}

// Usage implements subcommands.Command.
func (*Gofer) Usage() string {
	return "gofer [flags] <container ID>\n"
}

// SetFlags implements subcommands.Command.
func (g *Gofer) SetFlags(f *flag.FlagSet) {
	f.StringVar(&g.bundleDir, "bundle", "", "path to the root of the bundle directory, defaults to the current directory")
	f.BoolVar(&g.applyCaps, "apply-caps", true, "if true, apply capabilities to restrict what the Gofer process can do")
	f.BoolVar(&g.setUpRoot, "setup-root", true, "if true, set up an empty root for the process")

	// Open FDs that are donated to the gofer.
	f.Var(&g.ioFDs, "io-fds", "list of FDs to connect gofer servers. Follows the same order as --gofer-mount-confs. FDs are only donated if the mount is backed by lisafs.")
	f.Var(&g.mountConfs, "gofer-mount-confs", "information about how the gofer mounts have been configured. They must follow this order: root first, then mounts as defined in the spec.")
	f.IntVar(&g.devIoFD, "dev-io-fd", -1, "optional FD to connect /dev gofer server")
	f.IntVar(&g.specFD, "spec-fd", -1, "required fd with the container spec")
	f.IntVar(&g.rootfsOwnershipFD, "rootfs-ownership-fd", -1, "optional open TTC rootfs ownership manifest preserved across gofer re-exec")
	f.IntVar(&g.lazyPackFD, "ttc-lazy-pack-fd", -1, "optional TTC image pack preserved across gofer re-exec")
	f.IntVar(&g.lazyRootFD, "ttc-lazy-root-fd", -1, "optional TTC lazy image root preserved across gofer re-exec")
	f.IntVar(&g.lazyStateFD, "ttc-lazy-state-fd", -1, "optional TTC lazy image state directory preserved across gofer re-exec")
	f.StringVar(&g.lazyPrefix, "ttc-lazy-prefix", "", "optional TTC image pack path prefix preserved across gofer re-exec")
	f.IntVar(&g.mountsFD, "mounts-fd", -1, "mountsFD is the file descriptor to write list of mounts after they have been resolved (direct paths, no symlinks).")
	f.IntVar(&g.goferToHostRPCFD, "rpc-fd", -1, "gofer-to-host RPC file descriptor.")

	// IDs to run gofer as.
	f.IntVar(&g.uid, "uid", 0, "User ID")
	f.IntVar(&g.gid, "gid", 0, "Group ID")

	// Add synchronization FD flags.
	g.syncFDs.setFlags(f)

	// Profiling flags.
	g.profileFDs.SetFromFlags(f)

	extension.SetFlags(f)
}

// Execute implements subcommands.Command.
func (g *Gofer) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if g.bundleDir == "" || len(g.ioFDs) < 1 || g.specFD < 0 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	if f.NArg() != 1 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	containerID := f.Arg(0)

	conf := args[0].(*config.Config)

	// Set traceback level
	debug.SetTraceback(conf.Traceback)

	// Whether this process will re-execute itself to apply credential and
	// capability changes, rather than applying capabilities in-process.
	// Computed before the sync FDs are consumed below.
	willReexec := g.applyCaps && (config.CgoEnabled || g.syncFDs.usernsFD >= 0)

	specFile := os.NewFile(uintptr(g.specFD), "spec file")
	defer func() {
		if specFile != nil {
			_ = specFile.Close()
		}
	}()
	spec, err := specutils.ReadSpecFromFile(g.bundleDir, specFile, specutils.SpecOpts{Conf: conf})
	if err != nil {
		util.Fatalf("reading spec: %v", err)
	}
	mountHints, err := boot.NewPodMountHints(spec)
	if err != nil {
		util.Fatalf("parsing mount hints: %v", err)
	}
	rootfsHint, err := boot.NewRootfsHint(spec)
	if err != nil {
		util.Fatalf("parsing rootfs hint: %v", err)
	}
	virtualOwners, rootfsOwnershipFile, err := loadRootfsOwnership(spec, g.rootfsOwnershipFD)
	if err != nil {
		util.Fatalf("loading rootfs ownership: %v", err)
	}
	if rootfsOwnershipFile != nil {
		defer rootfsOwnershipFile.Close()
	}
	lazyFiles, err := loadRootfsLazyFiles(spec, g.lazyPackFD, g.lazyRootFD, g.lazyStateFD, g.lazyPrefix)
	if err != nil {
		util.Fatalf("loading lazy rootfs image: %v", err)
	}
	if lazyFiles != nil {
		defer lazyFiles.Close()
	}
	lisafsNeeded := lisafsNeededForDirectFSSuppression(spec, mountHints, rootfsHint, g.mountConfs)

	g.syncFDs.syncChroot()
	g.syncFDs.syncUsernsForRootless(uint32(g.uid), uint32(g.gid))

	goferToHostRPCSock, err := unet.NewSocket(g.goferToHostRPCFD)
	if err != nil {
		util.Fatalf("creating rpc socket: %v", err)
	}

	goferToHostRPC := urpc.NewClient(goferToHostRPCSock)
	defer goferToHostRPC.Close()

	if g.setUpRoot {
		if err := sandboxsetup.SetupRootFS(spec, conf, g.mountConfs, g.devIoFD, makeRPCMountOpener(goferToHostRPC), containerID, g.bundleDir); err != nil {
			util.Fatalf("Error setting up root FS: %v", err)
		}
		if willReexec && !conf.AllowRunAsCurrentUserWithoutChroot() {
			cleanupUnmounter := g.syncFDs.spawnProcUnmounter()
			defer cleanupUnmounter()
		}
	}
	extensionPrepare, err := extension.PrepareGofer(extension.GoferPrepareContext{
		Spec:        spec,
		ContainerID: containerID,
		BundleDir:   g.bundleDir,
	})
	if err != nil {
		util.Fatalf("preparing gofer extensions: %v", err)
	}
	capsToApply := goferCaps
	if conf.GetHostUDS().AllowOpen() {
		capsToApply = specutils.MergeCapabilities(capsToApply, goferUdsOpenCaps)
	}
	if g.applyCaps && willReexec {
		if config.CgoEnabled {
			log.Warningf("Need to re-exec in order to drop capabilities, due to cgo build. This slows down gVisor startup. Use a pure-Go gVisor build for faster startup.")
		} else {
			log.Infof("Re-execing. FYI, this step isn't necessary when running with root. gVisor will start up faster (and is just as secure) when started as root, as it has to do fewer hoops to get to its as-sandboxed-as-possible state.")
		}
		overrides := g.syncFDs.flags()
		overrides["apply-caps"] = "false"
		overrides["setup-root"] = "false"
		if rootfsOwnershipFile != nil {
			if _, _, errno := unix.RawSyscall(unix.SYS_FCNTL, rootfsOwnershipFile.Fd(), unix.F_SETFD, 0); errno != 0 {
				util.Fatalf("clearing CLOEXEC on rootfs ownership manifest: %v", errno)
			}
			overrides["rootfs-ownership-fd"] = strconv.Itoa(int(rootfsOwnershipFile.Fd()))
		}
		if lazyFiles != nil {
			packFD, rootFD, stateFD, prefix := lazyFiles.InheritedFDs()
			for _, inheritedFD := range []int{packFD, rootFD, stateFD} {
				if _, _, errno := unix.RawSyscall(unix.SYS_FCNTL, uintptr(inheritedFD), unix.F_SETFD, 0); errno != 0 {
					util.Fatalf("clearing CLOEXEC on lazy image fd %d: %v", inheritedFD, errno)
				}
			}
			overrides["ttc-lazy-pack-fd"] = strconv.Itoa(packFD)
			overrides["ttc-lazy-root-fd"] = strconv.Itoa(rootFD)
			overrides["ttc-lazy-state-fd"] = strconv.Itoa(stateFD)
			overrides["ttc-lazy-prefix"] = prefix
		}
		for key, value := range extensionPrepare.FlagOverrides {
			overrides[key] = value
		}
		args := sandboxsetup.PrepareArgs(g.Name(), f, overrides)
		util.Fatalf("setCapsAndCallSelf(%v, %v): %v", args, capsToApply, sandboxsetup.SetCapsAndCallSelf(args, capsToApply))
		panic("unreachable")
	}
	// No re-exec from here on out.

	// This can't happen until after setCapsAndCallSelf(), since otherwise the
	// re-executed gofer may reuse goferToHostRPCFD's file descriptor for an
	// unrelated file.
	goferToHostRPC.Close()

	// The spec descriptor is needed across the optional capability-changing
	// re-exec above, but the decoded spec is authoritative after that point.
	// Do not retain the descriptor for the lifetime of the gofer: TTC may
	// atomically update the on-disk bundle spec while promoting persistent
	// repositories for a remote fork, and NFS preserves an unlinked open file
	// as a .nfs placeholder.
	if err := specFile.Close(); err != nil {
		util.Fatalf("closing spec file: %v", err)
	}
	specFile = nil

	// Start profiling. This will be a noop if no profiling arguments were passed.
	// This *must* happen while procfs is still mounted (need `/proc/self/maps`).
	profileOpts := profile.MakeOpts(&g.profileFDs, conf.ProfileGCInterval)
	g.stopProfiling = profile.Start(profileOpts)

	// Find what path is going to be served by this gofer.
	root := spec.Root.Path
	if !conf.AllowRunAsCurrentUserWithoutChroot() {
		root = "/root"
	}

	// Resolve mount points paths, then replace mounts from our spec and send the
	// mount list over to the sandbox, so they are both in sync.
	//
	// Note that all mount points have been mounted in the proper location in
	// setupRootFS().
	cleanMounts, err := sandboxsetup.ResolveMounts(conf, spec.Mounts, root, g.mountConfs)
	if err != nil {
		util.Fatalf("Failure to resolve mounts: %v", err)
	}
	spec.Mounts = cleanMounts
	go func() {
		if err := sandboxsetup.WriteMounts(g.mountsFD, cleanMounts); err != nil {
			panic(fmt.Sprintf("Failed to write mounts: %v", err))
		}
	}()

	specutils.LogSpecDebug(spec, conf.OCISeccomp)
	// fsgofer should run with a umask of 0, because we want to preserve file
	// modes exactly as sent by the sandbox, which will have applied its own umask.
	unix.Umask(0)

	// Open `/proc/self/fd` (needs procfs).
	procFDPath := sandboxsetup.ProcFDBindMount
	if conf.AllowRunAsCurrentUserWithoutChroot() {
		procFDPath = "/proc/self/fd"
	}
	if err := fsgofer.OpenProcSelfFD(procFDPath); err != nil {
		util.Fatalf("failed to open /proc/self/fd: %v", err)
	}

	// Look up our own caps (needs procfs).
	var resolvedCaps *sandboxsetup.ResolvedThreadCaps
	if g.applyCaps {
		var err error
		resolvedCaps, err = sandboxsetup.ResolveThreadCaps(capsToApply, nil)
		if err != nil {
			util.Fatalf("resolving capabilities: %v", err)
		}
	}

	// Unmount procfs.
	if g.setUpRoot && !conf.AllowRunAsCurrentUserWithoutChroot() && g.syncFDs.procMountFD < 0 {
		sandboxsetup.UmountProcInProcess()
	} else {
		g.syncFDs.unmountProcfs()
	}

	if g.applyCaps {
		if err := resolvedCaps.Apply(nil); err != nil {
			util.Fatalf("applying caps to all threads: %v", err)
		}
	}

	// At this point we won't re-execute, so it's safe to limit via rlimits. Any
	// limit >= 0 works. If the limit is lower than the current number of open
	// files, then Setrlimit will succeed, and the next open will fail.
	if conf.FDLimit > -1 {
		rlimit := unix.Rlimit{
			Cur: uint64(conf.FDLimit),
			Max: uint64(conf.FDLimit),
		}
		switch err := unix.Setrlimit(unix.RLIMIT_NOFILE, &rlimit); err {
		case nil:
		case unix.EPERM:
			log.Warningf("FD limit %d is higher than the current hard limit or system-wide maximum", conf.FDLimit)
		default:
			util.Fatalf("Failed to set RLIMIT_NOFILE: %v", err)
		}
	}

	if conf.UnsafeNoHostIsolation {
		log.Warningf("Serving rootfs without chroot isolation from %q", root)
	} else {
		if err := unix.Chroot(root); err != nil {
			util.Fatalf("failed to chroot to %q: %v", root, err)
		}
		if err := unix.Chdir("/"); err != nil {
			util.Fatalf("changing working dir: %v", err)
		}
		log.Infof("Process chroot'd to %q", root)
	}
	ruid := unix.Getuid()
	euid := unix.Geteuid()
	rgid := unix.Getgid()
	egid := unix.Getegid()
	log.Debugf("Process running as uid=%d euid=%d gid=%d egid=%d", ruid, euid, rgid, egid)

	// Initialize filters.
	opts := filter.Options{
		UDSOpenEnabled:   conf.GetHostUDS().AllowOpen(),
		UDSCreateEnabled: conf.GetHostUDS().AllowCreate(),
		ProfileEnabled:   profileOpts.Enabled(),
		DirectFS:         conf.DirectFS,
		LisafsNeeded:     lisafsNeeded,
		CgoEnabled:       config.CgoEnabled,
	}
	for _, e := range extension.Registered() {
		opts.ExtraRules = append(opts.ExtraRules, e.SeccompRules())
	}
	if err := filter.Install(opts); err != nil {
		util.Fatalf("installing seccomp filters: %v", err)
	}

	return g.serve(spec, conf, root, ruid, euid, rgid, egid, virtualOwners, lazyFiles)
}

func loadRootfsLazyFiles(spec *specs.Spec, packFD, rootFD, stateFD int, prefix string) (*ttcpack.LazyDirectory, error) {
	name := spec.Annotations[ttcpack.LazyAnnotation]
	if name == "" {
		if packFD >= 0 || rootFD >= 0 || stateFD >= 0 || prefix != "" {
			return nil, errors.New("lazy image descriptors supplied without rootfs annotation")
		}
		return nil, nil
	}
	if packFD >= 0 || rootFD >= 0 || stateFD >= 0 {
		if packFD < 0 || rootFD < 0 || stateFD < 0 {
			return nil, errors.New("incomplete inherited lazy image descriptors")
		}
		return ttcpack.OpenLazyDirectoryFromFDs(packFD, rootFD, stateFD, prefix)
	}
	want := ttcpack.LazyDescriptorPath(spec.Root.Path)
	if !filepath.IsAbs(name) || filepath.Clean(name) != filepath.Clean(want) {
		return nil, fmt.Errorf("lazy image descriptor path %q does not match rootfs %q", name, spec.Root.Path)
	}
	descriptor, err := ttcpack.ReadLazyDescriptor(spec.Root.Path)
	if err != nil {
		return nil, err
	}
	return ttcpack.OpenLazyDirectory(spec.Root.Path, descriptor)
}

func loadRootfsOwnership(spec *specs.Spec, inheritedFD int) (_ *ownership.Manifest, manifestFile *os.File, retErr error) {
	name := ownership.AnnotationValue(spec.Annotations)
	if name == "" {
		if inheritedFD >= 0 {
			return nil, nil, fmt.Errorf("rootfs ownership fd %d supplied without %q annotation", inheritedFD, ownership.Annotation)
		}
		return nil, nil, nil
	}
	want := ownership.ManifestPath(spec.Root.Path)
	if !filepath.IsAbs(name) || filepath.Clean(name) != filepath.Clean(want) {
		return nil, nil, fmt.Errorf("ownership manifest path %q does not match rootfs %q", name, spec.Root.Path)
	}
	if inheritedFD >= 0 {
		manifestFile = os.NewFile(uintptr(inheritedFD), "rootfs ownership manifest")
		if manifestFile == nil {
			return nil, nil, fmt.Errorf("invalid rootfs ownership fd %d", inheritedFD)
		}
	} else {
		var err error
		manifestFile, err = os.Open(name)
		if err != nil {
			return nil, nil, err
		}
	}
	defer func() {
		if retErr != nil {
			manifestFile.Close()
		}
	}()
	if _, err := manifestFile.Seek(0, 0); err != nil {
		return nil, nil, fmt.Errorf("rewinding ownership manifest %q: %w", name, err)
	}
	manifest, err := ownership.ReadFrom(name, manifestFile)
	if err != nil {
		return nil, nil, err
	}
	log.Infof("Loaded virtual ownership for %d rootfs paths from %q", len(manifest.Entries), name)
	return manifest, manifestFile, nil
}

func goferMountPath(conf *config.Config, source, destination string) string {
	if conf.UnsafeNoHostIsolation {
		return source
	}
	return destination
}
func (g *Gofer) serve(spec *specs.Spec, conf *config.Config, root string, ruid int, euid int, rgid int, egid int, virtualOwners *ownership.Manifest, lazyFiles *ttcpack.LazyDirectory) subcommands.ExitStatus {
	type connectionConfig struct {
		sock          *unet.Socket
		mountPath     string
		readonly      bool
		mount         *specs.Mount
		virtualOwners *ownership.Manifest
		lazyFiles     fsgofer.LazyFileProvider
		// devGofer is true only for the dev gofer connection, which serves
		// host device files to the sentry's device proxies (e.g. nvproxy). A
		// container mount whose destination happens to be "/dev" is not a dev
		// gofer connection.
		devGofer bool
	}
	cfgs := make([]connectionConfig, 0, len(spec.Mounts)+1)

	ioFDs := g.ioFDs
	rootfsConf := g.mountConfs[0]
	if rootfsConf.ShouldUseLisafs() {
		mountPath := goferMountPath(conf, root, "/")
		// Start with root mount, then add any other additional mount as needed.
		cfgs = append(cfgs, connectionConfig{
			sock:          sandboxsetup.NewSocket(ioFDs[0]),
			mountPath:     mountPath,
			readonly:      spec.Root.Readonly || rootfsConf.ShouldUseOverlayfs(),
			virtualOwners: virtualOwners,
			lazyFiles:     lazyFiles,
		})
		log.Infof("Serving %q mapped to %q on FD %d (ro: %t)", mountPath, root, ioFDs[0], cfgs[0].readonly)
		ioFDs = ioFDs[1:]
	}

	mountIdx := 1 // first one is the root
	for i := range spec.Mounts {
		m := &spec.Mounts[i]
		if !specutils.HasMountConfig(*m) {
			continue
		}
		mountConf := g.mountConfs[mountIdx]
		mountIdx++
		if !mountConf.ShouldUseLisafs() {
			continue
		}
		if !filepath.IsAbs(m.Destination) {
			util.Fatalf("mount destination must be absolute: %q", m.Destination)
		}

		if len(ioFDs) == 0 {
			util.Fatalf("no FD found for mount. Did you forget --io-fd? FDs: %d, Mount: %+v", len(g.ioFDs), m)
		}
		ioFD := ioFDs[0]
		ioFDs = ioFDs[1:]
		readonly := specutils.IsReadonlyMount(m.Options) || mountConf.ShouldUseOverlayfs()
		mountPath := goferMountPath(conf, m.Source, m.Destination)
		cfgs = append(cfgs, connectionConfig{
			sock:      sandboxsetup.NewSocket(ioFD),
			mountPath: mountPath,
			readonly:  readonly,
			mount:     m,
		})
		log.Infof("Serving %q mapped on FD %d (ro: %t)", m.Destination, ioFD, readonly)
	}

	if len(ioFDs) > 0 {
		util.Fatalf("too many FDs passed for mounts. mounts: %d, FDs: %d", len(cfgs), len(g.ioFDs))
	}

	if g.devIoFD >= 0 {
		cfgs = append(cfgs, connectionConfig{
			sock:      sandboxsetup.NewSocket(g.devIoFD),
			mountPath: "/dev",
			devGofer:  true,
		})
		log.Infof("Serving /dev mapped on FD %d (ro: false)", g.devIoFD)
	}

	// These are global fsgofer configurations.
	fsgoferConf := &fsgofer.Config{
		HostUDS:              conf.GetHostUDS(),
		HostFifo:             conf.HostFifo,
		CharDevicePolicy:     conf.CharacterDevicePolicy,
		DonateMountPointFD:   conf.DirectFS,
		RUID:                 ruid,
		EUID:                 euid,
		RGID:                 rgid,
		EGID:                 egid,
		TrustedRootOwnership: conf.UnsafeNoHostIsolation,
	}
	// The dev gofer connection exists to open host device files on behalf of
	// the sentry's device proxies (e.g. nvproxy), which mediate all
	// application I/O on the resulting FDs. --character-device-policy governs
	// devices that the sandboxed application could open directly through a
	// gofer mount, not this connection, so character device opens are always
	// allowed here.
	devGoferConf := *fsgoferConf
	devGoferConf.CharDevicePolicy = config.CharDevPassthrough

	// Create the server and start connections.
	server := lisafs.NewServer()
	for _, cfg := range cfgs {
		var connImpl lisafs.ConnectionImpl
		var connOpts lisafs.ConnectionOpts
		// /dev is always served by the stock fsgofer.
		if cfg.mountPath != "/dev" {
			for _, e := range extension.Registered() {
				impl, opts, err := e.TryHandleMount(spec, cfg.mount, cfg.mountPath, cfg.readonly)
				if err != nil {
					util.Fatalf("extension %s for %q: %v", e.Name(), cfg.mountPath, err)
				}
				if impl != nil {
					connImpl = impl
					connOpts = opts
					log.Infof("Serving %q via extension %s on FD %d", cfg.mountPath, e.Name(), cfg.sock.FD())
					break
				}
			}
		}
		if connImpl == nil {
			connectionConf := *fsgoferConf
			if cfg.devGofer {
				connectionConf = devGoferConf
			}
			connectionConf.VirtualOwners = cfg.virtualOwners
			connectionConf.LazyFiles = cfg.lazyFiles
			connImpl = fsgofer.NewConnectionImpl(&connectionConf)
			connOpts = fsgofer.ConnectionOpts(cfg.readonly)
		}
		conn, err := server.CreateConnection(cfg.sock, cfg.mountPath, connOpts, connImpl)
		if err != nil {
			util.Fatalf("starting connection on FD %d for gofer mount failed: %v", cfg.sock.FD(), err)
		}
		server.StartConnection(conn)
	}
	if lazyFiles != nil {
		lazyFiles.StartPrefetch()
	}
	server.Wait()
	server.Destroy()
	log.Infof("All lisafs servers exited.")
	if g.stopProfiling != nil {
		g.stopProfiling()
	}
	return subcommands.ExitSuccess
}

// lisafsNeededForDirectFSSuppression returns true if this gofer serves a mount
// that suppresses directfs and therefore still needs LisaFS syscalls.
func lisafsNeededForDirectFSSuppression(spec *specs.Spec, mountHints *boot.PodMountHints, rootfsHint *boot.RootfsHint, mountConfs []specutils.GoferMountConf) bool {
	if len(mountConfs) > 0 && mountConfs[0].ShouldUseLisafs() &&
		rootfsHint != nil && rootfsHint.SuppressDirectFS {
		return true
	}
	if mountHints == nil {
		return false
	}
	mountIdx := 1 // First mount config is the root.
	for _, m := range spec.Mounts {
		if !specutils.HasMountConfig(m) {
			continue
		}
		mountConf := mountConfs[mountIdx]
		mountIdx++
		if !mountConf.ShouldUseLisafs() {
			continue
		}
		if hint := mountHints.FindMount(m.Source); hint != nil && hint.SuppressDirectFS {
			return true
		}
	}
	return false
}

// makeRPCMountOpener returns a MountOpener that opens mount sources via the
// gofer-to-host RPC channel.
func makeRPCMountOpener(goferToHostRPC *urpc.Client) sandboxsetup.MountOpener {
	return func(m *specs.Mount, flags uint32) (*os.File, error) {
		req := container.OpenMountArgs{
			Mount: m,
			Flags: flags,
		}
		var res container.OpenMountResult
		if err := goferToHostRPC.Call("goferToHostRPC.OpenMount", &req, &res); err != nil {
			return nil, fmt.Errorf("opening %s: %w", m.Source, err)
		}
		return res.Files[0], nil
	}
}

// setFlags sets sync FD flags on the given FlagSet.
func (g *goferSyncFDs) setFlags(f *flag.FlagSet) {
	f.IntVar(&g.chrootFD, "sync-chroot-fd", -1, "file descriptor that the gofer waits on until container filesystem setup is done")
	f.IntVar(&g.usernsFD, "sync-userns-fd", -1, "file descriptor the gofer waits on until userns mappings are set up")
	f.IntVar(&g.procMountFD, "proc-mount-sync-fd", -1, "file descriptor that the gofer writes to when /proc isn't needed anymore and can be unmounted")
}

// flags returns the flags necessary to pass along the current sync FD values
// to a re-executed version of this process.
func (g *goferSyncFDs) flags() map[string]string {
	return map[string]string{
		"sync-chroot-fd":     fmt.Sprintf("%d", g.chrootFD),
		"sync-userns-fd":     fmt.Sprintf("%d", g.usernsFD),
		"proc-mount-sync-fd": fmt.Sprintf("%d", g.procMountFD),
	}
}

// spawnProcUnmounter executes the /proc unmounter process, for gofers that
// will re-exec and would lose the capability to umount /proc on their own.
// It returns a function to clean up the unmounter process, which
// should be called (via defer) in case of errors.
func (g *goferSyncFDs) spawnProcUnmounter() func() {
	if g.procMountFD != -1 {
		util.Fatalf("procMountFD is set")
	}
	cmd, w := sandboxsetup.ExecProcUmounter()
	// Clear FD_CLOEXEC so procMountFD survives re-exec.
	if _, _, errno := unix.RawSyscall(unix.SYS_FCNTL, w.Fd(), unix.F_SETFD, 0); errno != 0 {
		util.Fatalf("error clearing CLOEXEC: %v", errno)
	}
	g.procMountFD = int(w.Fd())
	return func() {
		g.procMountFD = -1
		w.Close()
		cmd.Wait()
	}
}

// unmountProcfs signals the proc unmounter process that procfs is no longer
// needed. No-op when no unmounter process was spawned.
func (g *goferSyncFDs) unmountProcfs() {
	if g.procMountFD < 0 {
		return
	}
	sandboxsetup.UmountProc(g.procMountFD)
	g.procMountFD = -1
}

// syncUsernsForRootless waits on usernsFD to be closed and then sets
// UID/GID to uid/gid. Note that this function calls runtime.LockOSThread().
// This function is a no-op if usernsFD is -1.
//
// Postcondition: All callers must re-exec themselves after this returns,
// unless usernsFD was -1.
func (g *goferSyncFDs) syncUsernsForRootless(uid, gid uint32) {
	if g.usernsFD < 0 {
		return
	}
	sandboxsetup.SyncUsernsForRootless(g.usernsFD, uid, gid)
	g.usernsFD = -1
}

// syncChroot waits on chrootFD to be closed.
// Used for synchronization during container filesystem setup which is done
// from the non-gofer process.
// This function is a no-op if chrootFD is -1.
func (g *goferSyncFDs) syncChroot() {
	if g.chrootFD < 0 {
		return
	}
	if err := sandboxsetup.WaitForFD(g.chrootFD, "chroot sync FD"); err != nil {
		util.Fatalf("failed to sync on chroot FD: %v", err)
	}
	g.chrootFD = -1
}
