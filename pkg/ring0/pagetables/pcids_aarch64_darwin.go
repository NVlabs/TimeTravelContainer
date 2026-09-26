//go:build darwin && arm64

package pagetables

// GetASIDBits returns the ASID width selected by the EL1 monitor's TCR_EL1.
// Host userspace must not read TCR_EL1 or ID_AA64MMFR0_EL1 on Darwin.
func GetASIDBits() uint8 { return 16 }

func archASIDBits() uint8 { return GetASIDBits() }
