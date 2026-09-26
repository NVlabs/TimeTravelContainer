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
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// invalidUTF8Name is a maximum-length Linux file name that is not UTF-8.
// Replacing each byte with U+FFFD would triple its length.
var invalidUTF8Name = strings.Repeat("\xff", 255)

func TestFilesystemRecordsRoundTripArbitraryBytes(t *testing.T) {
	for _, record := range []DirectoryEntryRecord{
		{Parent: 1, Name: "plain", Inode: 2},
		{Parent: 1, Name: "caf\xc3\xa9 <&>", Inode: 2},
		{Parent: 1, Name: invalidUTF8Name, Inode: 2},
		{Parent: 1, Name: "a\x80b\xfe\x01", Inode: 2},
	} {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var got DirectoryEntryRecord
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got != record {
			t.Errorf("directory entry %q round-tripped as %q", record.Name, got.Name)
		}
	}

	inode := InodeRecord{
		Number: 3,
		Type:   "symlink",
		Target: "../\xff\xfe/target",
		Xattrs: map[string][]byte{"user.\xff": []byte("binary\x00value"), "user.plain": []byte("v")},
	}
	data, err := json.Marshal(inode)
	if err != nil {
		t.Fatal(err)
	}
	var got InodeRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, inode) {
		t.Errorf("inode round-tripped as %+v, want %+v", got, inode)
	}
}

// TestFilesystemRecordsKeepLegacyEncoding checks that records whose strings
// are valid UTF-8 keep the encoding that existing journals and objects use.
func TestFilesystemRecordsKeepLegacyEncoding(t *testing.T) {
	entry := DirectoryEntryRecord{Parent: 1, Name: "caf\xc3\xa9", Inode: 2}
	got, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(plainDirectoryEntryRecord(entry))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("directory entry encoding = %s, want %s", got, want)
	}

	inode := InodeRecord{Number: 3, Type: "symlink", Target: "target", Xattrs: map[string][]byte{"user.b": {1}, "user.a": {2}}}
	got, err = json.Marshal(inode)
	if err != nil {
		t.Fatal(err)
	}
	want, err = json.Marshal(plainInodeRecord(inode))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("inode encoding = %s, want %s", got, want)
	}
}

// TestFilesystemReplaysNonUTF8Names checks that a journal holding a name
// that is not UTF-8 replays after reopening the store. Such a name used to be
// stored with U+FFFD replacements, which made replay reject the journal.
func TestFilesystemReplaysNonUTF8Names(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := store.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	link := InodeRecord{Number: 2, Type: "symlink", Mode: 0777, Nlink: 1, Target: "\xfe\xff"}
	if _, err := store.ApplyFilesystem(origin, "branch/main/filesystem", []FilesystemOperation{
		{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755, Nlink: 2}},
		{PutInode: &link},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: invalidUTF8Name, Inode: 2}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "\x80", Inode: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyFilesystem(origin, "branch/main/filesystem", []FilesystemOperation{
		{DeleteEntry: &DirectoryEntryRecord{Parent: 1, Name: "\x80"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dir)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer store.Close()
	root, ok := store.Root("branch/main/filesystem")
	if !ok {
		t.Fatal("filesystem root missing after reopen")
	}
	entries, err := store.FilesystemDirectory(root, 1)
	if err != nil || len(entries) != 1 || entries[0].Name != invalidUTF8Name || entries[0].Inode != 2 {
		t.Fatalf("directory = (%+v, %v), want one entry named %q", entries, err, invalidUTF8Name)
	}
	got, ok, err := store.FilesystemInode(root, 2)
	if err != nil || !ok || got.Target != link.Target {
		t.Fatalf("symlink inode = (%+v, %t, %v), want target %q", got, ok, err, link.Target)
	}
	if err := store.Verify(); err != nil {
		t.Fatal(err)
	}
}
