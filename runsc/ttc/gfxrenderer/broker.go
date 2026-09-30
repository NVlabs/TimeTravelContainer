// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
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
}

// args returns the arguments of a renderer.
func (o *BrokerOptions) args() []string {
	args := []string{"serve"}
	if o.Options.Library != "" {
		args = append(args, "--library", o.Options.Library)
	}
	if o.Options.Features != "" {
		args = append(args, "--features", o.Options.Features)
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
		sentryFD, err := startRenderer(&opts)
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

// startRenderer starts a renderer, and returns the Sentry's end of a
// connection to it.
func startRenderer(opts *BrokerOptions) (int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	rendererEnd := os.NewFile(uintptr(fds[1]), "gfx-renderer-connection")
	defer rendererEnd.Close()
	cmd := exec.Command(opts.Renderer, opts.args()...)
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
