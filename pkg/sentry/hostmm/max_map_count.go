// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostmm

import (
	"os"
	"strconv"
	"strings"
	"sync"

	"gvisor.dev/gvisor/pkg/log"
)

// defaultMaxMapCount is Linux's default vm.max_map_count, which MaxMapCount
// assumes where the host does not show its own.
const defaultMaxMapCount = 65530

var maxMapCount struct {
	once sync.Once
	n    int

	// limit, if positive, is a test's stand-in for a lower host limit.
	limit int
}

// LimitMaxMapCount makes MaxMapCount return at most n, standing in for a host
// whose limit is lower, for tests. It must be called before MaxMapCount.
func LimitMaxMapCount(n int) {
	maxMapCount.limit = n
}

// MaxMapCount returns the host's limit on how many mappings a process may
// have (vm.max_map_count). The Sentry first calls it while it starts, before
// its syscall filters forbid reading the limit.
func MaxMapCount() int {
	maxMapCount.once.Do(func() {
		n := defaultMaxMapCount
		data, err := os.ReadFile("/proc/sys/vm/max_map_count")
		if err == nil {
			var host int
			if host, err = strconv.Atoi(strings.TrimSpace(string(data))); err == nil && host > 0 {
				n = host
			}
		}
		if err != nil {
			log.Warningf("Reading vm.max_map_count: %v; assuming %d", err, defaultMaxMapCount)
		}
		if limit := maxMapCount.limit; limit > 0 && limit < n {
			log.Infof("Assuming a limit of %d host mappings per process, below the host's %d", limit, n)
			n = limit
		}
		maxMapCount.n = n
	})
	return maxMapCount.n
}
