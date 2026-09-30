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
	host   gfxHost
	stderr io.Writer

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
// run/, and which starts renderers as host says.
func newGFXService(run string, host gfxHost, stderr io.Writer) *gfxService {
	return &gfxService{
		socket: filepath.Join(run, daemonRunSocketName("g", "gfx")),
		host:   host,
		stderr: stderr,
	}
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
