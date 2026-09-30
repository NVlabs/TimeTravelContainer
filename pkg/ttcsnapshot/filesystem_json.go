// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"encoding/json"
	"sort"
	"strconv"
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
	return appendDirectoryEntryRecordJSON(nil, &r)
}

// appendDirectoryEntryRecordJSON appends r as marshalDirectoryEntryRecord
// writes it, without reflection if its name is valid UTF-8, as nearly every
// name is.
func appendDirectoryEntryRecordJSON(out []byte, r *DirectoryEntryRecord) ([]byte, error) {
	if !utf8.ValidString(r.Name) {
		data, err := marshalDirectoryEntryRecord(r)
		return append(out, data...), err
	}
	out = append(out, `{"Parent":`...)
	out = strconv.AppendUint(out, r.Parent, 10)
	out = append(out, `,"Name":`...)
	out = appendJSONString(out, r.Name)
	out = append(out, `,"Inode":`...)
	out = strconv.AppendUint(out, r.Inode, 10)
	return append(out, '}'), nil
}

// marshalDirectoryEntryRecord encodes r with encoding/json.
func marshalDirectoryEntryRecord(r *DirectoryEntryRecord) ([]byte, error) {
	encoded := directoryEntryRecordJSON{plainDirectoryEntryRecord: plainDirectoryEntryRecord(*r)}
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
	return appendInodeRecordJSON(nil, &r)
}

// appendInodeRecordJSON appends r as marshalInodeRecord writes it, without
// reflection if its target and extended attribute names are valid UTF-8, as
// nearly every record's are: encoding/json took 2.8 us a record, this takes
// 0.45 us.
func appendInodeRecordJSON(out []byte, r *InodeRecord) ([]byte, error) {
	valid := utf8.ValidString(r.Target)
	for name := range r.Xattrs {
		valid = valid && utf8.ValidString(name)
	}
	if !valid {
		data, err := marshalInodeRecord(r)
		return append(out, data...), err
	}
	out = append(out, `{"Number":`...)
	out = strconv.AppendUint(out, r.Number, 10)
	out = append(out, `,"Type":`...)
	out = appendJSONString(out, r.Type)
	out = append(out, `,"Mode":`...)
	out = strconv.AppendUint(out, uint64(r.Mode), 10)
	out = append(out, `,"UID":`...)
	out = strconv.AppendUint(out, uint64(r.UID), 10)
	out = append(out, `,"GID":`...)
	out = strconv.AppendUint(out, uint64(r.GID), 10)
	out = append(out, `,"Nlink":`...)
	out = strconv.AppendUint(out, uint64(r.Nlink), 10)
	out = append(out, `,"Size":`...)
	out = strconv.AppendInt(out, r.Size, 10)
	out = append(out, `,"File":`...)
	out = appendObjectIDJSON(out, r.File)
	out = append(out, `,"Directory":`...)
	out = appendObjectIDJSON(out, r.Directory)
	out = append(out, `,"Target":`...)
	out = appendJSONString(out, r.Target)
	out = append(out, `,"DeviceMajor":`...)
	out = strconv.AppendUint(out, uint64(r.DeviceMajor), 10)
	out = append(out, `,"DeviceMinor":`...)
	out = strconv.AppendUint(out, uint64(r.DeviceMinor), 10)
	out = append(out, `,"AtimeNsec":`...)
	out = strconv.AppendInt(out, r.AtimeNsec, 10)
	out = append(out, `,"BtimeNsec":`...)
	out = strconv.AppendInt(out, r.BtimeNsec, 10)
	out = append(out, `,"CtimeNsec":`...)
	out = strconv.AppendInt(out, r.CtimeNsec, 10)
	out = append(out, `,"MtimeNsec":`...)
	out = strconv.AppendInt(out, r.MtimeNsec, 10)
	out = append(out, `,"Xattrs":`...)
	if r.Xattrs == nil {
		out = append(out, "null"...)
	} else {
		names := make([]string, 0, len(r.Xattrs))
		for name := range r.Xattrs {
			names = append(names, name)
		}
		sort.Strings(names)
		out = append(out, '{')
		for index, name := range names {
			if index != 0 {
				out = append(out, ',')
			}
			out = appendJSONString(out, name)
			out = append(out, ':')
			out = appendJSONBytes(out, r.Xattrs[name])
		}
		out = append(out, '}')
	}
	return append(out, '}'), nil
}

// marshalInodeRecord encodes r with encoding/json.
func marshalInodeRecord(r *InodeRecord) ([]byte, error) {
	encoded := inodeRecordJSON{plainInodeRecord: plainInodeRecord(*r)}
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

// decodeInodeRecord decodes an inode object's record. A record as its
// MarshalJSON writes one, with printable ASCII strings and no negative
// numbers, is decoded here without reflection; anything else, however valid,
// goes to UnmarshalJSON whole, so that the two can never differ.
// encoding/json took 20-40 us a record, 3.9 s of the 34 s it took to make a
// lazily imported tree of 100,000 files durable.
func decodeInodeRecord(data []byte) (InodeRecord, error) {
	if inode, ok := decodeCanonicalInodeRecord(data); ok {
		return inode, nil
	}
	var inode InodeRecord
	err := inode.UnmarshalJSON(data)
	return inode, err
}

// decodeDirectoryEntryRecord is decodeInodeRecord for a directory entry.
func decodeDirectoryEntryRecord(data []byte) (DirectoryEntryRecord, error) {
	if entry, ok := decodeCanonicalDirectoryEntryRecord(data); ok {
		return entry, nil
	}
	var entry DirectoryEntryRecord
	err := entry.UnmarshalJSON(data)
	return entry, err
}

// decodeCanonicalInodeRecord decodes a record of the canonical form, each
// field once and known. It reports false for anything else.
func decodeCanonicalInodeRecord(data []byte) (InodeRecord, bool) {
	d := jsonDecoder{data: data}
	var inode InodeRecord
	ok := d.inodeRecord(&inode)
	d.space()
	return inode, ok && d.pos == len(d.data)
}

// inodeRecord consumes an inode record of the canonical form.
func (d *jsonDecoder) inodeRecord(inode *InodeRecord) bool {
	var seen [17]bool
	return d.members(func(name []byte) bool {
		var index int
		var ok bool
		var value uint64
		switch string(name) {
		case "Number":
			inode.Number, ok = d.integer(64)
		case "Type":
			index = 1
			var text []byte
			text, ok = d.plainString()
			inode.Type = string(text)
		case "Mode":
			index = 2
			value, ok = d.integer(32)
			inode.Mode = uint32(value)
		case "UID":
			index = 3
			value, ok = d.integer(32)
			inode.UID = uint32(value)
		case "GID":
			index = 4
			value, ok = d.integer(32)
			inode.GID = uint32(value)
		case "Nlink":
			index = 5
			value, ok = d.integer(32)
			inode.Nlink = uint32(value)
		case "Size":
			index = 6
			value, ok = d.integer(63)
			inode.Size = int64(value)
		case "File":
			index = 7
			ok = d.objectID(&inode.File)
		case "Directory":
			index = 8
			ok = d.objectID(&inode.Directory)
		case "Target":
			index = 9
			var text []byte
			text, ok = d.plainString()
			inode.Target = string(text)
		case "DeviceMajor":
			index = 10
			value, ok = d.integer(32)
			inode.DeviceMajor = uint32(value)
		case "DeviceMinor":
			index = 11
			value, ok = d.integer(32)
			inode.DeviceMinor = uint32(value)
		case "AtimeNsec":
			index = 12
			value, ok = d.integer(63)
			inode.AtimeNsec = int64(value)
		case "BtimeNsec":
			index = 13
			value, ok = d.integer(63)
			inode.BtimeNsec = int64(value)
		case "CtimeNsec":
			index = 14
			value, ok = d.integer(63)
			inode.CtimeNsec = int64(value)
		case "MtimeNsec":
			index = 15
			value, ok = d.integer(63)
			inode.MtimeNsec = int64(value)
		case "Xattrs":
			index = 16
			ok = d.xattrs(&inode.Xattrs)
		default:
			return false
		}
		if seen[index] {
			return false
		}
		seen[index] = true
		return ok
	})
}

// decodeCanonicalDirectoryEntryRecord is decodeCanonicalInodeRecord for a
// directory entry.
func decodeCanonicalDirectoryEntryRecord(data []byte) (DirectoryEntryRecord, bool) {
	d := jsonDecoder{data: data}
	var entry DirectoryEntryRecord
	ok := d.directoryEntryRecord(&entry)
	d.space()
	return entry, ok && d.pos == len(d.data)
}

// directoryEntryRecord consumes a directory entry record of the canonical
// form.
func (d *jsonDecoder) directoryEntryRecord(entry *DirectoryEntryRecord) bool {
	var seen [3]bool
	return d.members(func(name []byte) bool {
		var index int
		var ok bool
		switch string(name) {
		case "Parent":
			entry.Parent, ok = d.integer(64)
		case "Name":
			index = 1
			var text []byte
			text, ok = d.plainString()
			entry.Name = string(text)
		case "Inode":
			index = 2
			entry.Inode, ok = d.integer(64)
		default:
			return false
		}
		if seen[index] {
			return false
		}
		seen[index] = true
		return ok
	})
}

// xattrs consumes a map of base64 values, or null, as json.Unmarshal does
// into a new map; a name it repeats is left to encoding/json.
func (d *jsonDecoder) xattrs(value *map[string][]byte) bool {
	if d.literal("null") {
		*value = nil
		return true
	}
	xattrs := make(map[string][]byte)
	*value = xattrs
	return d.members(func(name []byte) bool {
		if _, ok := xattrs[string(name)]; ok {
			return false
		}
		var data []byte
		if !d.bytes(&data) {
			return false
		}
		xattrs[string(name)] = data
		return true
	})
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
