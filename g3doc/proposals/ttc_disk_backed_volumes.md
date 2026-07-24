# TTC disk-backed managed volumes

## Status

Design approved in principle; implementation has not started. The former
`local-tmpfs` managed-volume driver and its tar snapshot lifecycle have been
removed. Until this proposal is implemented, TTC rejects named volumes,
anonymous volumes, and unoverridden OCI image `VOLUME` declarations. Explicit
tmpfs mounts and read-only host binds remain supported.

Existing directories created by the removed driver are ignored. TTC does not
delete them automatically, because doing so would destroy user data. This is a
pre-release design replacement, not a migration interface.

## Goals

A TTC-managed volume must:

* keep live file data in ordinary host filesystem files, not tmpfs;
* make an acknowledged write visible in its durable backing file immediately,
  subject to normal Linux page-cache and `fsync(2)` semantics;
* fork a volume without walking or copying its directory tree;
* isolate all namespace, metadata, file-data, open-FD, and `MAP_SHARED`
  mutations between branches;
* use host reflinks for the first write to a shared regular file;
* let the host filesystem perform extent/block CoW after that reflink;
* work rootlessly and without a host OverlayFS mount;
* fail closed when reflinks are unavailable unless the daemon was explicitly
  started with a whole-file-copy fallback;
* retain gVisor's host-isolation boundary and never expose the TTC data root
  through guest-controlled host paths.

“Written directly to disk” does not mean `O_DIRECT` or an `fsync` after every
write. Writes use host page cache as normal filesystem writes do. Guest
`fsync`, `fdatasync`, and synchronous-open flags must propagate to the backing
file and the volume metadata journal so applications receive normal durability
semantics.

## Non-goals for the first implementation

* sharing one writable named volume concurrently between unrelated sandbox
  families;
* host-mounted or host-editable volume directories;
* preserving host inode numbers, xattrs, ownership, locks, or ACLs;
* disk-image files, loop devices, host OverlayFS, FUSE, or a privileged helper;
* block-level quota accuracy for shared reflink extents;
* migration between hosts with different backing filesystems.

The first version should allow one writable family attachment per named volume.
Additional attachments may be read-only. This avoids undefined fork lineage
and lost-update semantics. Multi-writer attachment can be designed later.

## Host capability policy

The runtime should use `ioctl(FICLONE)`, the operation behind
`cp --reflink=always`; it must not invoke the `cp` program.

At daemon startup, `ttcd` creates two temporary regular files inside the actual
volume object store, reflinks one from the other, modifies the clone, verifies
that the source did not change, and removes both files.

The default policy is:

* refuse daemon startup if the probe reports unsupported, cross-device, or
  semantically broken reflinks;
* record the probed filesystem identity and capability in daemon status;
* re-probe if the data root's mount identity changes.

A future daemon-only option, tentatively
`--allow-slow-volume-copy`, relaxes the requirement. In that mode the first
write to a regular file shared by branches copies the complete file. It does
not copy the file on every write: later writes use the already-private file
until another fork shares it again.

The fallback copier should preserve sparseness with `SEEK_DATA`/`SEEK_HOLE`
where available, then use `copy_file_range`, and finally buffered copying. It
must never silently fall back unless the daemon option was set.

## On-disk repository

Each named volume owns a private repository under the TTC data root:

```text
volumes/<volume-id>/
  format
  metadata.db
  journal/
  objects/
    <object-id>
  staging/
  lock
```

Guest paths never map directly to paths under `objects/`. Regular-file data is
stored in opaque, randomly named objects opened relative to an already-open
repository directory. This removes guest-controlled symlink traversal from the
host access path.

The metadata database stores immutable persistent nodes:

* **branch root**: branch ID, parent branch, root inode-table node, generation;
* **virtual inode table**: virtual inode number to immutable inode version;
* **directory node**: persistent name-to-virtual-inode map;
* **inode version**: type, mode, virtual UID/GID, timestamps, link count,
  size, generation, and regular-file object ID;
* **object record**: host object ID, logical size, state, and reachability
  metadata.

A persistent radix tree or copy-on-write B-tree gives bounded lookup cost
independent of fork depth. A chain of per-branch delta maps is not acceptable:
deeply nested forks would make lookup and recovery proportional to ancestry
depth.

Virtual inode numbers are stable and branch-observable. Directory entries refer
to virtual inode numbers, while each branch's inode table resolves that number
to its current immutable inode version. This is necessary for correct hard-link
semantics.

## Fork

While the source branch is frozen:

1. finish or abort in-progress volume metadata transactions;
2. invalidate writable host-file translations that could mutate a shared
   object without passing through the CoW gate;
3. create a child branch-root record pointing to the same persistent
   inode-table root;
4. mark the referenced generation immutable/shared;
5. commit the child root transaction;
6. clone the Sentry filesystem object with the child's branch-root ID;
7. resume the source after the rest of live fork succeeds.

This operation is O(number of mounted volumes), not O(number of files).
Reference counting must not walk the tree at fork. Object reclamation should
use root reachability, generations, and a background garbage collector.

Once a generation is shared, no branch may modify any host object reachable
from it in place. Even the first writer must clone; allowing one branch to
retain and mutate the “original” would corrupt every sibling still referencing
it.

## Namespace and metadata CoW

Directory and inode metadata nodes are immutable. A mutation path-copies only
the persistent metadata nodes from the branch root to the changed entry.

Examples:

* create, unlink, rename, and link path-copy affected directory nodes;
* chmod, chown, timestamps, and truncate create a new inode version;
* unlink does not copy file data;
* deleting the last directory entry does not reclaim an object while an open
  file description or another branch still reaches the virtual inode;
* a hard link continues to resolve to the same branch-local virtual inode after
  that inode's backing object is privatized.

The metadata transaction must lock virtual inodes in a deterministic order so
rename, exchange, hard-link, and concurrent first-write operations cannot
deadlock or split one inode twice.

## Regular-file data CoW

Every Sentry file operation resolves a tuple:

```text
(volume repository, branch root, virtual inode) -> inode version -> object
```

An open file description stores the virtual inode and branch filesystem, not a
permanently writable host FD. Reads may cache a read-only object FD. All
mutating paths call `ensurePrivateBacking` before reaching a host write:

* `write`, `pwrite`, append, and writev;
* truncate and fallocate modes that mutate extents;
* writable `MAP_SHARED` faults;
* splice/copy destinations;
* any future direct-I/O, AIO, or io_uring destination path.

`ensurePrivateBacking`:

1. locks the branch-local virtual inode;
2. re-resolves its current inode version;
3. returns immediately if the object is already private to this branch;
4. creates an opaque staging file in the same host filesystem;
5. uses `FICLONE` from the shared object, or the explicitly enabled full-file
   fallback;
6. copies/records required host file attributes controlled by TTC;
7. publishes the object under a new object ID;
8. commits a new branch-local inode version referencing it;
9. invalidates stale mappings and cached writable FDs;
10. performs the requested mutation on the new object.

Racing first writers in one branch converge on one private object. First
writers in sibling branches create different objects.

After `FICLONE`, the host filesystem decides its extent/block CoW granularity.
TTC neither knows nor assumes that it is 4 KiB. A one-byte guest write may
cause the host filesystem to allocate one or more extents according to its own
implementation.

## `mmap` and open-FD correctness

This is the highest-risk integration area.

Read-only mappings may continue to map the shared immutable object. Before a
branch can install or retain a writable `MAP_SHARED` translation, the backing
object must be private. Fork must revoke pre-existing writable translations in
both source and child. The first later write fault runs
`ensurePrivateBacking`, then returns translations from the private object.

`MAP_PRIVATE` remains memory CoW and must not modify the volume object.

Pre-fork open FDs in each branch refer to the branch filesystem plus virtual
inode, so a later rename, unlink, hard link, or backing-object split preserves
Linux open-file semantics. Sentry owns file offsets, status flags, locks, and
leases; host open-file descriptions and host locks are not shared as guest
state.

## Crash consistency and durability

The metadata store needs a checksummed write-ahead log or a transactional
embedded database. Structural consistency is mandatory even when the Sentry or
host crashes during first-write privatization.

The minimum ordering is:

1. create the staging object;
2. clone/copy and close or sync it as required;
3. atomically publish the object name;
4. commit the new inode-version mapping;
5. perform or expose the guest mutation.

Orphaned unpublished/published objects may remain after a crash and are
reclaimed by recovery. A committed inode version must never point to a missing
object.

Ordinary unsynchronized writes have normal host filesystem crash semantics.
Guest `fsync(file)` must sync the private data object and all metadata required
to resolve it after restart. Guest `fsync(directory)` must sync relevant
namespace transactions. `syncfs` must cover the selected volume branch.

Stop/start does not export or re-import anything. The main branch reopens its
existing durable branch root. A child branch remains a durable filesystem
branch even if its process state cannot be restarted; TTC may retain, inspect,
export, promote, or delete that filesystem branch independently.

## Quotas and accounting

Initial quotas should be logical per branch: sum allocated logical extents or,
for a simpler first version, regular-file logical sizes plus fixed metadata
cost. This is deterministic and does not depend on host reflink accounting.

Physical exclusive/shared byte reporting can be added when the backing
filesystem exposes reliable extent ownership. It must be labeled separately
from the guest-visible logical quota.

## Security requirements

* The Sentry never follows a guest path in the host volume repository.
* Object access is by opaque ID beneath an open repository directory.
* Creation and publication use `openat2`-style beneath/no-symlink constraints
  where paths are unavoidable.
* Staging objects are mode 0600 and never guest-addressable by host pathname.
* Reflink source and destination must be verified as regular files in the same
  repository and host mount.
* No shell command or host `cp` is executed.
* Metadata lengths, names, object IDs, refcounts, and journal records are
  bounds-checked and checksummed.
* Recovery treats missing objects, duplicate IDs, invalid generations, and
  reference cycles as corruption and fails closed.
* Slow-copy mode changes performance only; it must not weaken path isolation or
  transactional publication.

## Implementation stages

1. **Repository and probe**
   * Define format version, object layout, locking, recovery, and `FICLONE`
     helper.
   * Add the daemon startup probe and `--allow-slow-volume-copy`.
   * Add crash/fault-injection tests for object publication.

2. **Read/write filesystem without fork**
   * Implement a new Sentry VFS filesystem, tentatively `ttcvol`.
   * Cover regular files, directories, symlinks, hard links, rename, unlink,
     stat, permissions, sparse files, truncate, fsync, and restart.
   * Keep device nodes, sockets, and external host FDs unsupported initially.

3. **Persistent metadata CoW**
   * Add immutable inode-table/directory nodes and O(1) branch-root creation.
   * Implement branch lookup, deletion, reachability, and garbage collection.
   * Verify deep fork trees have bounded lookup cost.

4. **Regular-file reflink CoW**
   * Gate every mutating file path through `ensurePrivateBacking`.
   * Add hard-link, open-unlinked-file, concurrent-writer, ENOSPC, and fallback
     tests.

5. **Mappings and live fork**
   * Add mapping invalidation and writable `MAP_SHARED` fault privatization.
   * Integrate branch-root creation with the frozen live-fork transaction.
   * Test parent/child/grandchild divergence under open FDs and mappings.

6. **TTC lifecycle and CLI**
   * Restore `ttc volume create/ls/inspect/rm/import/export`.
   * Restore `--volume`, `--mount type=volume`, anonymous volumes, image
     `VOLUME`, `volume-nocopy`, inspect projection, and `rm -v`.
   * Replace `commit` semantics with an explicit branch-head promotion
     operation; ordinary main-volume writes are already persistent.

7. **Audit and performance qualification**
   * Fault-inject every metadata/object transaction boundary.
   * Test reflink and slow-copy hosts, sparse/multi-gigabyte files, deep trees,
     hard links, rename storms, mmap databases, and disk-full behavior.
   * Security-review all host file operations and confirm no writable host FD
     can bypass the inode CoW gate.

## Acceptance criteria

The managed-volume CLI must not be re-enabled until:

* a one-million-file volume forks without walking those files;
* a write to one shared file clones only that file at TTC level;
* reflink mode does not read/copy the whole file in TTC;
* slow-copy mode is impossible without explicit daemon opt-in;
* parent, child, and grandchild hard links, open FDs, truncates, and
  `MAP_SHARED` mappings remain isolated;
* guest `fsync` survives forced Sentry and daemon termination;
* recovery never exposes a partially published object;
* restart reads the on-disk branch directly, without tar export/import;
* the host data root remains inaccessible from guest-controlled paths.
