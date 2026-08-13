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

package ttcchunk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

var consumptionMagic = [8]byte{'T', 'T', 'C', 'M', 'C', 'O', 'N', 1}

type consumptionRecord struct {
	Manifest ManifestID
	Objects  map[ID]struct{}
}

func encodeConsumption(record consumptionRecord) ([]byte, error) {
	objects := make([]ID, 0, len(record.Objects))
	for object := range record.Objects {
		if object == (ID{}) {
			return nil, errors.New("TTC consumption record contains a zero object")
		}
		objects = append(objects, object)
	}
	sort.Slice(objects, func(i, j int) bool { return bytes.Compare(objects[i][:], objects[j][:]) < 0 })
	data := append([]byte(nil), consumptionMagic[:]...)
	data = append(data, record.Manifest[:]...)
	var count [4]byte
	binary.LittleEndian.PutUint32(count[:], uint32(len(objects)))
	data = append(data, count[:]...)
	for _, object := range objects {
		data = append(data, object[:]...)
	}
	checksum := sha256.Sum256(data)
	return append(data, checksum[:]...), nil
}

func decodeConsumption(data []byte) (consumptionRecord, error) {
	var record consumptionRecord
	header := len(consumptionMagic) + len(record.Manifest) + 4
	if len(data) < header+sha256.Size || !bytes.Equal(data[:len(consumptionMagic)], consumptionMagic[:]) {
		return record, errors.New("invalid TTC memory consumption record")
	}
	count := binary.LittleEndian.Uint32(data[len(consumptionMagic)+len(record.Manifest) : header])
	want := header + int(count)*sha256.Size + sha256.Size
	if len(data) != want {
		return record, errors.New("invalid TTC memory consumption record length")
	}
	checksum := sha256.Sum256(data[:want-sha256.Size])
	if !bytes.Equal(data[want-sha256.Size:], checksum[:]) {
		return record, errors.New("TTC memory consumption record checksum mismatch")
	}
	copy(record.Manifest[:], data[len(consumptionMagic):len(consumptionMagic)+len(record.Manifest)])
	record.Objects = make(map[ID]struct{}, count)
	offset := header
	var previous ID
	for i := uint32(0); i < count; i++ {
		var object ID
		copy(object[:], data[offset:offset+sha256.Size])
		if object == (ID{}) || i != 0 && bytes.Compare(previous[:], object[:]) >= 0 {
			return consumptionRecord{}, errors.New("invalid TTC memory consumption object order")
		}
		record.Objects[object] = struct{}{}
		previous = object
		offset += sha256.Size
	}
	return record, nil
}

func (r *MemoryRepository) readConsumption(branch string) (consumptionRecord, error) {
	var data []byte
	var err error
	if r.rootFD != nil {
		path, pathErr := r.consumedRelativePath(branch)
		if pathErr != nil {
			return consumptionRecord{}, pathErr
		}
		data, err = readFileRelative(int(r.rootFD.Fd()), path)
	} else {
		path, pathErr := r.consumedPath(branch)
		if pathErr != nil {
			return consumptionRecord{}, pathErr
		}
		data, err = os.ReadFile(path)
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return consumptionRecord{Objects: make(map[ID]struct{})}, nil
	}
	if err != nil {
		return consumptionRecord{}, err
	}
	return decodeConsumption(data)
}

func (r *MemoryRepository) consumedRelativePath(branch string) (string, error) {
	path, err := r.consumedPath(branch)
	if err != nil {
		return "", err
	}
	return "consumed/" + filepath.Base(path), nil
}

// MarkConsumed durably records immutable objects that branch no longer needs
// as backing because their complete 1 MiB residency units have been installed.
// It is monotonic and idempotent. The caller must invoke it only after the
// corresponding page installation has completed successfully.
func (r *MemoryRepository) MarkConsumed(branch string, manifest ManifestID, objects []ID) error {
	if r == nil || branch == "" || manifest == (ManifestID{}) || len(objects) == 0 {
		return errors.New("invalid TTC memory consumption update")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	root, err := r.Branch(branch)
	if err != nil {
		return fmt.Errorf("reading consumption branch %q: %w", branch, err)
	}
	if root != manifest {
		return fmt.Errorf("consumption branch %q references manifest %s, want %s", branch, root, manifest)
	}
	loaded, err := r.LoadManifest(manifest)
	if err != nil {
		return err
	}
	valid := make(map[ID]struct{}, len(loaded.Runs))
	for _, run := range loaded.Runs {
		valid[run.Object] = struct{}{}
	}
	record, err := r.readConsumption(branch)
	if err != nil {
		return err
	}
	if record.Manifest != (ManifestID{}) && record.Manifest != manifest {
		return fmt.Errorf("consumption branch %q has manifest %s, want %s", branch, record.Manifest, manifest)
	}
	record.Manifest = manifest
	for _, object := range objects {
		if _, ok := valid[object]; !ok {
			return fmt.Errorf("consumption object %s is not in manifest %s", object, manifest)
		}
		record.Objects[object] = struct{}{}
	}
	data, err := encodeConsumption(record)
	if err != nil {
		return err
	}
	if r.rootFD != nil {
		path, err := r.consumedRelativePath(branch)
		if err != nil {
			return err
		}
		return atomicReplaceRelative(int(r.rootFD.Fd()), "consumed", filepath.Base(path), data)
	}
	path, err := r.consumedPath(branch)
	if err != nil {
		return err
	}
	return atomicReplace(r.consumedRoot, ".consume-", path, data)
}
