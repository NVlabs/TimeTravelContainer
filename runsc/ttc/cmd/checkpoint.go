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

package cmd

import (
	"compress/flate"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	ttccheckpoint "gvisor.dev/gvisor/runsc/ttc/checkpoint"
	"gvisor.dev/gvisor/runsc/ttc/cluster"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type checkpointRequest struct {
	target      selector
	destination string
	compression string
}

type checkpointCommandBackend interface {
	checkpoint(request checkpointRequest, output io.Writer) error
	restoreCheckpoint(source, name string) (string, error)
}

func runCheckpoint(args []string, stdout io.Writer, b backend) error {
	checkpoints, ok := b.(checkpointCommandBackend)
	if !ok {
		return errors.New("checkpoint is not supported by this backend")
	}
	fs := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	compression := fs.String("compression", "flate-best-speed", "checkpoint compression: none or flate-best-speed")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: ttc checkpoint [--compression none|flate-best-speed] CONTAINER@BRANCH FILE")
	}
	if *compression != "none" && *compression != "flate-best-speed" {
		return fmt.Errorf("unsupported checkpoint compression %q", *compression)
	}
	target, err := parseSelector(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := checkpoints.checkpoint(checkpointRequest{
		target:      target,
		destination: fs.Arg(1),
		compression: *compression,
	}, stdout); err != nil {
		return err
	}
	if fs.Arg(1) != "-" {
		fmt.Fprintln(stdout, fs.Arg(1))
	}
	return nil
}

func runRestoreCheckpoint(args []string, stdout io.Writer, b backend) error {
	checkpoints, ok := b.(checkpointCommandBackend)
	if !ok {
		return errors.New("restore is not supported by this backend")
	}
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	name := fs.String("name", "", "name for the restored container")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ttc restore [--name NAME] FILE")
	}
	if *name == "" {
		*name = "restored-" + time.Now().UTC().Format("20060102-150405")
	}
	id, err := checkpoints.restoreCheckpoint(fs.Arg(0), *name)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, id)
	return nil
}

// prepareCheckpointClientIO keeps checkpoint archive paths in the client. The
// daemon sees only a byte stream, which remains correct when a control-only
// client routes the operation to a different physical host.
func prepareCheckpointClientIO(args []string, stdin io.Reader, stdout io.Writer) ([]string, io.Reader, io.Writer, func(bool) error, error) {
	if len(args) == 0 || (args[0] != "checkpoint" && args[0] != "restore") {
		return args, stdin, stdout, nil, nil
	}
	normalized := append([]string(nil), args...)
	if args[0] == "checkpoint" {
		fs := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
		_ = fs.String("compression", "flate-best-speed", "")
		fs.SetOutput(io.Discard)
		if err := fs.Parse(args[1:]); err != nil {
			return nil, nil, nil, nil, err
		}
		if fs.NArg() != 2 {
			return args, stdin, stdout, nil, nil
		}
		destination := fs.Arg(1)
		if destination == "-" {
			return normalized, stdin, stdout, nil, nil
		}
		absolute, err := filepath.Abs(destination)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		output, err := os.CreateTemp(filepath.Dir(absolute), "."+filepath.Base(absolute)+".ttc-tmp-")
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("creating checkpoint output: %w", err)
		}
		for index := len(normalized) - 1; index >= 1; index-- {
			if normalized[index] == destination {
				normalized[index] = "-"
				break
			}
		}
		finish := func(success bool) error {
			var finishErr error
			if success {
				finishErr = errors.Join(output.Sync(), output.Close())
				if finishErr == nil {
					finishErr = os.Rename(output.Name(), absolute)
				}
			} else {
				finishErr = output.Close()
			}
			if finishErr != nil || !success {
				_ = os.Remove(output.Name())
			}
			return finishErr
		}
		return normalized, stdin, output, finish, nil
	}
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	_ = fs.String("name", "", "")
	_ = fs.String("node", "", "")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args[1:]); err != nil {
		return nil, nil, nil, nil, err
	}
	if fs.NArg() != 1 {
		return args, stdin, stdout, nil, nil
	}
	operand := fs.Arg(0)
	if operand == "-" {
		return normalized, stdin, stdout, nil, nil
	}
	absolute, err := filepath.Abs(operand)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	input, err := os.Open(absolute)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("opening checkpoint input: %w", err)
	}
	for index := len(normalized) - 1; index >= 1; index-- {
		if normalized[index] == operand {
			normalized[index] = "-"
			break
		}
	}
	return normalized, input, stdout, func(bool) error { return input.Close() }, nil
}

func (b *runtimeBackend) checkpoint(request checkpointRequest, output io.Writer) (retErr error) {
	target, info, err := b.resolveSelector(request.target)
	if err != nil {
		return err
	}
	families, err := b.listRuntimeFamiliesOf(info.ID)
	if err != nil {
		return err
	}
	sourceState := ""
	for _, family := range families {
		if family.ID != info.ID {
			continue
		}
		if family.Error != "" {
			return errors.New(family.Error)
		}
		for _, branch := range family.Branches {
			if branch.ID == target.branch {
				sourceState = branch.State
				break
			}
		}
	}
	if sourceState != "running" && sourceState != "paused" {
		if sourceState == "" {
			return fmt.Errorf("branch %s does not exist or is not live", target)
		}
		return fmt.Errorf("branch %s is %s, cannot checkpoint", target, sourceState)
	}
	spec, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	hiddenBranch := "checkpoint-" + requestID
	stageRoot := filepath.Join(b.activeDataRoot(), "tmp")
	if err := os.MkdirAll(stageRoot, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(stageRoot, "checkpoint-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	forkArgs := []string{"timetravel", "--source", target.branch, "--paused", info.ID, "fork", hiddenBranch}
	filesystemOrigins, finishFilesystemOrigins, err := b.allocateFilesystemForkOrigins(info.ID, 1)
	if err != nil {
		return fmt.Errorf("allocating checkpoint filesystem origins: %w", err)
	}
	var forkOutput []byte
	if len(filesystemOrigins) == 0 {
		forkOutput, err = b.command(forkArgs...)
	} else {
		originsReader, pipeErr := filesystemOriginAssignmentsPipe(filesystemOrigins)
		if pipeErr != nil {
			return errors.Join(pipeErr, finishFilesystemOrigins(false))
		}
		forkArgs = append([]string{"timetravel", "--filesystem-origins-fd=3"}, forkArgs[1:]...)
		forkOutput, err = b.commandWithFiles([]*os.File{originsReader}, forkArgs...)
		closeErr := originsReader.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return errors.Join(fmt.Errorf("creating checkpoint snapshot branch: %w", err), finishFilesystemOrigins(false))
	}
	// The source and hidden child now use the exact Raft-allocated working
	// origins. They cannot be abandoned even if archive construction fails:
	// the source may resume and mutate its new working origin.
	if err := finishFilesystemOrigins(true); err != nil {
		return fmt.Errorf("committing checkpoint filesystem origins: %w", err)
	}
	var hiddenInfo runscBranchInfo
	if err := json.Unmarshal(forkOutput, &hiddenInfo); err != nil {
		return fmt.Errorf("decoding checkpoint snapshot branch: %w", err)
	}
	if hiddenInfo.FilesystemBranch == "" {
		return errors.New("checkpoint snapshot branch has no filesystem identity")
	}
	hiddenExists := true
	defer func() {
		if hiddenExists {
			if _, err := b.command("timetravel", info.ID, "destroy-reclaim", hiddenBranch); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("destroying checkpoint snapshot branch: %w", err))
			} else if _, err := b.reconcileVolatileSnapshotOrigins(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("reclaiming checkpoint snapshot origins: %w", err))
			}
		}
	}()
	stateDir := filepath.Join(stage, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		return err
	}
	var manifestRepositories []ttccheckpoint.RepositorySnapshot
	var hostMounts []ttccheckpoint.HostMountSnapshot
	// Saving the branch's memory and exporting its files take time in
	// proportion to them; other commands go on meanwhile, while the source,
	// which keeps its state, is left as it is.
	if err := b.withoutLifecycleLock(info, longOperation{branch: target.branch, state: "checkpointing", hidden: hiddenBranch}, func() error {
		var err error
		manifestRepositories, hostMounts, err = b.saveCheckpointState(info, spec, request.compression, hiddenBranch, hiddenInfo.FilesystemBranch, stage, stateDir)
		return err
	}); err != nil {
		return err
	}
	if _, err := b.command("timetravel", info.ID, "destroy-reclaim", hiddenBranch); err != nil {
		return fmt.Errorf("destroying checkpoint snapshot branch: %w", err)
	}
	hiddenExists = false
	// destroy-reclaim drops the hidden branch's filesystem roots. Reconcile its
	// now-empty origin immediately so short-lived checkpoints do not grow the
	// cluster provider directory indefinitely.
	if _, err := b.reconcileVolatileSnapshotOrigins(); err != nil {
		return fmt.Errorf("reclaiming checkpoint snapshot origins: %w", err)
	}
	memoryCompression := request.compression
	if memoryCompression == "none" {
		memoryCompression = ""
	}
	if err := ttccheckpoint.WriteManifest(stage, ttccheckpoint.Manifest{
		SourceBranch:      target.branch,
		RestorePaused:     sourceState == "paused",
		Container:         info,
		Spec:              *spec,
		Repositories:      manifestRepositories,
		HostMounts:        hostMounts,
		MemoryCompression: memoryCompression,
	}); err != nil {
		return err
	}
	// Packing, like saving, takes time in proportion to the checkpoint.
	return b.withoutLifecycleLock(info, longOperation{branch: target.branch, state: "checkpointing"}, func() error {
		destination := request.destination
		stream := destination == "-"
		if stream {
			destination = stage + ".ttc"
			defer os.Remove(destination)
		}
		if err := ttccheckpoint.Pack(stage, destination); err != nil {
			return err
		}
		if stream {
			archive, err := os.Open(destination)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(output, archive)
			return errors.Join(copyErr, archive.Close())
		}
		return nil
	})
}

// saveCheckpointState saves a dehydration of the hidden checkpoint branch in
// stateDir, and its repositories and host mounts in stage.
func (b *runtimeBackend) saveCheckpointState(info ttcbundle.ContainerInfo, spec *specs.Spec, compression, hiddenBranch, persistentBranch, stage, stateDir string) (manifestRepositories []ttccheckpoint.RepositorySnapshot, hostMounts []ttccheckpoint.HostMountSnapshot, retErr error) {
	if err := b.saveCheckpointDehydration(info, hiddenBranch, compression, stateDir); err != nil {
		return nil, nil, fmt.Errorf("saving checkpoint state: %w", err)
	}

	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return nil, nil, err
	}
	manager, err := b.volumeManager()
	if err != nil {
		return nil, nil, err
	}
	manifestRepositories = make([]ttccheckpoint.RepositorySnapshot, 0, len(repositories))
	var opened []*ttcvolume.Repository
	defer func() {
		for _, repository := range opened {
			if err := repository.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("closing checkpoint repository: %w", err))
			}
		}
	}()
	for index, config := range repositories {
		repository, err := manager.OpenPrivate(config.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("opening checkpoint repository %q: %w", config.Source, err)
		}
		opened = append(opened, repository)
		relative := filepath.Join("repositories", strconv.Itoa(index))
		if _, err := repository.ExportSnapshot(persistentBranch, filepath.Join(stage, relative)); err != nil {
			return nil, nil, fmt.Errorf("exporting checkpoint repository %q: %w", config.Source, err)
		}
		manifestRepositories = append(manifestRepositories, ttccheckpoint.RepositorySnapshot{
			Source: config.Source,
			Path:   relative,
		})
	}
	for index, mount := range spec.Mounts {
		if mount.Type != "bind" {
			continue
		}
		sourceInfo, err := os.Stat(mount.Source)
		if err != nil {
			return nil, nil, fmt.Errorf("stating checkpoint host mount %q: %w", mount.Source, err)
		}
		if !sourceInfo.Mode().IsRegular() && !sourceInfo.IsDir() {
			return nil, nil, fmt.Errorf("checkpoint requires bind mount %q at %q to resolve to a regular file or directory", mount.Source, mount.Destination)
		}
		relative := filepath.Join("host-mounts", strconv.Itoa(index))
		if err := copyCheckpointHostPath(mount.Source, filepath.Join(stage, relative)); err != nil {
			return nil, nil, fmt.Errorf("capturing host mount %q: %w", mount.Source, err)
		}
		hostMounts = append(hostMounts, ttccheckpoint.HostMountSnapshot{
			MountIndex: index,
			Source:     mount.Source,
			Path:       relative,
		})
	}
	return manifestRepositories, hostMounts, nil
}

// releaseRestoredMemory releases the graph roots a failed restore installed
// for the family's branches.
func (b *runtimeBackend) releaseRestoredMemory(id string) error {
	info, err := b.containerStore().LoadContainer(id)
	if err != nil {
		return err
	}
	var result error
	for _, branch := range info.Branches {
		if len(branch.MemoryRoots) != 0 || branch.ControlRoot != "" {
			result = errors.Join(result, b.releaseBranchGraphRoots(branch))
		}
	}
	return result
}

// The files of a checkpoint's dehydration, in its state directory.
const (
	checkpointControlName  = "control.json"
	checkpointMetadataName = "metadata.state"
	checkpointMemoryName   = "memory.stream"
)

// saveCheckpointDehydration dehydrates the hidden checkpoint branch and saves
// the dehydration in stateDir: its capture, its kernel state, and a stream of
// the pages it uses. The stream holds the branch's own memory; saving its
// kernel as gVisor does wrote the whole memory file it shares with its family.
func (b *runtimeBackend) saveCheckpointDehydration(info ttcbundle.ContainerInfo, hiddenBranch, compression, stateDir string) error {
	if _, err := b.command("timetravel", info.ID, "dehydrate", hiddenBranch); err != nil {
		return fmt.Errorf("capturing the branch: %w", err)
	}
	metadata, err := os.OpenFile(filepath.Join(stateDir, checkpointMetadataName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer metadata.Close()
	memory, err := os.OpenFile(filepath.Join(stateDir, checkpointMemoryName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer memory.Close()
	// Unfiltered, the capture's stream is one-way, so a file can record it.
	stream, finish := memory, func() error { return nil }
	if compression != "none" {
		reader, writer, err := os.Pipe()
		if err != nil {
			return err
		}
		compressed := make(chan error, 1)
		go func() {
			compressor, err := flate.NewWriter(memory, flate.BestSpeed)
			if err == nil {
				_, err = io.Copy(compressor, reader)
				err = errors.Join(err, compressor.Close())
			}
			// Drain what is left if compressing failed, so the Sentry's
			// writes do not block.
			_, _ = io.Copy(io.Discard, reader)
			compressed <- errors.Join(err, reader.Close())
		}()
		stream = writer
		finish = func() error {
			_ = writer.Close()
			return <-compressed
		}
	}
	output, exportErr := b.commandWithFiles([]*os.File{stream, metadata}, "timetravel", "--dehydration-graph-stream-fd=3", "--dehydration-fd=4", info.ID, "dehydrate-export", hiddenBranch)
	if err := errors.Join(exportErr, finish()); err != nil {
		return fmt.Errorf("exporting the branch's memory: %w", err)
	}
	var exported boot.BranchDehydrationExportInfo
	if err := json.Unmarshal(output, &exported); err != nil {
		return fmt.Errorf("decoding the branch's capture: %w", err)
	}
	// The state directory is packed into the archive, which is what is kept.
	data, err := json.Marshal(&exported.Capture)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, checkpointControlName), data, 0600)
}

// recordedCapture replays a memory capture stream recorded to a file. The
// capture was not filtered, so importing it never answers it.
type recordedCapture struct {
	io.Reader
}

func (recordedCapture) Write([]byte) (int, error) {
	return 0, errors.New("a recorded memory capture cannot be answered")
}

// installCheckpointDehydration makes a checkpoint's dehydration the durable
// dehydration of the restored family's main branch, as dehydrating main would
// have left it: its pages in the snapshot graph, and its capture and kernel
// state in main's dehydration image. resume says whether main runs once
// rehydrated, or stays paused as the checkpointed branch was.
func (b *runtimeBackend) installCheckpointDehydration(info ttcbundle.ContainerInfo, stateDir, compression string, resume bool) (retErr error) {
	main, err := persistentBranchInfo(info, defaultBranchID)
	if err != nil {
		return err
	}
	controlData, err := os.ReadFile(filepath.Join(stateDir, checkpointControlName))
	if err != nil {
		return fmt.Errorf("reading the checkpoint's capture: %w", err)
	}
	var capture boot.BranchDehydrateInfo
	if err := json.Unmarshal(controlData, &capture); err != nil {
		return fmt.Errorf("decoding the checkpoint's capture: %w", err)
	}
	// The restored branch is main, in main's filesystem.
	capture.FilesystemBranch = main.FilesystemBranch
	capture.MemoryRoots = nil
	capture.Resume = resume

	finishGraphCapture, err := b.beginForegroundCapture(info, longOperation{state: "restoring"})
	if err != nil {
		return err
	}
	defer finishGraphCapture()
	origin, sealState, built, err := b.allocateGraphSnapshotOrigin()
	if err != nil {
		return fmt.Errorf("allocating a snapshot origin: %w", err)
	}
	var roots, rootObjects []string
	controlRoot := ""
	installed := false
	defer func() {
		if installed {
			return
		}
		if graphErr := b.releaseGraphMemoryRoots(appendGraphRoot(append([]string(nil), roots...), controlRoot)); graphErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("releasing the checkpoint's memory roots: %w", graphErr))
		}
		if graphErr := b.discardFailedGraphSnapshot(origin); graphErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("discarding the checkpoint's snapshot origin: %w", graphErr))
		}
	}()
	memory, err := os.Open(filepath.Join(stateDir, checkpointMemoryName))
	if err != nil {
		return fmt.Errorf("opening the checkpoint's memory: %w", err)
	}
	defer memory.Close()
	var reader io.Reader = memory
	switch compression {
	case "":
	case "flate-best-speed":
		decompressor := flate.NewReader(memory)
		defer decompressor.Close()
		reader = decompressor
	default:
		return fmt.Errorf("unsupported checkpoint memory compression %q", compression)
	}
	imported, err := b.importGraphCapture(recordedCapture{reader}, origin, fmt.Sprintf("memory/%016x", uint64(origin)))
	if err != nil {
		return fmt.Errorf("importing the checkpoint's memory: %w", err)
	}
	for _, root := range imported.Roots {
		capture.MemoryRoots = append(capture.MemoryRoots, boot.BranchMemoryRoot{ResourceID: root.ResourceID, Name: root.Name, Root: root.Root.String()})
		roots = append(roots, root.Name)
		rootObjects = append(rootObjects, root.Root.String())
	}
	capture.SnapshotID = uint64(origin)

	metadataDir := filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "dehydrated")
	encodedBranch := ttcbundle.MemoryBranchKey(main)
	if err := os.MkdirAll(metadataDir, 0700); err != nil {
		return err
	}
	stagingDir, err := os.MkdirTemp(metadataDir, "."+encodedBranch+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if stagingDir != "" {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	if err := writeDurableJSON(filepath.Join(stagingDir, checkpointControlName), &capture); err != nil {
		return err
	}
	if err := copyCheckpointHostFile(filepath.Join(stateDir, checkpointMetadataName), filepath.Join(stagingDir, checkpointMetadataName), 0600); err != nil {
		return fmt.Errorf("installing the checkpoint's kernel state: %w", err)
	}
	controlRoot = graphCaptureControlRootName(uint64(origin))
	controlObject, err := ttcsnapshot.ImportDirectory(context.Background(), b.snapshotGraph, b.snapshotData, origin, controlRoot, stagingDir)
	if err != nil {
		return fmt.Errorf("importing the checkpoint's control state: %w", err)
	}
	if err := b.snapshotGraph.Seal(origin, sealState); err != nil {
		return fmt.Errorf("sealing the checkpoint's snapshot origin %d: %w", origin, err)
	}
	finalDir := filepath.Join(metadataDir, encodedBranch)
	if err := os.Rename(stagingDir, finalDir); err != nil {
		return fmt.Errorf("publishing the checkpoint's dehydration image: %w", err)
	}
	stagingDir = ""
	store := b.containerStore()
	if err := store.UpdateBranchSnapshotGraph(info.ID, defaultBranchID, uint64(origin), roots, rootObjects, controlRoot, controlObject.String(), ""); err != nil {
		_ = os.RemoveAll(finalDir)
		return fmt.Errorf("recording the checkpoint's memory roots: %w", err)
	}
	if err := store.UpdateBranchState(info.ID, defaultBranchID, "dehydrated"); err != nil {
		return err
	}
	installed = true
	return nil
}

func copyCheckpointHostPath(source, destination string) error {
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	source = resolved
	root, err := os.Stat(source)
	if err != nil {
		return err
	}
	if root.Mode().IsRegular() {
		return copyCheckpointHostFile(source, destination, root.Mode().Perm())
	}
	if !root.IsDir() {
		return fmt.Errorf("source has unsupported mode %v", root.Mode())
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	var directories []struct {
		path string
		mode os.FileMode
	}
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			if err := os.Mkdir(target, 0700); err != nil {
				return err
			}
			directories = append(directories, struct {
				path string
				mode os.FileMode
			}{target, info.Mode().Perm()})
			return nil
		case info.Mode().IsRegular():
			return copyCheckpointHostFile(path, target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("host mount entry %q has unsupported mode %v", path, info.Mode())
		}
	})
	for index := len(directories) - 1; index >= 0; index-- {
		if chmodErr := os.Chmod(directories[index].path, directories[index].mode); err == nil && chmodErr != nil {
			err = chmodErr
		}
	}
	if chmodErr := os.Chmod(destination, root.Mode().Perm()); err == nil && chmodErr != nil {
		err = chmodErr
	}
	return err
}

func copyCheckpointHostFile(source, destination string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func (b *runtimeBackend) restoreCheckpoint(source, name string) (id string, retErr error) {
	stageRoot := filepath.Join(b.activeDataRoot(), "tmp")
	if err := os.MkdirAll(stageRoot, 0700); err != nil {
		return "", err
	}
	stageBase, err := os.MkdirTemp(stageRoot, "restore-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stageBase)
	stage := filepath.Join(stageBase, "checkpoint")
	// Receiving and unpacking the archive take time in proportion to it,
	// before there is a family to protect: other commands go on meanwhile,
	// and one that names the family to be is refused.
	if err := b.withoutLifecycleLock(ttcbundle.ContainerInfo{ID: "restore:" + stageBase, Name: name}, longOperation{state: "restoring", shown: true}, func() error {
		if source == "-" {
			if b.stdin == nil {
				return errors.New("checkpoint restore from '-' requires standard input")
			}
			source = filepath.Join(stageBase, "input.ttc")
			archive, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(archive, b.stdin)
			if err := errors.Join(copyErr, archive.Close()); err != nil {
				return fmt.Errorf("receiving checkpoint archive: %w", err)
			}
		}
		return ttccheckpoint.Unpack(source, stage)
	}); err != nil {
		return "", err
	}
	manifest, err := ttccheckpoint.ReadManifest(stage)
	if err != nil {
		return "", err
	}
	sourceBranch, err := persistentBranchInfo(manifest.Container, manifest.SourceBranch)
	if err != nil {
		return "", fmt.Errorf("reading checkpoint source branch state: %w", err)
	}
	repositories := make([]ttcbundle.RestoreRepository, 0, len(manifest.Repositories))
	for _, repository := range manifest.Repositories {
		repositories = append(repositories, ttcbundle.RestoreRepository{
			Source:       repository.Source,
			SnapshotPath: filepath.Join(stage, repository.Path),
		})
	}
	hostMounts := make([]ttcbundle.RestoreHostMount, 0, len(manifest.HostMounts))
	for _, mount := range manifest.HostMounts {
		hostMounts = append(hostMounts, ttcbundle.RestoreHostMount{
			MountIndex:   mount.MountIndex,
			Source:       mount.Source,
			SnapshotPath: filepath.Join(stage, mount.Path),
		})
	}
	filesystemOrigins, finishFilesystemOrigins := b.newFilesystemOriginScope()
	prepared, err := b.containerStore().PrepareRestore(context.Background(), ttcbundle.RestoreOptions{
		Name:                    name,
		Container:               manifest.Container,
		Spec:                    &manifest.Spec,
		Repositories:            repositories,
		HostMounts:              hostMounts,
		VolumeCopyMode:          b.volumeCopyMode,
		RestorePaused:           manifest.RestorePaused,
		CheckpointBranch:        manifest.SourceBranch,
		NodeID:                  b.nodeID,
		AllocateSnapshotOrigins: filesystemOrigins,
	})
	if err != nil {
		return "", errors.Join(err, finishFilesystemOrigins(false))
	}
	if err := finishFilesystemOrigins(true); err != nil {
		return "", errors.Join(err, b.containerStore().RemoveContainer(prepared.Container.ID))
	}
	// The cleanup below must not use id: a failed return has set it to "".
	restored := prepared.Container.ID
	id = restored
	reservation, err := b.reserveNewContainerPlacement(prepared.Container)
	if err != nil {
		return "", errors.Join(err, b.containerStore().RemoveContainer(restored))
	}
	cleanup := true
	defer func() {
		if cleanup {
			cleanupErr := errors.Join(
				b.cancelNewContainerPlacement(reservation),
				b.releaseRestoredMemory(restored),
				b.containerStore().RemoveContainer(restored),
			)
			if cleanupErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("cleaning failed restore: %w", cleanupErr))
			}
		}
	}()
	// Rehydration applies the recorded quarantine.
	if err := b.containerStore().UpdateBranchQuarantine(id, defaultBranchID, sourceBranch.Quarantined); err != nil {
		return "", fmt.Errorf("restoring quarantine state: %w", err)
	}
	// Importing the memory takes time in proportion to it.
	if err := b.withoutLifecycleLock(prepared.Container, longOperation{state: "restoring", shown: true}, func() error {
		return b.installCheckpointDehydration(prepared.Container, filepath.Join(stage, "state"), manifest.MemoryCompression, !manifest.RestorePaused)
	}); err != nil {
		return "", err
	}
	if err := b.rehydrate(selector{container: id, branch: defaultBranchID}); err != nil {
		return "", fmt.Errorf("rehydrating the restored branch: %w", err)
	}
	placementState := cluster.PlacementRunning
	if manifest.RestorePaused {
		placementState = cluster.PlacementPaused
	}
	if err := b.activateNewContainerPlacementState(reservation, placementState); err != nil {
		return "", err
	}
	cleanup = false
	return id, nil
}
