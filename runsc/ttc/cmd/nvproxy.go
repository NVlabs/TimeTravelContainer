// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/devices/nvproxy"
	"gvisor.dev/gvisor/pkg/sentry/devices/nvproxy/nvconf"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

// nvproxyDriverVersionEnv names, in ttcd's environment, the NVIDIA driver
// whose interface nvproxy uses for the host's, which it does not support,
// such as a pre-release driver of a branch that it supports.
const nvproxyDriverVersionEnv = "TTC_NVPROXY_DRIVER_VERSION"

// ttcd gives the containers of --nvproxy-fork=snapshot families its
// ttc-cuda-checkpoint, with which their forks checkpoint the processes that
// use the GPUs: the one that TTC_CUDA_CHECKPOINT names, the one that ttcd
// carries, or the one beside ttcd.
const (
	cudaCheckpointName = "ttc-cuda-checkpoint"
	cudaCheckpointEnv  = "TTC_CUDA_CHECKPOINT"
	// cudaCheckpointPath is where containers have it.
	cudaCheckpointPath = "/usr/lib/ttc-nvidia/cuda-checkpoint"
)

// cudaCheckpointTool returns the path of ttcd's ttc-cuda-checkpoint, which
// it writes in dir if it carries it.
func cudaCheckpointTool(dir string) (string, error) {
	tool := os.Getenv(cudaCheckpointEnv)
	if tool == "" {
		carried, err := carriedCUDACheckpoint(dir)
		if err != nil {
			return "", fmt.Errorf("writing the %s that ttcd carries: %w", cudaCheckpointName, err)
		}
		if carried != "" {
			return carried, nil
		}
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		tool = filepath.Join(filepath.Dir(exe), cudaCheckpointName)
	}
	if _, err := os.Stat(tool); err != nil {
		return "", fmt.Errorf("--nvproxy-fork=snapshot needs %s: %w; this ttcd carries none: install it beside ttcd, or name it with %s", cudaCheckpointName, err, cudaCheckpointEnv)
	}
	return tool, nil
}

// nvidiaDriverLibraries are the libraries of NVIDIA's user-space driver that
// containers get, by the driver capability that needs them, as
// libnvidia-container's src/nvc_info.c lists them. Each is named
// NAME.so.VERSION, VERSION being the kernel driver's, which they must match.
var nvidiaDriverLibraries = []struct {
	name string
	caps nvconf.DriverCaps
}{
	{"libnvidia-ml", nvconf.CapUtility},
	{"libnvidia-cfg", nvconf.CapUtility},
	{"libcuda", nvconf.CapCompute},
	{"libcudadebugger", nvconf.CapCompute},
	{"libnvidia-opencl", nvconf.CapCompute},
	{"libnvidia-gpucomp", nvconf.CapCompute | nvconf.CapGraphics},
	{"libnvidia-ptxjitcompiler", nvconf.CapCompute},
	{"libnvidia-allocator", nvconf.CapCompute},
	{"libnvidia-nvvm", nvconf.CapCompute},
	{"libvdpau_nvidia", nvconf.CapVideo},
	{"libnvidia-encode", nvconf.CapVideo},
	{"libnvidia-opticalflow", nvconf.CapVideo},
	{"libnvcuvid", nvconf.CapVideo},
	{"libnvidia-eglcore", nvconf.CapGraphics},
	{"libnvidia-glcore", nvconf.CapGraphics},
	{"libnvidia-tls", nvconf.CapGraphics},
	{"libnvidia-glsi", nvconf.CapGraphics},
	{"libnvidia-fbc", nvconf.CapGraphics},
	{"libnvidia-rtcore", nvconf.CapGraphics},
	{"libnvoptix", nvconf.CapGraphics},
	{"libGLX_nvidia", nvconf.CapGraphics},
	{"libEGL_nvidia", nvconf.CapGraphics},
	{"libGLESv2_nvidia", nvconf.CapGraphics},
	{"libGLESv1_CM_nvidia", nvconf.CapGraphics},
	{"libnvidia-glvkspirv", nvconf.CapGraphics},
}

// nvidiaDriverFiles are the other files of NVIDIA's driver that containers
// get where the host has them: its tools, and the manifests by which the
// OpenCL, Vulkan, and EGL loaders find its libraries.
var nvidiaDriverFiles = []struct {
	path string
	caps nvconf.DriverCaps
}{
	{"/usr/bin/nvidia-smi", nvconf.CapUtility},
	{"/usr/bin/nvidia-debugdump", nvconf.CapUtility},
	{"/usr/bin/nvidia-cuda-mps-control", nvconf.CapCompute},
	{"/usr/bin/nvidia-cuda-mps-server", nvconf.CapCompute},
	{"/etc/OpenCL/vendors/nvidia.icd", nvconf.CapCompute},
	{"/etc/vulkan/icd.d/nvidia_icd.json", nvconf.CapGraphics},
	{"/usr/share/vulkan/icd.d/nvidia_icd.json", nvconf.CapGraphics},
	{"/etc/vulkan/implicit_layer.d/nvidia_layers.json", nvconf.CapGraphics},
	{"/usr/share/vulkan/implicit_layer.d/nvidia_layers.json", nvconf.CapGraphics},
	{"/usr/share/glvnd/egl_vendor.d/10_nvidia.json", nvconf.CapGraphics},
}

// nvidiaLibraryDirs are where hosts keep NVIDIA's driver libraries.
var nvidiaLibraryDirs = []string{
	"/usr/lib/x86_64-linux-gnu",
	"/usr/lib/aarch64-linux-gnu",
	"/usr/lib64",
	"/usr/lib",
	"/lib64",
}

// nvidiaGPUDevice matches the device nodes of the host's GPUs.
var nvidiaGPUDevice = regexp.MustCompile(`^nvidia[0-9]+$`)

// nvproxyService gives the containers of --nvproxy families the host's
// NVIDIA GPUs, as NVIDIA's container toolkit gives Docker's, which TTC has
// no hook to run: the GPUs' device nodes, which runsc's nvproxy proxies, and
// NVIDIA's user-space driver, whose libraries must match the host's kernel
// driver. Containers get the driver's files where the host has them, which
// the dynamic loaders of images of the host's distribution family search.
// The service finds them for the first --nvproxy family.
type nvproxyService struct {
	// platform is the daemon's runtime platform.
	platform string
	// dir is the service's directory, which holds files that it gives
	// containers.
	dir string

	once sync.Once
	host *nvidiaHost
	err  error
}

// errNVProxyOnKVM explains why --nvproxy families need the systrap platform,
// for now. On KVM, the Sentry maps the UVM memory of every process of a
// family, in every branch, into its own address space, at the address at
// which the process maps it, as NVIDIA's UVM driver requires; and CUDA maps
// it at the same address in every process (0x207000000 on driver 590). So
// the second of two CUDA processes that run at once fails, and a snapshot
// fork's child cannot restore its CUDA processes beside their source's.
var errNVProxyOnKVM = errors.New("--nvproxy needs the systrap platform for now: on KVM, a family's CUDA processes map the GPU in one address space, at addresses that CUDA picks alike in every process, so that the second of two that run at once fails")

// nvidiaHost is what a host has of NVIDIA's driver.
type nvidiaHost struct {
	// runscArgs are runsc's flags for the driver: which driver's interface
	// nvproxy uses, if not the host's.
	runscArgs []string
	// devices are the device nodes of the driver and the GPUs.
	devices []specs.LinuxDevice
	// files are the driver's files that containers get.
	files []nvidiaFile
}

// nvidiaFile is a file of NVIDIA's driver that containers get.
type nvidiaFile struct {
	// source is the host's file.
	source string
	// paths are where containers get it: the host's path, and, for a
	// library, the path of its SONAME, which links to it on the host.
	paths []string
	// caps are the driver capabilities that need it.
	caps nvconf.DriverCaps
}

// find finds the host's NVIDIA driver, once.
func (s *nvproxyService) find() (*nvidiaHost, error) {
	if s == nil {
		return nil, errors.New("the daemon has no NVIDIA GPU service")
	}
	s.once.Do(func() {
		s.host, s.err = findNVIDIAHost()
		if s.err != nil {
			s.err = fmt.Errorf("the host's NVIDIA GPUs are unavailable: %w", s.err)
		}
	})
	return s.host, s.err
}

func findNVIDIAHost() (*nvidiaHost, error) {
	hostVersion, err := nvconf.HostDriverVersion()
	if err != nil {
		return nil, err
	}
	version, err := nvconf.DriverVersionFrom(hostVersion)
	if err != nil {
		return nil, err
	}
	nvproxy.Init()
	host := &nvidiaHost{}
	if override := os.Getenv(nvproxyDriverVersionEnv); override != "" {
		abi, err := nvconf.DriverVersionFrom(override)
		if err != nil {
			return nil, fmt.Errorf("%s=%s: %w", nvproxyDriverVersionEnv, override, err)
		}
		if _, ok := nvproxy.SupportedIoctls(abi); !ok {
			return nil, fmt.Errorf("%s=%s: nvproxy knows no such driver", nvproxyDriverVersionEnv, override)
		}
		host.runscArgs = []string{"--nvproxy-driver-version=" + abi.String(), "--nvproxy-allow-unsupported-driver=true"}
	} else if _, ok := nvproxy.ExpectedDriverChecksum(version); !ok {
		return nil, fmt.Errorf("nvproxy does not support the host's driver %s; it supports %v, and %s names one whose interface to use instead", hostVersion, nvproxy.SupportedDrivers(), nvproxyDriverVersionEnv)
	}
	if host.devices, err = nvidiaDevices(); err != nil {
		return nil, err
	}
	if host.files, err = nvidiaFiles(hostVersion); err != nil {
		return nil, err
	}
	return host, nil
}

// nvidiaDevices returns the device nodes of the host's NVIDIA driver and
// GPUs that nvproxy proxies.
func nvidiaDevices() ([]specs.LinuxDevice, error) {
	paths := []string{"/dev/nvidiactl", "/dev/nvidia-uvm"}
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil, err
	}
	var gpus []string
	for _, entry := range entries {
		if nvidiaGPUDevice.MatchString(entry.Name()) {
			gpus = append(gpus, entry.Name())
		}
	}
	if len(gpus) == 0 {
		return nil, errors.New("the host has no NVIDIA GPU devices, /dev/nvidiaN")
	}
	sort.Slice(gpus, func(i, j int) bool {
		a, _ := strconv.Atoi(gpus[i][len("nvidia"):])
		b, _ := strconv.Atoi(gpus[j][len("nvidia"):])
		return a < b
	})
	for _, gpu := range gpus {
		paths = append(paths, filepath.Join("/dev", gpu))
	}
	devices := make([]specs.LinuxDevice, 0, len(paths))
	for _, path := range paths {
		var stat unix.Stat_t
		if err := unix.Stat(path, &stat); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFCHR {
			return nil, fmt.Errorf("%s is not a character device", path)
		}
		mode := os.FileMode(0666)
		uid, gid := uint32(0), uint32(0)
		devices = append(devices, specs.LinuxDevice{
			Path:     path,
			Type:     "c",
			Major:    int64(unix.Major(uint64(stat.Rdev))),
			Minor:    int64(unix.Minor(uint64(stat.Rdev))),
			FileMode: &mode,
			UID:      &uid,
			GID:      &gid,
		})
	}
	return devices, nil
}

// nvidiaFiles returns the files of the host's NVIDIA driver, of the given
// version, that containers get.
func nvidiaFiles(version string) ([]nvidiaFile, error) {
	dir := ""
	for _, candidate := range nvidiaLibraryDirs {
		if _, err := os.Stat(filepath.Join(candidate, "libnvidia-ml.so."+version)); err == nil {
			dir = candidate
			break
		}
	}
	if dir == "" {
		return nil, fmt.Errorf("the host has no NVIDIA driver libraries of its driver %s, libnvidia-ml.so.%s, in %v", version, version, nvidiaLibraryDirs)
	}
	var files []nvidiaFile
	for _, library := range nvidiaDriverLibraries {
		source := filepath.Join(dir, library.name+".so."+version)
		if _, err := os.Stat(source); err != nil {
			continue
		}
		file := nvidiaFile{source: source, paths: []string{source}, caps: library.caps}
		soname, err := elfSONAME(source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
		if soname != "" && soname != filepath.Base(source) {
			file.paths = append(file.paths, filepath.Join(dir, soname))
		}
		if library.name == "libcuda" {
			// What programs that link with CUDA's driver by -lcuda load.
			file.paths = append(file.paths, filepath.Join(dir, "libcuda.so"))
		}
		files = append(files, file)
	}
	for _, other := range nvidiaDriverFiles {
		if info, err := os.Stat(other.path); err == nil && info.Mode().IsRegular() {
			files = append(files, nvidiaFile{source: other.path, paths: []string{other.path}, caps: other.caps})
		}
	}
	return files, nil
}

// elfSONAME returns the SONAME of a shared library, or "" if it has none.
func elfSONAME(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	names, err := f.DynString(elf.DT_SONAME)
	if err != nil || len(names) == 0 {
		return "", err
	}
	return names[0], nil
}

// nvproxyDriverCaps returns the driver capabilities of a family with the
// given GPU options.
func nvproxyDriverCaps(opts ttcbundle.GPUOptions) (nvconf.DriverCaps, error) {
	caps, all, err := nvconf.DriverCapsFromString(opts.NVProxyDriverCaps())
	if err != nil {
		return 0, err
	}
	if all {
		caps |= nvconf.AllContainerDriverCaps
	}
	return caps, nil
}

// edits returns what gives the containers of a family with the given GPU
// options the host's NVIDIA GPUs: the mounts of the driver's files that its
// driver capabilities need, and the device nodes.
func (s *nvproxyService) edits(opts ttcbundle.GPUOptions) ([]ttcbundle.HostMount, []specs.LinuxDevice, error) {
	if !opts.NVProxy {
		return nil, nil, nil
	}
	if s != nil && s.platform == "kvm" {
		return nil, nil, errNVProxyOnKVM
	}
	host, err := s.find()
	if err != nil {
		return nil, nil, err
	}
	caps, err := nvproxyDriverCaps(opts)
	if err != nil {
		return nil, nil, err
	}
	var mounts []ttcbundle.HostMount
	for _, file := range host.files {
		if file.caps&caps == 0 {
			continue
		}
		for _, path := range file.paths {
			mounts = append(mounts, ttcbundle.HostMount{Source: file.source, Destination: path})
		}
	}
	if opts.NVProxyFork == ttcbundle.GPUForkSnapshot {
		var dir string
		if s != nil {
			dir = s.dir
		}
		tool, err := cudaCheckpointTool(dir)
		if err != nil {
			return nil, nil, err
		}
		mounts = append(mounts, ttcbundle.HostMount{Source: tool, Destination: cudaCheckpointPath})
	}
	return mounts, append([]specs.LinuxDevice(nil), host.devices...), nil
}

// runscArgs returns runsc's flags for a branch of a family with the given GPU
// options.
func (s *nvproxyService) runscArgs(opts ttcbundle.GPUOptions) ([]string, error) {
	if !opts.NVProxy {
		return nil, nil
	}
	host, err := s.find()
	if err != nil {
		return nil, err
	}
	fork := "ineligible"
	if opts.NVProxyFork != "" {
		fork = opts.NVProxyFork
	}
	args := []string{
		"--nvproxy=true",
		"--nvproxy-allowed-driver-capabilities=" + opts.NVProxyDriverCaps(),
		"--nvproxy-fork=" + fork,
	}
	if opts.NVProxyFork == ttcbundle.GPUForkSnapshot {
		args = append(args, "--nvproxy-cuda-checkpoint="+cudaCheckpointPath)
	}
	return append(args, host.runscArgs...), nil
}
