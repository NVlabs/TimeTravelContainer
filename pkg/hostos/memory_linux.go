//go:build linux

package hostos

import "os"

// TotalSystemMemory extracts MemTotal from Linux procfs.
func TotalSystemMemory() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return parseTotalSystemMemory(f)
}
