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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

func (r *Repository) emptyTree() (string, error) {
	return r.putTreeNode(treeNode{})
}

func (r *Repository) getTreeNode(id string) (treeNode, error) {
	if len(id) != sha256.Size*2 {
		return treeNode{}, fmt.Errorf("invalid metadata node ID %q", id)
	}
	data, err := r.readFile(filepath.Join("nodes", id))
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
	}
	return node, nil
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
	path := filepath.Join("nodes", id)
	if file, err := r.openFile(path, 0, 0); err == nil {
		file.Close()
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := r.atomicWriteFile(path, data, 0600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return id, nil
		}
		return "", fmt.Errorf("publishing metadata node %q: %w", id, err)
	}
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
