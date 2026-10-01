# A Venus renderer for TTC

Status: in progress (2026-10-02).

## Goal

TTC's virtual GPU (`--gfx`) renders with gfxstream, a large C++ host that
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
*   **Vulkan:** the renderer loads the Vulkan loader and MoltenVK at run
    time; generated C trampolines call through per-device dispatch tables.

## Renderers per family

A family's branches share one Sentry, and one gofer; with Venus, they share
one renderer too, a connection per branch, which the broker of the Sentry's
family asks the renderer to serve. Each connection keeps its own namespace of
contexts and resources, as each branch's device numbers them. A fork's child
context shares its parent's host device, so that it can share the parent's
compiled objects.

ttcd selects the backend with `TTC_GFX_BACKEND` (`gfxstream`, the default, or
`venus`), and gives containers the matching guest driver: Mesa's
`libvulkan_virtio.so` for Venus, built by
`runsc/ttc/gfxrenderer/guest/build_mesa_venus.sh`.

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
it. Checkpoints, dehydration, and remote forks, which leave the process, lose
the GPU's contexts, as `--gfx-fork=device-lost` does.

## Android

Mesa's Venus driver, built with the NDK, serves as Android's Vulkan HAL, and
ANGLE runs on it. Android's buffers (gralloc, minigbm) must be importable
into Vulkan by resource ID. Their memory will be shared memory, as
host-visible memory is, laid out with the row pitch that MoltenVK uses for
linear images of that format, so that importing them shares rather than
copies.

## Status

*   Done: the generator and protocol (1); the host core (2): `vulkaninfo`,
    `vkoffscreen`, and OpenGL by Zink in a Linux guest; one renderer per
    family (5); fork cloning (6): forks, fan-outs, and forks of children
    render on, Vulkan and Zink alike.
*   Next: Linux parity checks (3), Android (4), kernel metadata (7), file
    pages (8).

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
