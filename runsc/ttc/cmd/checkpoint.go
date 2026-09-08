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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	ttccheckpoint "gvisor.dev/gvisor/runsc/ttc/checkpoint"
)

type checkpointRequest struct {
	target      selector
	destination string
	compression string
}

type checkpointCommandBackend interface {
	checkpoint(request checkpointRequest) error
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
	}); err != nil {
		return err
	}
	fmt.Fprintln(stdout, fs.Arg(1))
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

// normalizeCheckpointCommandArgs resolves checkpoint files in the client
// process before forwarding to a daemon with an unrelated working directory.
func normalizeCheckpointCommandArgs(args []string) ([]string, error) {
	if len(args) == 0 || (args[0] != "checkpoint" && args[0] != "restore") {
		return args, nil
	}
	normalized := append([]string(nil), args...)
	if args[0] == "checkpoint" {
		fs := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
		_ = fs.String("compression", "flate-best-speed", "")
		fs.SetOutput(io.Discard)
		if err := fs.Parse(args[1:]); err != nil {
			return nil, err
		}
		if fs.NArg() != 2 {
			return args, nil
		}
		index := len(normalized) - 1
		absolute, err := filepath.Abs(normalized[index])
		if err != nil {
			return nil, err
		}
		normalized[index] = absolute
		return normalized, nil
	}
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	_ = fs.String("name", "", "")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args[1:]); err != nil {
		return nil, err
	}
	if fs.NArg() != 1 {
		return args, nil
	}
	operand := fs.Arg(0)
	absolute, err := filepath.Abs(operand)
	if err != nil {
		return nil, err
	}
	for index := len(normalized) - 1; index >= 1; index-- {
		if normalized[index] == operand {
			normalized[index] = absolute
			break
		}
	}
	return normalized, nil
}

func (b *runtimeBackend) checkpoint(request checkpointRequest) (retErr error) {
	target, info, err := b.resolveSelector(request.target)
	if err != nil {
		return err
	}
	families, err := b.listRuntimeFamilies()
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

	if _, err := b.command("timetravel", "--source", target.branch, "--paused", info.ID, "fork", hiddenBranch); err != nil {
		return fmt.Errorf("creating checkpoint snapshot branch: %w", err)
	}
	hiddenExists := true
	defer func() {
		if hiddenExists {
			if _, err := b.command("timetravel", info.ID, "destroy", hiddenBranch); err != nil && retErr == nil {
				retErr = fmt.Errorf("destroying checkpoint snapshot branch: %w", err)
			}
		}
	}()
	stateDir := filepath.Join(stage, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		return err
	}
	if _, err := b.command(
		"timetravel",
		"--image-path", stateDir,
		"--compression", request.compression,
		info.ID, "checkpoint", hiddenBranch,
	); err != nil {
		return fmt.Errorf("saving checkpoint state: %w", err)
	}

	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	manager, err := b.volumeManager()
	if err != nil {
		return err
	}
	persistentBranch := ttcvolume.ScopedBranchID(info.ID, hiddenBranch)
	manifestRepositories := make([]ttccheckpoint.RepositorySnapshot, 0, len(repositories))
	var opened []*ttcvolume.Repository
	defer func() {
		for _, repository := range opened {
			if err := repository.RemoveBranch(persistentBranch); err != nil && !errors.Is(err, os.ErrNotExist) && retErr == nil {
				retErr = fmt.Errorf("removing checkpoint filesystem branch: %w", err)
			}
			_ = repository.Close()
		}
	}()
	for index, config := range repositories {
		repository, err := manager.OpenPrivate(config.Path)
		if err != nil {
			return fmt.Errorf("opening checkpoint repository %q: %w", config.Source, err)
		}
		opened = append(opened, repository)
		relative := filepath.Join("repositories", strconv.Itoa(index))
		if _, err := repository.ExportSnapshot(persistentBranch, filepath.Join(stage, relative)); err != nil {
			return fmt.Errorf("exporting checkpoint repository %q: %w", config.Source, err)
		}
		manifestRepositories = append(manifestRepositories, ttccheckpoint.RepositorySnapshot{
			Source: config.Source,
			Path:   relative,
		})
	}

	var hostMounts []ttccheckpoint.HostMountSnapshot
	for index, mount := range spec.Mounts {
		if mount.Type != "bind" {
			continue
		}
		sourceInfo, err := os.Stat(mount.Source)
		if err != nil {
			return fmt.Errorf("stating checkpoint host mount %q: %w", mount.Source, err)
		}
		if !sourceInfo.Mode().IsRegular() && !sourceInfo.IsDir() {
			return fmt.Errorf("checkpoint requires bind mount %q at %q to resolve to a regular file or directory", mount.Source, mount.Destination)
		}
		relative := filepath.Join("host-mounts", strconv.Itoa(index))
		if err := copyCheckpointHostPath(mount.Source, filepath.Join(stage, relative)); err != nil {
			return fmt.Errorf("capturing host mount %q: %w", mount.Source, err)
		}
		hostMounts = append(hostMounts, ttccheckpoint.HostMountSnapshot{
			MountIndex: index,
			Source:     mount.Source,
			Path:       relative,
		})
	}
	if _, err := b.command("timetravel", info.ID, "destroy", hiddenBranch); err != nil {
		return fmt.Errorf("destroying checkpoint snapshot branch: %w", err)
	}
	hiddenExists = false
	if err := ttccheckpoint.WriteManifest(stage, ttccheckpoint.Manifest{
		SourceBranch:  target.branch,
		RestorePaused: sourceState == "paused",
		Container:     info,
		Spec:          *spec,
		Repositories:  manifestRepositories,
		HostMounts:    hostMounts,
	}); err != nil {
		return err
	}
	if err := ttccheckpoint.Pack(stage, request.destination); err != nil {
		return err
	}
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
	if err := ttccheckpoint.Unpack(source, stage); err != nil {
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
	prepared, err := b.containerStore().PrepareRestore(context.Background(), ttcbundle.RestoreOptions{
		Name:             name,
		Container:        manifest.Container,
		Spec:             &manifest.Spec,
		Repositories:     repositories,
		HostMounts:       hostMounts,
		VolumeCopyMode:   b.volumeCopyMode,
		RestorePaused:    manifest.RestorePaused,
		CheckpointBranch: manifest.SourceBranch,
		NodeID:           b.nodeID,
	})
	if err != nil {
		return "", err
	}
	id = prepared.Container.ID
	cleanup := true
	defer func() {
		if cleanup {
			if err := b.remove(id, true, true); err != nil && retErr == nil {
				retErr = fmt.Errorf("cleaning failed restore: %w", err)
			}
		}
	}()
	configured := b.configuredRunscArgsForNetworkConfig(
		prepared.Container.NetworkMode,
		prepared.Container.NetworkNAT,
		prepared.Container.NetworkIP,
		prepared.Container.NetworkMAC,
	)
	status, err := b.runConfiguredCommandWithArgs(
		configured,
		"restore",
		"--bundle", prepared.BundleDir,
		"--image-path", filepath.Join(stage, "state"),
		"--detach",
		id,
	)
	if err != nil || status != 0 {
		if err == nil {
			err = fmt.Errorf("runsc restore exited with status %d", status)
		}
		return "", err
	}
	target := selector{container: id, branch: defaultBranchID}
	if _, err := b.ensureConsoleDrain(target, true); err != nil {
		return "", fmt.Errorf("starting restored console drain: %w", err)
	}
	store := b.containerStore()
	if err := store.UpdateBranchQuarantine(id, defaultBranchID, sourceBranch.Quarantined); err != nil {
		return "", fmt.Errorf("restoring quarantine state: %w", err)
	}
	if err := store.UpdateState(id, "running", nil); err != nil {
		return "", err
	}
	if manifest.RestorePaused {
		if _, err := b.command("timetravel", id, "pause", defaultBranchID); err != nil {
			return "", fmt.Errorf("pausing restored container: %w", err)
		}
		if err := store.UpdateState(id, "paused", nil); err != nil {
			return "", err
		}
	}
	cleanup = false
	return id, nil
}
