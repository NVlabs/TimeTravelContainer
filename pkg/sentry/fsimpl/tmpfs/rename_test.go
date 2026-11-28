// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tmpfs

import (
	"slices"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/ttcvolume"
)

// Renaming a file onto another link of itself does nothing, as on Linux (whose
// vfs_rename compares inodes), and both names stay. tmpfs compared dentries:
// it moved the renamed name onto the other and dropped a link. On a volume,
// which keeps both names, the name then was listed but not found, and could
// not be made again (EEXIST): gnulib's configure check of rename does this,
// and coreutils' configure failed on it.
func TestRenameOntoAnotherLinkOfTheFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		volume bool
	}{{"memory", false}, {"volume", true}} {
		t.Run(tc.name, func(t *testing.T) {
			// A volume's root is root's, mode 0755.
			ctx := contexttest.RootContext(t)
			creds := auth.CredentialsFromContext(ctx)
			var opts vfs.GetFilesystemOptions
			if tc.volume {
				manager, err := ttcvolume.OpenManager(t.TempDir())
				if err != nil {
					t.Fatalf("OpenManager: %v", err)
				}
				repository, err := manager.Create("rename-link-test")
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				defer repository.Close()
				opts.InternalData = FilesystemOpts{DiskVolume: &DiskVolumeOptions{Repository: repository, Branch: ttcvolume.MainBranch}}
			}
			vfsObj := &vfs.VirtualFilesystem{}
			if err := vfsObj.Init(ctx); err != nil {
				t.Fatalf("VFS init: %v", err)
			}
			vfsObj.MustRegisterFilesystemType("tmpfs", FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{AllowUserMount: true})
			mntns, err := vfsObj.NewMountNamespace(ctx, creds, "", "tmpfs", &vfs.MountOptions{GetFilesystemOptions: opts}, nil)
			if err != nil {
				t.Fatalf("NewMountNamespace: %v", err)
			}
			defer mntns.DecRef(ctx)
			// Unlink and rename take the mount namespace from the context.
			ctx = vfs.WithMountNamespace(ctx, mntns)
			root := mntns.Root(ctx)
			defer root.DecRef(ctx)
			pop := func(name string) *vfs.PathOperation {
				return &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(name)}
			}
			create := func(name string) error {
				fd, err := vfsObj.OpenAt(ctx, creds, pop(name), &vfs.OpenOptions{Flags: linux.O_CREAT | linux.O_EXCL | linux.O_WRONLY, Mode: 0644})
				if err == nil {
					fd.DecRef(ctx)
				}
				return err
			}
			list := func() []string {
				fd, err := vfsObj.OpenAt(ctx, creds, pop("d"), &vfs.OpenOptions{Flags: linux.O_RDONLY | linux.O_DIRECTORY})
				if err != nil {
					t.Fatalf("open d: %v", err)
				}
				defer fd.DecRef(ctx)
				var names []string
				if err := fd.IterDirents(ctx, vfs.IterDirentsCallbackFunc(func(dirent vfs.Dirent) error {
					if dirent.Name != "." && dirent.Name != ".." {
						names = append(names, dirent.Name)
					}
					return nil
				})); err != nil {
					t.Fatalf("IterDirents: %v", err)
				}
				slices.Sort(names)
				return names
			}
			// A directory of the filesystem's own, as the configure check's
			// conftest directory: it does not look names up in the volume.
			if err := vfsObj.MkdirAt(ctx, creds, pop("d"), &vfs.MkdirOptions{Mode: 0755}); err != nil {
				t.Fatalf("mkdir d: %v", err)
			}
			if err := create("d/f"); err != nil {
				t.Fatalf("create d/f: %v", err)
			}
			if err := vfsObj.LinkAt(ctx, creds, pop("d/f"), pop("d/f1")); err != nil {
				t.Fatalf("link d/f d/f1: %v", err)
			}
			if err := vfsObj.RenameAt(ctx, creds, pop("d/f"), pop("d/f1"), &vfs.RenameOptions{}); err != nil {
				t.Fatalf("rename d/f d/f1: %v", err)
			}
			for _, name := range []string{"d/f", "d/f1"} {
				stat, err := vfsObj.StatAt(ctx, creds, pop(name), &vfs.StatOptions{Mask: linux.STATX_NLINK})
				if err != nil || stat.Nlink != 2 {
					t.Errorf("after the rename, stat %s = (%d links, %v), want the file's 2 links", name, stat.Nlink, err)
				}
			}
			if names := list(); !slices.Equal(names, []string{"f", "f1"}) {
				t.Errorf("after the rename, d lists %q, want [f f1]", names)
			}
			if err := vfsObj.RenameAt(ctx, creds, pop("d/f1"), pop("d/f"), &vfs.RenameOptions{Flags: linux.RENAME_NOREPLACE}); !linuxerr.Equals(linuxerr.EEXIST, err) {
				t.Errorf("rename d/f1 d/f with RENAME_NOREPLACE: %v, want EEXIST", err)
			}
			for _, name := range []string{"d/f1", "d/f"} {
				if err := vfsObj.UnlinkAt(ctx, creds, pop(name)); err != nil {
					t.Fatalf("unlink %s: %v", name, err)
				}
			}
			if names := list(); len(names) != 0 {
				t.Errorf("with both names removed, d lists %q", names)
			}
			if err := create("d/f"); err != nil {
				t.Errorf("create d/f again: %v", err)
			}
		})
	}
}
