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
	"encoding/json"
	"sort"
	"unicode/utf8"
)

// Linux file names, symlink targets, and xattr names are arbitrary bytes, but
// encoding/json replaces invalid UTF-8 in strings with U+FFFD. Filesystem
// records that contain such strings therefore carry them as base64 byte
// fields instead. Records whose strings are valid UTF-8 keep the original
// encoding byte for byte. Readers that predate the byte fields see an empty
// name and reject the entry rather than use a corrupted one.

type plainDirectoryEntryRecord DirectoryEntryRecord

type directoryEntryRecordJSON struct {
	plainDirectoryEntryRecord
	NameBytes []byte `json:",omitempty"`
}

// MarshalJSON implements json.Marshaler.
func (r DirectoryEntryRecord) MarshalJSON() ([]byte, error) {
	encoded := directoryEntryRecordJSON{plainDirectoryEntryRecord: plainDirectoryEntryRecord(r)}
	if !utf8.ValidString(r.Name) {
		encoded.Name = ""
		encoded.NameBytes = []byte(r.Name)
	}
	return json.Marshal(encoded)
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *DirectoryEntryRecord) UnmarshalJSON(data []byte) error {
	var decoded directoryEntryRecordJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = DirectoryEntryRecord(decoded.plainDirectoryEntryRecord)
	if decoded.NameBytes != nil {
		r.Name = string(decoded.NameBytes)
	}
	return nil
}

type plainInodeRecord InodeRecord

type xattrRecord struct {
	Name  []byte
	Value []byte
}

type inodeRecordJSON struct {
	plainInodeRecord
	TargetBytes []byte        `json:",omitempty"`
	XattrList   []xattrRecord `json:",omitempty"`
}

// MarshalJSON implements json.Marshaler.
func (r InodeRecord) MarshalJSON() ([]byte, error) {
	encoded := inodeRecordJSON{plainInodeRecord: plainInodeRecord(r)}
	if !utf8.ValidString(r.Target) {
		encoded.Target = ""
		encoded.TargetBytes = []byte(r.Target)
	}
	for name := range r.Xattrs {
		if utf8.ValidString(name) {
			continue
		}
		names := make([]string, 0, len(r.Xattrs))
		for name := range r.Xattrs {
			names = append(names, name)
		}
		sort.Strings(names)
		encoded.Xattrs = nil
		encoded.XattrList = make([]xattrRecord, 0, len(names))
		for _, name := range names {
			encoded.XattrList = append(encoded.XattrList, xattrRecord{Name: []byte(name), Value: r.Xattrs[name]})
		}
		break
	}
	return json.Marshal(encoded)
}

// decodeInodeRecord decodes an inode object's record. json.Unmarshal would
// scan the record twice before this type's UnmarshalJSON scans it twice
// again: a third of the time of walking a lazily imported tree.
func decodeInodeRecord(data []byte) (InodeRecord, error) {
	var inode InodeRecord
	err := inode.UnmarshalJSON(data)
	return inode, err
}

// decodeDirectoryEntryRecord is decodeInodeRecord for a directory entry.
func decodeDirectoryEntryRecord(data []byte) (DirectoryEntryRecord, error) {
	var entry DirectoryEntryRecord
	err := entry.UnmarshalJSON(data)
	return entry, err
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *InodeRecord) UnmarshalJSON(data []byte) error {
	var decoded inodeRecordJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = InodeRecord(decoded.plainInodeRecord)
	if decoded.TargetBytes != nil {
		r.Target = string(decoded.TargetBytes)
	}
	if decoded.XattrList != nil {
		r.Xattrs = make(map[string][]byte, len(decoded.XattrList))
		for _, xattr := range decoded.XattrList {
			r.Xattrs[string(xattr.Name)] = xattr.Value
		}
	}
	return nil
}
