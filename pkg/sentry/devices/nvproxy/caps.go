package nvproxy

import "gvisor.dev/gvisor/pkg/sentry/devices/nvproxy/nvconf"

// compUtil is the default compute+utility capability set used by the
// platform-independent NVIDIA ABI descriptions.
const compUtil = nvconf.CapCompute | nvconf.CapUtility
