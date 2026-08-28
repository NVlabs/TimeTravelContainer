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

// Package copyhelper implements the Linux-side half of ttc cp. It is kept
// independent of the host CLI so non-Linux builds can embed a small Linux
// executable without cross-compiling the complete TTC dependency graph.
package copyhelper

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const sourceContentsPAXKey = "TTC.source-contents"

// Run executes one copy-helper operation and returns a process exit status.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "ttc copy helper: expected export or import")
		return 2
	}
	var err error
	switch args[0] {
	case "export":
		follow := false
		args = args[1:]
		if len(args) != 0 && args[0] == "--follow-link" {
			follow = true
			args = args[1:]
		}
		if len(args) != 1 {
			err = errors.New("export requires one source path")
		} else {
			err = WriteArchive(args[0], follow, stdout)
		}
	case "import":
		preserveOwner := false
		rawArchive := false
		args = args[1:]
		for len(args) != 0 && strings.HasPrefix(args[0], "--") {
			switch args[0] {
			case "--archive":
				preserveOwner = true
			case "--raw":
				rawArchive = true
			default:
				err = fmt.Errorf("unknown import option %q", args[0])
			}
			if err != nil {
				break
			}
			args = args[1:]
		}
		if err == nil {
			if len(args) != 1 {
				err = errors.New("import requires one destination path")
			} else {
				err = ExtractArchive(stdin, args[0], preserveOwner, 0, 0, rawArchive)
			}
		}
	default:
		err = fmt.Errorf("unknown operation %q", args[0])
	}
	if err != nil {
		fmt.Fprintf(stderr, "ttc copy helper: %v\n", err)
		return 1
	}
	return 0
}

type fileIdentity struct {
	dev uint64
	ino uint64
}

func sourceContents(name string) bool {
	return name == string(filepath.Separator) || strings.HasSuffix(name, string(filepath.Separator)+".")
}

func archiveRootName(source string) string {
	trimmed := strings.TrimSuffix(source, string(filepath.Separator)+".")
	clean := filepath.Clean(trimmed)
	if clean == string(filepath.Separator) || clean == "." {
		return "."
	}
	return filepath.Base(clean)
}

func readXattrs(name string) (map[string]string, error) {
	size, err := unix.Llistxattr(name, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) {
			return nil, nil
		}
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	names := make([]byte, size)
	size, err = unix.Llistxattr(name, names)
	if err != nil {
		return nil, err
	}
	xattrs := make(map[string]string)
	for _, attribute := range strings.Split(string(names[:size]), "\x00") {
		if attribute == "" {
			continue
		}
		valueSize, err := unix.Lgetxattr(name, attribute, nil)
		if err != nil {
			return nil, err
		}
		value := make([]byte, valueSize)
		valueSize, err = unix.Lgetxattr(name, attribute, value)
		if err != nil {
			return nil, err
		}
		xattrs[attribute] = string(value[:valueSize])
	}
	return xattrs, nil
}

// WriteArchive writes a tar stream rooted at source.
func WriteArchive(source string, followLink bool, output io.Writer) error {
	contents := sourceContents(source)
	physical := filepath.Clean(strings.TrimSuffix(source, string(filepath.Separator)+"."))
	if physical == "" {
		physical = string(filepath.Separator)
	}
	rootName := archiveRootName(source)
	if followLink {
		info, err := os.Lstat(physical)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(physical)
			if err != nil {
				return err
			}
			physical = resolved
		}
	}
	if _, err := os.Lstat(physical); err != nil {
		return fmt.Errorf("stating source %q: %w", source, err)
	}

	writer := tar.NewWriter(output)
	hardlinks := make(map[fileIdentity]string)
	var add func(string, string, bool) error
	add = func(hostPath, archivePath string, root bool) error {
		info, err := os.Lstat(hostPath)
		if err != nil {
			return err
		}
		linkTarget := ""
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(hostPath)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(archivePath)
		if info.IsDir() && !strings.HasSuffix(header.Name, "/") {
			header.Name += "/"
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			header.Uid = int(stat.Uid)
			header.Gid = int(stat.Gid)
			header.Devmajor = int64(unix.Major(uint64(stat.Rdev)))
			header.Devminor = int64(unix.Minor(uint64(stat.Rdev)))
			if info.Mode().IsRegular() && stat.Nlink > 1 {
				identity := fileIdentity{dev: uint64(stat.Dev), ino: stat.Ino}
				if first, ok := hardlinks[identity]; ok {
					header.Typeflag = tar.TypeLink
					header.Linkname = first
					header.Size = 0
				} else {
					hardlinks[identity] = header.Name
				}
			}
		}
		xattrs, err := readXattrs(hostPath)
		if err != nil {
			return fmt.Errorf("reading xattrs for %q: %w", hostPath, err)
		}
		header.Xattrs = xattrs
		if root && contents {
			if header.PAXRecords == nil {
				header.PAXRecords = make(map[string]string)
			}
			header.PAXRecords[sourceContentsPAXKey] = "1"
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() && header.Typeflag != tar.TypeLink {
			file, err := os.Open(hostPath)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(writer, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if info.IsDir() {
			entries, err := os.ReadDir(hostPath)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if err := add(filepath.Join(hostPath, entry.Name()), path.Join(strings.TrimSuffix(header.Name, "/"), entry.Name()), false); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := add(physical, rootName, true); err != nil {
		_ = writer.Close()
		return fmt.Errorf("archiving %q: %w", source, err)
	}
	return writer.Close()
}

func cleanArchivePath(name string) (string, error) {
	name = filepath.ToSlash(name)
	clean := path.Clean(name)
	clean = strings.TrimPrefix(clean, "./")
	if clean == "" || clean == "." {
		return ".", nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, 0) {
		return "", fmt.Errorf("archive path %q escapes its destination", name)
	}
	return clean, nil
}

type extractionPlan struct {
	boundary       string
	rootTarget     string
	rootName       string
	skipRoot       bool
	raw            bool
	preserveOwner  bool
	defaultUID     int
	defaultGID     int
	directoryQueue []directoryMetadata
}

type directoryMetadata struct {
	path   string
	header tar.Header
}

func destinationStat(name string) (string, os.FileInfo, error) {
	info, err := os.Stat(name)
	if err == nil {
		if info.IsDir() {
			resolved, resolveErr := filepath.EvalSymlinks(name)
			if resolveErr != nil {
				return "", nil, resolveErr
			}
			return resolved, info, nil
		}
		return filepath.Clean(name), info, nil
	}
	if !os.IsNotExist(err) {
		return "", nil, err
	}
	return filepath.Clean(name), nil, nil
}

func newExtractionPlan(first *tar.Header, destination string, preserveOwner, raw bool, defaultUID, defaultGID int) (*extractionPlan, error) {
	destinationHadSlash := strings.HasSuffix(destination, string(filepath.Separator))
	destination, destinationInfo, err := destinationStat(destination)
	if err != nil {
		return nil, err
	}
	if destinationHadSlash && destinationInfo == nil {
		return nil, fmt.Errorf("destination directory %q does not exist", destination)
	}
	if raw {
		if destinationInfo == nil || !destinationInfo.IsDir() {
			return nil, fmt.Errorf("destination %q must be an existing directory when copying an archive from '-'", destination)
		}
		return &extractionPlan{boundary: destination, rootTarget: destination, raw: true, preserveOwner: preserveOwner, defaultUID: defaultUID, defaultGID: defaultGID}, nil
	}
	rootName, err := cleanArchivePath(first.Name)
	if err != nil {
		return nil, err
	}
	sourceDirectory := first.Typeflag == tar.TypeDir
	sourceContents := first.PAXRecords[sourceContentsPAXKey] == "1"
	if sourceContents && !sourceDirectory {
		return nil, errors.New("copy-contents archive root is not a directory")
	}
	baseName := path.Base(rootName)
	if baseName == "." {
		sourceContents = true
	}
	plan := &extractionPlan{rootName: rootName, preserveOwner: preserveOwner, defaultUID: defaultUID, defaultGID: defaultGID}
	switch {
	case sourceContents:
		if destinationInfo != nil && !destinationInfo.IsDir() {
			return nil, fmt.Errorf("cannot copy directory contents onto non-directory %q", destination)
		}
		if destinationInfo == nil {
			parent := filepath.Dir(destination)
			if info, err := os.Stat(parent); err != nil || !info.IsDir() {
				return nil, fmt.Errorf("destination parent %q does not exist or is not a directory", parent)
			}
			if err := os.Mkdir(destination, 0700); err != nil {
				return nil, err
			}
		}
		plan.boundary, plan.rootTarget, plan.skipRoot = destination, destination, true
	case sourceDirectory:
		if destinationInfo != nil && !destinationInfo.IsDir() {
			return nil, fmt.Errorf("cannot copy directory onto non-directory %q", destination)
		}
		if destinationInfo != nil {
			plan.rootTarget, plan.boundary = filepath.Join(destination, baseName), destination
		} else {
			parent := filepath.Dir(destination)
			if info, err := os.Stat(parent); err != nil || !info.IsDir() {
				return nil, fmt.Errorf("destination parent %q does not exist or is not a directory", parent)
			}
			plan.rootTarget, plan.boundary = destination, parent
		}
	default:
		if destinationInfo != nil && destinationInfo.IsDir() {
			plan.rootTarget, plan.boundary = filepath.Join(destination, baseName), destination
		} else {
			parent := filepath.Dir(destination)
			if info, err := os.Stat(parent); err != nil || !info.IsDir() {
				return nil, fmt.Errorf("destination parent %q does not exist or is not a directory", parent)
			}
			plan.rootTarget, plan.boundary = destination, parent
		}
	}
	return plan, nil
}

func (p *extractionPlan) target(headerName string) (string, error) {
	name, err := cleanArchivePath(headerName)
	if err != nil {
		return "", err
	}
	var relative string
	if p.raw {
		relative = name
		if relative == "." {
			return p.rootTarget, nil
		}
	} else {
		switch {
		case name == p.rootName:
			relative = ""
		case p.rootName == "." && p.skipRoot:
			relative = name
		case strings.HasPrefix(name, p.rootName+"/"):
			relative = strings.TrimPrefix(name, p.rootName+"/")
		default:
			return "", fmt.Errorf("archive entry %q is outside source root %q", name, p.rootName)
		}
		if p.skipRoot && relative == "" {
			return p.rootTarget, nil
		}
	}
	target := p.rootTarget
	if relative != "" {
		target = filepath.Join(target, filepath.FromSlash(relative))
	}
	check, err := filepath.Rel(p.boundary, target)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes destination", headerName)
	}
	return target, nil
}

func rejectSymlinkAncestor(boundary, target string) error {
	relative, err := filepath.Rel(boundary, filepath.Dir(target))
	if err != nil {
		return err
	}
	if relative == "." {
		return nil
	}
	current := boundary
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive entry traverses symlink %q", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("archive entry parent %q is not a directory", current)
		}
	}
	return nil
}

func removeDestination(target string, wantDirectory bool) error {
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if wantDirectory {
		if !info.IsDir() {
			return fmt.Errorf("cannot overwrite non-directory %q with a directory", target)
		}
		return nil
	}
	if info.IsDir() {
		return fmt.Errorf("cannot overwrite directory %q with a non-directory", target)
	}
	return os.Remove(target)
}

func applyXattrs(target string, header *tar.Header) error {
	for name, value := range header.Xattrs {
		if err := unix.Lsetxattr(target, name, []byte(value), 0); err != nil {
			if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) {
				continue
			}
			return fmt.Errorf("setting xattr %q on %q: %w", name, target, err)
		}
	}
	return nil
}

func applyMetadata(target string, header *tar.Header, preserveOwner bool, defaultUID, defaultGID int) error {
	uid, gid := defaultUID, defaultGID
	if preserveOwner {
		uid, gid = header.Uid, header.Gid
	}
	if err := os.Lchown(target, uid, gid); err != nil {
		return fmt.Errorf("setting ownership on %q: %w", target, err)
	}
	if header.Typeflag != tar.TypeSymlink {
		if err := os.Chmod(target, os.FileMode(header.Mode)&07777); err != nil {
			return fmt.Errorf("setting mode on %q: %w", target, err)
		}
	}
	if err := applyXattrs(target, header); err != nil {
		return err
	}
	mtime := header.ModTime
	if mtime.IsZero() {
		mtime = time.Unix(0, 0)
	}
	times := []unix.Timespec{unix.NsecToTimespec(mtime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
	flags := 0
	if header.Typeflag == tar.TypeSymlink {
		flags = unix.AT_SYMLINK_NOFOLLOW
	}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, target, times, flags); err != nil && !errors.Is(err, unix.ENOTSUP) {
		return fmt.Errorf("setting timestamps on %q: %w", target, err)
	}
	return nil
}

func (p *extractionPlan) extractEntry(reader *tar.Reader, header *tar.Header, first bool) error {
	target, err := p.target(header.Name)
	if err != nil {
		return err
	}
	if first && p.skipRoot {
		return nil
	}
	if err := rejectSymlinkAncestor(p.boundary, target); err != nil {
		return err
	}
	switch header.Typeflag {
	case tar.TypeDir:
		if err := removeDestination(target, true); err != nil {
			return err
		}
		if err := os.Mkdir(target, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		p.directoryQueue = append(p.directoryQueue, directoryMetadata{path: target, header: *header})
		return nil
	case tar.TypeReg, tar.TypeRegA:
		if err := removeDestination(target, false); err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, reader)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	case tar.TypeSymlink:
		if err := removeDestination(target, false); err != nil {
			return err
		}
		if err := os.Symlink(header.Linkname, target); err != nil {
			return err
		}
	case tar.TypeLink:
		if err := removeDestination(target, false); err != nil {
			return err
		}
		linkTarget, err := p.target(header.Linkname)
		if err != nil {
			return fmt.Errorf("invalid hard-link target: %w", err)
		}
		if err := rejectSymlinkAncestor(p.boundary, linkTarget); err != nil {
			return err
		}
		if err := os.Link(linkTarget, target); err != nil {
			return err
		}
	case tar.TypeFifo:
		if err := removeDestination(target, false); err != nil {
			return err
		}
		if err := unix.Mkfifo(target, uint32(header.Mode)&07777); err != nil {
			return err
		}
	case tar.TypeChar, tar.TypeBlock:
		if err := removeDestination(target, false); err != nil {
			return err
		}
		mode := uint32(header.Mode) & 07777
		if header.Typeflag == tar.TypeChar {
			mode |= unix.S_IFCHR
		} else {
			mode |= unix.S_IFBLK
		}
		device := int(unix.Mkdev(uint32(header.Devmajor), uint32(header.Devminor)))
		if err := unix.Mknod(target, mode, device); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported archive entry type %q for %q", header.Typeflag, header.Name)
	}
	return applyMetadata(target, header, p.preserveOwner, p.defaultUID, p.defaultGID)
}

// ExtractArchive extracts a helper tar stream at destination.
func ExtractArchive(input io.Reader, destination string, preserveOwner bool, defaultUID, defaultGID int, rawArchive bool) error {
	reader := tar.NewReader(input)
	first, err := reader.Next()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	plan, err := newExtractionPlan(first, destination, preserveOwner, rawArchive, defaultUID, defaultGID)
	if err != nil {
		return err
	}
	if err := plan.extractEntry(reader, first, true); err != nil {
		return err
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := plan.extractEntry(reader, header, false); err != nil {
			return err
		}
	}
	for i := len(plan.directoryQueue) - 1; i >= 0; i-- {
		entry := &plan.directoryQueue[i]
		if err := applyMetadata(entry.path, &entry.header, plan.preserveOwner, plan.defaultUID, plan.defaultGID); err != nil {
			return err
		}
	}
	return nil
}
