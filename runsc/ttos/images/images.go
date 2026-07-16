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

// Package images implements TTOS image operations backed by containerd.
package images

import (
	"context"
	"fmt"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	distref "github.com/distribution/reference"
)

// PullOptions controls image resolution, download, and unpacking.
type PullOptions struct {
	Address     string
	Namespace   string
	Reference   string
	Platform    string
	Snapshotter string
	Unpack      bool
}

// Info describes a pulled platform-specific image.
type Info struct {
	Reference   string `json:"reference"`
	Digest      string `json:"digest"`
	MediaType   string `json:"media_type"`
	Size        int64  `json:"size"`
	Platform    string `json:"platform"`
	Snapshotter string `json:"snapshotter,omitempty"`
	Unpacked    bool   `json:"unpacked"`
}

// NormalizeReference applies Docker's familiar-name and default-tag rules.
func NormalizeReference(input string) (string, error) {
	named, err := distref.ParseNormalizedNamed(input)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", input, err)
	}
	return distref.TagNameOnly(named).String(), nil
}

// Pull resolves and stores one image in containerd.
func Pull(ctx context.Context, opts PullOptions) (Info, error) {
	reference, err := NormalizeReference(opts.Reference)
	if err != nil {
		return Info{}, err
	}
	client, err := containerd.New(opts.Address, containerd.WithDefaultNamespace(opts.Namespace))
	if err != nil {
		return Info{}, fmt.Errorf("connecting to containerd at %q: %w", opts.Address, err)
	}
	defer client.Close()

	var remoteOpts []containerd.RemoteOpt
	if opts.Platform != "" {
		remoteOpts = append(remoteOpts, containerd.WithPlatform(opts.Platform))
	}
	if opts.Unpack {
		remoteOpts = append(remoteOpts, containerd.WithPullUnpack)
		if opts.Snapshotter != "" {
			remoteOpts = append(remoteOpts, containerd.WithPullSnapshotter(opts.Snapshotter))
		}
	}
	image, err := client.Pull(ctx, reference, remoteOpts...)
	if err != nil {
		return Info{}, fmt.Errorf("pulling %q: %w", reference, err)
	}
	size, err := image.Size(ctx)
	if err != nil {
		return Info{}, fmt.Errorf("measuring %q: %w", reference, err)
	}
	spec, err := image.Spec(ctx)
	if err != nil {
		return Info{}, fmt.Errorf("reading config for %q: %w", reference, err)
	}
	platform := strings.TrimSuffix(spec.OS+"/"+spec.Architecture+"/"+spec.Variant, "/")
	target := image.Target()
	return Info{
		Reference:   image.Name(),
		Digest:      target.Digest.String(),
		MediaType:   target.MediaType,
		Size:        size,
		Platform:    platform,
		Snapshotter: opts.Snapshotter,
		Unpacked:    opts.Unpack,
	}, nil
}
