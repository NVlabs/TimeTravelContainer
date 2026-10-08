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
	// Renderer is the path of the renderer's executable: ttc-gfx-renderer,
	// or ttcd, which is the renderer when it runs as Name.
	Renderer string

	// Stderr receives the renderers' logs.
	Stderr io.Writer

	// Launcher, if not nil, starts the renderers, for a daemon whose sandbox
	// bars its children from the GPU.
	Launcher *Launcher

	// Unconfined runs the renderers without their sandbox, as a daemon
	// without host isolation runs its sandboxes.
	Unconfined bool
}

// command returns the command that runs a renderer in mode, "serve-family",
// "probe" or "launch", as Name.
func (o *BrokerOptions) command(ctx context.Context, mode string) *exec.Cmd {
	args := []string{mode}
	if o.Unconfined {
		args = append(args, "--unconfined")
	}
	cmd := exec.CommandContext(ctx, o.Renderer, args...)
	cmd.Args[0] = Name
	return cmd
}

// ServeBroker connects the branches of the Sentry connected to the Unix
// stream socket fd, which it takes, to the renderer of their family, until
// the Sentry disconnects, starting one when there is none. The renderer
// exits once it serves no branch.
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
		sentryFD, family, err = connectFamilyRenderer(&opts, family)
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
			err = recvServing(family)
		}
		if err == nil {
			return fds[0], family, nil
		}
		// The renderer exited, as it does once it serves no branch: start
		// another.
		unix.Close(fds[0])
		family.Close()
		family = nil
		if !errors.Is(err, gfxproto.ErrClosed) {
			log.Printf("the family's GPU renderer is gone (%v); starting another", err)
		}
	}
	return -1, nil, errors.New("the family's GPU renderer exits")
}

// recvServing receives the reply of the renderer of a family that serves the
// connection that the broker gave it.
func recvServing(family *gfxproto.Conn) error {
	m, err := family.Recv()
	if err != nil {
		return err
	}
	m.CloseFDs()
	if m.Type != gfxproto.MsgReply {
		return fmt.Errorf("unexpected %v", m.Type)
	}
	var reply gfxproto.Reply
	if _, err := m.Decode(&reply); err != nil {
		return err
	}
	if reply.Errno != 0 {
		return unix.Errno(reply.Errno)
	}
	return nil
}

// startFamilyRenderer starts a renderer for a family, and returns its
// control connection, on which it takes connections to serve.
func startFamilyRenderer(opts *BrokerOptions) (*gfxproto.Conn, error) {
	var fd int
	if opts.Launcher != nil {
		var err error
		if fd, err = opts.Launcher.StartRenderer(); err != nil {
			return nil, err
		}
	} else {
		fds, err := socketpair()
		if err != nil {
			return nil, err
		}
		rendererEnd := os.NewFile(uintptr(fds[1]), "gfx-renderer-control")
		defer rendererEnd.Close()
		cmd := opts.command(context.Background(), "serve-family")
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

// ProbeRenderer returns the renderer's capability sets, as its probe prints
// them, through opts.Launcher if it is not nil. If the probe fails, what it
// logged goes to opts.Stderr.
func ProbeRenderer(opts BrokerOptions) ([]byte, error) {
	if opts.Launcher != nil {
		return opts.Launcher.Probe()
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := opts.command(ctx, "probe")
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
