// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// PortableSchemaVersion is the clean-break portable checkpoint schema.
	// Checkpoints without this schema are intentionally not accepted.
	PortableSchemaVersion = 1

	// PortableLogicalPageSize is the canonical memory unit in portable
	// checkpoints, independent of host allocation or hypervisor geometry.
	PortableLogicalPageSize = 4096

	metadataSchema       = "ttc_checkpoint_schema"
	metadataStateABI     = "ttc_state_abi"
	metadataGuestISA     = "ttc_guest_isa"
	metadataPageSize     = "ttc_logical_page_size"
	metadataCPUFeatures  = "ttc_cpu_features"
	metadataResources    = "ttc_resource_profile"
	metadataCapabilities = "ttc_required_sentry_capabilities"
)

// ResourceProfile describes whether all resources represented by a checkpoint
// can be reconstructed without referring to the source host.
type ResourceProfile string

const (
	// ResourceProfileTTCManaged contains only TTC-owned memory, rootfs,
	// volumes, netstack state, and virtual devices.
	ResourceProfileTTCManaged ResourceProfile = "ttc-managed-v1"

	// ResourceProfileHostBound marks a checkpoint that refers to one or more
	// host resources. The checkpoint is valid but is not cross-host portable.
	ResourceProfileHostBound ResourceProfile = "host-bound-v1"
)

// PortableContract is the execution-independent compatibility envelope for a
// checkpoint. It deliberately excludes platform, monitor, GPA, host virtual
// address, host page geometry, page tables, and vCPU state.
type PortableContract struct {
	SchemaVersion        int
	StateABI             string
	GuestISA             string
	LogicalPageSize      uint64
	CPUFeatures          []string
	Resources            ResourceProfile
	RequiredCapabilities []string
}

// NewPortableContract constructs a canonical contract.
func NewPortableContract(stateABI, guestISA string, cpuFeatures []string, resources ResourceProfile, requiredCapabilities []string) PortableContract {
	return PortableContract{
		SchemaVersion:        PortableSchemaVersion,
		StateABI:             stateABI,
		GuestISA:             guestISA,
		LogicalPageSize:      PortableLogicalPageSize,
		CPUFeatures:          canonicalStrings(cpuFeatures),
		Resources:            resources,
		RequiredCapabilities: canonicalStrings(requiredCapabilities),
	}
}

// IsCrossHostPortable reports whether resources in c are reconstructible on a
// different host.
func (c PortableContract) IsCrossHostPortable() bool {
	return c.Resources == ResourceProfileTTCManaged
}

// AddToMetadata writes c to statefile metadata.
func (c PortableContract) AddToMetadata(metadata map[string]string) error {
	if metadata == nil {
		return fmt.Errorf("nil checkpoint metadata")
	}
	if err := c.validateShape(); err != nil {
		return err
	}
	metadata[metadataSchema] = strconv.Itoa(c.SchemaVersion)
	metadata[metadataStateABI] = c.StateABI
	metadata[metadataGuestISA] = c.GuestISA
	metadata[metadataPageSize] = strconv.FormatUint(c.LogicalPageSize, 10)
	metadata[metadataCPUFeatures] = strings.Join(canonicalStrings(c.CPUFeatures), ",")
	metadata[metadataResources] = string(c.Resources)
	metadata[metadataCapabilities] = strings.Join(canonicalStrings(c.RequiredCapabilities), ",")
	return nil
}

// PortableContractFromMetadata parses and validates the portable checkpoint
// envelope. Legacy checkpoints fail here by design.
func PortableContractFromMetadata(metadata map[string]string) (PortableContract, error) {
	get := func(key string) (string, error) {
		value, ok := metadata[key]
		if !ok {
			return "", fmt.Errorf("portable checkpoint metadata %q is missing", key)
		}
		return value, nil
	}
	schemaString, err := get(metadataSchema)
	if err != nil {
		return PortableContract{}, err
	}
	schema, err := strconv.Atoi(schemaString)
	if err != nil {
		return PortableContract{}, fmt.Errorf("invalid portable checkpoint schema %q: %w", schemaString, err)
	}
	stateABI, err := get(metadataStateABI)
	if err != nil {
		return PortableContract{}, err
	}
	guestISA, err := get(metadataGuestISA)
	if err != nil {
		return PortableContract{}, err
	}
	pageSizeString, err := get(metadataPageSize)
	if err != nil {
		return PortableContract{}, err
	}
	pageSize, err := strconv.ParseUint(pageSizeString, 10, 64)
	if err != nil {
		return PortableContract{}, fmt.Errorf("invalid portable checkpoint page size %q: %w", pageSizeString, err)
	}
	cpuFeatures, err := get(metadataCPUFeatures)
	if err != nil {
		return PortableContract{}, err
	}
	resources, err := get(metadataResources)
	if err != nil {
		return PortableContract{}, err
	}
	capabilities, err := get(metadataCapabilities)
	if err != nil {
		return PortableContract{}, err
	}
	c := PortableContract{
		SchemaVersion:        schema,
		StateABI:             stateABI,
		GuestISA:             guestISA,
		LogicalPageSize:      pageSize,
		CPUFeatures:          splitCanonical(cpuFeatures),
		Resources:            ResourceProfile(resources),
		RequiredCapabilities: splitCanonical(capabilities),
	}
	if err := c.validateShape(); err != nil {
		return PortableContract{}, err
	}
	return c, nil
}

// ValidateDestination checks c against a fresh execution environment.
func (c PortableContract) ValidateDestination(stateABI, guestISA string, cpuFeatures, capabilities []string) error {
	if err := c.validateShape(); err != nil {
		return err
	}
	if c.StateABI != stateABI {
		return fmt.Errorf("checkpoint state ABI %q is incompatible with destination ABI %q", c.StateABI, stateABI)
	}
	if c.GuestISA != guestISA {
		return fmt.Errorf("checkpoint guest ISA %q is incompatible with destination ISA %q", c.GuestISA, guestISA)
	}
	if missing := missingStrings(c.CPUFeatures, cpuFeatures); len(missing) != 0 {
		return fmt.Errorf("destination CPU is missing checkpoint features: %s", strings.Join(missing, ", "))
	}
	if missing := missingStrings(c.RequiredCapabilities, capabilities); len(missing) != 0 {
		return fmt.Errorf("destination sentry is missing checkpoint capabilities: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c PortableContract) validateShape() error {
	if c.SchemaVersion != PortableSchemaVersion {
		return fmt.Errorf("unsupported portable checkpoint schema %d, want %d", c.SchemaVersion, PortableSchemaVersion)
	}
	if c.StateABI == "" {
		return fmt.Errorf("portable checkpoint state ABI is empty")
	}
	if c.GuestISA == "" {
		return fmt.Errorf("portable checkpoint guest ISA is empty")
	}
	if c.LogicalPageSize != PortableLogicalPageSize {
		return fmt.Errorf("portable checkpoint logical page size is %d, want %d", c.LogicalPageSize, PortableLogicalPageSize)
	}
	switch c.Resources {
	case ResourceProfileTTCManaged, ResourceProfileHostBound:
		return nil
	default:
		return fmt.Errorf("unsupported checkpoint resource profile %q", c.Resources)
	}
}

func splitCanonical(value string) []string {
	if value == "" {
		return nil
	}
	return canonicalStrings(strings.Split(value, ","))
}

func canonicalStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func missingStrings(required, available []string) []string {
	have := make(map[string]struct{}, len(available))
	for _, value := range canonicalStrings(available) {
		have[value] = struct{}{}
	}
	var missing []string
	for _, value := range canonicalStrings(required) {
		if _, ok := have[value]; !ok {
			missing = append(missing, value)
		}
	}
	return missing
}
