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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func cleanRelative(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid repository-relative path %q", path)
		}
	}
	return parts, nil
}

// openDirRelative walks every component with O_NOFOLLOW. This deliberately
// avoids converting a donated descriptor back to /proc/self/fd/N: doing so
// would require AT_FDCWD path resolution in the isolated Sentry and would
// weaken the seccomp boundary.
func openDirRelative(rootFD int, path string) (*os.File, error) {
	parts, err := cleanRelative(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Dup(rootFD)
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openFileRelative(rootFD int, path string, flags int, mode uint32) (*os.File, error) {
	parts, err := cleanRelative(path)
	if err != nil || len(parts) == 0 {
		if err == nil {
			err = fmt.Errorf("repository-relative file path is empty")
		}
		return nil, err
	}
	parent, err := openDirRelative(rootFD, strings.Join(parts[:len(parts)-1], "/"))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), parts[len(parts)-1], flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func mkdirRelative(rootFD int, path string) (bool, error) {
	parts, err := cleanRelative(path)
	if err != nil || len(parts) == 0 {
		return false, err
	}
	parent, err := openDirRelative(rootFD, strings.Join(parts[:len(parts)-1], "/"))
	if err != nil {
		return false, err
	}
	defer parent.Close()
	if err := unix.Mkdirat(int(parent.Fd()), parts[len(parts)-1], 0700); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func readFileRelative(rootFD int, path string) ([]byte, error) {
	file, err := openFileRelative(rootFD, path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func syncDirRelative(rootFD int, path string) error {
	dir, err := openDirRelative(rootFD, path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func unlinkRelative(rootFD int, path string) error {
	parts, err := cleanRelative(path)
	if err != nil || len(parts) == 0 {
		return err
	}
	parent, err := openDirRelative(rootFD, strings.Join(parts[:len(parts)-1], "/"))
	if err != nil {
		return err
	}
	defer parent.Close()
	return unix.Unlinkat(int(parent.Fd()), parts[len(parts)-1], 0)
}

func touchRelative(rootFD int, path string) error {
	file, err := openFileRelative(rootFD, path, unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return touchFD(int(file.Fd()))
}

func writeImmutableRelative(rootFD int, dirPath, finalName string, data []byte) error {
	if _, err := mkdirRelative(rootFD, dirPath); err != nil {
		return err
	}
	dir, err := openDirRelative(rootFD, dirPath)
	if err != nil {
		return err
	}
	defer dir.Close()
	nonce, err := randomName()
	if err != nil {
		return err
	}
	tempName := ".publish-" + nonce
	fd, err := unix.Openat(int(dir.Fd()), tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	temp := os.NewFile(uintptr(fd), tempName)
	defer temp.Close()
	defer unix.Unlinkat(int(dir.Fd()), tempName, 0)
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := unix.Linkat(int(dir.Fd()), tempName, int(dir.Fd()), finalName, 0); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return err
		}
		existing, readErr := openFileRelative(rootFD, dirPath+"/"+finalName, unix.O_RDONLY, 0)
		if readErr != nil {
			return readErr
		}
		existingData, readErr := io.ReadAll(existing)
		closeErr := existing.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if !bytes.Equal(existingData, data) {
			return errors.New("immutable TTC memory object differs from existing content")
		}
		return nil
	}
	return dir.Sync()
}

func atomicReplaceRelative(rootFD int, dirPath, finalName string, data []byte) error {
	dir, err := openDirRelative(rootFD, dirPath)
	if err != nil {
		return err
	}
	defer dir.Close()
	nonce, err := randomName()
	if err != nil {
		return err
	}
	tempName := ".txn-" + nonce
	fd, err := unix.Openat(int(dir.Fd()), tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	temp := os.NewFile(uintptr(fd), tempName)
	defer temp.Close()
	defer unix.Unlinkat(int(dir.Fd()), tempName, 0)
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := unix.Renameat(int(dir.Fd()), tempName, int(dir.Fd()), finalName); err != nil {
		return err
	}
	return dir.Sync()
}

func (r *MemoryRepository) beginDonatedTransaction(manifest ManifestID, objects []ID) (string, error) {
	data, err := encodeTransaction(publicationTransaction{Manifest: manifest, Objects: objects})
	if err != nil {
		return "", err
	}
	name, err := randomName()
	if err != nil {
		return "", err
	}
	name += ".txn"
	if err := atomicReplaceRelative(int(r.rootFD.Fd()), "transactions", name, data); err != nil {
		return "", err
	}
	return "transactions/" + name, nil
}

func (r *MemoryRepository) publishDonated(objects []Object, needed map[ID][]StoredBinding, objectIDs []ID, manifestData []byte, manifestID ManifestID) (ManifestID, error) {
	txnPath, err := r.beginDonatedTransaction(manifestID, objectIDs)
	if err != nil {
		return ManifestID{}, fmt.Errorf("beginning donated memory publication: %w", err)
	}
	keepTransaction := false
	defer func() {
		if !keepTransaction {
			_ = unlinkRelative(int(r.rootFD.Fd()), txnPath)
		}
	}()
	provided := make(map[ID]Object, len(objects))
	decoded := make(map[ID]Chunk, len(objects))
	for _, object := range objects {
		if _, ok := provided[object.ID]; ok {
			return ManifestID{}, fmt.Errorf("publication repeats memory object %s", object.ID)
		}
		if _, ok := needed[object.ID]; !ok {
			return ManifestID{}, fmt.Errorf("publication includes unreferenced memory object %s", object.ID)
		}
		chunk, err := Decode(object.Data, &object.ID)
		if err != nil {
			return ManifestID{}, fmt.Errorf("validating memory object %s: %w", object.ID, err)
		}
		if chunk.Logical != object.Logical {
			return ManifestID{}, fmt.Errorf("memory object %s has logical coordinate %+v, want %+v", object.ID, chunk.Logical, object.Logical)
		}
		provided[object.ID] = object
		decoded[object.ID] = chunk
	}
	for objectID, objectBindings := range needed {
		chunk, ok := decoded[objectID]
		if !ok {
			chunk, err = r.chunks.Get(objectID)
			if err != nil {
				return ManifestID{}, fmt.Errorf("manifest references unavailable memory object %s: %w", objectID, err)
			}
		}
		pages := make(map[uint8]Page, len(chunk.Pages))
		for _, page := range chunk.Pages {
			pages[page.Slot] = page
		}
		for _, binding := range objectBindings {
			page, ok := pages[binding.Binding.Slot]
			if chunk.Logical != binding.Binding.Chunk || !ok || page.Version != binding.Binding.Version {
				return ManifestID{}, fmt.Errorf("memory object %s does not contain requested page version %+v at (%+v,%d)", objectID, binding.Binding.Version, binding.Binding.Chunk, binding.Binding.Slot)
			}
		}
	}
	for _, object := range objects {
		if err := r.chunks.PutObject(object); err != nil {
			return ManifestID{}, fmt.Errorf("publishing donated memory object %s: %w", object.ID, err)
		}
		if err := touchRelative(int(r.rootFD.Fd()), txnPath); err != nil {
			return ManifestID{}, fmt.Errorf("refreshing donated memory publication transaction: %w", err)
		}
	}
	name := manifestID.String()
	if err := writeImmutableRelative(int(r.rootFD.Fd()), "manifests/"+name[:4], name+".manifest", manifestData); err != nil {
		return ManifestID{}, fmt.Errorf("publishing donated memory manifest %s: %w", manifestID, err)
	}
	keepTransaction = true
	return manifestID, nil
}
