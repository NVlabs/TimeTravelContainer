// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer"
)

const (
	// gfxRendererName is the file name of the GPU renderer, which ttcd looks
	// for beside itself unless TTC_GFX_RENDERER names it.
	gfxRendererName = "ttc-gfx-renderer"

	// gfxProbeTimeout is how long the renderer may take to report its
	// capability sets.
	gfxProbeTimeout = time.Minute
)

// gfxService starts GPU renderers for the daemon's sandboxes. Each Sentry of
// a --gfx family connects, through runsc, to the service's broker socket at
// boot, and asks it for a renderer for each branch whose GPU is in use.
type gfxService struct {
	// socket is the path of the broker socket.
	socket string
	// isolated is whether the daemon confines itself, and so its children,
	// with the macOS sandbox, whose profile does not admit the GPU.
	isolated bool
	stderr   io.Writer

	// mu protects the fields below, which start sets once.
	mu       sync.Mutex
	started  bool
	err      error
	capsets  string
	listener *net.UnixListener
}

// newGFXService returns a service whose broker socket is in run, the daemon's
// run/.
func newGFXService(run string, isolated bool, stderr io.Writer) *gfxService {
	return &gfxService{
		socket:   filepath.Join(run, daemonRunSocketName("g", "gfx")),
		isolated: isolated,
		stderr:   stderr,
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
	fork := "ineligible"
	if opts.GFXFork != "" {
		fork = opts.GFXFork
	}
	return []string{
		"--gfx=true",
		"--gfx-fork=" + fork,
		"--gfx-capsets=" + g.capsets,
		"--gfx-broker=" + g.socket,
	}, nil
}

// startLocked probes the renderer, and serves the broker socket.
//
// Preconditions: g.mu is locked.
func (g *gfxService) startLocked() error {
	if g.isolated && runtime.GOOS == "darwin" {
		return errors.New("the daemon's macOS sandbox denies renderers the GPU; restart the daemon with --unsafe-no-host-isolation to use --gfx")
	}
	opts, err := rendererOptions()
	if err != nil {
		return err
	}
	capsets, err := probeRenderer(opts)
	if err != nil {
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
	g.capsets = capsets
	g.listener = listener
	opts.Stderr = g.stderr
	go g.serve(listener, opts)
	return nil
}

// probeRenderer returns the renderer's capability sets, as runsc's
// --gfx-capsets spells them.
func probeRenderer(opts gfxrenderer.BrokerOptions) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gfxProbeTimeout)
	defer cancel()
	args := []string{"probe"}
	if opts.Options.Library != "" {
		args = append(args, "--library", opts.Options.Library)
	}
	cmd := exec.CommandContext(ctx, opts.Renderer, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("probing the GPU renderer: %w: %s", err, lastLine(stderr.String()))
	}
	var capsets []gfxrenderer.Capset
	if err := json.Unmarshal(stdout.Bytes(), &capsets); err != nil {
		return "", fmt.Errorf("the GPU renderer's capability sets: %w", err)
	}
	if len(capsets) == 0 {
		return "", errors.New("the GPU renderer has no capability sets")
	}
	return base64.StdEncoding.EncodeToString(bytes.TrimSpace(stdout.Bytes())), nil
}

// lastLine returns the last line of s.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
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
// renderers, run until the Sentries disconnect.
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
