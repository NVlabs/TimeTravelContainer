# HVF host-mapping coherence probe

`hvf_map_probe` records what an Apple Silicon Hypervisor.framework stage-2
mapping observes after mutations to the host VM region originally passed to
`hv_vm_map`.

These are qualification observations, not API conformance tests. Apple's
public contract says that `hv_vm_map` maps a current-process VM region, but it
does not specify what happens when that region is later reclaimed, unmapped,
or replaced. Do not promote a successful observation into a TTC invariant.

Build, sign, and run each case in a separate process:

```shell
bazel build //tools/hvf_map_probe
cp bazel-bin/tools/hvf_map_probe/hvf_map_probe /tmp/hvf_map_probe
codesign --force --sign - --entitlements runsc/hvf.entitlements /tmp/hvf_map_probe
for c in baseline dontneed reusable reusable-guest map-fixed unmap-remap explicit-remap; do
  /tmp/hvf_map_probe "$c"
done
```

The two replacement cases deliberately accept any guest value or stage-2
fault. `explicit-remap` is the control and requires the guest to observe the
new host mapping.

`reusable` applies Darwin's reusable advice before the first guest access;
`reusable-guest` applies it after a guest stage-2 access. The page-disposition
records show whether XNU actually marked the page reusable; a successful
`madvise` return alone does not provide that information.

`reusable-pressure` and `reusable-pressure-host` are intentionally omitted from
the loop. The former guest-dirties a 64 MiB target and leaves it mapped by HVF;
the latter is a host-only control with no HVF data mapping. Both mark the target
reusable and commit 2 GiB of temporary host memory. Run them explicitly on an
otherwise idle qualification machine. They report reclamation but never access
a target page after applying pressure.

## Memory primitives: `hvf_mem_probe`

`hvf_mem_probe` characterizes what a demand-paged memory design relies on:
lazy stage-2 population of untouched memory (`fresh*`), mappings of ordinary
data files (`data-file-*`), stage-2 faults and their cost (`demand`), which
release operations lower the footprint with and without `hv_vm_unmap`
(`release-*`), Mach memory entries (`memory-entry`), and map costs
(`map-cost`). Build, sign, and run it the same way:

```shell
bazel build //tools/hvf_map_probe:hvf_mem_probe
cp bazel-bin/tools/hvf_map_probe/hvf_mem_probe /tmp/hvf_mem_probe
codesign --force --sign - --entitlements runsc/hvf.entitlements /tmp/hvf_mem_probe
for c in fresh fresh-host-read fresh-map-host-read data-file-private-ro \
    data-file-shared-ro data-file-private-rw data-file-shared-rw demand \
    release-reusable release-dontneed release-free release-reusable-unmapped \
    release-dontneed-unmapped release-dealloc-unmapped memory-entry map-cost; do
  /tmp/hvf_mem_probe "$c"
done
```

**Never map a page that the host executes into stage 2**, for example an
executable's `__TEXT`, even through a second plain `mmap` of the file. Apple's
Secure Page Table Monitor rejects such a mapping by panicking the host
(`[SPTM] VIOLATION_ILLEGAL_MAPPING_TYPE`, observed on macOS 26.5.2);
`hv_vm_map` of dyld's own `__TEXT` address merely returns `HV_ERROR`.
