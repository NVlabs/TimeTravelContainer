// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
)

// BrokerOptions configures a renderers' broker.
type BrokerOptions struct {
	// Renderer is the path of ttc-gfx-renderer.
	Renderer string

	// Options configures the renderers.
	Options Options

	// Stderr receives the renderers' logs.
	Stderr io.Writer

	// Launcher, if not nil, starts the renderers, for a daemon whose sandbox
	// bars its children from the GPU.
	Launcher *Launcher

	// Unconfined runs the renderers without their sandbox, as a daemon
	// without host isolation runs its sandboxes.
	Unconfined bool
}

// args returns the arguments of a renderer in mode, "serve" or "probe".
func (o *BrokerOptions) args(mode string) []string {
	args := append([]string{mode}, o.Options.flags()...)
	if o.Unconfined {
		args = append(args, "--unconfined")
	}
	return args
}

// ServeBroker starts renderers for the Sentry connected to the Unix stream
// socket fd, which it takes, until the Sentry disconnects. Each renderer
// exits when its Sentry disconnects from it.
//
// With the Venus backend, one renderer serves the Sentry's family, a
// connection for each branch, so that a fork's child shares what its parent
// compiled. Otherwise, each branch has a renderer of its own.
func ServeBroker(fd int, opts BrokerOptions) error {
	conn, err := gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		return err
	}
	defer conn.Close()
	var family *gfxproto.Conn
	defer func() {
		if family != nil {
			// The renderer exits once its connections close too.
			family.Close()
		}
	}()
	for {
		m, err := conn.Recv()
		if errors.Is(err, gfxproto.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		m.CloseFDs()
		if m.Type != gfxproto.MsgStartRenderer {
			return fmt.Errorf("unexpected %v", m.Type)
		}
		var p gfxproto.StartRenderer
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		var sentryFD int
		if opts.Options.Backend == BackendVenus {
			sentryFD, family, err = connectFamilyRenderer(&opts, family)
		} else {
			sentryFD, err = startRenderer(&opts, p.Flags)
		}
		if err != nil {
			log.Printf("starting a GPU renderer: %v", err)
			if err := conn.Send(gfxproto.MsgReply, m.Seq, &gfxproto.Reply{Errno: uint32(unix.ENODEV)}, nil); err != nil {
				return err
			}
			continue
		}
		err = conn.Send(gfxproto.MsgReply, m.Seq, &gfxproto.Reply{}, nil, sentryFD)
		unix.Close(sentryFD)
		if err != nil {
			return err
		}
	}
}

// startRenderer starts a renderer with flags, gfxproto.RendererFlag*, and
// returns the Sentry's end of a connection to it.
func startRenderer(opts *BrokerOptions, flags uint32) (int, error) {
	if opts.Launcher != nil {
		return opts.Launcher.StartRenderer(flags)
	}
	fds, err := socketpair()
	if err != nil {
		return -1, err
	}
	rendererEnd := os.NewFile(uintptr(fds[1]), "gfx-renderer-connection")
	defer rendererEnd.Close()
	withFlags := *opts
	withFlags.Options.Snapshots = flags&gfxproto.RendererFlagSnapshots != 0
	cmd := exec.Command(opts.Renderer, withFlags.args("serve")...)
	// The connection is the renderer's fd 3.
	cmd.ExtraFiles = []*os.File{rendererEnd}
	cmd.Stdout = opts.Stderr
	cmd.Stderr = opts.Stderr
	if err := cmd.Start(); err != nil {
		unix.Close(fds[0])
		return -1, err
	}
	go cmd.Wait()
	return fds[0], nil
}

// connectFamilyRenderer returns the Sentry's end of a new connection to the
// renderer of its family, whose control connection is family, starting one
// if family is nil or its renderer exited. It returns the control
// connection of the renderer that it connected to.
func connectFamilyRenderer(opts *BrokerOptions, family *gfxproto.Conn) (int, *gfxproto.Conn, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if family == nil {
			var err error
			if family, err = startFamilyRenderer(opts); err != nil {
				return -1, nil, err
			}
		}
		fds, err := socketpair()
		if err != nil {
			return -1, family, err
		}
		err = family.Send(gfxproto.MsgServeConnection, 0, &gfxproto.ServeConnection{}, nil, fds[1])
		unix.Close(fds[1])
		if err == nil {
			return fds[0], family, nil
		}
		// The renderer exited: start another.
		unix.Close(fds[0])
		family.Close()
		family = nil
		log.Printf("the family's GPU renderer is gone (%v); starting another", err)
	}
	return -1, nil, errors.New("the family's GPU renderer exits")
}

// startFamilyRenderer starts a renderer for a family, and returns its
// control connection, on which it takes connections to serve.
func startFamilyRenderer(opts *BrokerOptions) (*gfxproto.Conn, error) {
	var fd int
	if opts.Launcher != nil {
		var err error
		if fd, err = opts.Launcher.StartRenderer(gfxproto.RendererFlagFamily); err != nil {
			return nil, err
		}
	} else {
		fds, err := socketpair()
		if err != nil {
			return nil, err
		}
		rendererEnd := os.NewFile(uintptr(fds[1]), "gfx-renderer-control")
		defer rendererEnd.Close()
		cmd := exec.Command(opts.Renderer, opts.args("serve-family")...)
		// The control connection is the renderer's fd 3.
		cmd.ExtraFiles = []*os.File{rendererEnd}
		cmd.Stdout = opts.Stderr
		cmd.Stderr = opts.Stderr
		if err := cmd.Start(); err != nil {
			unix.Close(fds[0])
			return nil, err
		}
		go cmd.Wait()
		fd = fds[0]
	}
	conn, err := gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	return conn, nil
}

// ProbeRenderer returns the capability sets of a renderer with
// opts.Options, as its probe prints them, through opts.Launcher if it is not
// nil. If the probe fails, what it logged goes to opts.Stderr.
func ProbeRenderer(opts BrokerOptions) ([]byte, error) {
	if opts.Launcher != nil {
		var flags uint32
		if opts.Options.Snapshots {
			flags |= gfxproto.RendererFlagSnapshots
		}
		return opts.Launcher.Probe(flags)
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, opts.Renderer, opts.args("probe")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if opts.Stderr != nil {
			opts.Stderr.Write(stderr.Bytes())
		}
		return nil, fmt.Errorf("probing the GPU renderer: %w: %s", err, lastLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}
