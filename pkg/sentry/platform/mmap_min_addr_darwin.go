//go:build darwin

package platform

// hostMMapMinAddr returns TTC's portable Linux guest policy. HVF guest virtual
// addresses are translated by the EL1 monitor and are not Darwin mmap VAs.
func hostMMapMinAddr() uint64 { return 64 << 10 }
