//go:build darwin

// Package embeddedbinary embeds an external binary and provides a function to
// execute it on Darwin.
package embeddedbinary

import (
	"bytes"
	"compress/flate"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

const BinaryName = "embedded.bin.name"

//go:embed embedded.bin.flate
var compressedBinary []byte

type Options struct {
	Argv        []string
	Envv        []string
	Files       []uintptr
	SysProcAttr *unix.SysProcAttr
}

const _ = flate.NoCompression

func materialize() (string, string, error) {
	parentDir := os.TempDir()
	tmpDir, err := os.MkdirTemp(parentDir, "gvisor.*.tmp")
	if err != nil {
		return "", "", fmt.Errorf("cannot create temp directory: %w", err)
	}
	binPath := filepath.Join(tmpDir, BinaryName)
	tmpFile, err := os.OpenFile(binPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("cannot create temporary executable: %w", err)
	}
	binaryReader := flate.NewReader(bytes.NewReader(compressedBinary))
	if _, err := io.Copy(tmpFile, binaryReader); err != nil {
		_ = tmpFile.Close()
		_ = os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("cannot decompress embedded binary: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("cannot close temporary executable: %w", err)
	}
	return binPath, tmpDir, nil
}

// cleanupAfterExec starts a tiny helper that blocks on a close-on-exec pipe.
// A successful exec closes writeEnd and lets the helper unlink the temporary
// executable; a failed exec closes it through the returned cleanup function.
func cleanupAfterExec(tmpDir string) (func(), error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	proc, err := os.StartProcess("/bin/sh", []string{"sh", "-c", `IFS= read -r _ <&3 || true; rm -rf -- "$1"`, "sh", tmpDir}, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr, readEnd},
	})
	_ = readEnd.Close()
	if err != nil {
		_ = writeEnd.Close()
		return nil, err
	}
	_ = proc.Release()
	return func() { _ = writeEnd.Close() }, nil
}

func run(options *Options, fork bool) (int, error) {
	if len(options.Argv) == 0 {
		options.Argv = []string{BinaryName}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	oldMask := unix.Umask(0077)
	binPath, tmpDir, err := materialize()
	unix.Umask(oldMask)
	if err != nil {
		return 0, err
	}
	if fork {
		pid, err := syscall.ForkExec(binPath, options.Argv, &syscall.ProcAttr{
			Env: options.Envv, Files: options.Files, Sys: options.SysProcAttr,
		})
		_ = os.RemoveAll(tmpDir)
		return pid, err
	}
	closeCleanupPipe, err := cleanupAfterExec(tmpDir)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return 0, fmt.Errorf("cannot arrange temporary executable cleanup: %w", err)
	}
	defer closeCleanupPipe()
	if err := unix.Exec(binPath, options.Argv, options.Envv); err != nil {
		return 0, fmt.Errorf("cannot exec embedded binary: %w", err)
	}
	panic("unreachable")
}

func Exec(options Options) error {
	_, err := run(&options, false)
	return err
}

func ForkExec(options Options) (int, error) { return run(&options, true) }
