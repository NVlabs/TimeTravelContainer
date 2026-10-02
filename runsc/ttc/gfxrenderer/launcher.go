// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
)

// The launcher starts renderers for a ttcd that confines itself with the
// macOS sandbox. ttcd's profile bars the GPU, and macOS lets no process
// change its profile, so the children of a confined daemon could not confine
// themselves to the renderer's, which admits the GPU. ttcd starts the
// launcher before it confines itself, and asks it for each renderer. The
// launcher stays unconfined for its children's sake, so it does nothing but
// start the renderer's executable with fixed arguments; the renderers
// confine themselves before they read anything.

// probeTimeout is how long the renderer's probe may take.
const probeTimeout = time.Minute

// socketpair returns a connected pair of Unix stream sockets, which no
// process that the daemon starts inherits.
func socketpair() ([2]int, error) {
	// macOS cannot create sockets close-on-exec: hold off concurrent forks
	// until they are.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds, nil
}

// ServeLauncher starts renderers, of the executable renderer, for the ttcd
// connected to the Unix stream socket fd, which it takes, until ttcd
// disconnects. Renderers run until the branches that they serve disconnect.
func ServeLauncher(fd int, renderer string) error {
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
		var trailer []byte
		switch m.Type {
		case gfxproto.MsgLaunchRenderer:
			var p gfxproto.LaunchRenderer
			if _, err = m.Decode(&p); err != nil {
				break
			}
			if len(m.FDs) != 1 {
				err = fmt.Errorf("%v with %d descriptors", m.Type, len(m.FDs))
				break
			}
			fd := m.FDs[0]
			m.FDs = nil
			err = launchRenderer(renderer, fd)
		case gfxproto.MsgLaunchProbe:
			var p gfxproto.LaunchProbe
			if _, err = m.Decode(&p); err != nil {
				break
			}
			trailer, err = ProbeRenderer(BrokerOptions{Renderer: renderer, Stderr: os.Stderr})
		default:
			m.CloseFDs()
			return fmt.Errorf("unexpected %v", m.Type)
		}
		m.CloseFDs()
		reply := gfxproto.Reply{}
		if err != nil {
			reply.Errno = uint32(unix.EIO)
			trailer = []byte(err.Error())
		}
		if err := conn.Send(gfxproto.MsgReply, m.Seq, &reply, trailer); err != nil {
			return err
		}
	}
}

// launchRenderer starts the renderer of a family, whose control connection
// is fd, which it takes, and the renderer gets as its fd 3.
func launchRenderer(renderer string, fd int) error {
	conn := os.NewFile(uintptr(fd), "gfx-renderer-control")
	defer conn.Close()
	cmd := exec.Command(renderer, "serve-family")
	cmd.ExtraFiles = []*os.File{conn}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// lastLine returns the last line of s.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Launcher is ttcd's connection to its renderers' launcher. Its methods may
// be called concurrently.
type Launcher struct {
	mu   sync.Mutex
	conn *gfxproto.Conn
	seq  uint64
}

// StartLauncher starts a launcher, opts.Renderer in its launch mode, which
// starts renderers, and logs to opts.Stderr. The launcher runs until the
// Launcher is closed, or its process exits.
func StartLauncher(opts BrokerOptions) (*Launcher, error) {
	fds, err := socketpair()
	if err != nil {
		return nil, err
	}
	launcherEnd := os.NewFile(uintptr(fds[1]), "gfx-launcher-connection")
	defer launcherEnd.Close()
	cmd := exec.Command(opts.Renderer, "launch")
	// The connection is the launcher's fd 3.
	cmd.ExtraFiles = []*os.File{launcherEnd}
	cmd.Stdout = opts.Stderr
	cmd.Stderr = opts.Stderr
	if err := cmd.Start(); err != nil {
		unix.Close(fds[0])
		return nil, err
	}
	go cmd.Wait()
	conn, err := gfxproto.NewConn(fds[0])
	if err != nil {
		return nil, err
	}
	return &Launcher{conn: conn}, nil
}

// call sends the launcher a request, and returns its reply's trailer.
func (l *Launcher) call(typ gfxproto.MsgType, p any, fds ...int) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	if err := l.conn.Send(typ, l.seq, p, nil, fds...); err != nil {
		return nil, fmt.Errorf("the GPU renderers' launcher: %w", err)
	}
	m, err := l.conn.Recv()
	if err != nil {
		return nil, fmt.Errorf("the GPU renderers' launcher: %w", err)
	}
	m.CloseFDs()
	if m.Type != gfxproto.MsgReply || m.Seq != l.seq {
		return nil, fmt.Errorf("the GPU renderers' launcher answered %v %d to %v %d", m.Type, m.Seq, typ, l.seq)
	}
	var reply gfxproto.Reply
	trailer, err := m.Decode(&reply)
	if err != nil {
		return nil, err
	}
	if reply.Errno != 0 {
		return nil, errors.New(string(trailer))
	}
	return trailer, nil
}

// StartRenderer asks the launcher to start the renderer of a family, and
// returns the other end of the renderer's control connection.
func (l *Launcher) StartRenderer() (int, error) {
	fds, err := socketpair()
	if err != nil {
		return -1, err
	}
	_, err = l.call(gfxproto.MsgLaunchRenderer, &gfxproto.LaunchRenderer{}, fds[1])
	unix.Close(fds[1])
	if err != nil {
		unix.Close(fds[0])
		return -1, err
	}
	return fds[0], nil
}

// Probe asks the launcher for the renderer's capability sets, as its probe
// prints them.
func (l *Launcher) Probe() ([]byte, error) {
	return l.call(gfxproto.MsgLaunchProbe, &gfxproto.LaunchProbe{})
}

// Close closes the connection, which makes the launcher exit.
func (l *Launcher) Close() error {
	return l.conn.Close()
}
