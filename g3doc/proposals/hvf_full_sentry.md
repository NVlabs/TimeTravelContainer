# Full-Sentry execution on Hypervisor.framework

*Status: experimental design, August 2026.*

## Decision

The Darwin HVF backend should converge on the same execution model as Linux
KVM: one Sentry process, one Go runtime, and one object graph, with Sentry
continuations executing at guest EL1 and returning to host EL0 only for Darwin
services. Linux application code remains at guest EL0. The current distinction
between a direct/correctness application path and a promoted EL1 path is a
transition mechanism, not the final engine architecture.

This can match KVM's execution model, but it cannot copy KVM's memory-coherence
mechanism. Linux KVM registers an MMU notifier against the Sentry process's
`mm`; Hypervisor.framework exposes no equivalent notifier. TTC must provide a
synchronous Darwin VM-mutation gate before arbitrary mutable Sentry memory can
be a safe EL1 data plane.

"The Sentry runs in both domains" does not mean that one continuation executes
twice. A continuation alternates between EL1 and host EL0. Different Sentry
threads may occupy the two domains concurrently, subject to the Go runtime and
the mutation gate.

## What the public contracts establish

Apple documents `hv_vm_map` as mapping a region in the current process's
virtual address space into the VM's intermediate physical address space. The
address and length use the host page granule, and the host memory should be a
single VM region, typically created by `mmap` or `mach_vm_allocate`.

Apple does not document what happens if the region is subsequently changed by
`MADV_FREE_REUSABLE`, `munmap`, `MAP_FIXED`, or a Mach VM overwrite. In
particular, the API does not promise that the stage-2 mapping follows the
future meaning of the same host virtual address.

Linux provides the missing contract inside the kernel. KVM's
`invalidate_range_start` MMU-notifier callback finds every memory slot that
overlaps a changing HVA range, removes the corresponding secondary mappings,
and flushes remote TLBs. The invalidation sequence prevents a KVM page fault
from installing a page that is concurrently being freed. This is why gVisor's
KVM backend can register large Sentry HVA ranges once and let the ordinary
Linux VM subsystem change their backing safely.

Primary references:

* [Apple `hv_vm_map`](https://developer.apple.com/documentation/hypervisor/hv_vm_map(_:_:_:_:))
* [Apple Hypervisor.framework VM lifecycle](https://developer.apple.com/documentation/hypervisor)
* [Linux KVM MMU-notifier implementation](https://github.com/torvalds/linux/blob/master/virt/kvm/kvm_main.c)
* [XNU `MAP_FIXED` overwrite path](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_mman.c)
* [XNU reusable-page path](https://github.com/apple-oss-distributions/xnu/blob/main/osfmk/vm/vm_map.c)

## HVF observations

`//tools/hvf_map_probe` executes raw EL1 loads and stores with stage 1 disabled,
so its results do not depend on TTC's monitor, page tables, Go runtime, or
application-switch code. The following was observed on Apple Silicon, macOS
26.5.2 (Darwin 25.5.0), with the default 16 KiB IPA granule:

| Host operation after `hv_vm_map` | Host view | EL1 view |
| --- | --- | --- |
| ordinary host store | new value | new value |
| EL1 store | new value | new value |
| `MADV_DONTNEED`, then host store | new value | new value |
| `MADV_FREE_REUSABLE`, then `MADV_FREE_REUSE` and host store | new value | new value |
| `MAP_FIXED` anonymous overwrite | new object/new value | old value |
| `munmap`, then `MAP_FIXED` at the same HVA | new object/new value | old value |
| `hv_vm_unmap`, overwrite, then `hv_vm_map` | new object/new value | new value |

The Mach region object ID changed across both overwrite cases. Thus the current
mirror's object-ID comparison can detect an overwrite after it happens, but it
cannot make an already-installed mapping safe during the overwrite.

`MADV_FREE_REUSABLE` set XNU's per-page reusable disposition even after EL1
dirtied the page, and EL1 could still read the original contents. A bounded
2 GiB pressure run reclaimed neither the HVF-mapped 64 MiB target nor an
unmapped host-only control, so that run is inconclusive about eventual
reclamation. It must not be used as a safety guarantee. Qualification on
macOS 15 remains required.

The overwrite result is decisive: `hv_vm_map` does not continuously resolve an
HVA to whatever VM object later occupies that address. An explicit
unmap/remap is a working sequential control on this OS build.

## Target execution boundary

The final split should be:

```text
guest EL0 application
        |
        | Linux syscall / fault / interrupt
        v
guest EL1: complete Sentry Go continuation
        |
        | only when Sentry needs a Darwin service
        v
host EL0: the same Sentry continuation and Go object graph
        |
        | Darwin syscall or host library operation
        v
Darwin kernel
```

The EL1 monitor remains small. It owns exception vectors, application page
tables, context transfer, TLB invalidation, and host-gate entry. It does not
implement Linux syscalls and does not contain a second Sentry. Time or similar
EL1 services may remain as optional leaf optimizations, but correctness must
never depend on a per-syscall fast-path list.

The engine has one application-switch policy:

1. Enter the Sentry continuation at EL1.
2. Enter Linux application code at EL0.
3. Return application syscall/fault state directly to the same EL1 Sentry.
4. Continue running Sentry logic at EL1 until a genuine host service is needed.
5. Redpill that exact continuation to host EL0, perform the service, and
   bluepill it back to EL1.

The direct-host application switch, promotion counters, cooldown, and
correctness-vs-fast-path state disappear after the mutation gate is qualified.

## Darwin VM-mutation gate

Periodic Mach-region rescans are diagnostic and recovery tools; they are not a
coherence protocol. The protocol must bracket every operation that can change
the VM object or physical backing of an HVA visible to EL1:

```text
pre-mutation(range, kind)
    block new EL1 entries
    make the global mirror epoch odd
    request exit from every running HVF vCPU
    wait until no continuation can access stage 2
    hv_vm_unmap every overlapping direct lease

perform Darwin VM operation

post-mutation(range, result)
    resnapshot Mach region/object identity
    update or retire leases
    remap live ranges and reconcile stage 1 as needed
    make the mirror epoch even
    unblock EL1 entries
```

No vCPU may run between the stage-2 unmap and the completed post-mutation
publication. The explicit unmap/remap sequence is the HVF analogue of KVM's
MMU-notifier critical section.

For Darwin Go heap reclamation, the efficient policy is asymmetric:

* Before `MADV_FREE_REUSABLE`, unmap the range from stage 2. Leave it absent
  while the Go runtime considers it unused, so HVF cannot pin memory that the
  scavenger intends to return.
* On `MADV_FREE_REUSE`, re-establish the mapping before any Go access.
* Before `munmap`, `MAP_FIXED`, or a Mach overwrite, unmap the old lease. After
  a successful replacement, map the new object identity. A removed mapping
  stays absent and its GPA lease is retired.
* Reconcile protection changes before resuming EL1. Executable changes also
  perform the required instruction-cache maintenance.

The pre/post path must be allocation-free and safe when invoked by the Go
runtime while runtime locks are held. The vCPU stop handshake therefore uses
preallocated C/assembly state and atomics, not Go maps, channels, or mutexes.
A dedicated coordinator completes any monitor work that cannot be performed
in the runtime hook itself.

## Required Go runtime seam

This is the one unavoidable difference from KVM. Host VM mutations can come
from the concurrent Go scavenger or stack/arena management, not only from a
Sentry continuation that just redpilled for a host syscall. Stopping at normal
HVF exits or rescanning before the next application entry leaves a race window.

The Darwin toolchain needs a narrow, version-checked VM-mutation hook around
the runtime's low-level operations:

* anonymous `mmap` allocation and `MAP_FIXED` commit;
* `munmap`;
* `MADV_FREE_REUSABLE` and `MADV_FREE_REUSE`;
* `PROT_NONE` faulting and protection changes.

The runtime cannot import TTC packages. It exposes a small no-allocation hook
ABI that the HVF backend registers at startup. A disabled hook is one predicted
branch, so ordinary Darwin Go programs and Linux TTC builds are unchanged.
The ABI is tied to the exact Go toolchain version and checked during the build.

Fixed, thread-affine HVF workers keep their native stacks alive for the VM
epoch. Cgo and arbitrary host libraries execute only after a redpill; before
the next bluepill, the mirror coordinator performs a full identity validation.
Any additional host facility that can mutate an EL1-visible range must either
join the hook protocol or be kept outside the mirror.

## Code ownership

* `pkg/ring0` continues to define architecture context and application
  exception semantics shared conceptually by VM backends.
* `pkg/sentry/platform/kvm` retains its Linux-specific bluepill and relies on
  kernel MMU notifiers. It should not depend on the HVF mutation gate.
* `pkg/sentry/platform/vmplatform` owns the portable `Platform`, address-space,
  GuestRAM, and application context contracts.
* `pkg/sentry/platform/hvf` owns continuation transfer, the Sentry host mirror,
  the Darwin mutation coordinator, and HVF vCPU workers.
* The freestanding monitor owns only EL1 mechanisms. Sentry syscall code stays
  in the existing Go packages and remains shared by KVM, systrap, and HVF.

Do not create a second syscall implementation or move the Sentry object graph
into monitor-private memory. Do not refactor KVM onto `vmplatform` merely to
share naming; the two backends have materially different host-coherence
providers.

## Delivery and removal sequence

1. Keep `hvf_map_probe` as a manual macOS-version qualification tool and add
   the replacement/reusable matrix to macOS 15 release qualification.
2. Add the Darwin runtime hook ABI and unit-test its disabled and nested-call
   behavior without HVF.
3. Add a preallocated Sentry-mirror lease table and global mutation epoch.
   Initially unmap all mutable leases for every runtime VM mutation; optimize
   to overlapping ranges only after correctness stress passes.
4. Run full Sentry at EL1 behind an experimental switch on one vCPU. Stress Go
   scavenging, `debug.FreeOSMemory`, recursive forks, stack growth, GC, cgo,
   host syscalls, and repeated `MAP_FIXED` identity replacement.
5. Add multi-vCPU torture. A mutation racing any EL1 load/store must either
   complete before that access or stop it before backing replacement. There
   must be zero stale-value observations and zero allocator corruption.
6. Make full-Sentry execution the only HVF engine, then remove direct-host
   application switching, promotion/cooldown state, and syscall-specific
   correctness fallbacks.

This sequence keeps the known-correct engine available until the mechanism
that replaces Linux's MMU notifier is independently proven.
