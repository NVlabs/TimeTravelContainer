// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

var topLevelCompletionCommands = []string{
	"attach", "branch", "completion", "container", "create", "daemon", "events", "exec", "fork",
	"help", "image", "images", "inspect", "kill", "logs", "ls", "pause",
	"port", "ps", "pull", "restart", "resume", "rm", "rmi", "run", "start", "stop",
	"top", "unpause", "volume", "wait",
}

const bashCompletion = `# bash completion for ttos
_ttos_completion() {
  local current i
  local -a before globals
  current="${COMP_WORDS[COMP_CWORD]}"
  before=()
  globals=()
  for ((i=1; i<COMP_CWORD; i++)); do
    before+=("${COMP_WORDS[i]}")
  done
  for ((i=0; i<${#before[@]}; i++)); do
    case "${before[i]}" in
      --root|--runsc|--data-root|--daemon-socket)
        globals+=("${before[i]}")
        if ((i+1 < ${#before[@]})); then
          globals+=("${before[i+1]}")
          ((i++))
        else
          break
        fi
        ;;
      --root=*|--runsc=*|--data-root=*|--daemon-socket=*|--direct|--unsafe-no-host-isolation)
        globals+=("${before[i]}")
        ;;
      *) break ;;
    esac
  done
  mapfile -t COMPREPLY < <(command ttos "${globals[@]}" _complete "${before[@]}" "$current")
}
complete -F _ttos_completion ttos
`

const zshCompletion = `#compdef ttos
_ttos_completion() {
  local -a before candidates globals
  local i
  before=()
  globals=()
  for ((i=2; i<CURRENT; i++)); do
    before+=("${words[i]}")
  done
  for ((i=1; i<=${#before[@]}; i++)); do
    case "${before[i]}" in
      --root|--runsc|--data-root|--daemon-socket)
        globals+=("${before[i]}")
        if ((i < ${#before[@]})); then
          ((i++))
          globals+=("${before[i]}")
        else
          break
        fi
        ;;
      --root=*|--runsc=*|--data-root=*|--daemon-socket=*|--direct|--unsafe-no-host-isolation)
        globals+=("${before[i]}")
        ;;
      *) break ;;
    esac
  done
  candidates=("${(@f)$(command ttos "${globals[@]}" _complete "${before[@]}" "${words[CURRENT]}")}")
  compadd -- "${candidates[@]}"
}
compdef _ttos_completion ttos
`

const fishCompletion = `function __ttos_completion
    set -l before (commandline -opc)
    if test (count $before) -gt 0
        set -e before[1]
    end
    set -l globals
    set -l scan $before
    while test (count $scan) -gt 0
        switch $scan[1]
            case --root --runsc --data-root --daemon-socket
                if test (count $scan) -lt 2
                    break
                end
                set -a globals $scan[1] $scan[2]
                set -e scan[1..2]
            case "--root=*" "--runsc=*" "--data-root=*" "--daemon-socket=*" --direct --unsafe-no-host-isolation
                set -a globals $scan[1]
                set -e scan[1]
            case "*"
                break
        end
    end
    command ttos $globals _complete $before (commandline -ct)
end
complete -c ttos -f -a "(__ttos_completion)"
`

func runCompletion(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: ttos completion bash|zsh|fish")
	}
	switch args[0] {
	case "bash":
		_, _ = io.WriteString(stdout, bashCompletion)
	case "zsh":
		_, _ = io.WriteString(stdout, zshCompletion)
	case "fish":
		_, _ = io.WriteString(stdout, fishCompletion)
	default:
		return fmt.Errorf("unsupported completion shell %q; supported shells are bash, zsh, and fish", args[0])
	}
	return nil
}

func runDynamicCompletion(args []string, stdout io.Writer, backend backend) error {
	prefix := ""
	words := args
	if len(args) != 0 {
		prefix = args[len(args)-1]
		words = args[:len(args)-1]
	}
	candidates, err := completionCandidates(stripGlobalCompletionOptions(words), backend)
	if err != nil {
		// Completion must remain unobtrusive when runtime or image state is
		// temporarily unavailable.
		return nil
	}
	seen := make(map[string]struct{}, len(candidates))
	sort.Strings(candidates)
	for _, candidate := range candidates {
		if !strings.HasPrefix(candidate, prefix) {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		fmt.Fprintln(stdout, candidate)
	}
	return nil
}

func stripGlobalCompletionOptions(words []string) []string {
	for len(words) != 0 {
		word := words[0]
		switch {
		case word == "--unsafe-no-host-isolation" || word == "--direct":
			words = words[1:]
		case word == "--root" || word == "--runsc" || word == "--data-root" || word == "--daemon-socket":
			if len(words) == 1 {
				return nil
			}
			words = words[2:]
		case strings.HasPrefix(word, "--root=") || strings.HasPrefix(word, "--runsc=") ||
			strings.HasPrefix(word, "--data-root=") || strings.HasPrefix(word, "--daemon-socket="):
			words = words[1:]
		default:
			return words
		}
	}
	return nil
}

func completionCandidates(words []string, backend backend) ([]string, error) {
	if len(words) == 0 {
		return append([]string(nil), topLevelCompletionCommands...), nil
	}
	command := words[0]
	rest := words[1:]
	if command == "unpause" {
		command = "resume"
	}
	if command == "container" {
		if len(rest) == 0 {
			return []string{"attach", "create", "exec", "inspect", "kill", "logs", "ls", "pause", "restart", "rm", "run", "start", "stop", "top", "unpause", "wait"}, nil
		}
		command = rest[0]
		if command == "unpause" {
			command = "resume"
		}
		rest = rest[1:]
	}
	switch command {
	case "daemon":
		if len(rest) == 0 {
			return []string{"request", "requests", "start", "status", "stop"}, nil
		}
		if rest[0] == "start" && len(rest) == 1 {
			return []string{"--unsafe-no-host-isolation"}, nil
		}
		if rest[0] == "request" && len(rest) == 1 {
			return []string{"inspect", "resolve"}, nil
		}
		return nil, nil
	case "image":
		if len(rest) == 0 {
			return []string{"inspect", "ls", "pull", "rm"}, nil
		}
		if rest[0] == "inspect" || rest[0] == "rm" || rest[0] == "remove" {
			return imageCompletionCandidates(backend)
		}
		return nil, nil
	case "volume":
		if len(rest) == 0 {
			return []string{"commit", "create", "export", "import", "inspect", "ls", "rm"}, nil
		}
		if rest[0] == "inspect" || rest[0] == "rm" || rest[0] == "remove" {
			return volumeCompletionCandidates(backend)
		}
		return nil, nil
	case "branch":
		if len(rest) == 0 {
			return []string{"rm"}, nil
		}
		if rest[0] == "rm" {
			return familyCompletionCandidates(backend, true, false)
		}
		return nil, nil
	case "port":
		if len(rest) == 0 {
			return []string{"ls", "publish", "unpublish"}, nil
		}
		if rest[0] == "ls" || rest[0] == "publish" || rest[0] == "unpublish" {
			return familyCompletionCandidates(backend, true, true)
		}
		return nil, nil
	case "exec":
		return execCompletionCandidates(rest, backend)
	case "pull", "run", "create":
		return imageCompletionCandidates(backend)
	case "rmi":
		return imageCompletionCandidates(backend)
	case "start", "restart", "rm", "inspect":
		return familyCompletionCandidates(backend, false, true)
	case "attach", "logs", "pause", "resume", "kill", "stop", "wait", "top":
		return familyCompletionCandidates(backend, true, true)
	case "fork":
		return familyCompletionCandidates(backend, true, true)
	default:
		return nil, nil
	}
}

func familyCompletionCandidates(backend backend, branches, includeMain bool) ([]string, error) {
	families, err := backend.listFamilies()
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, family := range families {
		name := familyDisplayName(family)
		candidates = append(candidates, name)
		if !branches {
			continue
		}
		for _, branch := range family.Branches {
			if branch.State == "deleted" || (!includeMain && branch.ID == defaultBranchID) {
				continue
			}
			candidates = append(candidates, name+"@"+branch.ID)
		}
	}
	return candidates, nil
}

func imageCompletionCandidates(backend backend) ([]string, error) {
	images, err := backend.listImages()
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, 2*len(images))
	for _, image := range images {
		candidates = append(candidates, image.Reference)
		if image.Digest != "" {
			candidates = append(candidates, image.Digest)
		}
	}
	return candidates, nil
}

func volumeCompletionCandidates(backend backend) ([]string, error) {
	volumes, err := backend.listVolumes()
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		candidates = append(candidates, volume.Name)
	}
	return candidates, nil
}

func execCompletionCandidates(rest []string, backend backend) ([]string, error) {
	if len(rest) == 0 {
		targets, err := familyCompletionCandidates(backend, true, true)
		return append([]string{"kill", "ls", "wait"}, targets...), err
	}
	switch rest[0] {
	case "ls":
		return familyCompletionCandidates(backend, true, true)
	case "wait", "kill":
		if len(rest) == 1 {
			return familyCompletionCandidates(backend, true, true)
		}
		if len(rest) == 2 {
			target, err := parseSelector(rest[1])
			if err != nil {
				return nil, nil
			}
			sessions, err := backend.execSessions(target)
			if err != nil {
				return nil, err
			}
			candidates := make([]string, 0, len(sessions))
			for _, session := range sessions {
				candidates = append(candidates, session.ID)
			}
			return candidates, nil
		}
		return nil, nil
	default:
		return familyCompletionCandidates(backend, true, true)
	}
}
