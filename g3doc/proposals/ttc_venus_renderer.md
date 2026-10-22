# A Venus renderer for TTC

Status: done (2026-10-02): the Venus renderer replaced gfxstream.

## Goal

TTC's virtual GPU (`--gfx`) rendered with gfxstream, a large C++ host that
assumes one guest per process and forks by serializing its state to files and
replaying it, recompiling every shader in the child. This replaces it with a
host that TTC owns, written in Go, for guests that use Mesa's Venus Vulkan
driver (`virtio`) instead of gfxstream's, on Linux and Android:

*   one renderer process per family, serving every branch;
*   live forks that clone a branch's GPU state in that process, sharing what
    is immutable (pipelines, shader modules, layouts, samplers) and copying
    what is not (memory, images, buffers), with no serialization;
*   kernel metadata cloned in memory, rather than encoded and decoded, for
    every live fork;
*   clean file-backed pages shared between branches, rather than dropped and
    refaulted at each fork, and dirty pages forked.

## The protocol

Venus is Mesa's Vulkan driver for virtio-gpu. It serializes Vulkan commands
as `VK_EXT_command_serialization` defines (`docs/` of venus-protocol): a
command is its type and flags, then its parameters, each little-endian and
padded to 4 bytes; handles are 64-bit object IDs that the guest assigns; a
pointer is a 64-bit count, then the values. Replies, when the guest asks for
them, carry the return value and the output parameters.

Mesa pins `venus-protocol` 1.1.3. Its generator (`vn_protocol.py`, a model of
`vk.xml` plus Venus's XMLs, and Mako templates) emits the C encoder that Mesa
uses and the C decoder that virglrenderer uses. TTC generates Go from the same
model (`runsc/ttc/gfxrenderer/venus/gen/venusgen.py`, into `venus/vn` and
`venus/vk`): decoders that fill C structs, so that they pass to Vulkan
unchanged; handle replacement, which turns object IDs into host handles
before a call; reply encoders; and the command dispatch, one C switch that
calls through per-device tables. Keeping the model keeps every serialization
rule and workaround of the reference implementation.

Because object IDs are the guest's, a forked context can keep its parent's
IDs, mapped to other host objects.

## The host

`runsc/ttc/gfxrenderer` stays the renderer process: it speaks `gfxproto` with
the Sentry's virtio-gpu device, and starts, confines, and serves renderers.
The Venus host is a backend behind the operations the device asks for:
contexts, classic and blob resources, mapping (shared memory descriptors),
command submission, transfers, and fences per ring.

*   **Contexts:** a Venus context has an object table (ID to host handle and
    state), an instance and devices created by guest commands, and rings.
*   **Transport:** the guest submits command streams with `EXECBUFFER`, and
    then mostly through rings in shared memory (`vkCreateRingMESA`), which a
    ring goroutine executes; replies go to a reply stream in shared memory.
*   **Memory:** host-visible memory is shared memory that the renderer maps,
    wraps as an `MTLBuffer`, and imports into Vulkan with
    `VK_EXT_external_memory_metal`; the Sentry maps the same descriptor into
    the guest, as it maps the renderer's memory today. (virglrenderer does
    the same on macOS.)
*   **Sync:** with multiple timelines, the guest binds each queue to a ring
    index; a fence on ring 0 signals once the commands before it are
    executed, and on another ring once its queue has finished the work
    submitted before it.
*   **Vulkan:** on macOS the renderer links MoltenVK, which it calls
    without the Vulkan loader, so that ttcd loads no library but Apple's;
    on Linux it loads the host's loader at run time. Generated C
    trampolines call through per-device dispatch tables.

## Renderers per family

A family's branches share one Sentry, and one gofer; with Venus, they share
one renderer too, a connection per branch, which the broker of the Sentry's
family asks the renderer to serve. Each connection keeps its own namespace of
contexts and resources, as each branch's device numbers them. A fork's child
context shares its parent's host device, so that it can share the parent's
compiled objects.

ttcd gives containers the guest driver, Mesa's `libvulkan_virtio.so`, built
by `runsc/ttc/gfxrenderer/guest/build_mesa_venus.sh`.

## Forks

A live fork, in-process, asks the family's renderer to clone each GPU context
of the source branch into the child (`venus/clone_unsafe.go`): the child's
contexts share the parent's VkInstance and VkDevices, and its immutable
objects (shader modules, pipelines, layouts, samplers, render passes), with
references. They create their mutable objects anew, with the parent's IDs, by
executing again the commands that created them, which contexts journal
(`venus/journal_unsafe.go`): memory and images, whose contents the GPU or a
memcpy copies, buffers, views, pools, descriptor sets, whose descriptors they
write, command buffers, which they record again, and fences, semaphores, and
events, whose states they take. They do so in the order in which their
parents did, one sequence across contexts, so that what each step needs
exists. Rings and reply streams resume where the parent's were. Nothing is
serialized and no shader is compiled: a clone takes a few milliseconds.

The snapshot that the device's save takes is a token that refers to the
clone, which the renderer keeps until the children restore it. The Sentry
counts the devices that hold a token (the save's source until the kernels
that load the save are loaded, and each loaded device until it restores the
token, promptly), so that the last restore takes the clone and the others
clone it again, and a save that no device loads tells the renderer to forget
it. The last waits for the others' copies, which the renderer serves on
other connections. A fan-out's first child, which has yet to restore the
token when its siblings' metadata is recorded, passes the token on to them,
rather than restoring it and taking another clone while the source waits. Checkpoints, dehydration, and remote forks, which leave the process, lose
the GPU's contexts, as `--gfx-fork=device-lost` does.

## Android

Mesa's Venus driver, built with the NDK
(`runsc/ttc/gfxrenderer/guest/build_mesa_venus_android.sh`), serves as
Android's Vulkan HAL, and ANGLE runs on it, for SurfaceFlinger and apps.
Android's buffers are minigbm's, its gralloc for virtio-gpu, which allocates
them with cross-domain contexts (capability set 5,
`venus/crossdomain_unsafe.go`): the renderer answers minigbm's queries of
images' requirements with the layouts of the host's linear images, which a
device of its own asks MoltenVK for (`venus/layout_unsafe.go`), and gives
it blobs of shared memory of those layouts. The guest's Vulkan imports a
buffer as a linear image of its memory (`venus/modifier_unsafe.go`, which
emulates `VK_EXT_image_drm_format_modifier` for linear images and
`VK_EXT_queue_family_foreign`), so that what the GPU renders, the CPU reads
and another process samples, without copies. minigbm built against
ChromeOS's headers creates its blobs with ChromeOS's
`VIRTGPU_BLOB_FLAG_CREATE_GUEST_HANDLE`, which the Sentry accepts.

Venus drivers before Mesa 24.2, such as redroid 14's own, offer Android's
window system only where the renderer's semaphores import sync files and
its fences export them, which neither the renderer nor MoltenVK has: the
renderer offers `VK_KHR_external_semaphore_fd` and `VK_KHR_external_fence_fd`
where the host lacks them, and does what those drivers ask of the sync
files (`venus/syncfd_unsafe.go`): it signals and waits on semaphores by
empty submissions, and replaces a fence whose signal is pending when the
driver resets it.

## Kernel metadata

A live fork's child was the source's kernel serialized and decoded.
`state.Clone` copies it in memory instead: it records the graph as Save
would encode it, calling each struct's StateSave once and taking what it
saves when it saves it, resolving objects by their address ranges, and
copies the recording as Load would decode it, calling StateLoad and the
callbacks that it registers in the order that their dependencies require.
A fan-out's siblings copy one recording of the first child (`state.Record`,
`Recording.Copy`), at once, while the source runs on.

## File pages

A fork shares its source's memory with the child, which a write copies. File
pages are the files', which a fork used to drop from its source's mappings,
and which each branch then faulted again:

*   the pages of files of the image's layers, which the gofer caches in the
    family's memory file, stay mapped, and the child references them;
*   the pages of files of a container's TTC volume, its upper layer, map the
    objects of the volume that a fork seals, which a write copies first.
    Files that a branch maps read-only map a sealed object with a mapping
    that every branch of the family shares (`tmpfs.diskMapping`), and its
    host page cache, and the copies of its pages that HVF's staging keeps;
    the fork's source keeps its translations, and the child takes them
    (`memmap.LiveForkSharedFile`). Software installed in a container, such
    as Chromium, mapped hundreds of megabytes so.

## Status

Done, every milestone, on macOS (2026-10-02):

*   Linux guests: `vulkaninfo`, `vkoffscreen`, and OpenGL by Zink
    (`e2e_gfx.sh`); and, checked by hand, llama.cpp's Vulkan backend (6,700
    tokens/s of prompt and 279 of generation for Qwen2.5 0.5B, against
    gfxstream's 257) and Chromium's WebGL by ANGLE.
*   Android: SurfaceFlinger, apps, screen recording, and APKs
    (`e2e_redroid.sh`).
*   Forks of each, fan-outs, and forks of children render on. A fork clones
    a branch's GPU in milliseconds, no shader compiled again.
*   Kernel metadata is cloned in memory, and fan-outs' siblings copy one
    recording.
*   Clean file pages are shared between branches.
*   gfxstream is removed.

A fork of Chromium rendering WebGL pauses it for 0.32 s, and a fan-out of
llama.cpp generating on the GPU, into two children, for 0.16 s, of which
cloning the GPU's 500 MB takes 60 ms; a fork of Android pauses it for about
1.8 s, of which recording and copying the kernel's metadata take 0.64 s,
and the HVF platform's flush of the guest's execution caches about 0.4 s.

The renderer runs on macOS, and on Linux (plan item B1 of
`ttc_gpu_gfx_nvproxy.md`, 2026-10-02), where it imports guest memory into
the host's Vulkan driver with `VK_EXT_external_memory_host`, as it does on
macOS with `VK_EXT_external_memory_metal`. On an NVIDIA GPU, `e2e_gfx.sh`,
llama.cpp, Chromium's WebGL, and redroid run and fork as on macOS. NVIDIA's
driver does not render to linear images, which Android's buffers are, so
the images that guests render to in their dma-bufs are shadows: optimal
images in GPU memory that the renderer copies to and from the linear images
(see `runsc/ttc/gfxrenderer/README.md`).

## Milestones

1.  Generator and generated Go protocol; round-trip tests against Mesa's
    encoder.
2.  Host core: contexts, objects, transport, memory, sync; `vulkaninfo` and
    `vkoffscreen` in a Linux guest with Venus.
3.  Linux parity: Zink, llama.cpp, Chromium; `e2e_gfx` on Venus.
4.  Android parity: Venus HAL, gralloc buffers, ANGLE, SurfaceFlinger, apps;
    `e2e_redroid` on Venus.
5.  One renderer per family.
6.  Fork cloning in the renderer, sharing compiled objects.
7.  Kernel metadata cloned in memory.
8.  Clean file pages shared between branches.
9.  gfxstream removed.
