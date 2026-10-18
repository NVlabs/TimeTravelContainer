# Address-ordered private memory under systrap

Status: implemented (2026-10-18): homes and placement, the Sentry's address
reservation, folding, collapse, and restored memory at its slots. Where the
implementation departs from the design below, the section says so.

## Problem

Under systrap, the Sentry cannot edit an application's page tables. It
expresses them as host mappings: every pma is mapped into the application's
stub process with its own `mmap(MAP_SHARED|MAP_FIXED)` of the memory file
(`pkg/sentry/platform/systrap/subprocess.go:1029`), and the host merges two
neighbouring mappings only if they continue the same file at consecutive
offsets with the same protection. Each place where that breaks costs a host
mapping (VMA), and a process may have at most `vm.max_map_count` of them:
262,144 on the farm's job hosts, 65,530 by default on many distributions.

Copies are where it breaks. The allocator places every copy, and every fresh
private page, wherever it has room (`pgalloc.MemoryFile.Allocate` has no notion
of the address a page will be mapped at), so pages written in random order end
up at unrelated offsets: about one host mapping per page. On a Xeon 8270 farm
host, a fork that rewrote 1 GiB of 4 KiB pages at random held 261,428 host
mappings, just under the limit. With 1200 MB it went over: before d01d6a104
the fork died with SIGSEGV; since, it survives but runs at 2.0-9.9 us per
access instead of 47 ns, indefinitely, as the Sentry drops and remaps the
address space.

A native `fork()` never changes the VMA list: copy-on-write swaps page-table
entries inside existing VMAs. The KVM platform behaves the same way, as the
Sentry writes the application's page tables itself
(`pkg/sentry/platform/kvm/address_space.go:145`).

The Sentry's own mappings do not scatter today: pgalloc maps the memory file in
1 GiB chunks, one `mmap` per growth (`pkg/sentry/pgalloc/pgalloc.go:1602`), and
changes them only at chunk boundaries (per-chunk `madvise` and `mbind`). The
exceptions are the `mlock` fallback of `tryPopulateMlock`, used only where
`MADV_POPULATE_WRITE` is missing, and the device proxies (nvproxy, rdmaproxy,
tpuproxy), which mirror each discontiguous piece of a pinned range.

## Goals

1. The Sentry process never scatters: its host mapping count is bounded by the
   memory file's size in GiB, whatever applications do.
2. In a stub process, private pages are address-ordered: two neighbouring
   private pages are in one host mapping, except where a public page lies
   between them.

A page is **private** when one address space holds it and nothing else does:
its only reference is that address space's pma. It is **public** when anything
else also holds it: another address space after a fork, a capture or remote
snapshot, the residency cache of a restore, a pin, or a file (tmpfs, shm,
gofer, `MAP_PRIVATE` file pages not yet written).

One more boundary is inherent: after a fork or a capture, TTC write-protects a
page until its first write, so that the write can give it a new version
(`needCOW`). Under systrap, protection is per host mapping, so pages written
since the last fork sit in different mappings from pages not yet written. Those
not-yet-written pages are treated as public (their content is shared with the
past). Making them writable without a write would mean renewing their versions
eagerly, which would make the next dehydration store them again. A write takes
over a whole pma at once (`isPMACopyOnWriteLocked`, `pkg/sentry/mm/pma.go:946`),
so these runs stay coarse.

## Concepts

- **Block**: an aligned 1 GiB range of an address space.
- **Home**: a 1 GiB chunk of the memory file reserved for one block of one
  address space. The page at address `a` of block `b` belongs at
  `home.Start + (a - b.Start)`, its **slot**. Homes are file offsets, not
  memory: the file is sparse, and a slot uses memory only once a page is
  placed in it.
- **Layer**: a second (third, ...) home for the same block, used when a page's
  slot in the first is still taken by a public page, typically the version a
  fork still shares.
- **Owner**: the address space that places pages into a home. Only the owner
  places pages, so two address spaces never claim the same slot. When the owner
  is released, the home becomes an **orphan**; another address space that holds
  pages in it may **adopt** it.
- **Fold**: moving an address space's private pages from extra layers into one
  home, once the pages that blocked their slots are gone.

Forks keep virtual addresses, so every layer of a block, whoever owns it, uses
the same rule from address to offset. That is what makes adoption and folding
work.

## pgalloc

### Home chunks

A home is a whole 1 GiB chunk, marked as a home. pgalloc's chunk list already
records `huge` and `node` per chunk (`pgalloc.go:359`). The home mark goes in a
spare high bit of `node`, as the 2 MiB mark went in `FirstSequence`'s top bit,
so the saved layout keeps its fields and images saved before still load.

A home chunk is a small chunk that `Allocate` never allocates from. Its free
slots stay void (zero-reference segments) in `unfreeSmall`, as the pages of
chunks of the other size are, so the gap search never finds them; its waste is
tracked in a set of its own (`unwasteHome`), so that only `AllocateAt` recycles
it, into its own slots. The releaser releases home waste as any other, back
into free slots rather than free pages. Every place that picks a set by
`chunkInfo.huge` stays right, since a home is small. The saved state has no set
for home waste: an export folds it into `unwasteSmall`, and a load takes the
waste of home chunks back out (a save drains all waste first).

New operations:

- `ReserveHome(owner) FileRange`: a free home chunk from a pool, or a new chunk
  at the end of the file. Nothing is allocated, committed or accounted.
- `AllocateAt(fr, opts)`: allocate exactly `fr` inside a home. It fails if any
  page of `fr` is in use, and waits for a release in progress over it. It takes
  the usual options (Kind, MemCgID, LogicalHugePages, Mode, ReaderFunc), and
  gives the pages fresh identities or, for a copy, the original's identity with
  a new version (`AdoptCOWIdentities`), as `Allocate` does today.
- `AdoptHome(chunk, owner)` and `OrphanHomes(owner)`.
- `MoveIdentities(src, dst)`: give `dst` the identities **and versions** of
  `src`, for folding. It existed until e81b6fd53 for gathering.

When a home has no pages and no owner, it goes back to the pool. The file never
shrinks, as today; the releaser punches holes in freed pages wherever they are.

### Host huge pages inside a home

A home holds 4 KiB pages and 2 MiB pages side by side, at their slots. The
Sentry must decide which the host backs with transparent huge pages without
`madvise` on parts of its own mapping, which would split it (goal 1).

- The Sentry maps home chunks with `MADV_NOHUGEPAGE` (whole chunks) where pgalloc
  uses `AdviseNoHugepage` (the huge-page tmpfs, mounted `huge=within_size`), and
  with no advice where it uses `AdviseHugepage`. 4 KiB pages are populated
  through this mapping and get 4 KiB host pages.
- A 2 MiB page is populated through a temporary mapping of its 2 MiB range with
  the advice that allows a huge page, then unmapped, as `bindFileRange` already
  does for NUMA binding (`numa_linux_unsafe.go:40`). That costs three system
  calls per 2 MiB page, against zeroing 2 MiB.
- The stub's mappings carry no advice, so on the huge-page tmpfs the stub's
  first touch of an empty 2 MiB range would get a huge page. That is right for a
  2 MiB page and wrong for a range of 4 KiB pages. So the first 4 KiB page placed
  in an empty 2 MiB range of a home is always populated by the Sentry: the host
  never puts a huge page over a range that already holds a page, so the stub's
  later touches there get 4 KiB pages. To be verified on the farm's 6.1 kernel.
- `tryPopulateMlock` is never used on homes (it splits the Sentry's mapping);
  where `MADV_POPULATE_WRITE` is missing, pages are populated by touching them.

### NUMA

Home chunks are not bound to a node, since one block's pages may belong on
several. When the first page is placed in an empty 2 MiB range of a home, the
range is bound to the placing task's node (`placementOf`) through a temporary
mapping (`bindFileRange`), so its later pages land there too: first-touch
placement per 2 MiB, as Linux places a huge page. On a host without
`--cpu-topology host` (no NUMA nodes in pgalloc) nothing is bound.

### Accounting

Unchanged. pgalloc counts a page only once it sees it committed
(`updateUsageLocked`, `pgalloc.go:2736`), and a home's free slots are not
allocated, so reservations cost nothing. Usage scans cover allocated pages only.

### The Sentry's mapping (goal 1)

Each home is a chunk, so each one can add a host mapping to the Sentry (one
`mmap` per growth). To keep that bounded, and in fact constant, the Sentry
reserves one large address range for the memory file at start (`PROT_NONE`,
`MAP_NORESERVE`) and maps every chunk into it with `MAP_FIXED` at
`base + offset`. Adjacent chunks with the same advice then merge, so the Sentry
holds one mapping per run of chunks of the same advice and node binding. The
first version applies this to systrap only: KVM maps the Sentry's address space
as guest physical memory within the CPU's physical address bits
(`pkg/sentry/platform/kvm/physical_map.go:57`), and a large reservation needs
checking there. KVM does not need homes at all (goal 2 holds there already).

Implemented as `MemoryFileOpts.ReserveMappings`, on wherever homes are: the
file reserves the largest of 64 TiB down to 64 GiB that the host has room for,
at its first growth, and maps growths, a checkpoint's load and a remote fork's
import there (`pgalloc/mapping_reservation.go`); chunks past it are mapped
apart, as before. A family whose Sentry held 19 to 32 mappings of its memory
file in the local suite (`vmacheck`) holds one.

### Limits

- At most `maxHomes` homes at once, by default the smaller of 16,384 (16 TiB of
  file offsets) and a quarter of the Sentry's own `vm.max_map_count`. Beyond it,
  new blocks place pages as today, and the Sentry logs a warning once.
- At most four layers per block; beyond that, copies are placed as today.

## mm

### Placement

Every allocation of private memory asks for its slot first:

1. Find the address space's homes for the block. If it has none, reserve one.
2. Take the slots of the faulting range in the first home where they are all
   free.
3. Otherwise reserve a new layer and use its slots.
4. Otherwise (limits reached) allocate as today.

The paths that allocate private memory (`pkg/sentry/mm`):

| Path | Today | With homes |
|---|---|---|
| Fresh anonymous memory, `pma.go:384` | Up to the 2 MiB block around the fault, 2 MiB pages when aligned | The same range at its slots; a 2 MiB page at an aligned slot |
| 4 KiB copy-on-write, `pma.go:549` | The copied range, at most to the 2 MiB boundary | At its slots, so a copy is contiguous with its private neighbours |
| 2 MiB copy-on-write, `pma.go:854` | A 2 MiB page | At its aligned slot |
| Writes to `MAP_PRIVATE` files | 4 KiB copies (path above) | At their slots |
| Copies of DMA-pinned pages for a live fork's children, `lifecycle.go:200` | Anywhere | At their slots in an orphan home that the children adopt |

`brk`, `MapStack`, `mremap` growth and `mmap` only create vmas: their pages come
through the paths above.

### Faults map what they insert

A fault that inserts pmas (a fresh allocation, a copy, a translation) maps only
what it inserted and the pages it faulted, not the whole pma they merge with.
Systrap (MapUnit 0) maps whole pmas on a fault, and with homes a fault's pages
merge into large pmas that are mapped already: mapping them again with
`MAP_FIXED` drops their host page tables, so the application faults on each of
their pages again. Measured locally before this rule, a fork rewriting 1200 MB
at random took passes of 700 ns per access long after the copies were done. The
host merges the new mapping with its neighbours. A fault on a pma that exists
but is not mapped (after a fork or a restore) still maps all of it.

### Writes take over pages held alone

A write to a page that the address space alone holds, though other pages of its
pma are still shared, takes the page over in place (new version) rather than
copy it, as Linux reuses a page that only the writer maps. Today's rule took
over a pma only whole. A parent whose fork has copied a page then writes it at
its slot, not into a layer.

### Address-space events

- **Free** (`munmap`, `MADV_DONTNEED`, shrinking `mremap`, exit): the page's
  slot becomes free when its last reference goes. A partly freed 2 MiB page
  splits into 4 KiB pages that stay at their slots, so a split never scatters.
- **`mremap` move** (`movePMAsLocked`, `pma.go:1146`, the only path that maps a
  private page at a new address): the pages keep their offsets. They stay
  contiguous with each other, so a moved range is one mapping; it is just not in
  the destination block's home. Folding may move it home later, within its
  budget; a large `realloc` is left alone.
- **`mprotect`, `mlock`**: unchanged. Splits by protection are the
  application's own, as on Linux.
- **Guest `fork`** (`MM.Fork`, `lifecycle.go:378`): unchanged. The child shares
  the parent's pages (public) and owns no homes until it allocates or copies.
  When the child writes, its copies go to its own homes, at their slots. When
  the parent writes, its slot holds the page the child still shares, so the
  copy goes to a layer.
- **Exec and exit**: the old address space's homes become orphans and return to
  the pool when empty.
- After placing or folding, the pmas of the range are merged (`MergeRange`), so
  that the address space keeps one pma per run, as the host keeps one mapping.

## Folding

A fork leaves layers behind once the fork or its parent is gone:

```
pages:            p0 p1 p2 p3 p4 p5 p6 p7
child writes p2, p5: copies at the slots of the child's home
parent removed:   parent's home (orphan): p0 p1 __ p3 p4 __ p6 p7
                  child's home:           __ __ p2 __ __ p5 __ __   -> 5 runs
fold:             copy p2, p5 into the parent's home; the child adopts it
                  and its own empty home goes back to the pool      -> 1 run
```

### What may move

A page moves only if it is private (its only reference is the address space's
pma: no other address space, capture, remote snapshot, residency cache, pin or
loader holds it), is not pinned for DMA, and has no load in flight. pgalloc's
reference count already counts all of these, so "unique" is
`hasUniqueRefLocked` plus `DMAPinned` and the loader check.

### Target

For each block, the target is the layer holding most of the address space's
private pages there, if the address space owns it or it is an orphan (which it
then adopts). Pages move into the target's free slots: a slot that held an old
version is free once that version's holders are gone, and a slot still taken
stays a residual run.

### Mechanics

Per batch of at most 2 MiB, under the address space's `activeMu`:

1. `AllocateAt` the target slots, filled from the source pages.
2. `MoveIdentities`: the pages keep their identities and versions, since their
   content does not change, so dehydration dedup is unaffected.
3. Replace the pmas with the same protection, including `needCOW`: a page still
   write-protected stays write-protected.
4. Unmap the range from the stub (or KVM's page tables) and drop cached internal
   mappings.
5. Release the old pages and merge the pmas.

Implemented as `pgalloc.MemoryFile.MovePages`, which does steps 1 and 2, and
`mm.MemoryManager.FoldStep`, with one change of order: the range is unmapped
from the stub before it is copied, not after, since the application writes its
pages through its own mappings without the Sentry's locks, and a write during
the copy would be lost.

### When

- After a branch is removed: fold checks for the address spaces of the branches
  left on the same memory file.
- After a guest process's address space is released: a fold check for the
  address space it was forked from, and for those forked from it (`MM.Fork`
  records the relationship).
- After a capture, remote snapshot or residency unit releases its references.
- When an address space's pma count passes a threshold (a cheap counter on
  insertion).
- When `mapFileLocked` gets ENOMEM: fold synchronously before the retry.

Folds run in a background goroutine per memory file, with a copy budget
(default 1 GiB/s across the Sentry). They stop while a branch is saved, live
forked or captured, and resume after.

Implemented (`kernel/fold.go`) as a goroutine per started Kernel on a file with
homes, which folds the address spaces of its tasks when asked, 50 ms after,
so that a burst of releases makes one pass, at up to 1 GiB/s. An address space
that releases its pages asks every Kernel on its memory file: that covers a
branch's removal and a guest process's exit without recording who forked whom.
A capture or remote snapshot that closes asks through the file
(`SetFoldNotify`). A residency unit's release asks nothing: what holds a unit's
pages besides the cache is what restored them. The pma-count trigger is not
implemented; the ENOMEM one asks for a fold rather than folding in the fault,
which holds `activeMu` and could wait for a fork that holds the folds back.
Folds are held back while `quiescePausedAnd` runs, which every save, live fork,
dehydration capture and remote-fork capture runs in, and not for the whole of a
pause, as a base branch may stay paused for long. An address space is folded
only if it may have something to fold: it placed a page in a layer or out of
its homes, forked or was forked, was loaded, or moved pages with `mremap`.

## Collapse

A block whose 512 pages of a 2 MiB range are all private, in one home, at their
slots, is already laid out as a 2 MiB page. Collapsing it changes metadata only
(one new identity and version, as a 2 MiB page), plus an `MADV_COLLAPSE` on a
temporary mapping of the range where the host supports it for tmpfs (6.1 and
later, to be verified), or else a copy out and back. This is what Linux's
khugepaged does, and it keeps memory that an allocator splits from staying in
4 KiB pages for good. Policy: collapse only fully present ranges; Linux by
default also refills discarded pages with zeroes, which would undo the
application's frees.

Implemented (`pgalloc.MemoryFile.CollapseHuge`, `mm/collapse.go`) with
`MADV_COLLAPSE` alone, through a temporary mapping of the range at a
2 MiB-aligned address, touched first: the host collapses only what it may map as
a huge page there, and Linux 6.1 maps the huge page into the mapping only where
that has a page table already, failing the call (EINVAL) otherwise though it
collapsed the range. A host without `MADV_COLLAPSE` (before 6.1: the advice
fails even for no range) turns collapse off; a range the host refuses is
dropped, and one without a free huge page (ENOMEM, EAGAIN, EBUSY) is asked again
on later passes, eight times at most. A range keeps its pages' identities if
they are a 2 MiB page's already (consecutive from a multiple of 512, as pages
allocated afresh at their slots are; see "Restored memory"), and takes one new
version. The fold steps collapse candidates: the 2 MiB of a page placed in a
home that fills its slots, of a write that takes pages over in place, of a
fold's block, and of a 2 MiB page freed in part in place (`MADV_DONTNEED`), once
each has waited 10 s, as khugepaged scans every 10 s, so that a short-lived
process's memory is not copied into huge pages just before it goes. A range is
collapsed only if every page is in memory (`mincore`), so that no page the
application discarded is filled; a range that waits for one to be written again,
which the Sentry does not see, is looked at again every 10 s, as khugepaged
scans, 4096 ranges at most per address space. Linux 7.0 and 6.1 (the farm's
hosts) collapse both a huge tmpfs's pages and a memfd's, whatever
`shmem_enabled` says.

## Lifecycles

| Lifecycle | What happens |
|---|---|
| Container start, `ttc run`, `ttc exec` | New address spaces; homes appear as they fault. |
| Guest `fork`/`exec`/`exit`, `vfork`, threads | As in "Address-space events". `vfork` and threads share one address space. |
| `ttc fork`, one or many children (same node) | Children share the parent's pages at the same offsets (`live_fork.go:436`); they own no homes until they write. Each child's copies go to its own homes. The parent's copies go to layers while children share. |
| Branch removed (child, parent, or one in the middle) | Its address spaces release their pages; their homes become orphans. Fold checks run for the branches left. Siblings that share pages in an orphan keep them public until each becomes private; the first to fold adopts the orphan, the others move their pages to their own homes. |
| Pause, unpause | Nothing. |
| `ttc stop`, `ttc start` | New kernel and address spaces. A start beside live peers restores through residency units (see below). |
| Remote fork, source | The capture's snapshots hold references (`remote_snapshot.go:242`), so the source's pages are public until acknowledged; its writes go to layers, and fold checks run on release. |
| Remote fork, migration target, cold or durable rehydrate, `ttc restore`, start beside peers | Pages arrive in 1 MiB or 2 MiB residency units, held by the residency cache, so they stay public until reclaimed; writes copy into homes. Units go to their slots in homes of their identities' window, which the restored address spaces adopt. See "Restored memory". |
| Local rehydrate (same memory file) | Pages come back at their old offsets (`dehydrate.go:589`), which were slots of now-orphaned homes; the restored address spaces adopt the homes whose slots match their addresses. |
| Dehydration capture | Holds a reference on every page of the memory file (`dehydrate.go:304`), so all pages are public while it lives; writes by live branches go to layers until it is released. |
| Checkpoint archive (format 2) | A dehydration of a hidden fork, then a cold restore: as above. |
| Legacy `checkpoint-branch` (`SaveTo`/`LoadFrom`) | Same offsets; home marks are saved; owners are rebuilt by adoption. |
| nvproxy, rdmaproxy, tpuproxy pins | Pinned pages hold extra references, so they never move; live-fork copies of DMA-pinned pages go to an orphan home. |
| GPU device memory | Not in the memory file (`LiveForkUnsharedFile`); unaffected. |
| Residency unit reclaim, image prune | A reclaimed unit's slots are free again; a home of units left with no page and no owner is spared. |
| Nested TTC | An inner Sentry's memory file is a shared mapping in the outer one, so public: unaffected. |
| KVM | Homes off in the first version: KVM has no per-pma host mappings. |
| macOS (HVF, platform backing) | Homes off: GuestRAM allocates frames per file offset. |

## Restored memory

Restored pages live in residency units placed by the allocator, ordered by
logical identity, so a large restored branch that only reads its memory holds a
mapping per run of units: up to one per 1 MiB unit of base pages or 2 MiB page.
Those are public-to-public boundaries (the cache shares every unit), which goal
2 allows, but they cap a read-only restored branch at roughly 256-512 GiB under
the farm's limit.

The fix, for a later phase, is to make identities follow addresses: a page
first allocated in a home takes its logical sequence from its slot (the home's
identity base plus the slot index), and a copy keeps its original's identity,
as today. A logical chunk, and so a residency unit, is then a contiguous 1 MiB
of one block, and a restore can place each unit at the restoring address
space's slots, so restored memory starts out address-ordered. Identities can
then repeat within a home's lifetime when a slot is freed and reused; versions
still never repeat, and every consumer keys pages by identity and version
(dedup, bindings, units), so this needs auditing before it is adopted.

Implemented so that identities never repeat, which the audit then needs not
cover (`pgalloc/home.go`):

- A home has up to four windows of identities, each a chunk's worth, aligned,
  taken from the file's sequence. A page allocated afresh at a slot takes the
  slot's identity in the first window in which no page took that slot afresh
  before; a page placed by a copy, a fold or a restore is given its identity by
  its caller. A slot taken afresh more often than four times takes an identity
  of no slot, as a page out of homes does. A region freed and taken again whole
  stays in one window, so that its logical chunks are whole.
- A restore installs a unit whose pages are at the positions their identities
  give (`unitSlots`) at those slots in a home of its window: one of the homes,
  without an owner, that hold units of that window, or a new one. The units of
  an address space's block, from one window, are then in address order, and
  the address space adopts their home when it folds (see "Target"); a unit
  that packs pages of several logical chunks goes anywhere, as before.
- A restore does not need the restoring address space's addresses: a slot is
  an address's offset in its block, whatever the home.

A 512 MiB buffer written at random, dehydrated and rehydrated, took 511 stub
mappings before, one per unit, and takes one. A restored branch whose pages
come from several windows of a block, as a fork's that wrote sparsely does,
alternates between their homes, unit by unit or finer.

## Remaining exceptions

- Device proxies mirror a pinned range into the Sentry one discontiguous piece
  at a time. With homes, a pinned buffer of private pages is one piece; a buffer
  that mixes private and public pages still costs a Sentry mapping per piece.
- Pages written since the last fork or capture and pages not yet written are in
  different stub mappings (see Goals), until a write takes over the rest of the
  pma.
- Past `maxHomes` or four layers, placement falls back to today's.

## Decisions

1. Write-protected pages count as public: no eager version renewal to merge
   them with written neighbours.
2. Every block gets a home (goal 2 strictly, within `maxHomes`), rather than
   only blocks holding more than 2 MiB of private memory.
3. Folding copies pages in the background, within a budget, and briefly holds
   both copies of a page it moves.
4. Collapse rebuilds only fully present 2 MiB ranges, without refilling discarded
   pages.
5. Restored memory keeps today's units in phases 1-3; identities from slots
   come last, after an audit. (Implemented in phase 4 so that identities never
   repeat; see "Restored memory".)
6. Homes on systrap only at first.

## Risks to verify

- The host refuses a huge page over a 2 MiB range that already holds a page
  (shmem large-folio insertion), on the farm's 6.1 kernel.
- `MADV_COLLAPSE` in 6.1: verified on the farm (6.1.62), for a huge tmpfs and
  a memfd, through an aligned mapping that has a page table (see "Collapse").
- Kernel memory for shared NUMA policies: one range per 2 MiB of homes, only with
  `--cpu-topology host`.
- The Sentry's address reservation alongside the Go runtime's.
- Folding cost: a page moves at most once per fork that shared it, within the
  budget, and memory briefly holds both copies of a page in flight.

## Tests and measurements

- **Unit tests**:
  - pgalloc: `AllocateAt`, the home pool, adoption, `MoveIdentities`, and saving
    and loading home marks (including images from before).
  - mm: placement, layers, `mremap`, guest fork in both orders, and folding (with
    pinned, captured and cached pages).
- **e2e**: every suite, on the plain build and on the diagnostic build with 2
  MiB pages.
- **Farm, counting host mappings in the stub and the Sentry**:
  - the 1200 MB split-page fork and its 4 GiB and 16 GiB versions;
  - fork-mutate-discard loops that discard the child, and loops that discard the
    parent;
  - a running base that writes while children live;
  - remote fork and rehydrate of a 64 GiB branch;
  - a fork-and-exec-heavy build.
- **Overhead**: fault latency, fold CPU and memory bandwidth, and file size.

The stress suite `vmacheck` counts real host mappings: a process writes a
buffer at random, and the stub's mappings over it are counted, after it reads
every page, against what goal 2 promises, through a fork that writes all,
writes while a fork shares, the fork's removal, a parent's removal with
adoption, a guest fork, a dehydration and rehydration, and (with 2 MiB pages)
frees in part and collapse; and the Sentry's mappings of its memory file,
against goal 1. Locally (Linux 7.0) and on the farm (Linux 6.1.62, hardened,
with a huge tmpfs, 1 GiB and 2 GiB buffers), every count where the pages are
written alike is one per block and the Sentry's is one; before folding, the
same steps left 65,467 to 86,067 stub mappings and 19 to 32 Sentry ones. On
the farm, a page freed in each 2 MiB splits every huge page of the buffer, and
once the pages are written again, collapse maps all of it with huge pages
again (1,048,576 and 2,097,152 kB). Its cluster counterpart, `c29`, counts a
remote fork's: a 256 MiB buffer written at random takes one stub mapping on
the target node, where it took 128 before phase 4, one per residency unit.

## Phases

1. Homes and placement (pgalloc homes, `AllocateAt`, the huge-page and NUMA
   rules, the Sentry's address reservation; mm placement with layers and
   limits). This alone keeps fresh memory and a fork's copies address-ordered,
   which covers the measured case. Implemented: a 1200 MB fork rewriting 4 KiB
   pages after a remote fork went from up to 246,000 host mappings and
   4.7-9.9 us per access to 633 mappings and 48 ns per access within 15 s; the
   Sentry's address reservation came with phase 2's commits.
2. Folding (`MoveIdentities`, the fold engine and its triggers, adoption), for
   loops that discard either side and for bases that keep writing.
   Implemented.
3. Collapse. Implemented.
4. Restored memory: identities from slots, units placed at slots. Implemented.
