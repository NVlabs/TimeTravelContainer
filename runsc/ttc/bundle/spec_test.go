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

package bundle

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcimages "gvisor.dev/gvisor/runsc/ttc/images"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

func TestSeedManagedVolumeCopiesImageTreeDirectly(t *testing.T) {
	imageDir := t.TempDir()
	rootfs := filepath.Join(imageDir, "rootfs")
	data := filepath.Join(rootfs, "var", "lib", "app")
	if err := os.MkdirAll(filepath.Join(data, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "nested", "payload"), []byte("persistent image data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(data, "nested", "payload"), "user.ttc-seed", []byte("xattr-data"), 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(data, "nested", "payload"), filepath.Join(data, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/payload", filepath.Join(data, "symlink")); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	manifest.Entries["var/lib/app"] = ownership.Owner{UID: 10, GID: 20}
	manifest.Entries["var/lib/app/nested"] = ownership.Owner{UID: 11, GID: 21}
	manifest.Entries["var/lib/app/nested/payload"] = ownership.Owner{UID: 12, GID: 22}
	manifest.Entries["var/lib/app/hardlink"] = ownership.Owner{UID: 12, GID: 22}
	manifest.Entries["var/lib/app/symlink"] = ownership.Owner{UID: 13, GID: 23}
	manifest.Xattrs["var/lib/app/nested/payload"] = map[string][]byte{"security.capability": []byte("virtual-capability")}
	manifest.Xattrs["var/lib/app/hardlink"] = map[string][]byte{"security.capability": []byte("virtual-capability")}

	manager, err := ttcvolume.OpenManager(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("seed-test")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := seedManagedVolume(repository, rootfs, "/var/lib/app", manifest); err != nil {
		t.Fatal(err)
	}

	root, err := repository.Inode(ttcvolume.MainBranch, ttcvolume.RootInode)
	if err != nil {
		t.Fatal(err)
	}
	if root.UID != 10 || root.GID != 20 || root.Mode != 0750 {
		t.Fatalf("root metadata = uid %d gid %d mode %#o, want 10:20 0750", root.UID, root.GID, root.Mode)
	}
	nested, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "nested")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := repository.Lookup(ttcvolume.MainBranch, nested, "payload")
	if err != nil {
		t.Fatal(err)
	}
	hardlink, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "hardlink")
	if err != nil {
		t.Fatal(err)
	}
	if hardlink != payload {
		t.Fatalf("hardlink inode = %d, want payload inode %d", hardlink, payload)
	}
	got := make([]byte, len("persistent image data"))
	if _, err := repository.ReadAt(ttcvolume.MainBranch, payload, got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "persistent image data" {
		t.Fatalf("payload = %q", got)
	}
	inode, err := repository.Inode(ttcvolume.MainBranch, payload)
	if err != nil {
		t.Fatal(err)
	}
	if inode.UID != 12 || inode.GID != 22 || inode.Mode != 0640 || inode.Nlink != 2 {
		t.Fatalf("payload metadata = %+v", inode)
	}
	if value, ok := inode.Xattrs["user.ttc-seed"]; ok && value != "xattr-data" {
		t.Fatalf("payload xattr = %q, want xattr-data", value)
	}
	if value := inode.Xattrs["security.capability"]; value != "virtual-capability" {
		t.Fatalf("payload virtual capability = %q, want virtual-capability", value)
	}
	link, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "symlink")
	if err != nil {
		t.Fatal(err)
	}
	linkInode, err := repository.Inode(ttcvolume.MainBranch, link)
	if err != nil {
		t.Fatal(err)
	}
	if linkInode.Type != "symlink" || linkInode.Target != "nested/payload" {
		t.Fatalf("symlink inode = %+v", linkInode)
	}
}

func TestSeedCopiedHostMountDirectoryAndFile(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(source, "nested", "payload")
	if err := os.WriteFile(payloadPath, []byte("host-data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(payloadPath, filepath.Join(source, "hardlink")); err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("copied-directory")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedCopiedHostMount(repository, source, info); err != nil {
		t.Fatal(err)
	}
	nested, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "nested")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := repository.Lookup(ttcvolume.MainBranch, nested, "payload")
	if err != nil {
		t.Fatal(err)
	}
	hardlink, err := repository.Lookup(ttcvolume.MainBranch, ttcvolume.RootInode, "hardlink")
	if err != nil {
		t.Fatal(err)
	}
	if payload != hardlink {
		t.Fatalf("hardlink inode=%d, want %d", hardlink, payload)
	}
	got := make([]byte, len("host-data"))
	if _, err := repository.ReadAt(ttcvolume.MainBranch, payload, got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "host-data" {
		t.Fatalf("copied payload=%q", got)
	}

	fileSource := filepath.Join(t.TempDir(), "single")
	if err := os.WriteFile(fileSource, []byte("single-file"), 0600); err != nil {
		t.Fatal(err)
	}
	fileRepository, err := manager.Create("copied-file")
	if err != nil {
		t.Fatal(err)
	}
	defer fileRepository.Close()
	fileInfo, err := os.Stat(fileSource)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := hostFileOwner(fileInfo)
	if err := fileRepository.InitializeRootRegular(uint32(fileInfo.Mode().Perm()), uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := seedCopiedHostMount(fileRepository, fileSource, fileInfo); err != nil {
		t.Fatal(err)
	}
	root, err := fileRepository.Inode(ttcvolume.MainBranch, ttcvolume.RootInode)
	if err != nil {
		t.Fatal(err)
	}
	if root.Type != "regular" || root.Mode != 0600 || root.Size != int64(len("single-file")) {
		t.Fatalf("copied file root=%+v", root)
	}
	got = make([]byte, len("single-file"))
	if _, err := fileRepository.ReadAt(ttcvolume.MainBranch, ttcvolume.RootInode, got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "single-file" {
		t.Fatalf("copied file=%q", got)
	}
}

func TestBuildSpecDonatesPersistentRootfsUpper(t *testing.T) {
	rootfs := t.TempDir()
	repository := filepath.Join(t.TempDir(), "rootfs-upper")
	spec, err := BuildSpec(
		ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}},
		SpecOptions{
			Name:                      "persistent",
			RootFS:                    rootfs,
			RootfsUpperRepositoryPath: repository,
			RootfsUpperCopyMode:       string(ttcvolume.CopyModeReflink),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 {
		t.Fatalf("repositories = %+v, want one rootfs upper", repositories)
	}
	got := repositories[0]
	if got.Source != specutils.TTCRootfsUpperSource || got.Path != repository || got.Branch != ttcvolume.MainBranch || got.CopyMode != string(ttcvolume.CopyModeReflink) {
		t.Fatalf("rootfs upper repository = %+v", got)
	}
	if _, ok := spec.Annotations[specutils.AnnotationRootfsUpperTar]; ok {
		t.Fatal("generated TTC spec still has a rootfs upper tar annotation")
	}
	if _, ok := spec.Annotations[specutils.AnnotationMountTarOnExit]; ok {
		t.Fatal("generated TTC spec still has an exit-time tar annotation")
	}
}

func TestValidateManagedVolumeAttachment(t *testing.T) {
	containers := []ContainerInfo{{
		Name: "writer",
		Volumes: []ManagedVolume{{
			ID: "volume-id",
		}},
	}}
	if err := validateManagedVolumeAttachment("volume-id", ManagedVolume{Name: "data"}, containers); err == nil {
		t.Fatal("second writable attachment succeeded")
	}
	if err := validateManagedVolumeAttachment("volume-id", ManagedVolume{Name: "data", Readonly: true}, containers); err != nil {
		t.Fatalf("read-only attachment failed: %v", err)
	}
	if err := validateManagedVolumeAttachment("other-id", ManagedVolume{Name: "other"}, containers); err != nil {
		t.Fatalf("unrelated writable attachment failed: %v", err)
	}
}

func TestBuildSpecCombinesImageAndOverrides(t *testing.T) {
	pidsLimit := int64(23)
	memoryLimit := int64(256 * 1024 * 1024)
	nanoCPUs := int64(1_500_000_000)
	cpuShares := uint64(512)
	shmSize := int64(32 * 1024 * 1024)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("app:x:123:456::/home/app:/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte("app:x:456:\nextra:x:789:app\nmanual:x:900:\n"), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(root), manifest); err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, "fixture"), []byte("host-data"), 0644); err != nil {
		t.Fatal(err)
	}
	image := ocispec.Image{Config: ocispec.ImageConfig{
		User:       "app",
		Env:        []string{"A=image", "B=first", "B=keep"},
		Entrypoint: []string{"/entry"},
		Cmd:        []string{"default"},
		WorkingDir: "/work",
		StopSignal: "SIGQUIT",
		Labels:     map[string]string{"keep": "yes", "suite": "image"},
	}}
	spec, err := BuildSpec(image, SpecOptions{
		Name:           "demo",
		RootFS:         root,
		ReadonlyRootfs: true,
		Terminal:       true,
		TerminalType:   "screen-256color",
		ConsoleSize:    &specs.Box{Height: 37, Width: 91},
		StopSignal:     "SIGUSR1",
		PidsLimit:      &pidsLimit,
		MemoryLimit:    &memoryLimit,
		NanoCPUs:       &nanoCPUs,
		CPUShares:      &cpuShares,
		CgroupsPath:    "ttc/demo-id",
		ShmSize:        &shmSize,
		GroupAdd:       []string{"manual", "901", "manual"},
		CapAdd:         []string{"sys_admin"},
		CapDrop:        []string{"CAP_NET_RAW"},
		Ulimits:        []Ulimit{{Name: "core", Soft: 0, Hard: -1}, {Name: "nofile", Soft: 64, Hard: 128}},
		Args:           []string{"override", "arg"},
		Env:            []string{"A=run", "B", "C=new"},
		Labels:         map[string]string{"empty": "", "extra": "value", "suite": "run"},
		HostMounts:     []HostMount{{Source: host, Destination: "/mnt/fixture", Readonly: true, ImmutableSource: true}},
		FilesystemMounts: []FilesystemMount{
			{Type: "tmpfs", Destination: "/scratch", Options: []string{"rw", "mode=0700", "size=1m"}, HostConfig: true, InspectOptions: "mode=0700,size=1m"},
			{Type: "proc", Destination: "/alt-proc", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
			{Type: "sysfs", Destination: "/alt-sys", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		},
	})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if got, want := spec.Annotations[ImmutableHostMountsAnnotation], `["/mnt/fixture"]`; got != want {
		t.Errorf("immutable host mounts annotation=%q, want %q", got, want)
	}

	if !spec.Root.Readonly {
		t.Error("root filesystem is writable")
	}
	if !spec.Process.Terminal || spec.Process.ConsoleSize == nil || spec.Process.ConsoleSize.Height != 37 || spec.Process.ConsoleSize.Width != 91 {
		t.Errorf("terminal configuration=%+v size=%+v", spec.Process.Terminal, spec.Process.ConsoleSize)
	}
	if got, want := spec.Process.Args, []string{"/entry", "override", "arg"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args=%q, want %q", got, want)
	}
	if got, want := spec.Process.Env, []string{"A=run", "C=new", "TERM=screen-256color"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env=%q, want %q", got, want)
	}
	if spec.Process.Cwd != "/work" || spec.Hostname != "demo" {
		t.Errorf("cwd=%q hostname=%q", spec.Process.Cwd, spec.Hostname)
	}
	if spec.Process.User.UID != 123 || spec.Process.User.GID != 456 || !reflect.DeepEqual(spec.Process.User.AdditionalGids, []uint32{789, 900, 901}) {
		t.Errorf("user=%+v", spec.Process.User)
	}
	wantCapabilities := []string{"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER", "CAP_MKNOD", "CAP_SETGID", "CAP_SETUID", "CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE", "CAP_SYS_ADMIN"}
	if !reflect.DeepEqual(spec.Process.Capabilities.Bounding, wantCapabilities) {
		t.Errorf("bounding capabilities=%+v, want %q", spec.Process.Capabilities.Bounding, wantCapabilities)
	}
	var wantEffectiveCapabilities []string
	if !reflect.DeepEqual(spec.Process.Capabilities.Effective, wantEffectiveCapabilities) || !reflect.DeepEqual(spec.Process.Capabilities.Permitted, wantEffectiveCapabilities) {
		t.Errorf("non-root effective/permitted capabilities=%+v, want %q", spec.Process.Capabilities, wantEffectiveCapabilities)
	}
	wantMounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=33554432"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/scratch", Type: "tmpfs", Source: "tmpfs", Options: []string{"rw", "mode=0700", "size=1m"}},
		{Destination: "/alt-proc", Type: "proc", Source: "proc", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		{Destination: "/alt-sys", Type: "sysfs", Source: "sysfs", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		{Destination: "/mnt/fixture", Type: "bind", Source: host, Options: []string{"rbind", "ro"}},
	}
	if !reflect.DeepEqual(spec.Mounts, wantMounts) {
		t.Errorf("mounts=%+v, want %+v", spec.Mounts, wantMounts)
	}
	if got := spec.Annotations["dev.gvisor.spec.rootfs.overlay"]; got != "memory" {
		t.Errorf("root overlay=%q, want memory", got)
	}
	if got := spec.Annotations["dev.gvisor.spec.rootfs.source"]; got != root {
		t.Errorf("root source=%q, want %q", got, root)
	}
	if got := spec.Annotations["dev.gvisor.spec.rootfs.type"]; got != "bind" {
		t.Errorf("root type=%q, want bind", got)
	}
	if got := spec.Annotations[TmpfsConfigAnnotation]; got != `{"/scratch":"mode=0700,size=1m"}` {
		t.Errorf("tmpfs configuration annotation=%q", got)
	}
	if got := spec.Annotations[ownership.Annotation]; got != ownership.ManifestPath(root) {
		t.Errorf("ownership annotation=%q, want %q", got, ownership.ManifestPath(root))
	}
	if got := spec.Annotations["dev.timetravelcontainer.internal-console"]; got != "init" {
		t.Errorf("internal console=%q, want init", got)
	}
	if got := spec.Annotations["dev.timetravelcontainer.config.network"]; got != "none" {
		t.Errorf("network annotation=%q, want none", got)
	}
	if spec.Linux.Resources == nil || spec.Linux.Resources.Pids == nil || spec.Linux.Resources.Pids.Limit != pidsLimit {
		t.Errorf("pids resources=%+v, want limit %d", spec.Linux.Resources, pidsLimit)
	}
	if spec.Linux.CgroupsPath != "ttc/demo-id" {
		t.Errorf("cgroups path=%q, want ttc/demo-id", spec.Linux.CgroupsPath)
	}
	if spec.Linux.Resources.Memory == nil || spec.Linux.Resources.Memory.Limit == nil || *spec.Linux.Resources.Memory.Limit != memoryLimit {
		t.Errorf("memory resources=%+v, want limit %d", spec.Linux.Resources.Memory, memoryLimit)
	}
	if cpu := spec.Linux.Resources.CPU; cpu == nil || cpu.Quota == nil || *cpu.Quota != 150000 || cpu.Period == nil || *cpu.Period != 100000 || cpu.Shares == nil || *cpu.Shares != cpuShares {
		t.Errorf("CPU resources=%+v", cpu)
	}
	wantRlimits := []specs.POSIXRlimit{
		{Type: "RLIMIT_CORE", Soft: 0, Hard: ^uint64(0)},
		{Type: "RLIMIT_NOFILE", Soft: 64, Hard: 128},
	}
	if !reflect.DeepEqual(spec.Process.Rlimits, wantRlimits) {
		t.Errorf("rlimits=%+v, want %+v", spec.Process.Rlimits, wantRlimits)
	}
	if got := spec.Annotations[PidsLimitAnnotation]; got != "sentry" {
		t.Errorf("pids enforcement annotation=%q, want sentry", got)
	}
	wantConfigAnnotations := map[string]string{
		"dev.timetravelcontainer.config.entrypoint": `["/entry"]`,
		"dev.timetravelcontainer.config.cmd":        `["override","arg"]`,
		"dev.timetravelcontainer.config.user":       "app",
		"dev.timetravelcontainer.config.labels":     `{"empty":"","extra":"value","keep":"yes","suite":"run"}`,
		StopSignalAnnotation:                        "SIGUSR1",
	}
	for key, want := range wantConfigAnnotations {
		if got := spec.Annotations[key]; got != want {
			t.Errorf("annotation %q=%q, want %q", key, got, want)
		}
	}
	if !hasNamespace(spec, specs.NetworkNamespace) {
		t.Error("default spec omitted isolated network namespace")
	}
	hostSpec, err := BuildSpec(image, SpecOptions{RootFS: root, NetworkMode: "host"})
	if err != nil {
		t.Fatalf("BuildSpec host network: %v", err)
	}
	if hasNamespace(hostSpec, specs.NetworkNamespace) {
		t.Error("host-network spec retained an isolated network namespace")
	}
	if got := hostSpec.Annotations["dev.timetravelcontainer.config.network"]; got != "host" {
		t.Errorf("host network annotation=%q, want host", got)
	}
	if got := hostSpec.Annotations[StopSignalAnnotation]; got != "SIGQUIT" {
		t.Errorf("image stop signal annotation=%q, want SIGQUIT", got)
	}
}

func TestBuildSpecPreservesConfiguredTerminalType(t *testing.T) {
	spec, err := BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{
		Cmd: []string{"sh"},
		Env: []string{"TERM=vt100"},
	}}, SpecOptions{RootFS: t.TempDir(), Terminal: true, TerminalType: "xterm-256color"})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if got, want := spec.Process.Env, []string{"TERM=vt100"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env=%q, want configured image terminal type %q", got, want)
	}
}

func hasNamespace(spec *specs.Spec, namespace specs.LinuxNamespaceType) bool {
	if spec.Linux == nil {
		return false
	}
	for _, candidate := range spec.Linux.Namespaces {
		if candidate.Type == namespace {
			return true
		}
	}
	return false
}

func TestDockerCapabilitySemantics(t *testing.T) {
	normalized, err := NormalizeCapabilities([]string{"sys_admin", "CAP_CHOWN", "all"})
	if err != nil || !reflect.DeepEqual(normalized, []string{"SYS_ADMIN", "CHOWN", "ALL"}) {
		t.Fatalf("NormalizeCapabilities=(%q, %v)", normalized, err)
	}
	if _, err := NormalizeCapabilities([]string{"not_a_capability"}); err == nil {
		t.Fatal("NormalizeCapabilities accepted an unknown capability")
	}
	onlyChown, err := buildCapabilities([]string{"chown"}, []string{"all"})
	if err != nil || !reflect.DeepEqual(onlyChown, []string{"CAP_CHOWN"}) {
		t.Errorf("drop ALL/add CHOWN=(%q, %v)", onlyChown, err)
	}
	allButAdmin, err := buildCapabilities([]string{"all"}, []string{"sys_admin"})
	if err != nil || containsString(allButAdmin, "CAP_SYS_ADMIN") || len(allButAdmin) != len(allCapabilityNames())-1 {
		t.Errorf("add ALL/drop SYS_ADMIN=(%q, %v)", allButAdmin, err)
	}
	all, err := buildCapabilities([]string{"all"}, []string{"all"})
	if err != nil || !reflect.DeepEqual(all, allCapabilityNames()) {
		t.Errorf("add ALL/drop ALL=(%q, %v), want all", all, err)
	}
}

func TestBuildProcessCapabilitiesAreUserAware(t *testing.T) {
	rootCaps, err := buildProcessCapabilities(0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rootCaps.Bounding, defaultCapabilities) ||
		!reflect.DeepEqual(rootCaps.Effective, defaultCapabilities) ||
		!reflect.DeepEqual(rootCaps.Permitted, defaultCapabilities) {
		t.Errorf("root capabilities=%+v, want defaults in bounding/effective/permitted", rootCaps)
	}

	nonRootCaps, err := buildProcessCapabilities(1000, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nonRootCaps.Bounding, defaultCapabilities) {
		t.Errorf("non-root bounding capabilities=%q, want %q", nonRootCaps.Bounding, defaultCapabilities)
	}
	if len(nonRootCaps.Effective) != 0 || len(nonRootCaps.Permitted) != 0 {
		t.Errorf("non-root capabilities=%+v, want empty effective/permitted sets", nonRootCaps)
	}
	if !containsString(nonRootCaps.Bounding, "CAP_SETUID") || !containsString(nonRootCaps.Bounding, "CAP_SETGID") {
		t.Errorf("non-root bounding set %q does not retain SUID helper capabilities", nonRootCaps.Bounding)
	}

	explicitCaps, err := buildProcessCapabilities(1000, []string{"SYS_PTRACE"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(explicitCaps.Bounding, "CAP_SYS_PTRACE") {
		t.Errorf("non-root --cap-add capability missing from bounding set: %+v", explicitCaps)
	}
	if len(explicitCaps.Effective) != 0 || len(explicitCaps.Permitted) != 0 {
		t.Errorf("non-root --cap-add capabilities=%+v, want empty effective/permitted sets", explicitCaps)
	}

	droppedCaps, err := buildProcessCapabilities(1000, nil, []string{"SETUID", "SETGID"})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(droppedCaps.Bounding, "CAP_SETUID") || containsString(droppedCaps.Bounding, "CAP_SETGID") {
		t.Errorf("explicitly dropped capabilities remain in bounding set: %q", droppedCaps.Bounding)
	}
}

func TestParseShmSize(t *testing.T) {
	for value, want := range map[string]int64{"32m": 32 * 1024 * 1024, "1.5g": 1536 * 1024 * 1024, "0": 0} {
		got, err := ParseShmSize(value)
		if err != nil || got != want {
			t.Errorf("ParseShmSize(%q)=(%d, %v), want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", "invalid", "-1"} {
		if _, err := ParseShmSize(value); err == nil {
			t.Errorf("ParseShmSize(%q) succeeded", value)
		}
	}
}

func TestParseUlimits(t *testing.T) {
	got, err := ParseUlimits([]string{"nofile=32:64", "core=0:-1", "nofile=128"})
	if err != nil {
		t.Fatalf("ParseUlimits: %v", err)
	}
	want := []Ulimit{{Name: "core", Soft: 0, Hard: -1}, {Name: "nofile", Soft: 128, Hard: 128}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseUlimits=%+v, want %+v", got, want)
	}
	for _, value := range []string{"unknown=1", "nofile", "nofile=2:1"} {
		if _, err := ParseUlimits([]string{value}); err == nil {
			t.Errorf("ParseUlimits(%q) succeeded", value)
		}
	}
}

func TestDockerNetworkConfiguration(t *testing.T) {
	extraHosts, err := ParseExtraHosts([]string{"db:192.0.2.10", "v6=[2001:0db8::10]"})
	if err != nil {
		t.Fatalf("ParseExtraHosts: %v", err)
	}
	if want := []string{"db:192.0.2.10", "v6:2001:db8::10"}; !reflect.DeepEqual(extraHosts, want) {
		t.Errorf("extra hosts=%q, want %q", extraHosts, want)
	}
	dns, err := ParseDNS([]string{"192.0.2.53", "2001:0db8::53"})
	if err != nil {
		t.Fatalf("ParseDNS: %v", err)
	}
	if want := []string{"192.0.2.53", "2001:db8::53"}; !reflect.DeepEqual(dns, want) {
		t.Errorf("DNS=%q, want %q", dns, want)
	}
	search, err := ParseDNSSearch([]string{"example.test", "."})
	if err != nil || !reflect.DeepEqual(search, []string{"example.test", "."}) {
		t.Errorf("DNS search=(%q, %v)", search, err)
	}
	options, err := ParseDNSOptions([]string{"ndots:2", "rotate"})
	if err != nil || !reflect.DeepEqual(options, []string{"ndots:2", "rotate"}) {
		t.Errorf("DNS options=(%q, %v)", options, err)
	}
	for _, value := range []string{"missing-address", ":192.0.2.1", "bad host:192.0.2.1", "host:not-an-ip", "host:host-gateway"} {
		if _, err := ParseExtraHosts([]string{value}); err == nil {
			t.Errorf("ParseExtraHosts(%q) succeeded", value)
		}
	}
	if _, err := ParseDNS([]string{"not-an-ip"}); err == nil {
		t.Error("ParseDNS accepted an invalid address")
	}
	for _, value := range []string{"-bad.example", "bad..example", "bad domain"} {
		if _, err := ParseDNSSearch([]string{value}); err == nil {
			t.Errorf("ParseDNSSearch(%q) succeeded", value)
		}
	}
	for _, value := range []string{"", "ndots:2 rotate", "rotate\noptions timeout:1"} {
		if _, err := ParseDNSOptions([]string{value}); err == nil {
			t.Errorf("ParseDNSOptions(%q) succeeded", value)
		}
	}
}

func TestNetworkFileMaterialization(t *testing.T) {
	containerDir := t.TempDir()
	opts := SpecOptions{
		Name: "demo", Hostname: "custom-host",
		ExtraHosts: []string{"db:192.0.2.10", "v6:2001:db8::10"},
		DNS:        []string{"203.0.113.53"}, DNSSearch: []string{"example.test"}, DNSOptions: []string{"ndots:2", "rotate"},
	}
	mounts, err := materializeNetworkFiles(containerDir, opts)
	if err != nil {
		t.Fatalf("materializeNetworkFiles: %v", err)
	}
	if len(mounts) != 3 {
		t.Fatalf("mounts=%+v, want three", mounts)
	}
	wantFiles := map[string]string{
		"/etc/hostname":    "custom-host\n",
		"/etc/hosts":       "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\nfe00::\tip6-localnet\nff00::\tip6-mcastprefix\nff02::1\tip6-allnodes\nff02::2\tip6-allrouters\n192.0.2.10\tdb\n2001:db8::10\tv6\n",
		"/etc/resolv.conf": "nameserver 203.0.113.53\nsearch example.test\noptions ndots:2 rotate\n",
	}
	for _, mount := range mounts {
		if !mount.Readonly || !mount.Internal {
			t.Errorf("network mount is not immutable/internal: %+v", mount)
		}
		data, err := os.ReadFile(mount.Source)
		if err != nil {
			t.Errorf("reading %s: %v", mount.Source, err)
			continue
		}
		if got, want := string(data), wantFiles[mount.Destination]; got != want {
			t.Errorf("%s contents=%q, want %q", mount.Destination, got, want)
		}
		if info, err := os.Stat(mount.Source); err != nil || info.Mode().Perm() != 0444 {
			t.Errorf("%s mode: info=%v err=%v", mount.Source, info, err)
		}
	}
	spec, err := BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}, SpecOptions{RootFS: t.TempDir(), HostMounts: mounts})
	if err != nil {
		t.Fatalf("BuildSpec with network files: %v", err)
	}
	if got, want := spec.Annotations[InternalMountsAnnotation], `["/etc/hostname","/etc/hosts","/etc/resolv.conf"]`; got != want {
		t.Errorf("internal mounts annotation=%q, want %q", got, want)
	}
}

func TestEnsureNetworkMountpoints(t *testing.T) {
	rootfs := t.TempDir()
	etc := filepath.Join(rootfs, "etc")
	if err := os.Mkdir(etc, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "hostname"), []byte("image-value"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../run/hosts", filepath.Join(etc, "hosts")); err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(rootfs), manifest); err != nil {
		t.Fatal(err)
	}
	if err := ensureNetworkMountpoints(rootfs); err != nil {
		t.Fatalf("ensureNetworkMountpoints: %v", err)
	}
	if err := ensureNetworkMountpoints(rootfs); err != nil {
		t.Fatalf("idempotent ensureNetworkMountpoints: %v", err)
	}
	for _, base := range []string{"hostname", "hosts", "resolv.conf"} {
		name := filepath.Join(etc, base)
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0644 {
			t.Errorf("mountpoint %s: info=%v err=%v", base, info, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(etc, "hostname")); err != nil || string(data) != "image-value" {
		t.Errorf("existing hostname contents=%q, err=%v", data, err)
	}
	updated, err := ownership.Read(ownership.ManifestPath(rootfs))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"etc/hostname", "etc/hosts", "etc/resolv.conf"} {
		if owner, ok := updated.Entries[name]; !ok || owner != (ownership.Owner{}) {
			t.Errorf("mountpoint owner %s=(%+v, %t), want root", name, owner, ok)
		}
	}
}

func TestBuildResolvConf(t *testing.T) {
	host := []byte("# generated\nnameserver 192.0.2.1\ndomain host.test\noptions timeout:1 rotate\n")
	if got, want := buildResolvConf(host, nil, nil, nil), "nameserver 192.0.2.1\nsearch host.test\noptions timeout:1 rotate\n"; got != want {
		t.Errorf("inherited resolver=%q, want %q", got, want)
	}
	if got, want := buildResolvConf(host, []string{"203.0.113.53"}, []string{"custom.test"}, []string{"ndots:3"}), "nameserver 203.0.113.53\nsearch custom.test\noptions ndots:3\n"; got != want {
		t.Errorf("overridden resolver=%q, want %q", got, want)
	}
	if got, want := buildResolvConf(nil, nil, nil, nil), "nameserver 127.0.0.1\n"; got != want {
		t.Errorf("fallback resolver=%q, want %q", got, want)
	}
	if got, want := buildResolvConfForNetwork([]byte("nameserver 127.0.0.53\n"), nil, nil, nil, "ttc"), "nameserver 10.0.2.53\n"; got != want {
		t.Errorf("TTC host stub resolver=%q, want %q", got, want)
	}
	if got, want := buildResolvConfForNetwork(nil, []string{"127.0.0.1", "203.0.113.53"}, nil, nil, "ttc"), "nameserver 10.0.2.2\nnameserver 203.0.113.53\n"; got != want {
		t.Errorf("TTC mixed resolver=%q, want %q", got, want)
	}
}

func TestBuildSpecRejectsInvalidOverrides(t *testing.T) {
	image := ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), Env: []string{"=INVALID"}}); err == nil {
		t.Error("BuildSpec accepted invalid environment override")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), Cwd: "relative"}); err == nil {
		t.Error("BuildSpec accepted relative working directory")
	}
	host := t.TempDir()
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{{Source: host, Destination: "/data"}}}); err == nil {
		t.Error("BuildSpec accepted writable host mount")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{{Source: "relative", Destination: "/data", Readonly: true}}}); err == nil {
		t.Error("BuildSpec accepted relative host source")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{{Source: host, Destination: "relative", Readonly: true}}}); err == nil {
		t.Error("BuildSpec accepted relative host destination")
	}
	missing := filepath.Join(host, "missing")
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{{Source: missing, Destination: "/data", Readonly: true}}}); err == nil {
		t.Error("BuildSpec accepted missing host source")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "ramfs", Destination: "/data"}}}); err == nil {
		t.Error("BuildSpec accepted unsupported filesystem mount type")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "tmpfs", Destination: "relative"}}}); err == nil {
		t.Error("BuildSpec accepted relative filesystem mount destination")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "tmpfs", Destination: "/data", Readonly: true, Options: []string{"rw"}}}}); err == nil {
		t.Error("BuildSpec accepted a conflicting filesystem access option")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "tmpfs", Destination: "/data", Options: []string{"rw", "ro"}}}}); err == nil {
		t.Error("BuildSpec accepted multiple filesystem access options")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "tmpfs", Destination: "/proc"}}}); err == nil {
		t.Error("BuildSpec accepted a filesystem mount over a default destination")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), FilesystemMounts: []FilesystemMount{{Type: "proc", Destination: "/alt-proc", HostConfig: true}}}); err == nil {
		t.Error("BuildSpec accepted Docker tmpfs projection for procfs")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), NetworkMode: "bridge"}); err == nil {
		t.Error("BuildSpec accepted unsupported network mode")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), StopSignal: "SIGINVALID"}); err == nil {
		t.Error("BuildSpec accepted an invalid stop signal")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), GroupAdd: []string{"missing-group"}}); err == nil {
		t.Error("BuildSpec accepted an unknown additional group")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), CapAdd: []string{"not-a-capability"}}); err == nil {
		t.Error("BuildSpec accepted an unknown capability")
	}
	invalidShmSize := int64(-1)
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), ShmSize: &invalidShmSize}); err == nil {
		t.Error("BuildSpec accepted a negative shared memory size")
	}
	invalidPidsLimit := int64(-2)
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), PidsLimit: &invalidPidsLimit}); err == nil {
		t.Error("BuildSpec accepted an invalid pids limit")
	}
	invalidMemoryLimit := int64(-1)
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), MemoryLimit: &invalidMemoryLimit}); err == nil {
		t.Error("BuildSpec accepted an invalid memory limit")
	}
	unlimitedPids := int64(-1)
	unlimitedSpec, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), PidsLimit: &unlimitedPids})
	if err != nil {
		t.Fatalf("BuildSpec rejected unlimited pids: %v", err)
	}
	if unlimitedSpec.Linux.Resources != nil || unlimitedSpec.Annotations[PidsLimitAnnotation] != "" {
		t.Errorf("unlimited pids unexpectedly enabled enforcement: resources=%+v annotation=%q", unlimitedSpec.Linux.Resources, unlimitedSpec.Annotations[PidsLimitAnnotation])
	}
}

func TestBuildSpecRequiresResolvedImageVolumes(t *testing.T) {
	image := ocispec.Image{Config: ocispec.ImageConfig{
		Cmd:     []string{"true"},
		Volumes: map[string]struct{}{"/data": {}},
	}}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not resolved") {
		t.Fatalf("BuildSpec image volume error=%v, want unresolved error", err)
	}
	if _, err := BuildSpec(image, SpecOptions{
		RootFS: t.TempDir(),
		FilesystemMounts: []FilesystemMount{{
			Type: "tmpfs", Destination: "/data", Options: []string{"rw"},
		}},
	}); err != nil {
		t.Fatalf("BuildSpec with explicit replacement mount: %v", err)
	}
	spec, err := BuildSpec(image, SpecOptions{
		RootFS: t.TempDir(),
		ManagedVolumes: []ManagedVolume{{
			Name: "data", ID: "0123456789abcdef0123456789abcdef",
			Destination: "/data", RepositoryPath: "/repository", CopyMode: "reflink",
		}},
	})
	if err != nil {
		t.Fatalf("BuildSpec with resolved managed volume: %v", err)
	}
	if got := spec.Annotations["dev.timetravelcontainer.volume-repositories"]; !strings.Contains(got, `"/repository"`) {
		t.Fatalf("volume repository annotation=%q", got)
	}
}

func TestBuildSpecRejectsOverflowingNumericIDsBeforeImageLookup(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	overflow := strings.Repeat("9", 64)
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte(fmt.Sprintf("%s:x:0:0::/root:/bin/sh\n", overflow)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte(fmt.Sprintf("%s:x:0:\n", overflow)), 0644); err != nil {
		t.Fatal(err)
	}
	image := ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}
	for _, tc := range []struct {
		name string
		opts SpecOptions
	}{
		{
			name: "user",
			opts: SpecOptions{User: overflow},
		},
		{
			name: "primary group",
			opts: SpecOptions{User: "123:" + overflow},
		},
		{
			name: "additional group",
			opts: SpecOptions{GroupAdd: []string{overflow}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.RootFS = root
			if _, err := BuildSpec(image, tc.opts); err == nil {
				t.Fatalf("BuildSpec accepted overflowing numeric ID %q through image name lookup", overflow)
			}
		})
	}
}

func TestBuildSpecAcceptsMaximumNumericUserAndGroupIDs(t *testing.T) {
	spec, err := BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{
		Cmd:  []string{"true"},
		User: "2147483647:2147483647",
	}}, SpecOptions{RootFS: t.TempDir()})
	if err != nil {
		t.Fatalf("BuildSpec rejected maximum numeric IDs: %v", err)
	}
	if got, want := spec.Process.User.UID, uint32(maxContainerID); got != want {
		t.Errorf("UID=%d, want %d", got, want)
	}
	if got, want := spec.Process.User.GID, uint32(maxContainerID); got != want {
		t.Errorf("GID=%d, want %d", got, want)
	}
}

func TestPullPolicyAndStoredImageSelection(t *testing.T) {
	for _, policy := range []string{"", "missing", "always", "never"} {
		got, err := normalizePullPolicy(policy)
		if err != nil {
			t.Errorf("normalizePullPolicy(%q): %v", policy, err)
		}
		if policy == "" && got != "missing" || policy != "" && got != policy {
			t.Errorf("normalizePullPolicy(%q)=%q", policy, got)
		}
	}
	if _, err := normalizePullPolicy("sometimes"); err == nil {
		t.Fatal("normalizePullPolicy accepted an unsupported policy")
	}

	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:aaaa" + strings.Repeat("b", 60)
	stored := []ttcimages.Info{
		{Reference: "docker.io/library/alpine:3.20", Digest: digestA, Platform: "linux/amd64"},
		{Reference: "docker.io/library/alpine:latest", Digest: digestA, Platform: "linux/amd64"},
		{Reference: "docker.io/library/alternate:latest", Digest: digestB, Platform: "linux/arm64"},
	}
	got, found, err := selectStoredImage(stored, "alpine:3.20", "linux/amd64")
	if err != nil || !found || got.Reference != "docker.io/library/alpine:3.20" {
		t.Fatalf("selectStoredImage by name=(%+v, %t, %v)", got, found, err)
	}
	got, found, err = selectStoredImage(stored, strings.Repeat("a", 12), "linux/amd64")
	if err != nil || !found || got.Digest != digestA {
		t.Fatalf("selectStoredImage by digest=(%+v, %t, %v)", got, found, err)
	}
	if _, found, err := selectStoredImage(stored, "alpine:3.20", "linux/arm64"); err != nil || found {
		t.Errorf("platform mismatch=(found=%t, err=%v)", found, err)
	}
	if _, found, err := selectStoredImage(stored, "missing:latest", ""); err != nil || found {
		t.Errorf("missing image=(found=%t, err=%v)", found, err)
	}
	if _, _, err := selectStoredImage(stored, "aaaa", ""); !errors.Is(err, ttcimages.ErrAmbiguous) {
		t.Errorf("ambiguous prefix error=%v", err)
	}
}

func TestValidateName(t *testing.T) {
	for _, name := range []string{"web", "web-1", "Web_1.test"} {
		if err := validateName(name); err != nil {
			t.Errorf("validateName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "-bad", "bad/name", "bad@branch"} {
		if err := validateName(name); err == nil {
			t.Errorf("validateName(%q) succeeded", name)
		}
	}
}

func TestWriteJSONAtomicallyReplacesMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.json")
	if err := writeJSON(path, map[string]int{"generation": 1}); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := writeJSON(path, map[string]int{"generation": 2}); err != nil {
		t.Fatal(err)
	}
	oldData, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	newData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oldValue, newValue map[string]int
	if err := json.Unmarshal(oldData, &oldValue); err != nil {
		t.Fatalf("old metadata is incomplete: %v", err)
	}
	if err := json.Unmarshal(newData, &newValue); err != nil {
		t.Fatalf("new metadata is incomplete: %v", err)
	}
	if oldValue["generation"] != 1 || newValue["generation"] != 2 {
		t.Errorf("metadata generations old=%v new=%v", oldValue, newValue)
	}
}

func TestSelectLogAttributes(t *testing.T) {
	spec := &specs.Spec{
		Process:     &specs.Process{Env: []string{"APP_ENV=image", "TRACE_ID=abc", "APP_ENV=runtime"}},
		Annotations: map[string]string{"dev.timetravelcontainer.config.labels": `{"suite":"fast","owner":"ttc","skip":"no"}`},
	}
	got, err := selectLogAttributes(map[string]string{
		"labels": "suite", "labels-regex": "^owner$", "env": "APP_ENV", "env-regex": "^TRACE_",
	}, spec)
	if err != nil {
		t.Fatalf("selectLogAttributes: %v", err)
	}
	want := map[string]string{"suite": "fast", "owner": "ttc", "APP_ENV": "runtime", "TRACE_ID": "abc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attributes=%q, want %q", got, want)
	}
	if _, err := selectLogAttributes(map[string]string{"env-regex": "["}, spec); err == nil {
		t.Fatal("selectLogAttributes accepted an invalid regex")
	}
}

func TestStoreLifecycleMetadata(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	containerDir := filepath.Join(root, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"process":{"args":["true"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &ContainerInfo{Name: "demo", BundleDir: bundleDir, ConsoleLogMaxBytes: 8 << 20, ConsoleLogMaxFiles: 5, State: "created", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	status := 17
	if err := store.UpdateState("demo", "exited", &status); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	containers, err := store.ListContainers()
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers) != 1 || containers[0].State != "exited" || containers[0].ExitStatus == nil || *containers[0].ExitStatus != 17 || containers[0].ExitedAt == nil {
		t.Fatalf("containers=%+v", containers)
	}
	spec, err := store.LoadSpec("demo")
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	if spec.Process == nil || !reflect.DeepEqual(spec.Process.Args, []string{"true"}) {
		t.Errorf("loaded spec=%+v", spec)
	}
	if len(containers[0].Branches) != 1 || containers[0].Branches[0].ID != "main" || containers[0].Branches[0].State != "exited" {
		t.Errorf("legacy branch history=%+v, want exited main", containers[0].Branches)
	}
	childCreated := time.Now().UTC()
	child := BranchInfo{ID: "child", ParentID: "main", State: "running", CreatedAt: childCreated, ForkTotalNS: 123}
	if err := store.RecordBranch("demo", child); err != nil {
		t.Fatalf("RecordBranch: %v", err)
	}
	if err := store.RecordBranch("demo", BranchInfo{ID: "grandchild", ParentID: "child", State: "paused", CreatedAt: childCreated.Add(time.Second)}); err != nil {
		t.Fatalf("RecordBranch grandchild: %v", err)
	}
	if err := store.MarkBranchDeleted("demo", "child"); err != nil {
		t.Fatalf("MarkBranchDeleted: %v", err)
	}
	withHistory, err := store.LoadContainer("demo")
	if err != nil {
		t.Fatalf("LoadContainer with lineage: %v", err)
	}
	if len(withHistory.Branches) != 3 || withHistory.Branches[1].State != "deleted" || withHistory.Branches[1].DeletedAt == nil || withHistory.Branches[2].ParentID != "child" {
		t.Fatalf("persisted lineage=%+v", withHistory.Branches)
	}
	if err := store.RecordBranch("demo", BranchInfo{ID: "child", ParentID: "other", State: "running"}); err == nil {
		t.Fatal("RecordBranch accepted a changed historical parent")
	}
	if err := store.UpdateState("demo", "exited", &status); err != nil {
		t.Fatalf("UpdateState with descendants: %v", err)
	}
	withHistory, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatalf("LoadContainer after family exit: %v", err)
	}
	if withHistory.Branches[1].State != "deleted" || withHistory.Branches[2].State != "exited" {
		t.Errorf("family exit states=%+v, want tombstone plus exited descendant", withHistory.Branches)
	}
	if containers[0].ConsoleLogMaxBytes != 8<<20 || containers[0].ConsoleLogMaxFiles != 5 {
		t.Errorf("retention after metadata update=(%d, %d), want (%d, 5)", containers[0].ConsoleLogMaxBytes, containers[0].ConsoleLogMaxFiles, 8<<20)
	}
	if containers[0].LogDriver != DefaultLogDriver {
		t.Errorf("legacy log driver=%q, want %q", containers[0].LogDriver, DefaultLogDriver)
	}
	if containers[0].NetworkMode != "none" {
		t.Errorf("legacy network mode=%q, want none", containers[0].NetworkMode)
	}
	if containers[0].StopSignal != DefaultStopSignal || containers[0].StopTimeout == nil || *containers[0].StopTimeout != DefaultStopTimeoutSeconds {
		t.Errorf("legacy stop policy=(%q, %v), want (%q, %d)", containers[0].StopSignal, containers[0].StopTimeout, DefaultStopSignal, DefaultStopTimeoutSeconds)
	}
	if containers[0].ShmSize == nil || *containers[0].ShmSize != DefaultShmSizeBytes {
		t.Errorf("legacy shared memory size=%v, want %d", containers[0].ShmSize, DefaultShmSizeBytes)
	}
	paths, err := store.BranchConsolePaths("demo", "test-a")
	if err != nil {
		t.Fatalf("BranchConsolePaths: %v", err)
	}
	wantBase := filepath.Join(containerDir, "console", "test-a")
	if paths.Log != wantBase+".log" || paths.Lock != wantBase+".lock" ||
		paths.StdinLock != wantBase+".stdin.lock" || paths.Error != wantBase+".error.log" ||
		paths.WaitStatus != wantBase+".wait-status" {
		t.Errorf("console paths=%+v, want base %q", paths, wantBase)
	}
	if info, err := os.Stat(filepath.Dir(paths.Log)); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Errorf("console directory: info=%v err=%v", info, err)
	}
	execPaths, err := store.ExecConsolePaths("demo", "test-a", "exec-1")
	if err != nil {
		t.Fatalf("ExecConsolePaths: %v", err)
	}
	wantExecBase := filepath.Join(containerDir, "console", "exec", "test-a", "exec-1")
	if execPaths.Log != wantExecBase+".log" || execPaths.WaitStatus != wantExecBase+".wait-status" {
		t.Errorf("exec console paths=%+v, want base %q", execPaths, wantExecBase)
	}
	if _, err := store.ExecConsolePaths("demo", "test-a", "../escape"); err == nil {
		t.Fatal("ExecConsolePaths accepted invalid exec ID")
	}

	if _, err := store.BranchConsolePaths("demo", "../escape"); err == nil {
		t.Fatal("BranchConsolePaths accepted invalid branch")
	}
	if _, err := store.BranchConsolePaths("missing", "main"); err == nil {
		t.Fatal("BranchConsolePaths accepted missing container")
	}
	if err := store.RemoveContainer("demo"); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if containers, err := store.ListContainers(); err != nil || len(containers) != 0 {
		t.Fatalf("containers after remove=%+v, err=%v", containers, err)
	}
}

func TestStoreBranchPortBindings(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	containerDir := filepath.Join(root, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"process":{"args":["true"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	info := ContainerInfo{
		ID: "demo", Name: "demo", BundleDir: bundleDir, NetworkMode: "ttc", State: "running", CreatedAt: time.Now(),
		Branches:       []BranchInfo{{ID: "main", State: "running"}, {ID: "child", ParentID: "main", State: "running"}},
		PublishedPorts: []PortBinding{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &info); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.PublishedPorts[0].BranchID; got != "main" {
		t.Fatalf("legacy binding branch=%q, want main", got)
	}
	child := PortBinding{HostIP: "127.0.0.1", HostPort: 8081, ContainerPort: 80, Protocol: "tcp", BranchID: "child"}
	if err := store.AddPortBinding("demo", child); err != nil {
		t.Fatalf("AddPortBinding: %v", err)
	}
	if err := store.AddPortBinding("demo", PortBinding{HostIP: "127.0.0.1", HostPort: 8081, ContainerPort: 81, Protocol: "tcp", BranchID: "main"}); err == nil {
		t.Fatal("AddPortBinding accepted a duplicate host endpoint")
	}
	if err := store.RemovePortBinding("demo", child); err != nil {
		t.Fatalf("RemovePortBinding: %v", err)
	}
	if err := store.AddPortBinding("demo", child); err != nil {
		t.Fatalf("re-adding child binding: %v", err)
	}
	if err := store.MarkBranchDeleted("demo", "child"); err != nil {
		t.Fatalf("MarkBranchDeleted: %v", err)
	}
	loaded, err = store.LoadContainer("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.PublishedPorts) != 1 || loaded.PublishedPorts[0].Branch() != "main" {
		t.Fatalf("bindings after branch deletion=%+v, want main only", loaded.PublishedPorts)
	}
}

func TestContainerIdentityResolution(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	generatedID, generatedDir, err := store.createContainerDir(nil)
	if err != nil {
		t.Fatalf("createContainerDir: %v", err)
	}
	if len(generatedID) != 2*containerIDBytes {
		t.Fatalf("generated ID length=%d, want %d", len(generatedID), 2*containerIDBytes)
	}
	if _, err := hex.DecodeString(generatedID); err != nil {
		t.Fatalf("generated ID %q is not hexadecimal: %v", generatedID, err)
	}
	if filepath.Base(generatedDir) != generatedID {
		t.Fatalf("generated directory=%q, want ID basename", generatedDir)
	}
	if err := os.RemoveAll(generatedDir); err != nil {
		t.Fatal(err)
	}

	writeRecord := func(key, id, name string) string {
		t.Helper()
		containerDir := filepath.Join(root, "containers", key)
		bundleDir := filepath.Join(containerDir, "bundle")
		if err := os.MkdirAll(bundleDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(containerDir, "container.json"), &ContainerInfo{
			ID: id, Name: name, BundleDir: bundleDir, State: "created", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		return containerDir
	}
	idAlpha := "abc1" + strings.Repeat("a", 60)
	idBeta := "abc2" + strings.Repeat("b", 60)
	idNamedPrefix := strings.Repeat("c", 64)
	alphaDir := writeRecord(idAlpha, idAlpha, "alpha")
	betaDir := writeRecord(idBeta, idBeta, "beta")
	writeRecord(idNamedPrefix, idNamedPrefix, "abc")
	legacyDir := writeRecord("legacy", "", "legacy")

	for identifier, wantID := range map[string]string{
		"alpha": idAlpha, idAlpha: idAlpha, idAlpha[:4]: idAlpha,
		"abc": idNamedPrefix, "legacy": "legacy", idNamedPrefix: idNamedPrefix, idNamedPrefix[:8]: idNamedPrefix,
	} {
		info, err := store.LoadContainer(identifier)
		if err != nil {
			t.Errorf("LoadContainer(%q): %v", identifier, err)
		} else if info.ID != wantID {
			t.Errorf("LoadContainer(%q).ID=%q, want %q", identifier, info.ID, wantID)
		}
	}
	if _, err := store.LoadContainer("ab"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous ID prefix error=%v", err)
	}
	paths, err := store.BranchConsolePaths("alpha", "main")
	if err != nil {
		t.Fatalf("BranchConsolePaths by name: %v", err)
	}
	if !strings.HasPrefix(paths.Log, filepath.Join(alphaDir, "console")+string(filepath.Separator)) {
		t.Errorf("console path=%q, want under ID directory %q", paths.Log, alphaDir)
	}
	status := 9
	if err := store.UpdateState(idAlpha[:4], "exited", &status); err != nil {
		t.Fatalf("UpdateState by prefix: %v", err)
	}
	if info, err := store.LoadContainer("alpha"); err != nil || info.ExitStatus == nil || *info.ExitStatus != status {
		t.Fatalf("updated container=%+v, err=%v", info, err)
	}
	if err := store.RemoveContainer("beta"); err != nil {
		t.Fatalf("RemoveContainer by name: %v", err)
	}
	if _, err := os.Stat(betaDir); !os.IsNotExist(err) {
		t.Errorf("removed ID directory still exists: %v", err)
	}
	if info, err := store.LoadContainer("legacy"); err != nil || info.ID != "legacy" {
		t.Fatalf("legacy record=%+v, err=%v", info, err)
	}
	if _, err := os.Stat(legacyDir); err != nil {
		t.Errorf("legacy directory changed: %v", err)
	}
}

func TestImageRootInventoryAndRemoval(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	makeRoot := func(imageDigest, platform string) string {
		t.Helper()
		dir := filepath.Join(root, "images", imageRootKey(imageDigest, platform))
		if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		manifest := ownership.NewManifest()
		manifest.Entries["."] = ownership.Owner{}
		if err := ownership.Write(ownership.ManifestPath(filepath.Join(dir, "rootfs")), manifest); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	amd64 := makeRoot(digest, "linux/amd64")
	arm64 := makeRoot(digest, "linux/arm64/v8")
	other := makeRoot(otherDigest, "linux/amd64")
	legacy := filepath.Join(root, "images", safeComponent("v2-"+digest+"-linux/386"))
	if err := os.MkdirAll(filepath.Join(legacy, "rootfs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "image.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	legacyManifest := ownership.NewManifest()
	legacyManifest.Entries["."] = ownership.Owner{}
	if err := ownership.Write(ownership.ManifestPath(filepath.Join(legacy, "rootfs")), legacyManifest); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{amd64, arm64, legacy} {
		restricted := filepath.Join(dir, "rootfs", "restricted")
		if err := os.Mkdir(restricted, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(restricted, "payload"), []byte("x"), 0440); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(restricted, 0550); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(root, "images", imageRootKey(digest, "linux/amd64")+".tmp-stale")
	staleRestricted := filepath.Join(stale, "rootfs", "restricted")
	if err := os.MkdirAll(staleRestricted, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleRestricted, "payload"), []byte("x"), 0440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staleRestricted, 0550); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleImageRootTemps(filepath.Join(root, "images"), imageRootKey(digest, "linux/amd64")); err != nil {
		t.Fatalf("removeStaleImageRootTemps: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale restrictive image root still exists: %v", err)
	}

	for _, platform := range []string{"linux/amd64", "linux/arm64/v8"} {
		found, err := store.HasImageRoot(digest, platform)
		if err != nil || !found {
			t.Fatalf("HasImageRoot(%q)=(%t, %v), want true", platform, found, err)
		}
	}
	if found, err := store.HasImageRoot(digest, "linux/s390x"); err != nil || found {
		t.Fatalf("HasImageRoot(missing)=(%t, %v), want false", found, err)
	}
	removed, err := store.RemoveImageRoots(digest)
	if err != nil {
		t.Fatalf("RemoveImageRoots: %v", err)
	}
	if removed != 3 {
		t.Errorf("RemoveImageRoots removed %d roots, want 3", removed)
	}
	for _, dir := range []string{amd64, arm64, legacy} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("removed image root %q still exists: %v", dir, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("unrelated image root was removed: %v", err)
	}
	if _, err := store.RemoveImageRoots(""); err == nil {
		t.Fatal("RemoveImageRoots accepted an empty digest")
	}
}

func TestBuildSpecManagedCoWHostMount(t *testing.T) {
	host := t.TempDir()
	image := ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}
	spec, err := BuildSpec(image, SpecOptions{
		RootFS: t.TempDir(),
		HostMounts: []HostMount{{
			Source: host, Destination: "/data", ImmutableSource: true, ManagedCoW: true,
			RepositoryID:   "0123456789abcdef0123456789abcdef",
			RepositoryPath: "/repository", CopyMode: "reflink",
		}},
	})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	mount := spec.Mounts[len(spec.Mounts)-1]
	if got, want := mount.Options, []string{"rbind", "rw"}; !reflect.DeepEqual(got, want) {
		t.Errorf("managed CoW mount options=%q, want %q", got, want)
	}
	if got, want := spec.Annotations[ImmutableHostMountsAnnotation], `["/data"]`; got != want {
		t.Errorf("immutable annotation=%q, want %q", got, want)
	}
	if got, want := spec.Annotations[ManagedCoWHostMountsAnnotation], `["/data"]`; got != want {
		t.Errorf("managed CoW annotation=%q, want %q", got, want)
	}
	for _, invalid := range []HostMount{
		{Source: host, Destination: "/data", ManagedCoW: true},
		{
			Source: host, Destination: "/data", Readonly: true, ManagedCoW: true,
			RepositoryID:   "0123456789abcdef0123456789abcdef",
			RepositoryPath: "/repository", CopyMode: "reflink",
		},
	} {
		if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{invalid}}); err == nil {
			t.Errorf("BuildSpec accepted invalid managed CoW mount %+v", invalid)
		}
	}
}

func TestBuildSpecCopiedHostMount(t *testing.T) {
	host := t.TempDir()
	spec, err := BuildSpec(ocispec.Image{Config: ocispec.ImageConfig{Cmd: []string{"true"}}}, SpecOptions{
		RootFS: t.TempDir(),
		HostMounts: []HostMount{{
			Source: host, Destination: "/data", Copy: true,
			RepositoryID:   "0123456789abcdef0123456789abcdef",
			RepositoryPath: "/repository", CopyMode: "reflink",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mount := spec.Mounts[len(spec.Mounts)-1]
	if mount.Type != "ttcvol" || mount.Destination != "/data" || !reflect.DeepEqual(mount.Options, []string{"rw"}) {
		t.Fatalf("copied mount=%+v", mount)
	}
	if got, want := spec.Annotations[CopiedHostMountsAnnotation], `{"`+"/data"+`":"`+host+`"}`; got != want {
		t.Errorf("copied annotation=%q, want %q", got, want)
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || !repositories[0].Private || repositories[0].OverlayDestination != "" {
		t.Errorf("copied repositories=%+v", repositories)
	}
}

func TestCopyRestoreHostDirectory(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "data"), []byte("value"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/data", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err := copyRestoreHostPath(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "nested", "data"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "value" {
		t.Fatalf("restored data=%q", data)
	}
	target, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "nested/data" {
		t.Fatalf("restored symlink=%q", target)
	}
}
