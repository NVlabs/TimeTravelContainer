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

package ttcvolume

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// treeNode is one immutable node in a byte-wise persistent radix trie. A
// branch mutation writes at most len(key)+1 new nodes, and a fork writes none.
type treeNode struct {
	Value    json.RawMessage   `json:"value,omitempty"`
	Children map[string]string `json:"children,omitempty"`
}

const maxCachedTreeNodes = 8192

const (
	treeNodePackName       = "nodes.pack"
	treeNodePackHeaderSize = 4 + sha256.Size + 4
	maxPackedTreeNodeSize  = 16 << 20
)

var treeNodePackMagic = [4]byte{'T', 'T', 'C', 'N'}

type packedTreeNode struct {
	offset int64
	length uint32
}

func (r *Repository) openTreeNodePackLocked() error {
	if r.pack != nil {
		return nil
	}
	pack, err := r.openFile(treeNodePackName, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	r.pack = pack
	if r.packIndex == nil {
		r.packIndex = make(map[string]packedTreeNode)
	}
	return nil
}

// scanTreeNodePackLocked indexes complete records appended by this or another
// repository client. An incomplete tail is ignored: it was not made reachable
// by a subsequently published branch root and a later complete record can only
// be appended while the repository flock excludes this scanner.
func (r *Repository) scanTreeNodePackLocked() error {
	if err := r.openTreeNodePackLocked(); err != nil {
		return err
	}
	info, err := r.pack.Stat()
	if err != nil {
		return err
	}
	for r.packRead+treeNodePackHeaderSize <= info.Size() {
		var header [treeNodePackHeaderSize]byte
		if _, err := r.pack.ReadAt(header[:], r.packRead); err != nil {
			return err
		}
		if !bytes.Equal(header[:4], treeNodePackMagic[:]) {
			return fmt.Errorf("invalid tree-node pack record at offset %d", r.packRead)
		}
		length := binary.LittleEndian.Uint32(header[4+sha256.Size:])
		if length == 0 || length > maxPackedTreeNodeSize {
			return fmt.Errorf("invalid tree-node pack record length %d at offset %d", length, r.packRead)
		}
		next := r.packRead + treeNodePackHeaderSize + int64(length)
		if next > info.Size() {
			break
		}
		id := hex.EncodeToString(header[4 : 4+sha256.Size])
		r.packIndex[id] = packedTreeNode{offset: r.packRead + treeNodePackHeaderSize, length: length}
		r.packRead = next
	}
	return nil
}

func (r *Repository) appendPackedTreeNode(id string, data []byte) error {
	rawID, err := hex.DecodeString(id)
	if err != nil || len(rawID) != sha256.Size {
		return fmt.Errorf("invalid metadata node ID %q", id)
	}
	if len(data) == 0 || len(data) > maxPackedTreeNodeSize {
		return fmt.Errorf("invalid packed metadata node size %d", len(data))
	}
	r.packMu.Lock()
	defer r.packMu.Unlock()
	if err := r.scanTreeNodePackLocked(); err != nil {
		return err
	}
	if _, ok := r.packIndex[id]; ok {
		return nil
	}
	info, err := r.pack.Stat()
	if err != nil {
		return err
	}
	if info.Size() != r.packRead {
		// Discard an incomplete record left by an interrupted, non-durable
		// append before adding another record after it.
		if err := r.pack.Truncate(r.packRead); err != nil {
			return err
		}
	}
	offset, err := r.pack.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	record := make([]byte, treeNodePackHeaderSize+len(data))
	copy(record[:4], treeNodePackMagic[:])
	copy(record[4:4+sha256.Size], rawID)
	binary.LittleEndian.PutUint32(record[4+sha256.Size:treeNodePackHeaderSize], uint32(len(data)))
	copy(record[treeNodePackHeaderSize:], data)
	n, err := r.pack.Write(record)
	if err != nil {
		return err
	}
	if n != len(record) {
		return io.ErrShortWrite
	}
	r.packIndex[id] = packedTreeNode{offset: offset + treeNodePackHeaderSize, length: uint32(len(data))}
	r.packRead = offset + int64(len(record))
	return nil
}

func (r *Repository) readPackedTreeNode(id string) ([]byte, bool, error) {
	r.packMu.Lock()
	defer r.packMu.Unlock()
	if err := r.scanTreeNodePackLocked(); err != nil {
		return nil, false, err
	}
	record, ok := r.packIndex[id]
	if !ok {
		return nil, false, nil
	}
	data := make([]byte, record.length)
	if _, err := r.pack.ReadAt(data, record.offset); err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (r *Repository) syncTreeNodePack() error {
	r.packMu.Lock()
	defer r.packMu.Unlock()
	if r.pack == nil {
		return nil
	}
	return r.pack.Sync()
}

func cloneTreeNode(node treeNode) treeNode {
	cloned := treeNode{Value: append(json.RawMessage(nil), node.Value...)}
	if len(node.Children) != 0 {
		cloned.Children = make(map[string]string, len(node.Children))
		for edge, child := range node.Children {
			cloned.Children[edge] = child
		}
	}
	return cloned
}

func (r *Repository) cachedTreeNode(id string) (treeNode, bool) {
	r.nodeMu.Lock()
	defer r.nodeMu.Unlock()
	node, ok := r.nodeCache[id]
	return cloneTreeNode(node), ok
}

func (r *Repository) cacheTreeNode(id string, node treeNode) {
	r.nodeMu.Lock()
	defer r.nodeMu.Unlock()
	if r.nodeCache == nil {
		r.nodeCache = make(map[string]treeNode)
	}
	if _, ok := r.nodeCache[id]; ok {
		return
	}
	if len(r.nodeOrder) < maxCachedTreeNodes {
		r.nodeOrder = append(r.nodeOrder, id)
	} else {
		delete(r.nodeCache, r.nodeOrder[r.nodeNext])
		r.nodeOrder[r.nodeNext] = id
		r.nodeNext = (r.nodeNext + 1) % maxCachedTreeNodes
	}
	r.nodeCache[id] = cloneTreeNode(node)
}

func (r *Repository) emptyTree() (string, error) {
	return r.putTreeNode(treeNode{})
}

func (r *Repository) getTreeNode(id string) (treeNode, error) {
	if len(id) != sha256.Size*2 {
		return treeNode{}, fmt.Errorf("invalid metadata node ID %q", id)
	}
	if _, err := hex.DecodeString(id); err != nil {
		return treeNode{}, fmt.Errorf("invalid metadata node ID %q", id)
	}
	if node, ok := r.cachedTreeNode(id); ok {
		return node, nil
	}
	data, err := r.treeNodeData(id)
	if err != nil {
		return treeNode{}, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != id {
		return treeNode{}, fmt.Errorf("metadata node %q failed checksum validation", id)
	}
	var node treeNode
	if err := json.Unmarshal(data, &node); err != nil {
		return treeNode{}, fmt.Errorf("decoding metadata node %q: %w", id, err)
	}
	for edge, child := range node.Children {
		raw, err := hex.DecodeString(edge)
		if err != nil || len(raw) != 1 {
			return treeNode{}, fmt.Errorf("metadata node %q has invalid edge %q", id, edge)
		}
		if len(child) != sha256.Size*2 {
			return treeNode{}, fmt.Errorf("metadata node %q has invalid child %q", id, child)
		}
		if _, err := hex.DecodeString(child); err != nil {
			return treeNode{}, fmt.Errorf("metadata node %q has invalid child %q", id, child)
		}
	}
	r.cacheTreeNode(id, node)
	return node, nil
}

func (r *Repository) treeNodeData(id string) ([]byte, error) {
	data, err := r.readFile(filepath.Join("nodes", id))
	if errors.Is(err, os.ErrNotExist) {
		var ok bool
		data, ok, err = r.readPackedTreeNode(id)
		if err == nil && !ok {
			return nil, os.ErrNotExist
		}
	}
	return data, err
}

func (r *Repository) putTreeNode(node treeNode) (string, error) {
	if len(node.Children) == 0 {
		node.Children = nil
	}
	data, err := json.Marshal(node)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])
	if _, ok := r.cachedTreeNode(id); ok {
		return id, nil
	}
	path := filepath.Join("nodes", id)
	if file, err := r.openFile(path, 0, 0); err == nil {
		file.Close()
		r.cacheTreeNode(id, node)
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// nodes.pack is the durability authority for immutable trie nodes. Loose
	// files are retained only as a backward-compatible read path for existing
	// repositories and imported snapshots.
	if err := r.appendPackedTreeNode(id, data); err != nil {
		return "", fmt.Errorf("packing metadata node %q: %w", id, err)
	}
	r.cacheTreeNode(id, node)
	return id, nil
}

func edgeName(b byte) string {
	var raw [1]byte
	raw[0] = b
	return hex.EncodeToString(raw[:])
}

func (r *Repository) treeGet(root string, key []byte) (json.RawMessage, bool, error) {
	id := root
	for _, b := range key {
		node, err := r.getTreeNode(id)
		if err != nil {
			return nil, false, err
		}
		child, ok := node.Children[edgeName(b)]
		if !ok {
			return nil, false, nil
		}
		id = child
	}
	node, err := r.getTreeNode(id)
	if err != nil {
		return nil, false, err
	}
	if len(node.Value) == 0 {
		return nil, false, nil
	}
	return append(json.RawMessage(nil), node.Value...), true, nil
}

func (r *Repository) treeSet(root string, key []byte, value json.RawMessage) (string, error) {
	if len(key) == 0 {
		return "", errors.New("metadata key must not be empty")
	}
	return r.treeSetAt(root, key, 0, value)
}

func (r *Repository) treeSetAt(id string, key []byte, index int, value json.RawMessage) (string, error) {
	node, err := r.getTreeNode(id)
	if err != nil {
		return "", err
	}
	if index == len(key) {
		node.Value = append(json.RawMessage(nil), value...)
		return r.putTreeNode(node)
	}
	if node.Children == nil {
		node.Children = make(map[string]string)
	}
	edge := edgeName(key[index])
	child := node.Children[edge]
	if child == "" {
		child, err = r.emptyTree()
		if err != nil {
			return "", err
		}
	}
	child, err = r.treeSetAt(child, key, index+1, value)
	if err != nil {
		return "", err
	}
	node.Children[edge] = child
	return r.putTreeNode(node)
}

func (r *Repository) treeDelete(root string, key []byte) (string, error) {
	if len(key) == 0 {
		return "", errors.New("metadata key must not be empty")
	}
	id, _, err := r.treeDeleteAt(root, key, 0)
	return id, err
}

func (r *Repository) treeDeleteAt(id string, key []byte, index int) (string, bool, error) {
	node, err := r.getTreeNode(id)
	if err != nil {
		return "", false, err
	}
	if index == len(key) {
		if len(node.Value) == 0 {
			return id, len(node.Children) == 0, nil
		}
		node.Value = nil
	} else {
		edge := edgeName(key[index])
		child := node.Children[edge]
		if child == "" {
			return id, false, nil
		}
		child, empty, err := r.treeDeleteAt(child, key, index+1)
		if err != nil {
			return "", false, err
		}
		if empty {
			delete(node.Children, edge)
		} else {
			node.Children[edge] = child
		}
	}
	newID, err := r.putTreeNode(node)
	return newID, len(node.Value) == 0 && len(node.Children) == 0, err
}

func (r *Repository) treePrefix(root string, prefix []byte) ([]json.RawMessage, error) {
	id := root
	for _, b := range prefix {
		node, err := r.getTreeNode(id)
		if err != nil {
			return nil, err
		}
		child := node.Children[edgeName(b)]
		if child == "" {
			return nil, nil
		}
		id = child
	}
	var values []json.RawMessage
	var walk func(string) error
	walk = func(id string) error {
		node, err := r.getTreeNode(id)
		if err != nil {
			return err
		}
		if len(node.Value) != 0 {
			values = append(values, append(json.RawMessage(nil), node.Value...))
		}
		edges := make([]string, 0, len(node.Children))
		for edge := range node.Children {
			edges = append(edges, edge)
		}
		sort.Strings(edges)
		for _, edge := range edges {
			if err := walk(node.Children[edge]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(id); err != nil {
		return nil, err
	}
	return values, nil
}

func equalJSON(a, b json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
}
