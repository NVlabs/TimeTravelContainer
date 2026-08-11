// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
)

const (
	daemonEventSchemaVersion = 1
	daemonEventMaxBytes      = 16 << 20
	daemonEventFiles         = 4
)

// DaemonEvent is one durable manager-level lifecycle observation.
type DaemonEvent struct {
	Version    int               `json:"version"`
	ID         string            `json:"id"`
	Time       time.Time         `json:"time"`
	Type       string            `json:"type"`
	Action     string            `json:"action"`
	Subject    string            `json:"subject,omitempty"`
	Parent     string            `json:"parent,omitempty"`
	Status     int               `json:"status"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type daemonEventsOptions struct {
	follow     bool
	jsonOutput bool
	since      time.Time
	until      time.Time
	filters    map[string]string
}

func daemonEventsPath(dataRoot string) string {
	return filepath.Join(dataRoot, "events", "events.jsonl")
}

func daemonEventLogPath(path string, generation int) string {
	if generation == 0 {
		return path
	}
	return path + "." + strconv.Itoa(generation)
}

func retainedDaemonEventPaths(path string, files int) []string {
	paths := make([]string, 0, files)
	for generation := files - 1; generation >= 0; generation-- {
		paths = append(paths, daemonEventLogPath(path, generation))
	}
	return paths
}

// rotateDaemonEventLog bounds durable operational history independently of
// branch lifecycle state. The caller holds eventMu, so a follower can never
// leave unread records behind in a file that is being rotated.
func rotateDaemonEventLog(path string, incoming, maxBytes int64, files int) error {
	if maxBytes <= 0 || files < 1 {
		return errors.New("event log retention must have positive bytes and files")
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() == 0 || info.Size()+incoming <= maxBytes {
		return nil
	}
	if err := os.Remove(daemonEventLogPath(path, files-1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for generation := files - 2; generation >= 0; generation-- {
		oldPath := daemonEventLogPath(path, generation)
		newPath := daemonEventLogPath(path, generation+1)
		if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *daemonServer) recordCommandEvents(dataRoot string, args []string, status int) {
	if dataRoot == "" {
		return
	}
	for _, event := range commandEvents(args, status) {
		s.enrichConsoleRetentionEvent(dataRoot, &event)
		s.recordEvent(dataRoot, event)
	}
}

func (s *daemonServer) enrichConsoleRetentionEvent(dataRoot string, event *DaemonEvent) {
	if event.Subject == "" {
		return
	}
	var target selector
	var execID string
	var err error
	if event.Type == "exec" && strings.Contains(event.Subject, ":") {
		var execTarget execSelector
		execTarget, err = parseExecSelector(event.Subject)
		target = execTarget.target
		execID = execTarget.execID
	} else {
		target, err = parseSelector(event.Subject)
	}
	if err != nil {
		return
	}
	store := ttcbundle.Store{Root: dataRoot}
	var paths ttcbundle.ConsolePaths
	if execID == "" {
		paths, err = store.BranchConsolePaths(target.container, target.branch)
	} else {
		paths, err = store.ExecConsolePaths(target.container, target.branch, execID)
	}
	if err != nil {
		return
	}
	stats, err := ttcconsole.ReadRetentionStats(paths.RetentionStats)
	if err != nil || (stats.EvictedBytes == 0 && stats.EvictedGenerations == 0) {
		return
	}
	if event.Attributes == nil {
		event.Attributes = make(map[string]string)
	}
	event.Attributes["output_evicted_bytes"] = strconv.FormatInt(stats.EvictedBytes, 10)
	event.Attributes["output_evicted_generations"] = strconv.FormatUint(stats.EvictedGenerations, 10)
	if !stats.UpdatedAt.IsZero() {
		event.Attributes["output_retention_updated_at"] = stats.UpdatedAt.Format(time.RFC3339Nano)
	}
}

func (s *daemonServer) recordEvent(dataRoot string, event DaemonEvent) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	s.eventSeq++
	event.Version = daemonEventSchemaVersion
	event.Time = time.Now().UTC()
	event.ID = fmt.Sprintf("%d-%d-%d", event.Time.UnixNano(), os.Getpid(), s.eventSeq)
	dir := filepath.Dir(daemonEventsPath(dataRoot))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	data = append(data, '\n')
	if err := rotateDaemonEventLog(daemonEventsPath(dataRoot), int64(len(data)), daemonEventMaxBytes, daemonEventFiles); err != nil {
		return
	}
	file, err := os.OpenFile(daemonEventsPath(dataRoot), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	if writeDaemonBytes(file, data) == nil {
		_ = file.Sync()
	}
	_ = file.Close()
}

func commandEvents(args []string, status int) []DaemonEvent {
	if len(args) == 0 {
		return nil
	}
	command := args[0]
	rest := args[1:]
	if command == "unpause" {
		command = "resume"
	}
	if command == "container" && len(rest) != 0 {
		command = rest[0]
		rest = rest[1:]
		if command == "unpause" {
			command = "resume"
		}
	}
	if command == "image" && len(rest) != 0 {
		switch rest[0] {
		case "pull":
			command = "pull"
			rest = rest[1:]
		case "rm", "remove":
			command = "rmi"
			rest = rest[1:]
		}
	}
	event := DaemonEvent{Status: status}
	switch command {
	case "pull", "rmi", "images", "image":
		event.Type = "image"
		event.Action = command
		event.Subject = lastNonFlag(rest)
	case "create", "run", "start", "restart", "stop", "kill", "wait", "rm", "logs", "inspect", "dehydrate", "rehydrate":
		event.Type = "container"
		event.Action = command
		if command == "wait" || command == "rm" {
			event.Action = map[string]string{"wait": "exit", "rm": "destroy"}[command]
		}
		if command == "logs" || command == "inspect" {
			event.Subject = lastNonFlag(rest)
		} else {
			event.Subject = lifecycleTarget(command, rest)
		}
		if strings.Contains(event.Subject, "@") {
			event.Type = "branch"
		}
		if command == "stop" {
			exitEvent := event
			exitEvent.Action = "exit"
			return []DaemonEvent{event, exitEvent}
		}
	case "pause", "resume":
		event.Type = "branch"
		event.Action = command
		event.Subject = firstNonFlag(rest)
	case "fork":
		positionals := forkPositionals(rest)
		event.Type = "branch"
		event.Action = "fork"
		if len(positionals) != 0 {
			event.Parent = positionals[0]
			event.Subject = positionals[len(positionals)-1]
		}
	case "exec":
		if len(rest) != 0 && (rest[0] == "ls" || rest[0] == "wait" || rest[0] == "kill") {
			event.Type = "exec"
			event.Action = rest[0]
			if event.Action == "wait" {
				event.Action = "exit"
			}
			event.Subject = lastNonFlag(rest[1:])
		} else {
			event.Type = "exec"
			event.Action = "create"
			event.Subject = execTarget(rest)
		}
	case "attach":
		event.Type = "attach"
		event.Action = "attach"
		event.Subject = lastNonFlag(rest)
	default:
		return nil
	}
	if event.Subject == "" && event.Parent == "" {
		event.Attributes = map[string]string{"command": command}
	}
	return []DaemonEvent{event}
}

func firstNonFlag(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

func lastNonFlag(args []string) string {
	for i := len(args) - 1; i >= 0; i-- {
		if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return ""
}

func forkPositionals(args []string) []string {
	out := make([]string, 0, 2)
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			out = append(out, arg)
		}
	}
	return out
}

func lifecycleTarget(command string, args []string) string {
	if command == "run" || command == "create" {
		for i := 0; i < len(args); i++ {
			if args[i] == "--name" && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(args[i], "--name=") {
				return strings.TrimPrefix(args[i], "--name=")
			}
		}
	}
	valueFlags := map[string]bool{
		"--time": true, "-t": true, "--signal": true, "-s": true,
		"--stop-timeout": true, "--condition": true,
	}
	return firstCommandPositional(args, valueFlags)
}

func execTarget(args []string) string {
	valueFlags := map[string]bool{
		"--env": true, "-e": true, "--workdir": true, "-w": true,
		"--user": true, "-u": true, "--additional-gids": true,
		"--cap": true, "--process": true,
	}
	return firstCommandPositional(args, valueFlags)
}

func firstCommandPositional(args []string, valueFlags map[string]bool) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, "--") {
			if !strings.Contains(arg, "=") && valueFlags[arg] {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if valueFlags[arg] {
				i++
			}
			continue
		}
		return arg
	}
	return ""
}

func parseDaemonEventsOptions(args []string) (daemonEventsOptions, error) {
	options := daemonEventsOptions{filters: make(map[string]string)}
	now := time.Now()
	for len(args) != 0 {
		arg := args[0]
		switch {
		case arg == "--follow" || arg == "-f":
			options.follow = true
			args = args[1:]
		case arg == "--json":
			options.jsonOutput = true
			args = args[1:]
		case (arg == "--since" || arg == "--until" || arg == "--filter") && len(args) >= 2:
			value := args[1]
			args = args[2:]
			switch arg {
			case "--since":
				parsed, err := parseLogTime(value, now)
				if err != nil {
					return options, fmt.Errorf("invalid --since: %w", err)
				}
				options.since = parsed
			case "--until":
				parsed, err := parseLogTime(value, now)
				if err != nil {
					return options, fmt.Errorf("invalid --until: %w", err)
				}
				options.until = parsed
			case "--filter":
				key, filterValue, ok := strings.Cut(value, "=")
				if !ok || filterValue == "" {
					return options, fmt.Errorf("event filter %q must be KEY=VALUE", value)
				}
				switch key {
				case "type", "event", "action", "subject", "container":
					options.filters[key] = filterValue
				default:
					return options, fmt.Errorf("unsupported event filter %q", key)
				}
			}
		case strings.HasPrefix(arg, "--since=") || strings.HasPrefix(arg, "--until=") || strings.HasPrefix(arg, "--filter="):
			key, value, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			args = append([]string{"--" + key, value}, args[1:]...)
		default:
			return options, fmt.Errorf("usage: ttc events [--json] [-f|--follow] [--since TIME] [--until TIME] [--filter KEY=VALUE]")
		}
	}
	if !options.until.IsZero() && !options.since.IsZero() && options.until.Before(options.since) {
		return options, errors.New("--until precedes --since")
	}
	return options, nil
}

func eventMatches(event DaemonEvent, options daemonEventsOptions) bool {
	if !options.since.IsZero() && event.Time.Before(options.since) {
		return false
	}
	if !options.until.IsZero() && event.Time.After(options.until) {
		return false
	}
	for key, value := range options.filters {
		switch key {
		case "type", "event":
			if event.Type != value {
				return false
			}
		case "action":
			if event.Action != value {
				return false
			}
		case "subject", "container":
			if event.Subject != value && !strings.HasPrefix(event.Subject, value+"@") {
				return false
			}
		}
	}
	return true
}

func writeDaemonEvent(writer io.Writer, event DaemonEvent, jsonOutput bool) error {
	if jsonOutput {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(writer, string(data))
		return err
	}
	subject := event.Subject
	if subject == "" {
		subject = "-"
	}
	_, err := fmt.Fprintf(writer, "%s %s %s %s status=%d id=%s\n",
		event.Time.UTC().Format(time.RFC3339Nano), event.Type, event.Action,
		subject, event.Status, event.ID)
	return err
}

func (s *daemonServer) streamEvents(dataRoot string, args []string, writer io.Writer) error {
	if dataRoot == "" {
		return errors.New("events require a persistent --data-root")
	}
	options, err := parseDaemonEventsOptions(args)
	if err != nil {
		return err
	}
	path := daemonEventsPath(dataRoot)
	var offset int64
	var currentInfo os.FileInfo
	initial := true
	for {
		s.eventMu.Lock()
		paths := []string{path}
		if initial {
			paths = retainedDaemonEventPaths(path, daemonEventFiles)
		}
		var events []DaemonEvent
		for _, eventPath := range paths {
			file, err := os.Open(eventPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				s.eventMu.Unlock()
				return fmt.Errorf("opening event log: %w", err)
			}
			info, err := file.Stat()
			if err != nil {
				file.Close()
				s.eventMu.Unlock()
				return err
			}
			fileOffset := int64(0)
			if eventPath == path {
				if !initial && currentInfo != nil && os.SameFile(currentInfo, info) {
					fileOffset = offset
				}
				currentInfo = info
			}
			if _, err := file.Seek(fileOffset, io.SeekStart); err != nil {
				file.Close()
				s.eventMu.Unlock()
				return err
			}
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 4096), daemonMaxFrame)
			for scanner.Scan() {
				fileOffset += int64(len(scanner.Bytes()) + 1)
				var event DaemonEvent
				if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
					file.Close()
					s.eventMu.Unlock()
					return fmt.Errorf("decoding event log %q at offset %d: %w", filepath.Base(eventPath), fileOffset, err)
				}
				if eventMatches(event, options) {
					events = append(events, event)
				}
			}
			if err := scanner.Err(); err != nil {
				file.Close()
				s.eventMu.Unlock()
				return err
			}
			if err := file.Close(); err != nil {
				s.eventMu.Unlock()
				return err
			}
			if eventPath == path {
				offset = fileOffset
			}
		}
		s.eventMu.Unlock()
		initial = false
		for _, event := range events {
			if err := writeDaemonEvent(writer, event, options.jsonOutput); err != nil {
				return err
			}
		}
		if !options.follow || (!options.until.IsZero() && !time.Now().Before(options.until)) {
			return nil
		}
		// An empty frame is a heartbeat that detects a disconnected follower
		// without producing output in the client.
		if _, err := writer.Write(nil); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}
