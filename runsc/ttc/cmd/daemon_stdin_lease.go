// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type daemonStdinLease struct {
	cancel func()
	done   chan struct{}
}

func daemonAttachStdinLease(opts options, args []string, stdin bool) ([]string, string, bool, error) {
	rawCommandArgs := args
	if len(rawCommandArgs) != 0 && rawCommandArgs[0] == "container" {
		rawCommandArgs = rawCommandArgs[1:]
	}
	if len(rawCommandArgs) == 0 || rawCommandArgs[0] != "attach" {
		return append([]string(nil), args...), "", false, nil
	}
	clean := make([]string, 0, len(args))
	takeover := false
	for _, arg := range args {
		switch {
		case arg == "--takeover":
			takeover = true
		case strings.HasPrefix(arg, "--takeover="):
			value, err := strconv.ParseBool(strings.TrimPrefix(arg, "--takeover="))
			if err != nil {
				return nil, "", false, fmt.Errorf("invalid --takeover value: %w", err)
			}
			takeover = value
		default:
			clean = append(clean, arg)
		}
	}
	commandArgs := clean
	if len(commandArgs) != 0 && commandArgs[0] == "container" {
		commandArgs = commandArgs[1:]
	}
	if takeover && !stdin {
		return nil, "", false, errors.New("--takeover requires --interactive")
	}
	if !stdin {
		return clean, "", false, nil
	}
	execID := ""
	targetValue := ""
	values := commandArgs[1:]
	for index := 0; index < len(values); index++ {
		arg := values[index]
		switch {
		case arg == "--exec" || arg == "--detach-keys":
			if index+1 < len(values) {
				if arg == "--exec" {
					execID = values[index+1]
				}
				index++
			}
		case strings.HasPrefix(arg, "--exec="):
			execID = strings.TrimPrefix(arg, "--exec=")
		case strings.HasPrefix(arg, "-"):
		default:
			targetValue = arg
		}
	}
	if targetValue == "" {
		return clean, "", takeover, nil
	}
	target, err := parseSelector(targetValue)
	if err != nil {
		return nil, "", false, err
	}
	backend := directBackend(opts, strings.NewReader(""), io.Discard, io.Discard)
	resolved, _, err := backend.resolveSelector(target)
	if err != nil {
		return nil, "", false, err
	}
	key := filepath.Clean(opts.dataRoot) + "\x00" + resolved.String()
	if execID != "" {
		key += ":" + execID
	}
	return clean, key, takeover, nil
}

func (s *daemonServer) acquireStdinLease(key string, takeover bool, cancel func()) (func(), error) {
	for {
		s.stdinMu.Lock()
		if s.stdinLeases == nil {
			s.stdinLeases = make(map[string]*daemonStdinLease)
		}
		old := s.stdinLeases[key]
		if old == nil {
			lease := &daemonStdinLease{cancel: cancel, done: make(chan struct{})}
			s.stdinLeases[key] = lease
			s.stdinMu.Unlock()
			return func() {
				s.stdinMu.Lock()
				if s.stdinLeases[key] == lease {
					delete(s.stdinLeases, key)
					close(lease.done)
				}
				s.stdinMu.Unlock()
			}, nil
		}
		if !takeover {
			s.stdinMu.Unlock()
			display := key
			if parts := strings.SplitN(key, "\x00", 2); len(parts) == 2 {
				display = parts[1]
			}
			return nil, fmt.Errorf("stdin for %s is already attached; use --takeover to replace its owner", display)
		}
		done := old.done
		cancel := old.cancel
		s.stdinMu.Unlock()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			// The old owner was told to let go and did not. Its client is
			// usually already gone — a dropped ssh pipe, a laptop closed
			// mid-turn — and whatever its handler is still blocked on, the
			// session it holds is one nobody can reach until the lease moves.
			//
			// Taking it is safe because cancel() has already closed the pipe
			// that owner writes through: it cannot reach the terminal again,
			// whatever it does next. Its own release is a no-op once this
			// entry is somebody else's.
			s.stdinMu.Lock()
			if s.stdinLeases[key] == old {
				lease := &daemonStdinLease{cancel: cancel, done: make(chan struct{})}
				s.stdinLeases[key] = lease
				s.stdinMu.Unlock()
				return func() {
					s.stdinMu.Lock()
					if s.stdinLeases[key] == lease {
						delete(s.stdinLeases, key)
						close(lease.done)
					}
					s.stdinMu.Unlock()
				}, nil
			}
			s.stdinMu.Unlock()
		}
	}
}
