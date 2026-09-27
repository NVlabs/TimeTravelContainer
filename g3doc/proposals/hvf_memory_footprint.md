# HVF memory footprint: demand-paged stage 2

*Status: in progress, September 2026; phases 1-4 of the delivery plan are
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
bytes, and Go's heap goal doubles that. Touching 1.5 GiB in the guest grew the
Sentry's runtime regions by 280 MiB, and none of it returns after the guest
frees the memory, since the mirror suppresses the runtime's releases.

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
| Go runtime regions | Imported in 2 MiB windows on first touch (phase 4); frees of mirrored windows suppressed | Unmapped before release, imported again on reuse | tracks the live heap, and falls after spikes |
| Thread stacks | 1 MiB stacks, each imported whole | Imported in windows | ~2 MiB |
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
   for the same reason. Timings match phase 2's. Stacks are still imported
   whole: the native runtime hooks run on them.
5. **Release instead of suppress** in the runtime VM hook.
6. **Compact GuestRAM frame metadata.** Not HVF-specific, but only VM
   platforms keep it.
7. **Shared `__TEXT`.**

## Open questions

* **macOS 15.** Every observation above is from macOS 26.5.2. Phases 1-4
  use no new primitive. Run `hvf_mem_probe` and `hvf_map_probe` on the oldest
  supported release before phase 5 relies on release after `hv_vm_unmap`.
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
* **The footprint outside user regions.** The footprint exceeded the sum of
  `vmmap`'s per-region dirty memory by 53 MiB at idle after phase 1, and by
  7.5 MiB after phase 2. The double charge above accounts for part of it;
  attribute the rest.
