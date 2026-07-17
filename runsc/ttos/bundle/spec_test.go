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
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	ttosimages "gvisor.dev/gvisor/runsc/ttos/images"
	"gvisor.dev/gvisor/runsc/ttos/ownership"
)

func TestBuildSpecCombinesImageAndOverrides(t *testing.T) {
	pidsLimit := int64(23)
	shmSize := int64(32 * 1024 * 1024)
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
		StopSignal: "SIGQUIT",
		Labels:     map[string]string{"keep": "yes", "suite": "image"},
	}}
	spec, err := BuildSpec(image, SpecOptions{
		Name:           "demo",
		RootFS:         root,
		ReadonlyRootfs: true,
		StopSignal:     "SIGUSR1",
		PidsLimit:      &pidsLimit,
		ShmSize:        &shmSize,
		Ulimits:        []Ulimit{{Name: "core", Soft: 0, Hard: -1}, {Name: "nofile", Soft: 64, Hard: 128}},
		Args:           []string{"override", "arg"},
		Env:            []string{"A=run", "B", "C=new"},
		Labels:         map[string]string{"empty": "", "extra": "value", "suite": "run"},
		HostMounts:     []HostMount{{Source: host, Destination: "/mnt/fixture", Readonly: true}},
		FilesystemMounts: []FilesystemMount{
			{Type: "tmpfs", Destination: "/scratch", Options: []string{"rw", "mode=0700", "size=1m"}, HostConfig: true, InspectOptions: "mode=0700,size=1m"},
			{Type: "proc", Destination: "/alt-proc", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
			{Type: "sysfs", Destination: "/alt-sys", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		},
		ManagedVolumes: []ManagedVolume{
			{Name: "cache", Destination: "/cache"},
			{Name: "cache", Destination: "/cache-ro", Readonly: true},
		},
	})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if !spec.Root.Readonly {
		t.Error("root filesystem is writable")
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
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=33554432"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/scratch", Type: "tmpfs", Source: "tmpfs", Options: []string{"rw", "mode=0700", "size=1m"}},
		{Destination: "/alt-proc", Type: "proc", Source: "proc", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		{Destination: "/alt-sys", Type: "sysfs", Source: "sysfs", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
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
	if got := spec.Annotations[TmpfsConfigAnnotation]; got != `{"/scratch":"mode=0700,size=1m"}` {
		t.Errorf("tmpfs configuration annotation=%q", got)
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
	if spec.Linux.Resources == nil || spec.Linux.Resources.Pids == nil || spec.Linux.Resources.Pids.Limit != pidsLimit {
		t.Errorf("pids resources=%+v, want limit %d", spec.Linux.Resources, pidsLimit)
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
		StopSignalAnnotation:                 "SIGUSR1",
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
	if got := hostSpec.Annotations[StopSignalAnnotation]; got != "SIGQUIT" {
		t.Errorf("image stop signal annotation=%q, want SIGQUIT", got)
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
	invalidShmSize := int64(-1)
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), ShmSize: &invalidShmSize}); err == nil {
		t.Error("BuildSpec accepted a negative shared memory size")
	}
	invalidPidsLimit := int64(-2)
	if _, err := BuildSpec(image, SpecOptions{RootFS: t.TempDir(), PidsLimit: &invalidPidsLimit}); err == nil {
		t.Error("BuildSpec accepted an invalid pids limit")
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
	stored := []ttosimages.Info{
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
	if _, _, err := selectStoredImage(stored, "aaaa", ""); !errors.Is(err, ttosimages.ErrAmbiguous) {
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
