// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stack

import (
	"context"
	"fmt"
	"sort"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// savedStackConfig is emitted only for TTC branch snapshots. NIC link
// endpoints are part of the state graph, so cross-namespace objects such as a
// veth pair retain their identity and are cloned rather than borrowed.
//
// +stateify savable
type savedStackConfig struct {
	routes                []tcpip.Route
	nics                  map[tcpip.NICID]*nic
	loopback              *nic
	nextNICID             int32
	tables                *IPTables
	addresses             map[tcpip.NICID][]tcpip.ProtocolAddress
	allowLiveTCPMigration bool
}

// SetLiveForkSaveConfig controls inclusion of runtime network configuration in
// the state graph. The owning Kernel must be paused and the stack quiesced.
func (s *Stack) SetLiveForkSaveConfig(enabled bool) {
	s.mu.Lock()
	wasEnabled := s.liveForkSaveConfig
	if enabled && !wasEnabled {
		s.liveForkPreviousTCPMigration = s.allowLiveTCPMigration
		s.allowLiveTCPMigration = true
	} else if !enabled && wasEnabled {
		s.allowLiveTCPMigration = s.liveForkPreviousTCPMigration
	}
	s.liveForkSaveConfig = enabled
	tables := s.tables
	s.mu.Unlock()
	if wasEnabled && !enabled && tables != nil {
		tables.resumeAfterLiveForkSave()
	}
}

// HasLiveForkRestoredConfig reports whether this stack was reconstructed from
// a TTC state stream containing guest-managed network configuration.
func (s *Stack) HasLiveForkRestoredConfig() bool {
	s.mu.RLock()
	restored := s.liveForkConfigRestored
	s.mu.RUnlock()
	return restored
}

func (s *Stack) saveSavedConfig() *savedStackConfig {
	s.mu.RLock()
	if !s.liveForkSaveConfig {
		s.mu.RUnlock()
		return nil
	}
	nics := make(map[tcpip.NICID]*nic, len(s.nics))
	for id, nic := range s.nics {
		nics[id] = nic
	}
	config := &savedStackConfig{
		nics:                  nics,
		loopback:              s.loopbackNIC,
		nextNICID:             s.nicIDGen.Load(),
		tables:                s.tables,
		allowLiveTCPMigration: s.liveForkPreviousTCPMigration,
	}
	s.mu.RUnlock()
	config.routes = s.GetRouteTable()
	// AddressableEndpointState intentionally omits addresses from ordinary
	// gVisor checkpoint state because runsc normally recreates them from host
	// network configuration. TTC instead clones the Sentry-managed graph.
	config.addresses = s.AllAddresses()
	return config
}

func (s *Stack) loadSavedConfig(_ context.Context, config *savedStackConfig) {
	if config == nil {
		return
	}
	s.mu.Lock()
	s.liveForkPendingConfig = config
	s.liveForkConfigRestored = true
	s.mu.Unlock()
}

// PrepareLiveForkRestore reconstructs configuration that ordinary runsc
// checkpoint restore expects to receive from the host. It must run after the
// whole state graph has decoded (so veth peers and link endpoints are ready),
// but before transport endpoints are restored.
func (s *Stack) PrepareLiveForkRestore() error {
	s.mu.Lock()
	config := s.liveForkPendingConfig
	if config == nil {
		s.mu.Unlock()
		return nil
	}
	s.liveForkPendingConfig = nil
	s.nics = make(map[tcpip.NICID]*nic)
	s.loopbackNIC = nil
	s.tables = config.tables
	s.allowLiveTCPMigration = true
	s.liveForkRestoreTCPMigration = config.allowLiveTCPMigration
	s.mu.Unlock()

	ids := make([]int, 0, len(config.nics))
	for id := range config.nics {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, rawID := range ids {
		id := tcpip.NICID(rawID)
		oldNIC := config.nics[id]
		linkEP, ok := oldNIC.NetworkLinkEndpoint.(LinkEndpoint)
		if !ok {
			return fmt.Errorf("restoring TTC NIC %d: link endpoint %T is not recreatable", id, oldNIC.NetworkLinkEndpoint)
		}
		opts := NICOptions{
			Name:                     oldNIC.name,
			Disabled:                 !oldNIC.enabled.Load(),
			Context:                  oldNIC.context,
			DeliverLinkPackets:       oldNIC.deliverLinkPackets,
			EnableExperimentIPOption: oldNIC.experimentIPOptionEnabled,
			Kind:                     oldNIC.kind,
		}
		if err := s.CreateNICWithOptions(id, linkEP, opts); err != nil {
			return fmt.Errorf("restoring TTC NIC %d: %v", id, err)
		}
		newNIC, err := s.GetNICByID(id)
		if err != nil {
			return fmt.Errorf("looking up restored TTC NIC %d: %v", id, err)
		}
		oldNIC.stats.local.Clone(&newNIC.stats.local)
		newNIC.spoofing.Store(oldNIC.spoofing.Load())
		newNIC.promiscuous.Store(oldNIC.promiscuous.Load())
		for proto, oldEP := range oldNIC.networkEndpoints {
			if forwardingEP, ok := oldEP.(ForwardingNetworkEndpoint); ok {
				if _, err := newNIC.setForwarding(proto, forwardingEP.Forwarding()); err != nil {
					return fmt.Errorf("restoring TTC NIC %d forwarding for protocol %d: %v", id, proto, err)
				}
			}
			if multicastEP, ok := oldEP.(MulticastForwardingNetworkEndpoint); ok {
				if _, err := newNIC.setMulticastForwarding(proto, multicastEP.MulticastForwarding()); err != nil {
					return fmt.Errorf("restoring TTC NIC %d multicast forwarding for protocol %d: %v", id, proto, err)
				}
			}
		}
	}
	existingAddresses := s.AllAddresses()
	for nicID, addresses := range config.addresses {
		for _, address := range addresses {
			alreadyPresent := false
			for _, existing := range existingAddresses[nicID] {
				if existing == address {
					alreadyPresent = true
					break
				}
			}
			if alreadyPresent {
				continue
			}
			if err := s.AddProtocolAddress(nicID, address, AddressProperties{}); err != nil {
				return fmt.Errorf("restoring TTC address %+v on NIC %d: %v", address, nicID, err)
			}
			existingAddresses[nicID] = append(existingAddresses[nicID], address)
		}
	}
	s.SetRouteTable(config.routes)
	s.nicIDGen.Store(config.nextNICID)
	// The saved NICs, which those made here replace, own their addresses
	// still.
	for _, oldNIC := range config.nics {
		for _, ep := range oldNIC.networkEndpoints {
			if replaced, ok := ep.(ReplacedNetworkEndpoint); ok {
				replaced.ReleaseAddresses()
			}
		}
	}
	return nil
}

// ReplacedNetworkEndpoint is a network endpoint of a saved NIC that
// PrepareLiveForkRestore replaces with a NIC of its own.
type ReplacedNetworkEndpoint interface {
	// ReleaseAddresses gives up the references that the endpoint holds on
	// its addresses, as their owner, and changes nothing that its
	// replacement shares with it: the NIC's link endpoint, and the protocol.
	ReleaseAddresses()
}
