//go:build linux && arm64

package pagetables

// GetASIDBits returns the active Linux host ASID width. It is implemented in
// assembly because Linux permits the required architectural register reads.
func GetASIDBits() uint8

func archASIDBits() uint8 { return GetASIDBits() }
