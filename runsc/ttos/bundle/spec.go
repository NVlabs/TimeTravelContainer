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

// Package bundle materializes image roots and generates OCI bundles for ttos.
package bundle

import (
	"fmt"
	"path/filepath"
	"strings"

	mobyuser "github.com/moby/sys/user"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

var defaultCapabilities = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FSETID",
	"CAP_FOWNER",
	"CAP_MKNOD",
	"CAP_NET_RAW",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETFCAP",
	"CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT",
	"CAP_KILL",
	"CAP_AUDIT_WRITE",
}

// SpecOptions contains user-facing overrides for an image's OCI configuration.
type SpecOptions struct {
	Name       string
	RootFS     string
	Args       []string
	Env        []string
	Cwd        string
	User       string
	Hostname   string
	Terminal   bool
	Entrypoint []string
}

// BuildSpec combines an OCI image configuration with ttos run overrides.
func BuildSpec(image ocispec.Image, opts SpecOptions) (*specs.Spec, error) {
	rootfs, err := filepath.Abs(opts.RootFS)
	if err != nil {
		return nil, fmt.Errorf("resolving rootfs path: %w", err)
	}
	cmd := image.Config.Cmd
	if len(opts.Args) != 0 {
		cmd = opts.Args
	}
	entrypoint := image.Config.Entrypoint
	if opts.Entrypoint != nil {
		entrypoint = opts.Entrypoint
	}
	args := append(append([]string(nil), entrypoint...), cmd...)
	if len(args) == 0 {
		return nil, fmt.Errorf("image and command override specify no command")
	}
	env, err := mergeEnv(image.Config.Env, opts.Env)
	if err != nil {
		return nil, err
	}
	if len(env) == 0 {
		env = []string{"PATH=" + defaultPath}
	}
	cwd := image.Config.WorkingDir
	if opts.Cwd != "" {
		cwd = opts.Cwd
	}
	if cwd == "" {
		cwd = "/"
	}
	if !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("working directory %q is not absolute", cwd)
	}
	userSpec := image.Config.User
	if opts.User != "" {
		userSpec = opts.User
	}
	resolvedUser, err := mobyuser.GetExecUserPath(userSpec, &mobyuser.ExecUser{}, filepath.Join(rootfs, "etc/passwd"), filepath.Join(rootfs, "etc/group"))
	if err != nil {
		return nil, fmt.Errorf("resolving user %q: %w", userSpec, err)
	}
	additionalGIDs := make([]uint32, 0, len(resolvedUser.Sgids))
	for _, gid := range resolvedUser.Sgids {
		additionalGIDs = append(additionalGIDs, uint32(gid))
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname = opts.Name
	}
	return &specs.Spec{
		Version: specs.Version,
		Root: &specs.Root{
			Path:     rootfs,
			Readonly: false,
		},
		Process: &specs.Process{
			Terminal: opts.Terminal,
			User: specs.User{
				UID:            uint32(resolvedUser.Uid),
				GID:            uint32(resolvedUser.Gid),
				AdditionalGids: additionalGIDs,
			},
			Args: args,
			Env:  env,
			Cwd:  cwd,
			Capabilities: &specs.LinuxCapabilities{
				Bounding:  append([]string(nil), defaultCapabilities...),
				Effective: append([]string(nil), defaultCapabilities...),
				Permitted: append([]string(nil), defaultCapabilities...),
			},
			Rlimits: []specs.POSIXRlimit{{
				Type: "RLIMIT_NOFILE",
				Hard: 1024,
				Soft: 1024,
			}},
		},
		Hostname: hostname,
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.IPCNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.MountNamespace},
				{Type: specs.NetworkNamespace},
			},
		},
		Annotations: map[string]string{
			"dev.gvisor.spec.rootfs.overlay": "memory",
		},
	}, nil
}

func mergeEnv(imageEnv, overrides []string) ([]string, error) {
	result := append([]string(nil), imageEnv...)
	positions := make(map[string]int)
	for i, value := range result {
		name, _, ok := strings.Cut(value, "=")
		if ok {
			positions[name] = i
		}
	}
	for _, value := range overrides {
		name, _, ok := strings.Cut(value, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("environment override %q must be NAME=VALUE", value)
		}
		if i, ok := positions[name]; ok {
			result[i] = value
		} else {
			positions[name] = len(result)
			result = append(result, value)
		}
	}
	return result, nil
}
