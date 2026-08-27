//go:build darwin

package specutils

import (
	"os/exec"

	"github.com/moby/sys/capability"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// GetNS returns the requested OCI namespace entry. OCI specs retain their
// Linux shape on every runsc host even when host namespace operations are not
// available.
func GetNS(nst specs.LinuxNamespaceType, s *specs.Spec) (specs.LinuxNamespace, bool) {
	if s.Linux == nil {
		return specs.LinuxNamespace{}, false
	}
	for _, ns := range s.Linux.Namespaces {
		if ns.Type == nst {
			return ns, true
		}
	}
	return specs.LinuxNamespace{}, false
}

func ApplyNS(specs.LinuxNamespace) (func() error, error) { return nil, unix.EOPNOTSUPP }

// StartInNS intentionally ignores Linux namespace requests on Darwin. This is
// reachable only after runsc's mandatory unsafe trusted-mode validation.
func StartInNS(cmd *exec.Cmd, nss []specs.LinuxNamespace) error { return cmd.Start() }

func SetUIDGIDMappings(cmd *exec.Cmd, s *specs.Spec) {}

func HasCapabilities(cs ...capability.Cap) bool { return false }

// Darwin has no Linux user namespace in which to acquire capabilities.
func MaybeRunAsRoot() error { return nil }

const NullNetNSFilename = "null-netns"

func UnmountNullNetNS(sharedRootDir string) {}

func KnownNamespaces() []string { return nil }
