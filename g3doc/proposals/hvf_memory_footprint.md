# HVF memory footprint: demand-paged stage 2

*Status: in progress, September 2026; phases 1-8 of the delivery plan are
implemented. Measurements: Apple M5 Pro, macOS 26.5.2.*

## Summary

An idle ubuntu container's Sentry has a 308 MiB physical footprint on HVF,
and running python and tmux twice raises it to 578 MiB. The guest itself uses
about 8 MiB. Nearly everything else is the Sentry's own memory, which the HVF
engine mirrors into stage 2 in a way that commits whole regions and never
returns them. Most of that is a fixed cost that every container family pays
again, since each family has its own Sentry.

Measured primitives show that Hypervisor.framework supports what Linux KVM
provides: lazily populated, coherent stage-2 mappings of untouched memory,
stage-2 faults delivered to the VMM, and release once a range is unmapped. The
design below uses them to make the Sentry's footprint track the memory it
touches, like KVM, subject to one hard constraint: pages that the host
executes must never enter stage 2.

Progress, for the same container and sequence:

| Build | Idle | After python and tmux, twice each |
|---|---|---|
| Before | 308 MiB | 578 MiB |
| Phase 1: FD-table fix, 1 MiB thread stacks, no checkpoint gofer | 233 MiB | 305 MiB |
| Phase 2: Sentry image mirrored on first EL1 touch | 109 MiB | 192 MiB |
| Phase 3: monitor page-table pool grown on demand | 93 MiB | 176 MiB |
| Phase 4: Go runtime regions mirrored on first EL1 touch | 55 MiB | 137 MiB |

What remains at idle: Go runtime regions (33 MiB: the live heap of 11-20
MiB, memory the scavenger has not yet returned, and runtime metadata), thread
stacks (5 MiB), copies of the image text that EL1 has run (3 MiB), guest RAM
(3 MiB), and the rest of the EL1 monitor (2 MiB).

A busy guest adds a second large cost: GuestRAM keeps a `guestFrame`, a
mapping set, and two index entries for every 4 KiB guest frame, about 300
bytes, and Go's heap goal doubles that. Touching 1.5 GiB in the guest grows
the Sentry's runtime regions by 280 MiB. Until phase 5 none of it returned
after the guest freed the memory, since the mirror suppressed the runtime's
releases. Now the scavenger returns it once a GC has collected it: within
five minutes on an idle Sentry, where the runtime forces a GC every two, and
sooner on a busy one. Twice allocating 1 GiB and running 200 processes left
the footprint at 595 MiB before phase 5 and 320 MiB after it.

`apt-get install build-essential` in a fresh ubuntu container shows where a
busy Sentry's memory goes. Its footprint peaks at about 650 MiB. After apt
exits it stayed at 440 MiB after phase 5, falling about 15 MiB per forced GC,
and did not fall at all before it. At that point:

| Memory | MiB | |
|---|---|---|
| Go runtime regions | 171 | 66 MiB live: the writable layer's metadata, mostly TTC volume and snapshot state (28 MiB), tmpfs inodes and dentries (19 MiB), and file names (7.5 MiB). The installed files themselves are in the volume on disk. |
| Pages charged twice | ~180 | What both the host and the VM had touched: above all the Go heap, and guest RAM. Dropping the mirror, as a fork does, freed 134 MiB of it at once. |
| Page tables | 33 | Hypervisor.framework's, grown by vCPU churn (below). |
| Thread stacks | 25 | 1 MiB for each of 37 threads, committed when mirrored. |
| Guest RAM | 27 | |

The Sentry ran 217 garbage collections in the 68 s install. Phase 6 trims the
idle Sentry, bringing the footprint to 230 MiB two seconds after apt exits.

vCPU churn has a cost of its own. An owner-affine vCPU was destroyed
whenever its task blocked, and created again when it ran, over 5,000 times
while 400 MiB passed through a pipe. Every `hv_vcpu_create` and
`hv_vcpu_destroy` pair grows the process's page tables by about 13 KiB, up to
a plateau of about 32 MiB per process that never shrinks, even after
`hv_vm_destroy`. Phase 7 keeps each thread's vCPU between residencies:

| Workload | vCPUs created before | after | Page tables after | Time before | after |
|---|---|---|---|---|---|
| 400 MiB through a pipe | 5,596 | 8 | 0.9 MiB | 0.40 s | 0.26 s |
| 300 process spawns | 1,559 | 15 | 1.1 MiB | 0.70 s | 0.58 s |
| `apt-get install --reinstall gdb` | 2,041 | 45 | 1.8 MiB | 8.6 s | 6-7 s |
| gdb running itself | 267 | 14 | 1.1 MiB | 1.0 s | 0.8 s |

Removing branches must return their memory too, since a family's branches
share its Sentry. Take a container with gdb installed, run gdb in it, fork it
four times, quit gdb in every branch, and remove the four forks. Without
phases 1 to 6, the Sentry's footprint went from 633 MiB with gdb running to
790 MiB, and did not fall. With them it left 286 MiB, down to 195 MiB only
after two and a half minutes: the trim followed only applications, and
removing a branch runs none. The Sentry releases the branch's kernel and
rewrites the snapshot graph of the family's writable layer, a 9 MiB JSON
document for which `encoding/json` grows a 16 MiB buffer, then keeps it in a
`sync.Pool`; about 50 MiB of garbage per branch. The trim now also follows the
Sentry's own allocation, and collects twice so that pools let go. Eight rounds
of four forks, gdb in each, and their removal leave the Sentry at 114 to 136
MiB four seconds after each round, from 102 MiB before the first. The page
tables of vCPU churn were nearly all the difference, 32 MiB after the eighth
round, and 5 MiB after phase 7, which leaves the Sentry at 112 MiB.

## Where the memory went

Idle Sentry before this work, fresh start, `vmmap` dirty memory by region:

| Memory | MiB | Why it is resident |
|---|---|---|
| Writable image (`__DATA`, `__bss`) | 81.7 | Committed whole before mirroring. Two blobs that a container never touches account for 81 MiB: the embedded GCS checkpoint gofer (a 49 MiB `[]byte`) and Go 1.26's FIPS entropy scratch buffer (32 MiB of bss). |
| Copies of read-only image segments | 46.6 | Every `__TEXT` (40.6), `__DATA_CONST` (5.7), and `__LINKEDIT` slab is copied into a private anonymous 16 KiB slab. |
| EL1 monitor | 17.1 | Its 16 MiB page-table pool is committed. |
| Go runtime regions (VM tag 240) | 73 | Every writable runtime region is imported and committed whole; the live heap is 11-14 MiB. |
| Thread stacks | 32.6 | cgo copies the creator's 8 MiB stack size; a mirrored stack is committed whole. |
| Guest RAM | ~3 | Demand-allocated already; gVisor accounts 5 MiB anonymous and 3 MiB page cache. |

About 50 MiB more of the footprint lies outside any user region.

Growth over python and tmux comes from two sources. Each new thread commits
8 MiB of stack (33 to 137 MiB). Every Go heap spike becomes permanent: the
runtime VM hook suppresses `munmap` and `MADV_FREE_REUSABLE` for mirrored
ranges for the VM's lifetime (73 to 188 MiB, while the live heap returns to
about 21 MiB). The largest spike is a gVisor bug: `FDTable.Remove` grows the
descriptor table to cover the descriptor it removes, and the tmux client calls
`close(0x7fffffff)`, so each client allocates 32 MiB.

## Measured primitives

`//tools/hvf_map_probe:hvf_mem_probe` and `:hvf_map_probe` run raw EL1 loads,
stores, and branches with stage 1 disabled. On macOS 26.5.2:

| Primitive | Observation |
|---|---|
| Untouched anonymous memory | `hv_vm_map` costs no footprint. The kernel populates stage 2 on first guest access. Host and guest views stay coherent in every order, including pages the host read first. Only touched pages become resident. |
| Ordinary data files | Private and shared, read-only and read-write mappings all work and stay coherent, including copy-on-write in both directions. Read-only pages come from the page cache, with no footprint. `F_PUNCHHOLE` while mapped zeroes both views. |
| Mach memory entries | Aliases of one named entry stay coherent with no commit. |
| Page changes within an object | `MADV_DONTNEED`, and `MADV_FREE_REUSABLE` followed by `MADV_FREE_REUSE`, stay coherent (`hvf_full_sentry.md`). |
| Object replacement | `MAP_FIXED` and `munmap` then `mmap` leave stage 2 on the old object. The mapping must be removed before and restored after (`hvf_full_sentry.md`). |
| Release while mapped | `MADV_FREE_REUSABLE`, `MADV_DONTNEED`, and `MADV_FREE` free nothing; contents survive. |
| Release after `hv_vm_unmap` | Each of them, and `mach_vm_deallocate`, drops the footprint at once; a later `hv_vm_map` works. |
| Wiring | HVF does not wire mapped pages. |
| Stage-2 faults | An unmapped IPA exits with EC 0x24 and the IPA, PC left on the faulting instruction. Map and resume: 5.2 us per fault, versus 0.7 us for a plain entry and exit. A write to a read-only mapping is a permission fault, resolved by `hv_vm_protect`. |
| Map cost | `hv_vm_map` 0.37 us and `hv_vm_unmap` 0.20 us per 16 KiB slab; about 1 us for a 512 MiB range. |
| vCPU lifetimes | Each `hv_vcpu_create` and `hv_vcpu_destroy` pair grows the task's page tables by about 13 KiB, on the same thread or a new one, until they plateau at about 32 MiB. Neither destroying the vCPUs, destroying the VM, nor idle time returns them. On one thread, a create takes 13-22 us and a destroy 7-9 us; 32 threads creating at once take about 1 ms each. |
| vCPU ownership | A thread owns at most one vCPU (a second `hv_vcpu_create` returns `HV_BUSY`), and only it can destroy the vCPU (another thread gets `HV_BAD_ARGUMENT`). `hv_vm_destroy` returns `HV_BUSY` while any vCPU exists, and a process has one VM at a time. An idle vCPU costs about 95 KiB. |
| Footprint accounting | A page that both the host and the guest have touched counts twice in the task's physical footprint: a 16 MiB region costs 16 MiB when the host writes it and 16 MiB more when the guest then reads it, and `hv_vm_unmap` returns the second 16 MiB. `vmmap` shows only the host's share in its per-region figures. |

### The hard constraint

Apple's Secure Page Table Monitor rejects a stage-2 mapping of a page that the
host executes, and it does so by panicking the host:

```text
[SPTM] VIOLATION_ILLEGAL_MAPPING_TYPE: sptm_map_page(sptm.c:485) - ...
page_fte->type(0xe) ...
```

`hv_vm_map` of dyld's own `__TEXT` address returns `HV_ERROR`, but a second
plain `mmap` of the executable file reaches the same physical pages and gets
past that check. So the Sentry's code cannot be shared through the executable's
page cache; stage 2 must see a copy in memory the host never executes. Guest
file mappings are safe: VM platforms copy file data into GuestRAM
(`forcePageCache`, staging), and the Sentry mirror skips file-backed regions.

The engine makes its copies with `mach_vm_read_overwrite`, which may share the
source's pages copy-on-write. It writes each copied page before `hv_vm_map`;
without that commit, a guest read of a page still shared with the executable
would map the executable's own frame. Keep the commit for copies, whatever
else becomes lazy.

## Design

Principles, in KVM's terms:

1. **Map on first touch.** Stage-2 translations exist only for Sentry memory
   that EL1 touches. The kernel populates an uncommitted `hv_vm_map` on
   demand, but the engine cannot rely on that yet: with its mirror mappings
   left uncommitted, the continuation qualification test
   (`TestRing0ContinuationTransfer`) fails with a spurious redpill in
   `ApplicationProbe`, for a reason not yet understood, and the copies above
   must be committed regardless. The engine instead imports memory when EL1
   first touches it. The Sentry's stage 1 maps only imported slabs, so an
   access to any other slab takes the host gate, and the next application
   entry imports a small window around it, committing it before
   `hv_vm_map`. A host gate resumes the continuation at host EL0, which
   ordinary Sentry code tolerates. Code that executes EL1-only instructions
   after checking that it runs at EL1 does not: the ring0asm bridge, the
   native runtime hooks, and the guest-critical section of
   `tryRunResidentMonitor`. The engine imports those, and the data they use,
   before the first entry.
2. **Invalidate before the host changes backing.** The Go runtime VM hook is
   the MMU notifier. Before `MADV_FREE_REUSABLE`, `munmap`, or `MAP_FIXED`,
   remove the stage-2 translations (with the existing vCPU rendezvous), then
   let the operation run. Map again on `MADV_FREE_REUSE` or after a
   replacement. This is the policy `hvf_full_sentry.md` planned; the release
   measurements above show that it returns memory.
3. **Never put host-executed pages in stage 2**, and never map the
   executable's own file pages. Stage 2 sees private anonymous memory, shared
   anonymous memory that TTC owns, or plain data files that nothing executes.

| Memory | Today | Proposed | Expected per family |
|---|---|---|---|
| Go runtime regions | Imported in 2 MiB windows on first touch (phase 4); unmapped before release, mapped again on reuse (phase 5) | unchanged | tracks the live heap, and falls after spikes |
| Thread stacks | Imported from the top, in windows on first touch (phase 8) | unchanged | ~2 MiB |
| Writable image | Imported in 528 KiB windows on first touch (phases 2 and 4) | unchanged | about 1 MiB |
| `__TEXT` | Private copies of 48 KiB windows on first touch (phase 2) | One copy per binary UUID in a shared object, mapped read+exec | ~0; 40.6 MiB once per host |
| `__DATA_CONST`, `__LINKEDIT` | Private copies of windows on first touch (phase 2) | unchanged (contents are slid per process) | under 1 MiB |
| Monitor page-table pool | 2 MiB, doubled on demand (phase 3) | unchanged | under twice the pages in use, or 16 MiB more |
| GuestRAM frame metadata | ~300 bytes of Go heap per 4 KiB guest frame | Fixed-size records in per-slab arrays, with a frame's usual single mapping inline | a fraction of today's, with no per-frame maps |
| Guest RAM | demand-allocated | unchanged | unchanged |

The shared `__TEXT` copy is built once from the Sentry binary and keyed by its
`LC_UUID`. A Sentry whose UUID does not match falls back to a private copy. The
copy can live in a file under the daemon root, mapped `MAP_SHARED` read-only,
or in a POSIX shared memory object whose descriptor ttcd passes to each
Sentry. Either way its pages are ordinary memory that no host process
executes, like today's private copies.

## Delivery

Each phase stands alone and is measured before the next.

1. **Independent fixes (done).** Stop `FDTable.Remove` from growing the table
   for descriptors that are not open (a gVisor bug on every platform). Give
   Sentry threads 1 MiB stacks. Stop building the GCS checkpoint gofer into
   the macOS binary.
2. **Image on first touch (done).** Import the Sentry image through host
   gates in 48 KiB windows, instead of whole at bootstrap; give image slabs
   fixed GPAs; import the guest-only code and data first. Wider windows import
   more of the image for no measurable speedup: 256 KiB windows leave the
   idle Sentry at 137 MiB. Single slabs save 5 MiB more but slow a cold
   python start by 10-15 ms. A fresh VM takes one host gate per window
   that a code path first touches; the syscall return path takes about 8.
3. **Monitor pool on demand (done).** Start the page-table pool at 2 MiB
   rather than 16, and double it on exhaustion through the existing
   expansion protocol. An idle Sentry uses 0.8 MiB of it and `apt-get update`
   1.5 MiB.
4. **Runtime regions on first touch (done).** Import the Go runtime's regions
   in 2 MiB windows too; a heap arena is 64 MiB, while the live heap is a few
   MiB. The guest-critical section of `tryRunResidentMonitor` now reads the
   heap before it starts and probes the stack its calls use, so neither can
   take a host gate inside it. The allocator's frontier moves through the
   heap, so windows narrower than 2 MiB cost more host gates; the writable
   image, where the runtime's hot globals live, went back to 528 KiB windows
   for the same reason. Timings match phase 2's. Stacks were still imported
   whole until phase 8: the native runtime hooks run on them.
5. **Release instead of suppress (done).** Before the scavenger's
   `MADV_FREE_REUSABLE` of a mirrored range, the runtime VM hook unmaps it
   from stage 2 under the vCPU rendezvous, and leaves it dormant until
   `MADV_FREE_REUSE` maps it again; the hook already had both paths, and
   suppressed the release only to avoid the rendezvous. The runtime touches
   scavenged memory only after reusing it, so EL1 never meets a dormant slab.
   `munmap` of a mirrored range is still suppressed, since the runtime may
   retire metadata that an EL1 continuation still addresses. The
   rendezvous has a cost: a 200-process burst took up to 18% longer while
   the scavenger released an earlier 1 GiB spike.
6. **Idle trim (done).** Once no application has run for two seconds, the
   engine drops the Sentry mirror, as a checkpoint does, and returns all free
   Go heap to Darwin. Without the mirror, the release needs no vCPU
   rendezvous. The next application entry mirrors its working set again.
   The Sentry's own work counts too: allocating 1 MiB between two looks, 250
   ms apart, keeps it busy, and 32 MiB allocated since the last trim is worth
   another, which is what removing a branch does. The trim collects twice,
   since what a `sync.Pool` holds survives one collection.
7. **Keep vCPUs across residencies (done).** When a residency ends, the thread
   keeps its vCPU, parked, and the next residency on the thread, of any task,
   takes it. Nearly every vCPU the engine created had been on a thread whose
   residency ended moments before; the workloads above ran on 7 to 21 threads.
   A parked vCPU holds a vCPU ID, and the epoch rollover must destroy every
   vCPU before it can destroy the VM, so the engine must be able to retire one
   on demand, though only its thread can, and not from a signal handler. The
   patched Go runtime provides the thread's safe points: while retirements are
   pending, every thread calls TTC before each libc call, which the scheduler
   makes whenever it looks for work, and TTC wakes idle threads, which sleep
   in a condition-variable loop that tolerates spurious wakeups, to reach one.
   A thread blocked in a system call reaches none, and some Sentry goroutines
   block in one indefinitely, such as the urpc server's `poll`. So a thread
   retires its parked vCPU when it exits, and before a goroutine's system call
   unless the goroutine declares that its system calls end
   (`hostthread.SyscallsEnd`): task goroutines, whose system calls are file
   I/O and gofer requests, the NAT relay, on non-blocking sockets, the
   timekeepers, and snapshot `fsync` workers. The remaining churn is the
   control plane's: eight rounds of four forks, gdb in each, and their removal
   create 308 vCPUs, leaving 5 MiB of page tables, mostly for urpc handlers,
   which may block indefinitely.
8. **Stacks on first touch (done).** Import the top 80 KiB of a thread's
   stack, and the rest in windows as EL1 touches it, rather than all of it. At
   EL1 it holds only the Go runtime's frames; goroutines run on stacks in the
   heap. The native runtime hooks run on it, called through `asmcgocall`, so
   each probes 512 bytes below its frame before it checks where it runs: a
   host gate for a stack slab not yet mirrored then happens before its
   EL1-only instructions. In eight rounds of four forks with gdb in each, the
   stacks of 40 threads cost 2 MiB rather than 28.
9. **Compact GuestRAM frame metadata.** Not HVF-specific, but only VM
   platforms keep it.
10. **Writable-layer metadata.** Not HVF-specific: the Sentry keeps TTC volume
    and snapshot state for the writable layer beside tmpfs's own metadata for
    it, 55 MiB for build-essential.
11. **Shared `__TEXT` (postponed).** Its probe could panic the host, so it
    waits for a time when the host can be rebooted.

## Open questions

* **macOS 15.** Every observation above is from macOS 26.5.2. Phase 5
  relies on release after `hv_vm_unmap`; run `hvf_mem_probe` and
  `hvf_map_probe` on the oldest supported release.
* **Uncommitted mirror mappings.** Why does the continuation qualification
  test fail when mirror mappings are not committed first? Committing only on
  first touch makes this moot for now, but lazy population would also avoid
  the host gate per window.
* **Read+exec of shared memory.** Today's private copies are anonymous pages
  mapped read+exec. The shared copy's pages are ordinary memory too, but
  mapping a shared object read+exec has not been measured, and a wrong answer
  panics the host. Qualify it with a minimal, deliberate probe case first.
* **Rendezvous cost of releases.** Scavenger releases now stop all vCPUs.
  Batch them, and release only ranges above a size threshold if needed.
* **Control-plane vCPU churn.** urpc handlers still make a thread retire its
  parked vCPU before each system call, about 40 times for a round of four
  forks and their removal. Their waits for the next request could go through
  Go's network poller instead of `poll`; each handler would then have to be
  shown to make no system call that blocks indefinitely.
* **Returning a spike on a busy Sentry.** Phase 6 trims only an idle one. A
  busy Sentry still returns its heap only as the scavenger paces it.
* **The footprint outside user regions** is the double charge and
  Hypervisor.framework's page tables (`footprint -p` reports them).
