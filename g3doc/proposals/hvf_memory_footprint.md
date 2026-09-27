# HVF memory footprint: demand-paged stage 2

*Status: proposal, September 2026. Measurements: Apple M5 Pro, macOS 26.5.2.*

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

## Where the memory goes

Idle Sentry, fresh start, `vmmap` dirty memory by region:

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

## Design

Principles, in KVM's terms:

1. **Map lazily.** Stage-2 translations exist only for Sentry memory that EL1
   touches. For anonymous memory, `hv_vm_map` of the region without a commit
   gives this directly, since the kernel populates stage 2 on demand. Where a
   commit is still wanted, do it per 16 KiB slab when a stage-2 fault first
   reaches the slab, not for the whole region up front.
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
| Go runtime regions | Whole region committed; frees suppressed | Mapped without commit; unmapped before release, mapped again on reuse | tracks the live heap: 73 to ~20 MiB idle, and falls after spikes |
| Thread stacks | 8 MiB committed per mirrored thread | Mapped without commit; 1 MiB stack size | ~2 MiB |
| Writable image | 81.7 MiB committed | Commit per slab on first stage-2 fault, which keeps file pages out of stage 2 | a few MiB |
| `__TEXT` | 40.6 MiB private copy per family | One copy per binary UUID in a shared object, mapped read+exec | ~0; 40.6 MiB once per host |
| `__DATA_CONST`, `__LINKEDIT` | 6 MiB private copies | Copy per slab on first stage-2 fault (contents are slid per process) | ~1-2 MiB |
| Monitor page-table pool | 16 MiB committed | Mapped without commit | pages in use |
| Guest RAM | demand-allocated | unchanged | unchanged |

The shared `__TEXT` copy is built once from the Sentry binary and keyed by its
`LC_UUID`. A Sentry whose UUID does not match falls back to a private copy. The
copy can live in a file under the daemon root, mapped `MAP_SHARED` read-only,
or in a POSIX shared memory object whose descriptor ttcd passes to each
Sentry. Either way its pages are ordinary memory that no host process
executes, like today's private copies.

## Delivery

Each phase stands alone and is measured before the next.

1. **Independent fixes.** Stop `FDTable.Remove` from growing the table for
   descriptors that are not open (a gVisor bug on every platform). Give
   Sentry threads 1 MiB stacks. Stop building the GCS checkpoint gofer into
   the macOS binary.
2. **Map without commit** for anonymous mirrored regions: runtime (tag 240),
   stacks, monitor pool. Gate on a qualified macOS version; keep the eager
   commit elsewhere.
3. **Release instead of suppress** in the runtime VM hook.
4. **Lazy image data:** handle stage-2 faults for the writable image and the
   read-only copies, committing or copying one slab at a time.
5. **Shared `__TEXT`.**

## Open questions

* **macOS 15.** Every observation above is from macOS 26.5.2. Run
  `hvf_mem_probe` and `hvf_map_probe` on the oldest supported release before
  phase 2 relies on lazy population there.
* **Read+exec of shared memory.** Today's private copies are anonymous pages
  mapped read+exec. The shared copy's pages are ordinary memory too, but
  mapping a shared object read+exec has not been measured, and a wrong answer
  panics the host. Qualify it with a minimal, deliberate probe case first.
* **Stage-2 faults in the ring0 bridge.** Any exit other than the expected
  calls is fatal today. Phases 2 and 3 need none, because the kernel
  populates mapped regions itself; phase 4 needs the bridge to resolve an
  IPA to its host slab and map it from the exit path.
* **Rendezvous cost of releases.** Scavenger releases now stop all vCPUs.
  Batch them, and release only ranges above a size threshold if needed.
* **The 50 MiB outside user regions.** Check with `footprint` whether stage-2
  tables or other HVF kernel structures account for it, and whether lazy
  mapping shrinks it.
