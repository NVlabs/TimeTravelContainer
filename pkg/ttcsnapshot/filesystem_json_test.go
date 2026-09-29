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
	"math/rand"
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

// checkRecordDecodesAlike fails unless decodeInodeRecord and
// decodeDirectoryEntryRecord decode data as the records' UnmarshalJSON do,
// and reports whether the canonical decoders took it.
func checkRecordDecodesAlike(t *testing.T, data []byte) bool {
	t.Helper()
	var wantInode InodeRecord
	wantInodeErr := wantInode.UnmarshalJSON(data)
	inode, canonicalInode := decodeCanonicalInodeRecord(data)
	if canonicalInode && (wantInodeErr != nil || !reflect.DeepEqual(inode, wantInode)) {
		t.Fatalf("the canonical inode decoder took what UnmarshalJSON decodes otherwise (%v):\n%.300s", wantInodeErr, data)
	}
	if got, err := decodeInodeRecord(data); (err == nil) != (wantInodeErr == nil) || err == nil && !reflect.DeepEqual(got, wantInode) {
		t.Fatalf("decoded an inode (%v) differently from UnmarshalJSON (%v):\n%.300s", err, wantInodeErr, data)
	}
	var wantEntry DirectoryEntryRecord
	wantEntryErr := wantEntry.UnmarshalJSON(data)
	entry, canonicalEntry := decodeCanonicalDirectoryEntryRecord(data)
	if canonicalEntry && (wantEntryErr != nil || !reflect.DeepEqual(entry, wantEntry)) {
		t.Fatalf("the canonical entry decoder took what UnmarshalJSON decodes otherwise (%v):\n%.300s", wantEntryErr, data)
	}
	if got, err := decodeDirectoryEntryRecord(data); (err == nil) != (wantEntryErr == nil) || err == nil && !reflect.DeepEqual(got, wantEntry) {
		t.Fatalf("decoded an entry (%v) differently from UnmarshalJSON (%v):\n%.300s", err, wantEntryErr, data)
	}
	return canonicalInode || canonicalEntry
}

func randomInodeRecord(random *rand.Rand) InodeRecord {
	inode := InodeRecord{
		Number: random.Uint64(), Type: []string{"regular", "directory", "symlink", "fifo"}[random.Intn(4)],
		Mode: random.Uint32(), UID: random.Uint32(), GID: random.Uint32(), Nlink: random.Uint32(),
		Size: random.Int63(), DeviceMajor: random.Uint32(), DeviceMinor: random.Uint32(),
		AtimeNsec: random.Int63(), BtimeNsec: random.Int63(), CtimeNsec: random.Int63(), MtimeNsec: random.Int63(),
	}
	if random.Intn(2) == 0 {
		inode.File = ObjectID{Snapshot: SnapshotID(random.Uint64()), Kind: ObjectKindFile, Ordinal: random.Uint64()}
	}
	if random.Intn(2) == 0 {
		inode.Directory = ObjectID{Snapshot: SnapshotID(random.Uint64()), Kind: ObjectKindDirectoryNode, Ordinal: random.Uint64()}
	}
	if random.Intn(3) == 0 {
		inode.Target = "../target/" + strings.Repeat("x", random.Intn(20))
	}
	switch random.Intn(3) {
	case 0:
		inode.Xattrs = map[string][]byte{}
	case 1:
		inode.Xattrs = map[string][]byte{"user.a": nil, "security.capability": {1, 2, 3}, "user.empty": {}}
	}
	return inode
}

// A record as MarshalJSON writes it decodes, without encoding/json, to what
// UnmarshalJSON decodes; any other form goes to UnmarshalJSON.
func TestDecodeFilesystemRecords(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for round := 0; round < 200; round++ {
		inode := randomInodeRecord(random)
		data, err := json.Marshal(inode)
		if err != nil {
			t.Fatal(err)
		}
		if !checkRecordDecodesAlike(t, data) {
			t.Fatalf("round %d: the canonical decoder refused MarshalJSON's inode record %s", round, data)
		}
		if got, err := decodeInodeRecord(data); err != nil || !reflect.DeepEqual(got, inode) {
			t.Fatalf("round %d: inode record decodes (%v) to %+v, want %+v", round, err, got, inode)
		}
		entry := DirectoryEntryRecord{Parent: random.Uint64(), Name: "name-" + strings.Repeat("y", random.Intn(30)), Inode: random.Uint64()}
		if data, err = json.Marshal(entry); err != nil {
			t.Fatal(err)
		}
		if !checkRecordDecodesAlike(t, data) {
			t.Fatalf("round %d: the canonical decoder refused MarshalJSON's entry record %s", round, data)
		}
	}
	inode, err := json.Marshal(randomInodeRecord(random))
	if err != nil {
		t.Fatal(err)
	}
	text := string(inode)
	for _, variant := range []string{
		strings.Replace(text, `"Size":`, `"size":`, 1),
		strings.Replace(text, `"Size":`, `"Unknown":1,"Size":`, 1),
		strings.Replace(text, `"Size":`, `"Size":1,"Size":`, 1),
		strings.Replace(text, `"Size":`, `"Size":-`, 1),
		strings.Replace(text, `"Mode":`, `"Mode":4294967296,"x":`, 1),
		strings.Replace(text, `"Type":"`, `"Type":"\u0072`, 1),
		strings.Replace(text, `"Type":`, `"Type":null,"t":`, 1),
		strings.Replace(text, `"Xattrs":`, `"Xattrs":{"a":"AA==","a":"AA=="},"x":`, 1),
		strings.Replace(text, `"Xattrs":`, `"XattrList":[{"Name":"/w==","Value":null}],"Xattrs":`, 1),
		strings.Replace(text, `"Target":`, `"TargetBytes":"/w==","Target":`, 1),
		strings.Replace(text, `{`, "{\n ", 1),
		text + " x",
		`{"Parent":1,"Name":"a","Inode":2}`,
		`{"Parent":1,"Name":"a","NameBytes":"/w==","Inode":2}`,
		`{"Parent":1,"Name":"\u00e9","Inode":2}`,
		`{"Parent":1.5,"Name":"a","Inode":2}`,
		"null",
		"{}",
		"[]",
		"",
	} {
		checkRecordDecodesAlike(t, []byte(variant))
	}
}

// FuzzDecodeFilesystemRecords checks that whatever the canonical decoders
// take, UnmarshalJSON decodes alike.
func FuzzDecodeFilesystemRecords(f *testing.F) {
	data, err := json.Marshal(randomInodeRecord(rand.New(rand.NewSource(2))))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte(`{"Parent":1,"Name":"a","Inode":2}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		checkRecordDecodesAlike(t, data)
	})
}
