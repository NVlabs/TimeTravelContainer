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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const filesystemImportBatch = 256

type hostFileIdentity struct {
	device uint64
	inode  uint64
}

type importPath struct {
	host     string
	relative string
	info     fs.FileInfo
	stat     *syscall.Stat_t
}

// ImportDirectory imports a fully extracted host tree into the logical
// filesystem graph. Regular-file data is stored once per hardlinked inode and
// is never embedded in metadata or mirrored as a guest directory hierarchy.
func ImportDirectory(ctx context.Context, graph *Store, data *LocalDataStore, origin SnapshotID, rootName, source string) (ObjectID, error) {
	if graph == nil || data == nil || !filepath.IsAbs(source) {
		return ObjectID{}, errors.New("invalid TTC filesystem import")
	}
	var paths []importPath
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			relative = ""
		}
		if !validHostRelative(relative) {
			return fmt.Errorf("invalid imported path %q", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("path %q has no Linux stat metadata", path)
		}
		paths = append(paths, importPath{host: path, relative: filepath.ToSlash(relative), info: info, stat: stat})
		return nil
	})
	if err != nil {
		return ObjectID{}, err
	}
	sort.Slice(paths, func(i, j int) bool {
		leftDepth := strings.Count(paths[i].relative, "/")
		rightDepth := strings.Count(paths[j].relative, "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return paths[i].relative < paths[j].relative
	})
	fileIDs := make(map[hostFileIdentity]ObjectID)
	var regularItems []importPath
	for _, item := range paths {
		if !item.info.Mode().IsRegular() {
			continue
		}
		identity := hostFileIdentity{device: uint64(item.stat.Dev), inode: item.stat.Ino}
		if _, exists := fileIDs[identity]; exists {
			continue
		}
		fileIDs[identity] = ObjectID{}
		regularItems = append(regularItems, item)
	}
	if len(regularItems) != 0 {
		kinds := make([]ObjectKind, len(regularItems))
		for index := range kinds {
			kinds[index] = ObjectKindFile
		}
		ids, err := graph.ReserveObjects(origin, kinds)
		if err != nil {
			return ObjectID{}, err
		}
		reserved := make([]ReservedObject, 0, filesystemImportBatch)
		for index, item := range regularItems {
			id := ids[index]
			identity := hostFileIdentity{device: uint64(item.stat.Dev), inode: item.stat.Ino}
			fileIDs[identity] = id
			file, err := os.Open(item.host)
			if err != nil {
				return ObjectID{}, err
			}
			installErr := data.Install(id, file, item.stat.Size)
			closeErr := file.Close()
			if installErr != nil || closeErr != nil {
				return ObjectID{}, errors.Join(installErr, closeErr)
			}
			reserved = append(reserved, ReservedObject{ID: id, Pin: "import/" + id.key(), BulkLength: uint64(item.stat.Size)})
			if len(reserved) == filesystemImportBatch || index == len(regularItems)-1 {
				if err := graph.CommitReservedObjects(reserved); err != nil {
					return ObjectID{}, err
				}
				reserved = reserved[:0]
			}
		}
	}

	nextInode := uint64(1)
	inodesByPath := make(map[string]uint64, len(paths))
	hardlinks := make(map[hostFileIdentity]uint64)
	operations := make([]FilesystemOperation, 0, filesystemImportBatch)
	pins := make([]string, 0)
	var root ObjectID
	flush := func() error {
		if len(operations) == 0 {
			return nil
		}
		updated, err := graph.ApplyFilesystem(origin, rootName, operations)
		if err != nil {
			return err
		}
		root = updated
		operations = operations[:0]
		if err := graph.DeletePins(pins); err != nil {
			return err
		}
		pins = pins[:0]
		return nil
	}
	for _, item := range paths {
		if err := ctx.Err(); err != nil {
			return ObjectID{}, err
		}
		identity := hostFileIdentity{device: uint64(item.stat.Dev), inode: item.stat.Ino}
		inodeNumber, linked := hardlinks[identity]
		if !linked {
			inodeNumber = nextInode
			nextInode++
			hardlinks[identity] = inodeNumber
			record, pin, err := importInode(item, inodeNumber, fileIDs[identity])
			if err != nil {
				return ObjectID{}, err
			}
			operations = append(operations, FilesystemOperation{PutInode: &record})
			if pin != "" {
				pins = append(pins, pin)
			}
		}
		inodesByPath[item.relative] = inodeNumber
		if item.relative != "" {
			parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(item.relative)))
			if parent == "." {
				parent = ""
			}
			parentInode, ok := inodesByPath[parent]
			if !ok {
				return ObjectID{}, fmt.Errorf("import parent %q is missing", parent)
			}
			entry := DirectoryEntryRecord{Parent: parentInode, Name: filepath.Base(filepath.FromSlash(item.relative)), Inode: inodeNumber}
			operations = append(operations, FilesystemOperation{PutEntry: &entry})
		}
		if len(operations) >= filesystemImportBatch {
			if err := flush(); err != nil {
				return ObjectID{}, err
			}
		}
	}
	if err := flush(); err != nil {
		return ObjectID{}, err
	}
	return root, nil
}

func validHostRelative(path string) bool {
	if path == "" {
		return true
	}
	clean := filepath.Clean(path)
	return !filepath.IsAbs(clean) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func importInode(item importPath, number uint64, fileID ObjectID) (InodeRecord, string, error) {
	record := InodeRecord{
		Number: number, Mode: uint32(item.stat.Mode), UID: item.stat.Uid, GID: item.stat.Gid,
		Nlink: uint32(item.stat.Nlink), Size: item.stat.Size,
		AtimeNsec: item.stat.Atim.Sec*1e9 + item.stat.Atim.Nsec,
		CtimeNsec: item.stat.Ctim.Sec*1e9 + item.stat.Ctim.Nsec,
		MtimeNsec: item.stat.Mtim.Sec*1e9 + item.stat.Mtim.Nsec,
	}
	xattrs, err := hostXattrs(item.host, item.info.Mode()&os.ModeSymlink != 0)
	if err != nil {
		return InodeRecord{}, "", err
	}
	record.Xattrs = xattrs
	switch mode := item.info.Mode(); {
	case mode.IsDir():
		record.Type = "directory"
	case mode.IsRegular():
		record.Type = "regular"
		if fileID.IsZero() {
			return InodeRecord{}, "", fmt.Errorf("regular file %q has no reserved object", item.host)
		}
		record.File = fileID
		return record, "import/" + fileID.key(), nil
	case mode&os.ModeSymlink != 0:
		record.Type = "symlink"
		record.Target, err = os.Readlink(item.host)
		if err != nil {
			return InodeRecord{}, "", err
		}
	case mode&os.ModeNamedPipe != 0:
		record.Type = "fifo"
	case mode&os.ModeSocket != 0:
		return InodeRecord{}, "", fmt.Errorf("cannot import live socket %q", item.host)
	case mode&os.ModeDevice != 0:
		if item.stat.Rdev == 0 && mode&os.ModeCharDevice != 0 {
			record.Type = "whiteout"
		} else if mode&os.ModeCharDevice != 0 {
			record.Type = "char"
		} else {
			record.Type = "block"
		}
		record.DeviceMajor = unix.Major(uint64(item.stat.Rdev))
		record.DeviceMinor = unix.Minor(uint64(item.stat.Rdev))
	default:
		return InodeRecord{}, "", fmt.Errorf("unsupported imported file type %s at %q", mode.Type(), item.host)
	}
	return record, "", nil
}

func hostXattrs(path string, symlink bool) (map[string][]byte, error) {
	list := unix.Listxattr
	get := unix.Getxattr
	if symlink {
		list, get = unix.Llistxattr, unix.Lgetxattr
	}
	size, err := list(path, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, nil
		}
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	names := make([]byte, size)
	n, err := list(path, names)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte)
	for _, raw := range strings.Split(string(names[:n]), "\x00") {
		if raw == "" {
			continue
		}
		valueSize, err := get(path, raw, nil)
		if err != nil {
			return nil, err
		}
		value := make([]byte, valueSize)
		if _, err := get(path, raw, value); err != nil {
			return nil, err
		}
		result[raw] = value
	}
	return result, nil
}

// MaterializeFilesystem reconstructs a logical filesystem into an empty host
// directory. It is used by standalone image preparation and portable
// export, not by the clustered lazy read path.
func MaterializeFilesystem(ctx context.Context, graph *Store, data *LocalDataStore, root ObjectID, destination string) error {
	if graph == nil || data == nil || !filepath.IsAbs(destination) {
		return errors.New("invalid TTC filesystem materialization")
	}
	if entries, err := os.ReadDir(destination); err == nil && len(entries) != 0 {
		return errors.New("TTC filesystem materialization destination is not empty")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	rootInode, ok, err := graph.FilesystemInode(root, 1)
	if err != nil || !ok || rootInode.Type != "directory" {
		return fmt.Errorf("logical filesystem has no root directory: %w", err)
	}
	type work struct {
		path  string
		inode InodeRecord
	}
	queue := []work{{path: destination, inode: rootInode}}
	firstPath := make(map[uint64]string)
	var directories []work
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		if err := ctx.Err(); err != nil {
			return err
		}
		if existing := firstPath[current.inode.Number]; existing != "" && current.inode.Type != "directory" {
			if err := os.Link(existing, current.path); err != nil {
				return err
			}
			continue
		}
		firstPath[current.inode.Number] = current.path
		if err := materializeInode(data, current.inode, current.path); err != nil {
			return err
		}
		if current.inode.Type == "directory" {
			directories = append(directories, current)
			entries, err := graph.FilesystemDirectory(root, current.inode.Number)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.Name == "" || entry.Name == "." || entry.Name == ".." || filepath.Base(entry.Name) != entry.Name {
					return fmt.Errorf("invalid logical directory name %q", entry.Name)
				}
				inode, ok, err := graph.FilesystemInode(root, entry.Inode)
				if err != nil || !ok {
					return fmt.Errorf("resolving inode %d: %w", entry.Inode, err)
				}
				queue = append(queue, work{path: filepath.Join(current.path, entry.Name), inode: inode})
			}
		}
	}
	// Directory timestamps and permissions must be restored after descendants.
	for index := len(directories) - 1; index >= 0; index-- {
		if err := applyHostMetadata(directories[index].path, directories[index].inode, false); err != nil {
			return err
		}
	}
	return nil
}

func materializeInode(data *LocalDataStore, inode InodeRecord, path string) error {
	switch inode.Type {
	case "directory":
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	case "regular":
		source, err := data.Open(inode.File, inode.Size)
		if err != nil {
			return err
		}
		destination, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			source.Close()
			return err
		}
		_, copyErr := io.Copy(destination, source)
		closeErr := errors.Join(destination.Close(), source.Close())
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
	case "symlink":
		if err := os.Symlink(inode.Target, path); err != nil {
			return err
		}
	case "fifo":
		if err := unix.Mkfifo(path, inode.Mode&07777); err != nil {
			return err
		}
	case "char", "block", "whiteout":
		mode := inode.Mode
		if inode.Type == "char" || inode.Type == "whiteout" {
			mode |= unix.S_IFCHR
		} else {
			mode |= unix.S_IFBLK
		}
		if err := unix.Mknod(path, mode, int(unix.Mkdev(inode.DeviceMajor, inode.DeviceMinor))); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported logical inode type %q", inode.Type)
	}
	return applyHostMetadata(path, inode, inode.Type == "symlink")
}

func applyHostMetadata(path string, inode InodeRecord, symlink bool) error {
	if symlink {
		if err := unix.Lchown(path, int(inode.UID), int(inode.GID)); err != nil && !errors.Is(err, unix.EPERM) {
			return err
		}
	} else {
		if err := os.Chmod(path, fs.FileMode(inode.Mode&07777)); err != nil {
			return err
		}
		if err := os.Chown(path, int(inode.UID), int(inode.GID)); err != nil && !errors.Is(err, unix.EPERM) {
			return err
		}
	}
	for name, value := range inode.Xattrs {
		var err error
		if symlink {
			err = unix.Lsetxattr(path, name, value, 0)
		} else {
			err = unix.Setxattr(path, name, value, 0)
		}
		if err != nil && !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
			return err
		}
	}
	if !symlink {
		times := []unix.Timespec{unix.NsecToTimespec(inode.AtimeNsec), unix.NsecToTimespec(inode.MtimeNsec)}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, times, 0); err != nil {
			return err
		}
	}
	return nil
}
