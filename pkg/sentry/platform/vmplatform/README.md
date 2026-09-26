# VM platform boundary

`platform.Platform` is the selectable Sentry backend boundary. Linux KVM,
Linux systrap, and Darwin HVF are peers at that boundary:

```text
                         platform.Platform
                         /       |       \
                 Linux KVM  Linux systrap  Darwin HVF
                                               |
                                          vmplatform
                                               |
                                       HVF VMEngine + VMM
```

`vmplatform` is an opt-in substrate for backends that need a Linux Sentry to
run inside a host hypervisor without a guest Linux kernel. It is not a base
class for every Sentry platform. In particular, KVM and systrap retain their
existing execution and address-space implementations and must not depend on
this package merely to share names or policy.

The package boundaries are:

* `vmplatform` owns backend-independent GuestRAM, 4 KiB Linux mappings,
  staging/writeback, address-space records, epoch rebuilds, and the
  `platform.Platform` adapter.
* `vmplatform/vmm` exposes only host-hypervisor primitives: VM creation,
  stage-2 mappings, monitor-vCPU operations, exits, and host memory.
* `VMEngine` owns host- and architecture-specific continuation policy. It
  enters application code, returns syscall/fault state to Sentry, interrupts
  application vCPUs, and delivers serialized monitor transactions.
* `platform/hvf` contains the Apple-Silicon implementation: Hypervisor.framework
  calls, the ARM64 EL1 Sentry continuation, and the freestanding monitor.
* `vmplatform/hvf` is the small Darwin registration adapter that constructs the
  generic platform with the HVF VMM and engine.

A future WHPX backend may reuse `vmplatform` by implementing the VMM and
`VMEngine` contracts. That must not add Windows or HVF conditions to generic
Sentry memory-management code. Conversely, changes made only for KVM or
systrap belong in those backends unless a genuinely platform-wide interface is
required.

Hypervisor state, GPAs, monitor roots, vCPU state, and host page geometry are
execution caches. Sentry memory-management state and canonical 4 KiB memory
contents remain authoritative and are the checkpoint boundary.
