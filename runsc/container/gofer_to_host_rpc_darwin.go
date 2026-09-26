//go:build darwin

package container

import (
	"fmt"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/urpc"
)

// OpenMountArgs represents a host mount request from a gofer.
type OpenMountArgs struct {
	Mount *specs.Mount
	Flags uint32
}

// OpenMountResult contains a donated host descriptor.
type OpenMountResult struct{ urpc.FilePayload }

// goferToHostRPC retains the RPC ABI, but trusted-mode Darwin gofers always
// open their configured source paths directly and must never call OpenMount.
type goferToHostRPC struct{ goferPID int }

func (*goferToHostRPC) OpenMount(*OpenMountArgs, *OpenMountResult) error {
	return fmt.Errorf("host mount RPC is unavailable on native macOS: %w", unix.EOPNOTSUPP)
}
