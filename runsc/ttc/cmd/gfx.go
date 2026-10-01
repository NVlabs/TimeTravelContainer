// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer"
)

// gfxRendererName is the file name of the GPU renderer, which ttcd looks for
// beside itself unless TTC_GFX_RENDERER names it.
const gfxRendererName = "ttc-gfx-renderer"

// The guest's Vulkan driver, which ttcd gives the containers of --gfx
// families: Mesa's gfxstream driver, or its Venus driver with the Venus
// backend. ttcd looks for it in gfx-guest/ARCH beside itself, ARCH being the
// guest's, aarch64 or x86_64, unless TTC_GFX_GUEST_DRIVER names its
// directory.
type gfxGuestDriver struct {
	// name is the driver's file name.
	name string
	// manifest is where the Vulkan loader of containers finds the driver's
	// manifest, among those of the drivers that images install.
	manifest string
	// env are the environment variables of containers, unless they set
	// them.
	env []string
}

// gfxGuestDrivers are the guest's drivers, by the renderer's backend.
var gfxGuestDrivers = map[string]gfxGuestDriver{
	gfxrenderer.BackendGfxstream: {
		name:     "libvulkan_gfxstream.so",
		manifest: "/usr/share/vulkan/icd.d/ttc_gfxstream_icd.json",
		// The Vulkan loader skips the gfxstream drivers that images
		// install, of Mesa before TTC's patches, which would drive the
		// virtual GPU as a second device, the one that applications
		// prefer.
		env: []string{"VK_LOADER_DRIVERS_DISABLE=gfxstream_vk_icd*"},
	},
	gfxrenderer.BackendVenus: {
		name:     "libvulkan_virtio.so",
		manifest: "/usr/share/vulkan/icd.d/ttc_venus_icd.json",
		// The Vulkan loader skips the Venus drivers that images install,
		// which would drive the virtual GPU as a second device.
		env: []string{"VK_LOADER_DRIVERS_DISABLE=virtio_icd*"},
	},
}

const (
	// gfxGuestDriverDir is where containers find the driver.
	gfxGuestDriverDir = "/usr/lib/ttc-gfx"
	// gfxGuestDriconfPath is where Mesa's OpenGL in containers finds the
	// configuration that has it drive the virtual GPU by Zink, over the
	// Vulkan driver: the GPU has no OpenGL driver.
	gfxGuestDriconfPath = "/usr/share/drirc.d/50-ttc-gfx.conf"
)

// gfxBackend returns the renderer's backend, which TTC_GFX_BACKEND names.
func gfxBackend() string {
	if b := os.Getenv("TTC_GFX_BACKEND"); b != "" {
		return b
	}
	return gfxrenderer.BackendGfxstream
}

// gfxGuestDriconf has Mesa's OpenGL drive virtio-gpu devices by Zink. If
// Zink fails, as without a Vulkan driver for the device, Mesa renders in
// software, as it would without the configuration.
//
// Containers get it where Zink offers OpenGL 3 and more on the host's GPU,
// which takes transform feedback and geometry shaders. MoltenVK, macOS's
// Vulkan, has neither, and Zink on it offers only OpenGL 2.1 and OpenGL ES
// 2.0: applications that need more, Chromium's default ANGLE on OpenGL
// among them, would fail instead of rendering in software. There,
// applications opt in with MESA_LOADER_DRIVER_OVERRIDE=zink.
var gfxGuestZinkByDefault = runtime.GOOS != "darwin"

const gfxGuestDriconf = `<?xml version="1.0" standalone="yes"?>
<!-- TTC's virtual GPU has no OpenGL driver: drive it by Zink. -->
<driconf>
    <device driver="loader" kernel_driver="virtio_gpu">
        <application name="Default">
            <option name="dri_driver" value="zink" />
        </application>
    </device>
</driconf>
`

// gfxHost is how the daemon starts GPU renderers, which it settles before it
// confines itself.
type gfxHost struct {
	// launcher starts the renderers of a daemon that confines itself, and
	// so its children, with the macOS sandbox, whose profile bars the GPU.
	// It is nil where the daemon starts renderers itself.
	launcher *gfxrenderer.Launcher
	// err is why a daemon that needs a launcher has none.
	err error
	// unconfined is whether renderers run without their sandbox, as a
	// daemon without host isolation runs its sandboxes.
	unconfined bool
}

// startGFXHost settles how the daemon starts GPU renderers, before it
// confines itself. On macOS, a daemon with host isolation starts a launcher
// for them, if the renderer is installed.
func startGFXHost(unsafeNoHostIsolation bool, stderr io.Writer) gfxHost {
	if unsafeNoHostIsolation {
		return gfxHost{unconfined: true}
	}
	if runtime.GOOS != "darwin" {
		return gfxHost{}
	}
	opts, err := rendererOptions()
	if err != nil {
		return gfxHost{err: err}
	}
	opts.Stderr = stderr
	launcher, err := gfxrenderer.StartLauncher(opts)
	if err != nil {
		return gfxHost{err: fmt.Errorf("starting the GPU renderers' launcher: %w", err)}
	}
	return gfxHost{launcher: launcher}
}

// Close closes the launcher, if any.
func (h gfxHost) Close() {
	if h.launcher != nil {
		h.launcher.Close()
	}
}

// gfxService starts GPU renderers for the daemon's sandboxes. Each Sentry of
// a --gfx family connects, through runsc, to the service's broker socket at
// boot, and asks it for a renderer for each branch whose GPU is in use.
type gfxService struct {
	// socket is the path of the broker socket.
	socket string
	// dir is the service's directory, which holds files that it gives
	// containers.
	dir    string
	host   gfxHost
	stderr io.Writer

	// noGuestDriver reports, once, that the daemon has no guest driver.
	noGuestDriver sync.Once

	// mu protects the fields below, which start sets once.
	mu       sync.Mutex
	started  bool
	err      error
	opts     gfxrenderer.BrokerOptions
	listener *net.UnixListener
	// capsets are the renderer's capability sets, as runsc's --gfx-capsets
	// spells them, by whether the renderer takes snapshots: the features of
	// such renderers, which the capability sets offer guests, differ.
	capsets map[bool]string
}

// newGFXService returns a service whose broker socket is in run, the daemon's
// run/, whose files are in dir, and which starts renderers as host says.
func newGFXService(run, dir string, host gfxHost, stderr io.Writer) *gfxService {
	return &gfxService{
		socket: filepath.Join(run, daemonRunSocketName("g", "gfx")),
		dir:    dir,
		host:   host,
		stderr: stderr,
	}
}

// guestArch returns the architecture of the daemon's guests, as Linux spells
// it.
func guestArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

// guestDriver returns the path of the guest's Vulkan driver of the
// renderer's backend.
func guestDriver(d gfxGuestDriver) (string, error) {
	dir := os.Getenv("TTC_GFX_GUEST_DRIVER")
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(filepath.Dir(exe), "gfx-guest", guestArch())
	}
	driver := filepath.Join(dir, d.name)
	if _, err := os.Stat(driver); err != nil {
		return "", err
	}
	return driver, nil
}

// guestEdits returns what gives the containers of a family with the given
// GPU options the guest's side of the virtual GPU: the mounts of the guest's
// Vulkan driver of the renderer's backend, of the manifest that the Vulkan
// loader finds it by, and, with gfxGuestZinkByDefault, of Mesa's OpenGL
// configuration, and the driver's environment variables. Without a driver
// for the guest's architecture, it returns none: the containers must bring
// their own.
func (g *gfxService) guestEdits(opts ttcbundle.GPUOptions) ([]ttcbundle.HostMount, []string, error) {
	if !opts.GFX || g == nil {
		return nil, nil, nil
	}
	d, ok := gfxGuestDrivers[gfxBackend()]
	if !ok {
		return nil, nil, fmt.Errorf("no GPU renderer backend %q", gfxBackend())
	}
	driver, err := guestDriver(d)
	if err != nil {
		g.noGuestDriver.Do(func() {
			if g.stderr != nil {
				fmt.Fprintf(g.stderr, "ttcd: no guest Vulkan driver for --gfx (%v); containers must bring their own\n", err)
			}
		})
		return nil, nil, nil
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"file_format_version": "1.0.1",
		"ICD": map[string]string{
			"library_path": filepath.Join(gfxGuestDriverDir, d.name),
			"library_arch": "64",
			"api_version":  "1.1.363",
		},
	}, "", "    ")
	if err != nil {
		return nil, nil, err
	}
	manifestPath, err := g.writeGuestFile(filepath.Base(d.manifest), append(manifest, '\n'))
	if err != nil {
		return nil, nil, fmt.Errorf("the guest's Vulkan driver manifest: %w", err)
	}
	mounts := []ttcbundle.HostMount{
		{Source: driver, Destination: filepath.Join(gfxGuestDriverDir, d.name)},
		{Source: manifestPath, Destination: d.manifest},
	}
	if gfxGuestZinkByDefault {
		driconfPath, err := g.writeGuestFile(filepath.Base(gfxGuestDriconfPath), []byte(gfxGuestDriconf))
		if err != nil {
			return nil, nil, fmt.Errorf("the guest's OpenGL configuration: %w", err)
		}
		mounts = append(mounts, ttcbundle.HostMount{Source: driconfPath, Destination: gfxGuestDriconfPath})
	}
	return mounts, append([]string(nil), d.env...), nil
}

// writeGuestFile writes a file, named name, of the guest's side of the
// virtual GPU in the service's directory, unless it has it already, and
// returns its path.
func (g *gfxService) writeGuestFile(name string, content []byte) (string, error) {
	if err := os.MkdirAll(g.dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(g.dir, name)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
		return path, nil
	}
	tmp, err := os.CreateTemp(g.dir, name+".*")
	if err != nil {
		return "", err
	}
	_, err = tmp.Write(content)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	// Every user of a container reads it, as they read /etc/hosts.
	if err == nil {
		err = os.Chmod(tmp.Name(), 0444)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}

// rendererOptions returns where the renderer and gfxstream are.
func rendererOptions() (gfxrenderer.BrokerOptions, error) {
	renderer := os.Getenv("TTC_GFX_RENDERER")
	if renderer == "" {
		exe, err := os.Executable()
		if err != nil {
			return gfxrenderer.BrokerOptions{}, err
		}
		renderer = filepath.Join(filepath.Dir(exe), gfxRendererName)
	}
	if _, err := os.Stat(renderer); err != nil {
		return gfxrenderer.BrokerOptions{}, fmt.Errorf("the GPU renderer: %w; install %s beside ttcd, or name it with TTC_GFX_RENDERER", err, gfxRendererName)
	}
	return gfxrenderer.BrokerOptions{
		Renderer: renderer,
		Options: gfxrenderer.Options{
			Backend: gfxBackend(),
			Library: os.Getenv("TTC_GFXSTREAM_LIBRARY"),
		},
	}, nil
}

// runscArgs returns runsc's flags for a branch of a family with the given
// GPU options, starting the service the first time a family has a GPU.
func (g *gfxService) runscArgs(opts ttcbundle.GPUOptions) ([]string, error) {
	if !opts.GFX {
		return nil, nil
	}
	if g == nil {
		return nil, errors.New("the daemon has no GPU service")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.started {
		g.started = true
		g.err = g.startLocked()
		if g.err != nil && g.stderr != nil {
			fmt.Fprintf(g.stderr, "ttcd: the virtual GPU is unavailable: %v\n", g.err)
		}
	}
	if g.err != nil {
		return nil, fmt.Errorf("the virtual GPU is unavailable: %w", g.err)
	}
	capsets, err := g.capsetsLocked(opts.GFXFork == ttcbundle.GPUForkSnapshot)
	if err != nil {
		return nil, fmt.Errorf("the virtual GPU is unavailable: %w", err)
	}
	fork := "ineligible"
	if opts.GFXFork != "" {
		fork = opts.GFXFork
	}
	return []string{
		"--gfx=true",
		"--gfx-fork=" + fork,
		"--gfx-capsets=" + capsets,
		"--gfx-broker=" + g.socket,
	}, nil
}

// capsetsLocked returns the capability sets of a renderer that takes
// snapshots or not, probing it the first time.
//
// Preconditions: g.mu is locked. The service started.
func (g *gfxService) capsetsLocked(snapshots bool) (string, error) {
	if capsets, ok := g.capsets[snapshots]; ok {
		return capsets, nil
	}
	opts := g.opts
	opts.Options.Snapshots = snapshots
	capsets, err := probeRenderer(opts)
	if err != nil {
		return "", err
	}
	g.capsets[snapshots] = capsets
	return capsets, nil
}

// startLocked probes the renderer, and serves the broker socket.
//
// Preconditions: g.mu is locked.
func (g *gfxService) startLocked() error {
	if g.host.err != nil {
		return g.host.err
	}
	opts, err := rendererOptions()
	if err != nil {
		return err
	}
	opts.Launcher = g.host.launcher
	opts.Unconfined = g.host.unconfined
	opts.Stderr = g.stderr
	g.opts = opts
	g.capsets = make(map[bool]string)
	// A renderer that does not start fails the service, not only a family.
	if _, err := g.capsetsLocked(false); err != nil {
		return err
	}
	if err := os.Remove(g.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: g.socket, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listening for Sentries: %w", err)
	}
	if err := os.Chmod(g.socket, 0600); err != nil {
		listener.Close()
		return err
	}
	g.listener = listener
	go g.serve(listener, opts)
	return nil
}

// probeRenderer returns the renderer's capability sets, as runsc's
// --gfx-capsets spells them.
func probeRenderer(opts gfxrenderer.BrokerOptions) (string, error) {
	output, err := gfxrenderer.ProbeRenderer(opts)
	if err != nil {
		return "", err
	}
	var capsets []gfxrenderer.Capset
	if err := json.Unmarshal(output, &capsets); err != nil {
		return "", fmt.Errorf("the GPU renderer's capability sets: %w", err)
	}
	if len(capsets) == 0 {
		return "", errors.New("the GPU renderer has no capability sets")
	}
	return base64.StdEncoding.EncodeToString(bytes.TrimSpace(output)), nil
}

// serve starts a broker for each Sentry that connects.
func (g *gfxService) serve(listener *net.UnixListener, opts gfxrenderer.BrokerOptions) {
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && g.stderr != nil {
				fmt.Fprintf(g.stderr, "ttcd: accepting a Sentry's GPU broker connection: %v\n", err)
			}
			return
		}
		file, err := conn.File()
		conn.Close()
		if err != nil {
			if g.stderr != nil {
				fmt.Fprintf(g.stderr, "ttcd: a Sentry's GPU broker connection: %v\n", err)
			}
			continue
		}
		// ServeBroker takes a descriptor, which the File must not close.
		fd, err := unix.Dup(int(file.Fd()))
		file.Close()
		if err != nil {
			if g.stderr != nil {
				fmt.Fprintf(g.stderr, "ttcd: a Sentry's GPU broker connection: %v\n", err)
			}
			continue
		}
		unix.CloseOnExec(fd)
		go func() {
			if err := gfxrenderer.ServeBroker(fd, opts); err != nil && g.stderr != nil {
				fmt.Fprintf(g.stderr, "ttcd: GPU broker: %v\n", err)
			}
		}()
	}
}

// Close stops accepting Sentries. Brokers of connected Sentries, and their
// renderers, run until the Sentries disconnect; the launcher, if any, until
// the daemon exits, since brokers may still need it.
func (g *gfxService) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.listener == nil {
		return nil
	}
	err := g.listener.Close()
	g.listener = nil
	return err
}
