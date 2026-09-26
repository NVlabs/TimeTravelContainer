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

// Package cluster defines the cross-ttcd control-plane contract.
package cluster

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gvisor.dev/gvisor/pkg/ttcremote"
)

const (
	// ControlProtocolVersion 11 moves ephemeral dehydrated-fork metadata from
	// shared-filesystem paths to the transaction-fenced peer relay. Version 10
	// records the target-reported physical runtime identity before cutover.
	ControlProtocolVersion = 11

	FeatureRemotePull           = "remote-memory-pull"
	FeatureReversePush          = "reverse-page-push"
	FeatureAckBatch             = "ack-batch"
	FeatureNetworkRecreate      = "network-recreate"
	FeatureRuntimePrefetch      = "runtime-prefetch"
	FeatureFilesystemBranch     = "filesystem-branch"
	FeatureSharedMemorySnapshot = "shared-memory-snapshot"
)

var requiredForkFeatures = []string{
	FeatureRemotePull,
	FeatureNetworkRecreate,
	FeatureFilesystemBranch,
	FeatureSharedMemorySnapshot,
}

// Capabilities describes one ttcd peer without including credentials.
type Capabilities struct {
	NodeID          string           `json:"node_id"`
	ProtocolMin     uint32           `json:"protocol_min"`
	ProtocolMax     uint32           `json:"protocol_max"`
	BuildID         string           `json:"build_id"`
	Architecture    string           `json:"architecture"`
	CPUProfile      string           `json:"cpu_profile"`
	CPUFeatures     []string         `json:"cpu_features"`
	XsaveABI        string           `json:"xsave_abi,omitempty"`
	BasePageSize    uint64           `json:"base_page_size"`
	MaxTransferSize uint64           `json:"max_transfer_size"`
	RepositoryID    string           `json:"repository_id"`
	Features        []string         `json:"features"`
	PagingPolicy    ttcremote.Policy `json:"paging_policy"`
}

// Validate checks capability bounds before negotiation.
func (c Capabilities) Validate() error {
	if c.NodeID == "" || c.BuildID == "" || c.Architecture == "" || c.RepositoryID == "" || c.CPUProfile == "" {
		return errors.New("cluster capabilities omit node, build, architecture, or repository identity")
	}
	if c.ProtocolMin == 0 || c.ProtocolMax < c.ProtocolMin {
		return fmt.Errorf("invalid protocol range [%d,%d]", c.ProtocolMin, c.ProtocolMax)
	}
	if c.BasePageSize == 0 || c.BasePageSize&(c.BasePageSize-1) != 0 {
		return fmt.Errorf("base page size %d is not a power of two", c.BasePageSize)
	}
	if c.MaxTransferSize < c.BasePageSize || c.MaxTransferSize%c.BasePageSize != 0 || c.MaxTransferSize > 2<<20 {
		return fmt.Errorf("invalid maximum transfer size %d for page size %d", c.MaxTransferSize, c.BasePageSize)
	}
	if err := validatePagingPolicy(c, c.PagingPolicy); err != nil {
		return fmt.Errorf("paging policy: %w", err)
	}
	seen := make(map[string]struct{}, len(c.Features))
	for _, feature := range c.Features {
		if feature == "" || strings.TrimSpace(feature) != feature {
			return fmt.Errorf("invalid empty or untrimmed feature %q", feature)
		}
		if _, ok := seen[feature]; ok {
			return fmt.Errorf("duplicate feature %q", feature)
		}
		seen[feature] = struct{}{}
	}
	if !slices.IsSorted(c.CPUFeatures) {
		return errors.New("CPU feature profile is not sorted")
	}
	if len(c.CPUFeatures) == 0 {
		return errors.New("CPU feature profile is empty")
	}
	if slices.Contains(c.CPUFeatures, "xsave") && c.XsaveABI == "" {
		return errors.New("XSAVE CPU profile omits the physical XSAVE ABI")
	}
	seen = make(map[string]struct{}, len(c.CPUFeatures))
	for _, feature := range c.CPUFeatures {
		if feature == "" || strings.TrimSpace(feature) != feature || strings.Contains(feature, ",") {
			return fmt.Errorf("invalid CPU feature %q", feature)
		}
		if _, ok := seen[feature]; ok {
			return fmt.Errorf("duplicate CPU feature %q", feature)
		}
		seen[feature] = struct{}{}
	}
	return nil
}

// NegotiatedCapabilities is the immutable result recorded with a fork
// transaction.
type NegotiatedCapabilities struct {
	Protocol        uint32           `json:"protocol"`
	BasePageSize    uint64           `json:"base_page_size"`
	MaxTransferSize uint64           `json:"max_transfer_size"`
	Features        []string         `json:"features"`
	SourceBuildID   string           `json:"source_build_id"`
	TargetBuildID   string           `json:"target_build_id"`
	PagingPolicy    ttcremote.Policy `json:"paging_policy"`
}

func validatePagingPolicy(c Capabilities, p ttcremote.Policy) error {
	if p.PageSize != c.BasePageSize {
		return fmt.Errorf("page size %d does not match capability page size %d", p.PageSize, c.BasePageSize)
	}
	return p.Validate()
}

// Negotiate validates two peers and selects their highest common protocol and
// transfer limits. Fork-critical features, architecture, page size, and the
// shared repository identity must match exactly.
func Negotiate(source, target Capabilities) (NegotiatedCapabilities, error) {
	if err := source.Validate(); err != nil {
		return NegotiatedCapabilities{}, fmt.Errorf("source capabilities: %w", err)
	}
	if err := target.Validate(); err != nil {
		return NegotiatedCapabilities{}, fmt.Errorf("target capabilities: %w", err)
	}
	if source.NodeID == target.NodeID {
		return NegotiatedCapabilities{}, fmt.Errorf("source and target use the same node ID %q", source.NodeID)
	}
	if source.Architecture != target.Architecture {
		return NegotiatedCapabilities{}, fmt.Errorf("architecture mismatch: source=%s target=%s", source.Architecture, target.Architecture)
	}
	if source.CPUProfile != target.CPUProfile || !slices.Equal(source.CPUFeatures, target.CPUFeatures) || source.XsaveABI != target.XsaveABI {
		return NegotiatedCapabilities{}, fmt.Errorf("guest CPU profile mismatch: source=%s target=%s", source.CPUProfile, target.CPUProfile)
	}
	if source.BasePageSize != target.BasePageSize {
		return NegotiatedCapabilities{}, fmt.Errorf("base page size mismatch: source=%d target=%d", source.BasePageSize, target.BasePageSize)
	}
	if source.RepositoryID != target.RepositoryID {
		return NegotiatedCapabilities{}, fmt.Errorf("shared repository mismatch: source=%q target=%q", source.RepositoryID, target.RepositoryID)
	}
	if source.BuildID != target.BuildID {
		return NegotiatedCapabilities{}, fmt.Errorf("TTC build mismatch: source=%q target=%q", source.BuildID, target.BuildID)
	}
	protocolMin := max(source.ProtocolMin, target.ProtocolMin)
	protocolMax := min(source.ProtocolMax, target.ProtocolMax)
	if protocolMax < protocolMin {
		return NegotiatedCapabilities{}, fmt.Errorf("no common control protocol: source=[%d,%d] target=[%d,%d]", source.ProtocolMin, source.ProtocolMax, target.ProtocolMin, target.ProtocolMax)
	}
	targetFeatures := make(map[string]struct{}, len(target.Features))
	for _, feature := range target.Features {
		targetFeatures[feature] = struct{}{}
	}
	common := make([]string, 0, min(len(source.Features), len(target.Features)))
	for _, feature := range source.Features {
		if _, ok := targetFeatures[feature]; ok {
			common = append(common, feature)
		}
	}
	slices.Sort(common)
	for _, required := range requiredForkFeatures {
		if !slices.Contains(common, required) {
			return NegotiatedCapabilities{}, fmt.Errorf("target lacks required common feature %q", required)
		}
	}
	policy := target.PagingPolicy
	policy.Push = source.PagingPolicy.Push
	policy.TransferBytes = min(policy.TransferBytes, source.MaxTransferSize, target.MaxTransferSize)
	if policy.MaxOutstandingBytes < policy.TransferBytes {
		policy.MaxOutstandingBytes = policy.TransferBytes
	}
	if limit := int(policy.MaxOutstandingBytes / policy.TransferBytes); policy.MaxConcurrency > limit {
		policy.MaxConcurrency = limit
	}
	return NegotiatedCapabilities{
		Protocol: protocolMax, BasePageSize: source.BasePageSize,
		MaxTransferSize: min(source.MaxTransferSize, target.MaxTransferSize),
		Features:        common, SourceBuildID: source.BuildID, TargetBuildID: target.BuildID,
		PagingPolicy: policy,
	}, nil
}
