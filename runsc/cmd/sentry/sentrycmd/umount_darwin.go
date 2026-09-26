//go:build darwin

package sentrycmd

import (
	"context"
	"os"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
)

// Umount preserves the internal command ABI on Darwin. Trusted mode creates
// no private proc mount, so the command only participates in synchronization.
type Umount struct{ syncFD int }

func (*Umount) Name() string     { return "umount" }
func (*Umount) Synopsis() string { return "wait for trusted-mode mount teardown" }
func (*Umount) Usage() string    { return "umount --sync-fd=FD <directory path>\n" }
func (u *Umount) SetFlags(f *flag.FlagSet) {
	f.IntVar(&u.syncFD, "sync-fd", -1, "")
}
func (*Umount) FetchSpec(*config.Config, *flag.FlagSet) (string, *specs.Spec, error) {
	return "", nil, nil
}
func (u *Umount) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() != 1 || u.syncFD < 0 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	syncFile := os.NewFile(uintptr(u.syncFD), "sync file")
	defer syncFile.Close()
	var b [1]byte
	if n, err := syncFile.Read(b[:]); err != nil || n != 1 {
		util.Fatalf("unable to synchronize trusted-mode mount teardown: read %d bytes: %v", n, err)
	}
	return subcommands.ExitSuccess
}
