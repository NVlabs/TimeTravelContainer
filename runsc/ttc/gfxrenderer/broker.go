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
func ServeBroker(fd int, opts BrokerOptions) error {
	conn, err := gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		return err
	}
	defer conn.Close()
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
		sentryFD, err := startRenderer(&opts, p.Flags)
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
