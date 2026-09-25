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

package cmd

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

const (
	copyContentsPAXKey   = "TTC.source-contents"
	copyHelperArg        = "_cp-helper"
	copyPreparedInputArg = "--ttc-prepared-input"
)

type copyCommandOptions struct {
	archive       bool
	followLink    bool
	preparedInput bool
}

type copyContainerEndpoint struct {
	target selector
	path   string
}

type copyCommandBackend interface {
	copyInput() io.Reader
	exportContainerPath(target selector, source string, followLink bool, output io.Writer) error
	importContainerPath(target selector, destination string, preserveOwner, rawArchive bool, input io.Reader) error
}

func parseCopyCommandArgs(args []string) (copyCommandOptions, string, string, error) {
	var opts copyCommandOptions
	index := 0
	for index < len(args) {
		arg := args[index]
		if arg == "--" {
			index++
			break
		}
		if arg == "--archive" || arg == "-a" {
			opts.archive = true
			index++
			continue
		}
		if arg == "--follow-link" || arg == "-L" {
			opts.followLink = true
			index++
			continue
		}
		if arg == copyPreparedInputArg {
			opts.preparedInput = true
			index++
			continue
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
			valid := true
			for _, flag := range arg[1:] {
				switch flag {
				case 'a':
					opts.archive = true
				case 'L':
					opts.followLink = true
				default:
					valid = false
				}
			}
			if valid {
				index++
				continue
			}
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			return copyCommandOptions{}, "", "", fmt.Errorf("unknown cp option %q", arg)
		}
		break
	}
	if len(args)-index != 2 {
		return copyCommandOptions{}, "", "", errors.New("usage: ttc cp [-a|--archive] [-L|--follow-link] SRC_PATH DEST_PATH")
	}
	return opts, args[index], args[index+1], nil
}

func splitCopyContainerEndpoint(value string) (copyContainerEndpoint, bool, error) {
	colon := strings.IndexByte(value, ':')
	if colon <= 0 {
		return copyContainerEndpoint{}, false, nil
	}
	container := value[:colon]
	// A local relative path containing ':' can be disambiguated with an
	// explicit "./" prefix, matching Docker's Linux CLI behavior.
	if strings.ContainsRune(container, filepath.Separator) {
		return copyContainerEndpoint{}, false, nil
	}
	target, err := parseSelector(container)
	if err != nil {
		return copyContainerEndpoint{}, false, err
	}
	containerPath := value[colon+1:]
	if containerPath == "" {
		containerPath = "/"
	}
	if !path.IsAbs(containerPath) {
		containerPath = "/" + containerPath
	}
	contents := strings.HasSuffix(containerPath, "/.")
	containerPath = strings.TrimSuffix(containerPath, "/.")
	if containerPath == "" {
		containerPath = "/"
	}
	containerPath = path.Clean(containerPath)
	if contents && containerPath != "/" {
		containerPath += "/."
	}
	return copyContainerEndpoint{target: target, path: containerPath}, true, nil
}

func absoluteCopyHostPath(name string) (string, error) {
	if name == "-" {
		return name, nil
	}
	contents := strings.HasSuffix(name, string(filepath.Separator)+".")
	trailingSlash := strings.HasSuffix(name, string(filepath.Separator)) && !contents
	base := name
	if contents {
		base = strings.TrimSuffix(name, string(filepath.Separator)+".")
	}
	absolute, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	if contents {
		return absolute + string(filepath.Separator) + ".", nil
	}
	if trailingSlash && absolute != string(filepath.Separator) {
		return absolute + string(filepath.Separator), nil
	}
	return absolute, nil
}

// normalizeCopyCommandArgs resolves the local operand in the client process,
// before the request is sent to ttcd (whose working directory is unrelated).
func normalizeCopyCommandArgs(args []string) ([]string, error) {
	if len(args) == 0 || args[0] != "cp" {
		return args, nil
	}
	opts, source, destination, err := parseCopyCommandArgs(args[1:])
	if err != nil {
		return nil, err
	}
	_, sourceContainer, err := splitCopyContainerEndpoint(source)
	if err != nil {
		return nil, err
	}
	_, destinationContainer, err := splitCopyContainerEndpoint(destination)
	if err != nil {
		return nil, err
	}
	if sourceContainer == destinationContainer {
		return nil, errors.New("exactly one of SRC_PATH and DEST_PATH must name a container")
	}
	if !sourceContainer {
		source, err = absoluteCopyHostPath(source)
	} else {
		destination, err = absoluteCopyHostPath(destination)
	}
	if err != nil {
		return nil, err
	}
	normalized := []string{"cp"}
	if opts.archive {
		normalized = append(normalized, "--archive")
	}
	if opts.followLink {
		normalized = append(normalized, "--follow-link")
	}
	return append(normalized, source, destination), nil
}

func runCopy(args []string, stdout, _ io.Writer, backend backend) error {
	copyBackend, ok := backend.(copyCommandBackend)
	if !ok {
		return errors.New("cp is not supported by this backend")
	}
	opts, source, destination, err := parseCopyCommandArgs(args)
	if err != nil {
		return err
	}
	sourceEndpoint, sourceContainer, err := splitCopyContainerEndpoint(source)
	if err != nil {
		return err
	}
	destinationEndpoint, destinationContainer, err := splitCopyContainerEndpoint(destination)
	if err != nil {
		return err
	}
	if sourceContainer == destinationContainer {
		return errors.New("exactly one of SRC_PATH and DEST_PATH must name a container")
	}

	if sourceContainer {
		if destination == "-" {
			return copyBackend.exportContainerPath(sourceEndpoint.target, sourceEndpoint.path, opts.followLink, stdout)
		}
		reader, writer := io.Pipe()
		producer := make(chan error, 1)
		go func() {
			err := copyBackend.exportContainerPath(sourceEndpoint.target, sourceEndpoint.path, opts.followLink, writer)
			_ = writer.CloseWithError(err)
			producer <- err
		}()
		extractErr := extractCopyArchive(reader, destination, opts.archive, os.Geteuid(), os.Getegid(), false)
		_ = reader.CloseWithError(extractErr)
		return errors.Join(extractErr, <-producer)
	}

	input := copyBackend.copyInput()
	rawArchive := source == "-" && !opts.preparedInput
	if source != "-" {
		reader, writer := io.Pipe()
		producer := make(chan error, 1)
		go func() {
			err := writeCopyArchive(source, opts.followLink, writer)
			_ = writer.CloseWithError(err)
			producer <- err
		}()
		importErr := copyBackend.importContainerPath(destinationEndpoint.target, destinationEndpoint.path, opts.archive, false, reader)
		_ = reader.CloseWithError(importErr)
		return errors.Join(importErr, <-producer)
	}
	if input == nil {
		return errors.New("cp from '-' requires standard input")
	}
	return copyBackend.importContainerPath(destinationEndpoint.target, destinationEndpoint.path, opts.archive, rawArchive, input)
}

func (b *runtimeBackend) copyInput() io.Reader {
	return b.stdin
}

func (b *runtimeBackend) exportContainerPath(target selector, source string, followLink bool, output io.Writer) error {
	args := []string{"export"}
	if followLink {
		args = append(args, "--follow-link")
	}
	args = append(args, source)
	return b.runCopyHelper(target, args, nil, output)
}

func (b *runtimeBackend) importContainerPath(target selector, destination string, preserveOwner, rawArchive bool, input io.Reader) error {
	args := []string{"import"}
	if preserveOwner {
		args = append(args, "--archive")
	}
	if rawArchive {
		args = append(args, "--raw")
	}
	args = append(args, destination)
	return b.runCopyHelper(target, args, input, io.Discard)
}

func (b *runtimeBackend) runCopyHelper(target selector, helperArgs []string, input io.Reader, output io.Writer) error {
	resolved, info, err := b.resolveSelector(target)
	if err != nil {
		return err
	}
	// A copy takes time in proportion to what it copies. Other commands go
	// on meanwhile, and none may change the family: the copy's helper runs
	// in the branch's Kernel, or mounts the branch's filesystem itself.
	copying := func(copy func() error) error {
		return b.withoutLifecycleLock(info, longOperation{branch: resolved.branch, state: "copying"}, copy)
	}
	families, err := b.listRuntimeFamiliesOf(resolved.container)
	if err != nil {
		return err
	}
	for _, family := range families {
		if family.ID != resolved.container {
			continue
		}
		if family.State == "stopped" || family.State == "exited" {
			break
		}
		for _, branch := range family.Branches {
			if branch.ID != resolved.branch {
				continue
			}
			switch branch.State {
			case "running", "paused":
				return copying(func() error {
					return b.runLiveCopyHelper(resolved, helperArgs, input, output, branch.State == "paused")
				})
			default:
				if offlineCopyState(branch.State) {
					// These states have no Kernel or open filesystem handles for this
					// branch. Its persistent TTC volume branches can therefore be
					// mounted by the short-lived helper without disturbing live peers.
					return copying(func() error {
						return b.runOfflineCopyHelper(resolved, info, helperArgs, input, output)
					})
				}
				return fmt.Errorf("cannot copy to or from branch %q while it is %s", resolved.branch, branch.State)
			}
		}
		for _, branch := range info.Branches {
			if branch.ID == resolved.branch && offlineCopyState(branch.State) {
				return copying(func() error {
					return b.runOfflineCopyHelper(resolved, info, helperArgs, input, output)
				})
			}
		}
		return fmt.Errorf("cannot copy the offline branch %q while another branch in its family is live", resolved.branch)
	}
	for _, branch := range info.Branches {
		if branch.ID != resolved.branch {
			continue
		}
		return copying(func() error {
			return b.runOfflineCopyHelper(resolved, info, helperArgs, input, output)
		})
	}
	return fmt.Errorf("branch %q does not exist", resolved.branch)
}

func offlineCopyState(state string) bool {
	switch state {
	case "dehydrated", "exited", "stopped":
		return true
	default:
		return false
	}
}

func (b *runtimeBackend) openCopyHelper() (*os.File, error) {
	// The copy helper is built into ttcd. Donating the daemon executable keeps
	// the client binary out of the Sentry and out of the wire protocol.
	executable := currentExecutable()
	helper, err := os.Open(executable)
	if err != nil {
		return nil, fmt.Errorf("opening TTC copy helper %q: %w", executable, err)
	}
	return helper, nil
}

func (b *runtimeBackend) runLiveCopyHelper(resolved selector, helperArgs []string, input io.Reader, output io.Writer, paused bool) error {
	execID := ""
	if paused {
		requestID, err := newDaemonRequestID()
		if err != nil {
			return fmt.Errorf("allocating paused copy session ID: %w", err)
		}
		execID = "ttc-cp-" + requestID
	}
	helper, err := b.openCopyHelper()
	if err != nil {
		return err
	}
	defer helper.Close()
	args := []string{
		"--root", b.root, "exec", "--branch", resolved.branch,
		"--user", "0:0",
		"--cap", "CAP_CHOWN", "--cap", "CAP_DAC_OVERRIDE",
		"--cap", "CAP_FOWNER", "--cap", "CAP_MKNOD",
		"--exec-fd", "3",
	}
	if paused {
		args = append(args, "--paused-management", "--exec-id", execID)
	}
	args = append(args, resolved.container, "ttc", copyHelperArg)
	args = append(args, helperArgs...)
	command := runscCommand(b.runsc, args...)
	command.ExtraFiles = []*os.File{helper}
	command.Stdout = output
	command.Stderr = b.stderr
	var closeInput func()
	if input != nil {
		closeInput, err = setCommandStdin(command, input)
		if err != nil {
			return err
		}
		defer closeInput()
	}
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("container copy helper exited with status %d", exitErr.ExitCode())
		}
		return fmt.Errorf("running container copy helper: %w", err)
	}
	return nil
}

func offlineCopyCapabilities() *specs.LinuxCapabilities {
	values := []string{
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FOWNER",
		"CAP_MKNOD",
		"CAP_SETFCAP",
	}
	return &specs.LinuxCapabilities{
		Bounding:  append([]string(nil), values...),
		Effective: append([]string(nil), values...),
		Permitted: append([]string(nil), values...),
	}
}

// offlineCopyBranch is the storage a visible branch name stands for.
//
// Read from the record rather than derived from the name, because the two can
// differ: a distributed target keeps its source family's repository branch
// while showing as "main", and a renamed branch keeps the storage it was
// created with. Deriving it would look in the wrong place for both.
func offlineCopyBranch(info ttcbundle.ContainerInfo, visible string) string {
	for _, branch := range info.Branches {
		if branch.ID == visible && branch.FilesystemBranch != "" {
			return branch.FilesystemBranch
		}
	}
	if visible == defaultBranchID {
		return ttcvolume.MainBranch
	}
	return ttcvolume.ScopedBranchID(info.ID, visible)
}

// repositoryPhysicalBranch resolves the one intentional heterogeneous case:
// an initial container branch may mount a read-only named-volume snapshot
// whose physical branch differs from the family-private rootfs branch.
// Descendants use one scoped identity across all repositories.
func repositoryPhysicalBranch(config specutils.TTCVolumeRepository, filesystemBranch string) string {
	if filesystemBranch == ttcvolume.MainBranch && config.Branch != "" {
		return config.Branch
	}
	return filesystemBranch
}

func offlineCopySpec(original *specs.Spec, info ttcbundle.ContainerInfo, branch string, helperArgs []string) (*specs.Spec, error) {
	encoded, err := json.Marshal(original)
	if err != nil {
		return nil, fmt.Errorf("cloning container spec: %w", err)
	}
	var cloned specs.Spec
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return nil, fmt.Errorf("cloning container spec: %w", err)
	}
	if cloned.Process == nil {
		return nil, errors.New("container spec has no process")
	}
	cloned.Process.Args = append([]string{"ttc", copyHelperArg}, helperArgs...)
	cloned.Process.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	cloned.Process.Cwd = "/"
	cloned.Process.Terminal = false
	cloned.Process.ConsoleSize = nil
	cloned.Process.User = specs.User{UID: 0, GID: 0}
	cloned.Process.Capabilities = offlineCopyCapabilities()
	cloned.Process.Rlimits = nil
	cloned.Process.NoNewPrivileges = false
	cloned.Hooks = nil
	if cloned.Linux != nil {
		cloned.Linux.CgroupsPath = ""
		cloned.Linux.Resources = nil
	}
	if cloned.Annotations == nil {
		cloned.Annotations = make(map[string]string)
	}
	delete(cloned.Annotations, "dev.timetravelcontainer.internal-console")
	// The copy helper deliberately runs outside the container's host cgroup and
	// therefore drops Linux.Resources above. PidsLimitAnnotation is a contract
	// with the Sentry that linux.resources.pids is present; retaining only the
	// marker makes loader construction fail before the helper can inspect a
	// stopped or dehydrated filesystem.
	delete(cloned.Annotations, ttcbundle.PidsLimitAnnotation)
	repositories, err := specutils.TTCVolumeRepositories(&cloned)
	if err != nil {
		return nil, err
	}
	if cloned.Root == nil || !cloned.Root.Readonly {
		hasRootUpper := false
		for _, repository := range repositories {
			if repository.Source == specutils.TTCRootfsUpperSource {
				hasRootUpper = true
				break
			}
		}
		if !hasRootUpper {
			return nil, errors.New("writable container predates persistent rootfs uppers; recreate it before using stopped-container cp")
		}
	}
	persistentBranch := offlineCopyBranch(info, branch)
	for i := range repositories {
		repositories[i].Branch = repositoryPhysicalBranch(repositories[i], persistentBranch)
	}
	encoded, err = json.Marshal(repositories)
	if err != nil {
		return nil, fmt.Errorf("encoding offline copy repositories: %w", err)
	}
	if len(repositories) == 0 {
		delete(cloned.Annotations, specutils.AnnotationTTCVolumeRepositories)
	} else {
		cloned.Annotations[specutils.AnnotationTTCVolumeRepositories] = string(encoded)
	}
	return &cloned, nil
}

func writeOfflineCopySpec(bundleDir string, spec *specs.Spec) error {
	encoded, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), encoded, 0600); err != nil {
		return fmt.Errorf("writing offline copy spec: %w", err)
	}
	return nil
}

func (b *runtimeBackend) runOfflineCopyHelper(resolved selector, info ttcbundle.ContainerInfo, helperArgs []string, input io.Reader, output io.Writer) error {
	branch, err := persistentBranchInfo(info, resolved.branch)
	if err != nil {
		return err
	}
	if b.nodeID != "" && branch.StorageManifest != "" {
		if len(helperArgs) == 0 || helperArgs[0] != "export" {
			return fmt.Errorf("cannot copy into durable unplaced branch %q; rehydrate it first", resolved.branch)
		}
		// The canonical OCI spec records image identity, but its rootfs path was
		// created on the branch's previous owner. Offline cp has no live target
		// runtime that would normally populate this node's image cache, so do it
		// explicitly before asking the gofer to open the rewritten local path.
		refreshed, err := b.containerStore().RefreshContainerImageRoot(context.Background(), info.ID)
		if err != nil {
			return fmt.Errorf("materializing immutable image for offline copy: %w", err)
		}
		info = refreshed
		branch, err = persistentBranchInfo(info, resolved.branch)
		if err != nil {
			return err
		}
		// StorageState describes an in-progress local cache/placement action,
		// not whether the durable immutable backing ceased to exist. A cold fork
		// can leave its dehydrated source marked materializing while the child
		// starts from the same lazy repository. Offline helpers have no donated
		// relay FDs, so always make a manifested cold branch self-contained.
		if err := b.materializeColdFilesystem(info, branch); err != nil {
			return fmt.Errorf("materializing filesystem for durable branch %q: %w", resolved.branch, err)
		}
	}
	original, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return err
	}
	spec, err := offlineCopySpec(original, info, resolved.branch, helperArgs)
	if err != nil {
		return err
	}
	tempRoot := filepath.Join(b.activeDataRoot(), "tmp")
	if err := os.MkdirAll(tempRoot, 0700); err != nil {
		return fmt.Errorf("creating TTC temporary directory: %w", err)
	}
	bundleDir, err := os.MkdirTemp(tempRoot, "offline-cp-bundle-")
	if err != nil {
		return fmt.Errorf("creating offline copy bundle: %w", err)
	}
	defer os.RemoveAll(bundleDir)
	if err := writeOfflineCopySpec(bundleDir, spec); err != nil {
		return err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	runtimeID := "ttc-cp-" + requestID
	helper, err := b.openCopyHelper()
	if err != nil {
		return err
	}
	defer helper.Close()
	args := b.configuredRunscArgsForNetwork("none")
	args = append(args, "run", "--bundle", bundleDir, "--exec-fd", "3", runtimeID)
	command := runscCommand(b.runsc, args...)
	command.ExtraFiles = []*os.File{helper}
	command.Stdout = output
	command.Stderr = b.stderr
	var closeInput func()
	if input != nil {
		closeInput, err = setCommandStdin(command, input)
		if err != nil {
			return err
		}
		defer closeInput()
	}
	runErr := command.Run()
	_, cleanupErr := b.command("delete", "--force", runtimeID)
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			runErr = fmt.Errorf("offline container copy helper exited with status %d", exitErr.ExitCode())
		} else {
			runErr = fmt.Errorf("running offline container copy helper: %w", runErr)
		}
	}
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("removing offline copy runtime: %w", cleanupErr)
	}
	return errors.Join(runErr, cleanupErr)
}

func runCopyHelper(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
			err = writeCopyArchive(args[0], follow, stdout)
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
				err = extractCopyArchive(stdin, args[0], preserveOwner, 0, 0, rawArchive)
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

// RunCopyHelper runs the private container-side copy helper embedded in ttcd.
func RunCopyHelper(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runCopyHelper(args, stdin, stdout, stderr)
}

type copyFileIdentity struct {
	dev uint64
	ino uint64
}

func copySourceContents(name string) bool {
	return name == string(filepath.Separator) || strings.HasSuffix(name, string(filepath.Separator)+".")
}

func copyArchiveRootName(source string) string {
	trimmed := strings.TrimSuffix(source, string(filepath.Separator)+".")
	clean := filepath.Clean(trimmed)
	if clean == string(filepath.Separator) || clean == "." {
		return "."
	}
	return filepath.Base(clean)
}

func readCopyXattrs(name string) (map[string]string, error) {
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

func writeCopyArchive(source string, followLink bool, output io.Writer) error {
	contents := copySourceContents(source)
	physical := filepath.Clean(strings.TrimSuffix(source, string(filepath.Separator)+"."))
	if physical == "" {
		physical = string(filepath.Separator)
	}
	rootName := copyArchiveRootName(source)
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
	hardlinks := make(map[copyFileIdentity]string)
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
				identity := copyFileIdentity{dev: uint64(stat.Dev), ino: stat.Ino}
				if first, ok := hardlinks[identity]; ok {
					header.Typeflag = tar.TypeLink
					header.Linkname = first
					header.Size = 0
				} else {
					hardlinks[identity] = header.Name
				}
			}
		}
		xattrs, err := readCopyXattrs(hostPath)
		if err != nil {
			return fmt.Errorf("reading xattrs for %q: %w", hostPath, err)
		}
		header.Xattrs = xattrs
		if root && contents {
			if header.PAXRecords == nil {
				header.PAXRecords = make(map[string]string)
			}
			header.PAXRecords[copyContentsPAXKey] = "1"
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

func cleanCopyArchivePath(name string) (string, error) {
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

type copyExtractionPlan struct {
	boundary       string
	rootTarget     string
	rootName       string
	skipRoot       bool
	raw            bool
	preserveOwner  bool
	defaultUID     int
	defaultGID     int
	directoryQueue []copyDirectoryMetadata
}

type copyDirectoryMetadata struct {
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

func newCopyExtractionPlan(first *tar.Header, destination string, preserveOwner, raw bool, defaultUID, defaultGID int) (*copyExtractionPlan, error) {
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
		return &copyExtractionPlan{
			boundary: destination, rootTarget: destination, raw: true,
			preserveOwner: preserveOwner, defaultUID: defaultUID, defaultGID: defaultGID,
		}, nil
	}

	rootName, err := cleanCopyArchivePath(first.Name)
	if err != nil {
		return nil, err
	}
	sourceDirectory := first.Typeflag == tar.TypeDir
	sourceContents := first.PAXRecords[copyContentsPAXKey] == "1"
	if sourceContents && !sourceDirectory {
		return nil, errors.New("copy-contents archive root is not a directory")
	}
	baseName := path.Base(rootName)
	if baseName == "." {
		sourceContents = true
	}
	plan := &copyExtractionPlan{
		rootName: rootName, preserveOwner: preserveOwner,
		defaultUID: defaultUID, defaultGID: defaultGID,
	}
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
		plan.boundary = destination
		plan.rootTarget = destination
		plan.skipRoot = true
	case sourceDirectory:
		if destinationInfo != nil && !destinationInfo.IsDir() {
			return nil, fmt.Errorf("cannot copy directory onto non-directory %q", destination)
		}
		if destinationInfo != nil {
			plan.rootTarget = filepath.Join(destination, baseName)
			plan.boundary = destination
		} else {
			parent := filepath.Dir(destination)
			if info, err := os.Stat(parent); err != nil || !info.IsDir() {
				return nil, fmt.Errorf("destination parent %q does not exist or is not a directory", parent)
			}
			plan.rootTarget = destination
			plan.boundary = parent
		}
	default:
		if destinationInfo != nil && destinationInfo.IsDir() {
			plan.rootTarget = filepath.Join(destination, baseName)
			plan.boundary = destination
		} else {
			parent := filepath.Dir(destination)
			if info, err := os.Stat(parent); err != nil || !info.IsDir() {
				return nil, fmt.Errorf("destination parent %q does not exist or is not a directory", parent)
			}
			plan.rootTarget = destination
			plan.boundary = parent
		}
	}
	return plan, nil
}

func (p *copyExtractionPlan) target(headerName string) (string, error) {
	name, err := cleanCopyArchivePath(headerName)
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

func rejectCopySymlinkAncestor(boundary, target string) error {
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

func removeCopyDestination(target string, wantDirectory bool) error {
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

func applyCopyXattrs(target string, header *tar.Header) error {
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

func applyCopyMetadata(target string, header *tar.Header, preserveOwner bool, defaultUID, defaultGID int) error {
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
	if err := applyCopyXattrs(target, header); err != nil {
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

func (p *copyExtractionPlan) extractEntry(reader *tar.Reader, header *tar.Header, first bool) error {
	target, err := p.target(header.Name)
	if err != nil {
		return err
	}
	if first && p.skipRoot {
		return nil
	}
	if err := rejectCopySymlinkAncestor(p.boundary, target); err != nil {
		return err
	}
	switch header.Typeflag {
	case tar.TypeDir:
		if err := removeCopyDestination(target, true); err != nil {
			return err
		}
		if err := os.Mkdir(target, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		p.directoryQueue = append(p.directoryQueue, copyDirectoryMetadata{path: target, header: *header})
		return nil
	case tar.TypeReg, tar.TypeRegA:
		if err := removeCopyDestination(target, false); err != nil {
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
		if err := removeCopyDestination(target, false); err != nil {
			return err
		}
		if err := os.Symlink(header.Linkname, target); err != nil {
			return err
		}
	case tar.TypeLink:
		if err := removeCopyDestination(target, false); err != nil {
			return err
		}
		linkTarget, err := p.target(header.Linkname)
		if err != nil {
			return fmt.Errorf("invalid hard-link target: %w", err)
		}
		if err := rejectCopySymlinkAncestor(p.boundary, linkTarget); err != nil {
			return err
		}
		if err := os.Link(linkTarget, target); err != nil {
			return err
		}
	case tar.TypeFifo:
		if err := removeCopyDestination(target, false); err != nil {
			return err
		}
		if err := unix.Mkfifo(target, uint32(header.Mode)&07777); err != nil {
			return err
		}
	case tar.TypeChar, tar.TypeBlock:
		if err := removeCopyDestination(target, false); err != nil {
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
	return applyCopyMetadata(target, header, p.preserveOwner, p.defaultUID, p.defaultGID)
}

func extractCopyArchive(input io.Reader, destination string, preserveOwner bool, defaultUID, defaultGID int, rawArchive bool) error {
	reader := tar.NewReader(input)
	first, err := reader.Next()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	plan, err := newCopyExtractionPlan(first, destination, preserveOwner, rawArchive, defaultUID, defaultGID)
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
		if err := applyCopyMetadata(entry.path, &entry.header, plan.preserveOwner, plan.defaultUID, plan.defaultGID); err != nil {
			return err
		}
	}
	return nil
}
