// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"encoding/json"
	"errors"
	"strings"
)

// AuthorityQuery names the records of the authority state a read needs. A
// read copies only those, and a node that does not hold the authority is
// sent only those. Every read has the state's counters, its cluster policy
// and its shared-GC lease.
type AuthorityQuery struct {
	Nodes           Keys[string] `json:"nodes,omitzero"`
	RetiredNodes    bool         `json:"retired_nodes,omitempty"`
	Placements      Keys[string] `json:"placements,omitzero"`
	Transactions    Keys[string] `json:"transactions,omitzero"`
	Branches        Keys[string] `json:"branches,omitzero"`
	Publications    Keys[string] `json:"publications,omitzero"`
	VolatileOrigins Keys[uint64] `json:"volatile_origins,omitzero"`
	SharedOrigins   Keys[uint64] `json:"shared_origins,omitzero"`
	RetiredOrigins  Keys[uint64] `json:"retired_origins,omitzero"`
	// Family, if set, keeps of the placements, branches and publications
	// selected only those of this family.
	Family string `json:"family,omitempty"`
	// Owner, if set, keeps of the placements selected only those this node
	// owns.
	Owner string `json:"owner,omitempty"`
}

// Keys selects records of one kind: every one, or those with these keys.
type Keys[K comparable] struct {
	All  bool `json:"all,omitempty"`
	Keys []K  `json:"keys,omitempty"`
}

// All selects every record of a kind.
func All[K comparable]() Keys[K] { return Keys[K]{All: true} }

// Only selects the records of a kind with these keys.
func Only[K comparable](keys ...K) Keys[K] { return Keys[K]{Keys: keys} }

// WholeAuthority is the query of every record.
var WholeAuthority = AuthorityQuery{
	Nodes: All[string](), RetiredNodes: true, Placements: All[string](), Transactions: All[string](),
	Branches: All[string](), Publications: All[string](),
	VolatileOrigins: All[uint64](), SharedOrigins: All[uint64](), RetiredOrigins: All[uint64](),
}

// OriginsQuery is the query of what the authority says of the snapshot
// origins with these IDs: see AuthorityState.OriginDead.
func OriginsQuery(ids ...uint64) AuthorityQuery {
	return AuthorityQuery{VolatileOrigins: Only(ids...), SharedOrigins: Only(ids...), RetiredOrigins: Only(ids...)}
}

func selectRecords[K comparable, V any](records map[K]V, keys Keys[K]) map[K]V {
	if keys.All {
		return records
	}
	var selected map[K]V
	for _, key := range keys.Keys {
		if record, ok := records[key]; ok {
			if selected == nil {
				selected = make(map[K]V, len(keys.Keys))
			}
			selected[key] = record
		}
	}
	return selected
}

// ofFamily returns the records whose keys are family's, keyed as
// logicalBranchKey and publicationKey key them, or all of them if family is
// empty.
func ofFamily[V any](records map[string]V, family string) map[string]V {
	if family == "" {
		return records
	}
	prefix := family + "\x00"
	var selected map[string]V
	for key, record := range records {
		if strings.HasPrefix(key, prefix) {
			if selected == nil {
				selected = make(map[string]V)
			}
			selected[key] = record
		}
	}
	return selected
}

// ownedBy returns the placements node owns, or all of them if node is empty.
func ownedBy(placements map[string]PlacementRecord, node string) map[string]PlacementRecord {
	if node == "" {
		return placements
	}
	var owned map[string]PlacementRecord
	for key, placement := range placements {
		if placement.OwnerNode == node {
			if owned == nil {
				owned = make(map[string]PlacementRecord)
			}
			owned[key] = placement
		}
	}
	return owned
}

// view returns the records of s that query names. It shares them with s,
// which must not change until the view is encoded.
func (s *AuthorityState) view(query AuthorityQuery) *AuthorityState {
	view := *s
	view.Requests, view.RequestOrder = nil, nil
	view.Nodes = selectRecords(s.Nodes, query.Nodes)
	if !query.RetiredNodes {
		view.RetiredNodes = nil
	}
	view.Placements = ownedBy(ofFamily(selectRecords(s.Placements, query.Placements), query.Family), query.Owner)
	view.Transactions = selectRecords(s.Transactions, query.Transactions)
	view.Branches = ofFamily(selectRecords(s.Branches, query.Branches), query.Family)
	view.Publications = ofFamily(selectRecords(s.Publications, query.Publications), query.Family)
	view.VolatileOrigins = selectRecords(s.VolatileOrigins, query.VolatileOrigins)
	view.SharedOrigins = selectRecords(s.SharedOrigins, query.SharedOrigins)
	view.RetiredOrigins = selectRecords(s.RetiredOrigins, query.RetiredOrigins)
	return &view
}

// decodeAuthorityView decodes a view the authority encoded, which it
// validated as it applied it.
func decodeAuthorityView(data []byte) (*AuthorityState, error) {
	var state AuthorityState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Version != AuthorityFormatVersion {
		return nil, errors.New("invalid authority state view")
	}
	state.initMaps()
	return &state, nil
}
