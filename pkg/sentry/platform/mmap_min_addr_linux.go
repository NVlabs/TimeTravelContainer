//go:build linux

package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const systemMMapMinAddrSource = "/proc/sys/vm/mmap_min_addr"

func hostMMapMinAddr() uint64 {
	b, err := os.ReadFile(systemMMapMinAddrSource)
	if err != nil {
		panic(fmt.Sprintf("couldn't open %s: %v", systemMMapMinAddrSource, err))
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		panic(fmt.Sprintf("couldn't parse %s from %s: %v", string(b), systemMMapMinAddrSource, err))
	}
	return value
}
