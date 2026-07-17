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
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/ttos/ownership"
)

func TestBuildSpecCombinesImageAndOverrides(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("app:x:123:456::/home/app:/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte("app:x:456:\nextra:x:789:app\n"), 0644); err != nil {
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
		Labels:     map[string]string{"keep": "yes", "suite": "image"},
	}}
	spec, err := BuildSpec(image, SpecOptions{
		Name:       "demo",
		RootFS:     root,
		Args:       []string{"override", "arg"},
		Env:        []string{"A=run", "B", "C=new"},
		Labels:     map[string]string{"empty": "", "extra": "value", "suite": "run"},
		HostMounts: []HostMount{{Source: host, Destination: "/mnt/fixture", Readonly: true}},
		ManagedVolumes: []ManagedVolume{
			{Name: "cache", Destination: "/cache"},
			{Name: "cache", Destination: "/cache-ro", Readonly: true},
		},
	})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if got, want := spec.Process.Args, []string{"/entry", "override", "arg"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args=%q, want %q", got, want)
	}
	if got, want := spec.Process.Env, []string{"A=run", "C=new"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env=%q, want %q", got, want)
	}
	if spec.Process.Cwd != "/work" || spec.Hostname != "demo" {
		t.Errorf("cwd=%q hostname=%q", spec.Process.Cwd, spec.Hostname)
	}
	if spec.Process.User.UID != 123 || spec.Process.User.GID != 456 || !reflect.DeepEqual(spec.Process.User.AdditionalGids, []uint32{789}) {
		t.Errorf("user=%+v", spec.Process.User)
	}
	wantMounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/mnt/fixture", Type: "bind", Source: host, Options: []string{"rbind", "ro"}},
		{Destination: "/cache", Type: "tmpfs", Source: "ttos-volume-cache", Options: []string{"rw", "nosuid", "nodev", "mode=0755"}},
		{Destination: "/cache-ro", Type: "tmpfs", Source: "ttos-volume-cache", Options: []string{"ro", "nosuid", "nodev", "mode=0755"}},
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
	if got := spec.Annotations[ownership.Annotation]; got != ownership.ManifestPath(root) {
		t.Errorf("ownership annotation=%q, want %q", got, ownership.ManifestPath(root))
	}
	if got := spec.Annotations["dev.timetravelos.internal-console"]; got != "init" {
		t.Errorf("internal console=%q, want init", got)
	}
	if got := spec.Annotations["dev.timetravelos.config.network"]; got != "none" {
		t.Errorf("network annotation=%q, want none", got)
	}
	wantVolumeAnnotations := map[string]string{
		"dev.gvisor.spec.mount.ttos0.source":  "ttos-volume-cache",
		"dev.gvisor.spec.mount.ttos0.share":   "container",
		"dev.gvisor.spec.mount.ttos0.type":    "tmpfs",
		"dev.gvisor.spec.mount.ttos0.options": "rw,nosuid,nodev,mode=0755",
	}
	for key, want := range wantVolumeAnnotations {
		if got := spec.Annotations[key]; got != want {
			t.Errorf("annotation %q=%q, want %q", key, got, want)
		}
	}
	wantConfigAnnotations := map[string]string{
		"dev.timetravelos.config.entrypoint": `["/entry"]`,
		"dev.timetravelos.config.cmd":        `["override","arg"]`,
		"dev.timetravelos.config.user":       "app",
		"dev.timetravelos.config.labels":     `{"empty":"","extra":"value","keep":"yes","suite":"run"}`,
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
	if got := hostSpec.Annotations["dev.timetravelos.config.network"]; got != "host" {
		t.Errorf("host network annotation=%q, want host", got)
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
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), ManagedVolumes: []ManagedVolume{{Name: "-bad", Destination: "/data"}}}); err == nil {
		t.Error("BuildSpec accepted invalid managed volume name")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), ManagedVolumes: []ManagedVolume{{Name: "data", Destination: "relative"}}}); err == nil {
		t.Error("BuildSpec accepted relative managed volume destination")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), HostMounts: []HostMount{{Source: host, Destination: "/data", Readonly: true}}, ManagedVolumes: []ManagedVolume{{Name: "data", Destination: "/data"}}}); err == nil {
		t.Error("BuildSpec accepted duplicate host/managed destination")
	}
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), NetworkMode: "bridge"}); err == nil {
		t.Error("BuildSpec accepted unsupported network mode")
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
	if containers[0].ConsoleLogMaxBytes != 8<<20 || containers[0].ConsoleLogMaxFiles != 5 {
		t.Errorf("retention after metadata update=(%d, %d), want (%d, 5)", containers[0].ConsoleLogMaxBytes, containers[0].ConsoleLogMaxFiles, 8<<20)
	}
	if containers[0].NetworkMode != "none" {
		t.Errorf("legacy network mode=%q, want none", containers[0].NetworkMode)
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

func TestVolumeStoreLifecycle(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	created, err := store.EnsureVolume("cache")
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	again, err := store.EnsureVolume("cache")
	if err != nil {
		t.Fatalf("second EnsureVolume: %v", err)
	}
	if created != again || created.Driver != "local-tmpfs" {
		t.Errorf("idempotent volume metadata: first=%+v second=%+v", created, again)
	}
	volumes, err := store.ListVolumes()
	if err != nil || len(volumes) != 1 || volumes[0].Name != "cache" {
		t.Fatalf("ListVolumes=%+v, err=%v", volumes, err)
	}

	containerDir := filepath.Join(root, "containers", "demo")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(containerDir, "container.json"), &ContainerInfo{
		Name:      "demo",
		BundleDir: bundleDir,
		State:     "created",
		CreatedAt: time.Now(),
		Volumes:   []ManagedVolume{{Name: "cache", Destination: "/cache"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveVolume("cache"); err == nil {
		t.Fatal("RemoveVolume accepted a volume referenced by a container")
	}
	if err := store.RemoveContainer("demo"); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if err := store.RemoveVolume("cache"); err != nil {
		t.Fatalf("RemoveVolume: %v", err)
	}
	if volumes, err := store.ListVolumes(); err != nil || len(volumes) != 0 {
		t.Fatalf("volumes after remove=%+v, err=%v", volumes, err)
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
	if removed != 2 {
		t.Errorf("RemoveImageRoots removed %d roots, want 2", removed)
	}
	for _, dir := range []string{amd64, arm64} {
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
