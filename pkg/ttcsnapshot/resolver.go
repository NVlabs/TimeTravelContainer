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

package ttcsnapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
)

// OriginLocator returns the exact volatile provider for a snapshot origin.
// found=false means that shared storage is authoritative.
type OriginLocator interface {
	Provider(context.Context, SnapshotID) (node string, found bool, err error)
}

// ObjectSource opens one complete immutable object.
type ObjectSource interface {
	Fetch(context.Context, ObjectID) (io.ReadCloser, uint64, error)
}

// PeerSource selects a fenced provider client for node.
type PeerSource interface {
	Peer(context.Context, string) (ObjectSource, error)
}

type resolveCall struct {
	done chan struct{}
	err  error
}

// Resolver performs ancestry-independent object resolution and coalesces all
// concurrent local misses for the same logical object.
type Resolver struct {
	graph   *Store
	local   *LocalDataStore
	origins OriginLocator
	peers   PeerSource
	shared  ObjectSource

	mu       sync.Mutex
	inflight map[string]*resolveCall
}

// NewResolver creates a distributed complete-object resolver.
func NewResolver(graph *Store, local *LocalDataStore, origins OriginLocator, peers PeerSource, shared ObjectSource) (*Resolver, error) {
	if graph == nil || local == nil || origins == nil || peers == nil || shared == nil {
		return nil, errors.New("TTC resolver dependencies must not be nil")
	}
	return &Resolver{graph: graph, local: local, origins: origins, peers: peers, shared: shared, inflight: make(map[string]*resolveCall)}, nil
}

func objectLength(object Object) (int64, error) {
	if object.BulkLength > math.MaxInt64 {
		return 0, fmt.Errorf("object %s is too large", object.ID.key())
	}
	return int64(object.BulkLength), nil
}

// Resolve returns a local complete object. It never follows a branch or
// snapshot parent: id.Snapshot alone selects the authority.
func (resolver *Resolver) Resolve(ctx context.Context, id ObjectID) (*os.File, error) {
	object, ok := resolver.graph.Object(id)
	if !ok || object.State != ObjectLive {
		return nil, os.ErrNotExist
	}
	return resolver.resolveObject(ctx, object)
}

// ResolveKnown resolves an object described by a structurally validated
// immutable catalog. It avoids importing cache metadata into the authoritative
// local reference-count graph.
func (resolver *Resolver) ResolveKnown(ctx context.Context, object Object) (*os.File, error) {
	if err := validateObject(object); err != nil {
		return nil, err
	}
	if object.State != ObjectLive {
		return nil, os.ErrNotExist
	}
	return resolver.resolveObject(ctx, object)
}

func (resolver *Resolver) resolveObject(ctx context.Context, object Object) (*os.File, error) {
	id := object.ID
	length, err := objectLength(object)
	if err != nil {
		return nil, err
	}
	if file, err := resolver.local.Open(id, length); err == nil {
		return file, nil
	}

	key := id.key()
	resolver.mu.Lock()
	if call := resolver.inflight[key]; call != nil {
		resolver.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return nil, call.err
			}
			return resolver.local.Open(id, length)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &resolveCall{done: make(chan struct{})}
	resolver.inflight[key] = call
	resolver.mu.Unlock()

	// Once admitted, finish installation independently of one waiter being
	// canceled. Later readers can safely reuse the complete cached object.
	call.err = resolver.fetch(context.Background(), object)
	resolver.mu.Lock()
	delete(resolver.inflight, key)
	close(call.done)
	resolver.mu.Unlock()
	if call.err != nil {
		return nil, call.err
	}
	return resolver.local.Open(id, length)
}

func (resolver *Resolver) fetch(ctx context.Context, object Object) error {
	node, volatile, err := resolver.origins.Provider(ctx, object.ID.Snapshot)
	if err != nil {
		return err
	}
	source := resolver.shared
	if volatile {
		source, err = resolver.peers.Peer(ctx, node)
		if err != nil {
			return err
		}
	}
	reader, length, err := source.Fetch(ctx, object.ID)
	if err != nil {
		return err
	}
	defer reader.Close()
	if length != object.BulkLength {
		return fmt.Errorf("object %s source length is %d, want %d", object.ID.key(), length, object.BulkLength)
	}
	return resolver.local.Install(object.ID, reader, int64(length))
}
