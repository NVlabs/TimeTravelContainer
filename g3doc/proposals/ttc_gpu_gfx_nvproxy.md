# TTC GPUs: a virtual GPU (`--gfx`) and NVIDIA passthrough (`--nvproxy`)

*Status: design, 2026-09-30. Phase A, the virtual GPU, is implemented first on
macOS (HVF, Apple Silicon). Phase B, the rest, is implemented afterwards on
Linux (x86-64 and arm64, with NVIDIA GPUs) from this document, which is meant
to stand on its own.*

## Summary

TTC families gain two independent ways to use a GPU, each switched on and
configured on its own:

*   `--gfx` gives the family a virtual GPU. The Sentry implements the interface
    of Linux's virtio-gpu driver, so guests use Mesa's unmodified drivers, and
    a host process per branch renders what they submit with
    [gfxstream](https://github.com/google/gfxstream): on MoltenVK and Metal on
    macOS, and on the host's Vulkan driver on Linux. It works on every TTC
    platform and supports every fork semantics.
*   `--nvproxy[=CAPS]` exposes the host's NVIDIA GPUs through gVisor's existing
    nvproxy: CUDA, Vulkan, and NVENC/NVDEC at native speed, on Linux only.

A GPU's state cannot always follow a fork, so the fork semantics of each
feature is a family choice, made at creation:

| Feature | No fork option | `device-lost` | `snapshot` |
|---|---|---|---|
| `--gfx` | needs `--waive-live-fork` | the fork's GPU is new, and the contexts it inherited are lost | the fork gets a copy of the GPU's state |
| `--nvproxy` | needs `--waive-live-fork` | as for `--gfx`, for CUDA, Vulkan, and NVENC/NVDEC | CUDA only, through `cuda-checkpoint`; Vulkan and NVENC/NVDEC are not allowed |

The eventual demonstration is [redroid](https://github.com/remote-android),
Android in a container, with accelerated graphics.

## Decisions

1.  Two features, and no ambiguous `--gpu`: `--gfx` and `--nvproxy[=CAPS]`. A
    family may use either or both.
2.  Fork semantics are per feature: `--gfx-fork=device-lost|snapshot` and
    `--nvproxy-fork=device-lost|snapshot`. A family that enables a feature
    without its fork option must be created with `--waive-live-fork`, as a
    family with `--network=host` must today. A family whose enabled features
    all have fork options is live-fork eligible. A fork option together with
    `--waive-live-fork` is an error.
3.  `--nvproxy-fork=snapshot` allows only the `compute` and `utility` driver
    capabilities. Asking for `graphics` or `video` with it is a creation error,
    not a silent downgrade.
4.  The virtual GPU uses gfxstream. It snapshots GPU state at the API level
    (see below), Android's virtual devices already speak it, Mesa ships its
    guest Vulkan driver, and it runs on macOS and Linux. The host renders with
    Vulkan only: Linux guests get OpenGL through Zink, Mesa's OpenGL on Vulkan,
    and Android gets GLES through ANGLE on Vulkan.
5.  redroid rather than Waydroid: redroid runs Android's root filesystem
    directly as the container, where Waydroid nests LXC.
6.  A fork that a GPU cannot follow as its family chose fails, and its source
    keeps running unharmed. Nothing on the fork path may panic.

## Background

### What a Linux guest expects from a GPU

Applications call OpenGL, EGL, Vulkan, or CUDA. Vendor-neutral loaders, the
Vulkan loader and glvnd, pick a user-mode driver: NVIDIA's own libraries, or
Mesa's, which include the virtual GPUs' drivers (virgl, venus, gfxstream).
Mesa's drivers reach the kernel through DRM:

*   Device nodes: `/dev/dri/renderD128` renders, and any process may open it;
    `/dev/dri/card0` also drives displays (KMS), for a compositor. Mesa finds
    its driver from the node's name and from `/sys/class/drm` and
    `/sys/dev/char/226:*`.
*   ioctls: the DRM core's (version and capabilities, buffer handles, PRIME
    export and import, sync objects, KMS) and the driver's own.
*   `mmap` of buffers.
*   File descriptors that cross processes: a dma-buf is a buffer, as a client
    hands one to its compositor; a sync_file is a pollable fence that signals
    when the GPU finishes.

A display server, Wayland or X, either drives KMS or runs headless or nested.

### The virtio-gpu interface

Linux's virtio-gpu driver has a small interface of its own
(`include/uapi/drm/virtgpu_drm.h`), eleven ioctls:

| ioctl | Use |
|---|---|
| `GETPARAM`, `GET_CAPS` | features (`RESOURCE_BLOB`, `HOST_VISIBLE`, `CONTEXT_INIT`, supported capsets, blob alignment) and a capset's capabilities |
| `CONTEXT_INIT` | create the file's context of a capset type, with rings |
| `RESOURCE_CREATE`, `RESOURCE_CREATE_BLOB`, `RESOURCE_INFO` | buffers: guest-backed, or blobs in guest or host memory (`BLOB_MEM_GUEST`, `BLOB_MEM_HOST3D`), mappable and shareable |
| `MAP` | an offset to `mmap` a resource at |
| `EXECBUFFER` | submit a command buffer, with fence fds in and out, a ring, and sync objects |
| `TRANSFER_TO_HOST`, `TRANSFER_FROM_HOST` | copy between a guest-backed resource and the host's copy |
| `WAIT` | wait for a resource to be idle |

The command buffers are opaque to the kernel: the host's renderer decodes them.
Their protocol is chosen by capset: virgl (1, 2), gfxstream Vulkan (3), venus
(4), cross-domain Wayland (5), and native contexts (6). In a VM, the guest's
virtio-gpu driver and the VMM's device model split this work. In TTC the
Sentry is both: it implements the ioctls directly, forwards command buffers to
a renderer, and never needs to understand OpenGL or Vulkan.

### gfxstream

gfxstream is the renderer behind the Android Emulator, Cuttlefish, and crosvm's
Android guests. Its host side is a C API, `stream_renderer_*` in
`host/include/gfxstream/virtio-gpu-gfxstream-renderer.h`:

*   Setup and teardown: `init`, with renderer flags such as
    `STREAM_RENDERER_FLAGS_USE_VK_BIT`, and `teardown`.
*   Contexts: `context_create`, `context_destroy`, `ctx_attach_resource`, and
    `ctx_detach_resource`.
*   Resources: `resource_create`, `resource_attach_iov`, `transfer_read_iov`,
    and `transfer_write_iov`.
*   Blobs: `create_blob`, `export_blob`, `resource_map`, and
    `resource_map_info`.
*   Submission and completion: `submit_cmd`, and `create_fence` with a fence
    callback.

`virtio-gpu-gfxstream-renderer-unstable.h` adds `suspend`, `snapshot(dir)`,
`restore(dir)`, `resume`, and `import_resource`. The renderer is global to its
process: one instance per process.

Its snapshots are taken at the API level, which is what makes them independent
of the GPU's vendor:

*   The host decoder owns every name the guest sees. Vulkan handles are
    gfxstream's "boxed" handles, mapped to whatever the host driver returned.
    GLES names live in the translator's own name tables. Restoring creates new
    host objects, but the guest's handles stay valid.
*   Vulkan objects are rebuilt by replay. `VkReconstruction` keeps, for each
    live object, the raw guest command packets that created it, and a
    dependency graph: destroying a parent prunes its descendants, and objects
    created implicitly, such as `vkCreateDevice`'s queues, are tracked
    separately. Restoring feeds the packets back through the ordinary decoder,
    so the host driver rebuilds its internal state itself.
*   Contents are copied in portable layouts. Host-visible mapped memory is
    copied as is. Images go through a staging buffer with
    `vkCmdCopyImageToBuffer`, per mip level and array layer, tightly packed,
    rather than as raw tiled memory. Device-local buffers go through
    `vkCmdCopyBuffer`. Image layouts, which replay does not recover, are
    recorded and applied separately. GLES textures are read back with
    `glGetTexImage`, or `glReadPixels` through a framebuffer, and restored
    lazily by a background loader.

Its limits:

*   The guest sees the real device's limits, features, formats, and memory
    types, which differ between GPUs, and nothing checks them on restore.
*   Acceleration structures are not implemented, multisampled image contents
    are not saved, and some formats and GL texture targets are not handled.
*   Vulkan snapshots are gated by a `VulkanSnapshots` feature flag and are much
    newer than GLES snapshots.
*   Submitted work must be drained first.
*   The Android Emulator treats a snapshot as valid only for the same emulator
    version, system image, and configuration.

### NVIDIA and nvproxy

NVIDIA's drivers do not use DRM for rendering:

*   Their user-mode libraries call NVIDIA's Resource Manager API on
    `/dev/nvidiactl` and `/dev/nvidiaN`, and `/dev/nvidia-uvm` for unified
    memory.
*   Work submission is user-mode: the driver maps a command channel and a
    doorbell into the process, which writes commands and rings the doorbell
    itself. The kernel never sees the commands.
*   `nvidia-drm` and `nvidia-modeset` serve only displays and buffer sharing
    with compositors.

gVisor's nvproxy (`pkg/sentry/devices/nvproxy`) proxies the Resource Manager
and UVM ioctls and maps GPU memory and doorbells into the application:

*   It exposes only `/dev/nvidiactl`, `/dev/nvidia-uvm`, and `/dev/nvidia#`: no
    KMS, and no dma-buf sharing with a compositor.
*   Driver capabilities (`compute`, `utility`, `graphics`, `video`) are enforced
    for every ioctl, control command, allocation class, and UVM call
    (`handlers.go`). Only `compute` and `utility` are allowed by default.
*   Checkpoints are CUDA-only. `runsc checkpoint --cuda-checkpoint-path` runs
    `cuda-checkpoint --toggle` on every CUDA process first
    (`pkg/sentry/control/state_cuda.go`). NVIDIA's tool locks the CUDA APIs,
    drains work, copies device memory into host allocations inside the
    process, and releases the GPU.
*   On restore, nvproxy re-creates the remaining driver objects and remaps
    GPUs (`save_restore.go`, `runsc/boot/nvproxy.go`). Remapping requires GPUs
    of one model. Saving panics if an object that cannot be re-created
    remains.
*   `cuda-checkpoint` needs driver R550 or later, and supports neither UVM
    memory nor IPC memory exported with `cuMemExportToShareableHandle`.
    gVisor's documentation says arm64 is unsupported, while
    `cuda-checkpoint`'s README now lists aarch64.

### How a TTC fork treats devices

*   Families are live-fork eligible unless created with `--waive-live-fork`
    (`validateLiveForkCreationPolicy` in `runsc/ttc/cmd/cmd.go`). The family
    records `LiveForkEligible` and `LiveForkWaiver`, and distributed forks
    check them.
*   A local fork pauses its source and pipes its kernel's objects through
    `state.Save` straight into each child's `state.Load`, while the children
    share its memory copy-on-write (`Kernel.LiveForkMany` and
    `streamLiveForkState` in `pkg/sentry/kernel/live_fork.go`). So a device
    object's save and load hooks run on every fork. Dehydration
    (`CaptureDehydration` and `LoadDehydration` in `dehydrate.go`) and remote
    forks run the same hooks against a durable image.
*   nvproxy already hooks there: `beforeSave` checks objects, and `afterLoad`
    re-creates them, through a context value (`nvproxy.CtxDeviceRemapping`).

## Architecture

### Components

1.  **The Sentry's virtual GPU**, a new device package, for example
    `pkg/sentry/devices/virtgpu`, independent of the platform:
    *   `/dev/dri/renderD128` first; `/dev/dri/card0` with KMS only if a
        display needs it (neither redroid nor headless Linux does).
    *   The DRM core ioctls that Mesa's gfxstream driver and the loaders use,
        and the eleven virtio-gpu ioctls. The A0 spike records the exact set.
    *   File types: dma-buf (PRIME export and import, `mmap`, passing between
        processes), sync_file (fence fds, `poll`), and DRM sync objects if the
        guest driver needs them.
    *   sysfs: `/sys/class/drm/renderD128` and `/sys/dev/char/226:128`, with a
        parent device that presents a virtio GPU, built the way
        `pkg/sentry/fsimpl/sys/pci.go` and `rdma.go` build PCI trees.
    *   Every ID the renderer sees, context IDs and resource handles, is
        assigned by the Sentry, so it survives a fork unchanged.
2.  **The renderer**, one host process per branch that uses the GPU, for
    example `ttcd gfx-renderer`:
    *   It links gfxstream's host side. On macOS, gfxstream runs on MoltenVK;
        on Linux, on the host's Vulkan driver.
    *   It is per branch because gfxstream's renderer is global to its process,
        while a family's branches share one Sentry process.
    *   ttcd launches it, lazily on the branch's first context, and passes the
        connection to the Sentry, as it donates other descriptors today. The
        Sentry, which is confined, never spawns processes.
    *   Its protocol mirrors the `stream_renderer_*` calls over a socket for
        control and fence completions. Command buffers and blob memory are
        shared memory.
    *   It is confined:
        *   macOS: Seatbelt, allowing the GPU's IOKit user clients, POSIX
            shared memory of its own names, and reading system and package
            files, and nothing more (see the A2 status).
        *   Linux: seccomp and namespaces, with the render node only.
    *   Its memory counts toward the family's memory target.
3.  **The daemon and CLI**:
    *   the flags and their validation;
    *   the family record: each feature's fork semantics, and the renderer
        profile (renderer, driver, and device) for placement;
    *   renderer lifecycles;
    *   end-to-end tests.
4.  **nvproxy in TTC** (Linux):
    *   `--nvproxy[=CAPS]` configures the Sentry's nvproxy.
    *   The container gets NVIDIA's device nodes and driver libraries.
    *   nvproxy implements the fork semantics.

### Memory

*   **Guest-backed resources** live in guest memory, which a fork copies on
    write. Transfers copy them to the renderer through shared memory.
*   **Host-visible blobs**, which Vulkan requires, are the renderer's memory
    mapped into the guest:
    *   **macOS.** vmplatform maps only GuestRAM frames into guests, and
        copies ("stages") any other file's pages
        (`pkg/sentry/platform/vmplatform/platform.go`, `MapFile`). Blobs need
        a new kind of mapping:
        *   stage 2 maps the renderer's shared pages at an IPA range of their
            own, and stage 1 maps them into the application;
        *   libkrun (krunkit) maps MoltenVK's memory into its guests the same
            way, with `hv_vm_map`;
        *   the pages must first be mapped in the Sentry's process. With
            gfxstream's `ExternalBlob` and `SystemBlob` features, every blob
            is POSIX shared memory, which the renderer exports as a file
            descriptor and the Sentry maps (A0);
        *   they are never executable: mapping host-executable pages in stage 2
            panics macOS;
        *   they are aligned to 16 KiB pages.
    *   **Linux.** gfxstream exports blobs as file descriptors, which the Sentry
        maps as nvproxy maps device memory: a host-FD-backed `memmap.File`.
    *   **Memory the Sentry allocates.** The protocol can back host-visible
        allocations with guest memory instead (`GETPARAM` 10,
        `CREATE_GUEST_HANDLE`), and MoltenVK can import such memory by host
        pointer, but gfxstream implements the host side only on Linux and
        Android, through dma-buf. It would give the family's memory accounting
        the GPU's memory, not copy-on-write forks: the GPU's writes bypass the
        CPU's page tables, so memory it writes is copied at fork either way.

### Fork semantics

Each GPU device participates in a fork in four steps, with the semantics
carried in the capture and load context as nvproxy's device remapping is:

1.  **Check.** Before capture, refuse the fork with an error if the device
    cannot follow it as the family chose. A panic in a save hook would take
    down every branch in the Sentry, so `beforeSave` must never be where a fork
    fails.
2.  **Capture.** Always save the Sentry-side state. For `snapshot`, also save
    the renderer's state.
3.  **Load**, in the child: reconnect to a renderer, or mark contexts lost.
4.  **Resume** the source, undoing whatever quiesced it.

**gfx, `device-lost`:**

*   The child's device loads with every inherited context lost. Its submissions
    fail, its waits return, and its fences signal with an error.
*   Its mappings of the source's blob memory, which belongs to the source's
    renderer, raise `SIGBUS`. The guest driver waits for the renderer's
    replies by spinning on its command ring, and treats a reply that cannot
    come as fatal, so a process that was using the GPU cannot recover in the
    child. `SIGBUS` ends it at its next use of the GPU, where it would
    otherwise spin forever.
*   The child's next context gets a new renderer.

**gfx, `snapshot`:**

*   With the source paused, stop its submissions and wait for its fences.
    Then the source's renderer takes a snapshot, `stream_renderer_snapshot`,
    with its rings' consumers paused, and sends it to the Sentry, whose saved
    state carries it.
*   The child's renderer runs `stream_renderer_restore`, lazily on the
    child's first GPU use. The child's blob mappings are re-established, at the
    same guest addresses, against its own renderer.
*   The cost grows with the GPU memory in use, since device memory has no copy
    on write. Neither does memory the GPU writes that the CPU also maps: the
    GPU's writes bypass the CPU's page tables, so such memory is copied at the
    fork, never shared copy-on-write.
*   Dehydration, remote forks, and migrations carry the snapshot with the
    image, and restore only where the renderer profile matches.
*   A snapshot family's device profile hides what gfxstream cannot capture:
    acceleration structures, and buffer device address capture and replay.
    Multisampled image contents are not preserved.

**nvproxy, `device-lost`:**

*   The child's inherited NVIDIA descriptors fail every ioctl with a GPU-lost
    status.
*   Its mappings of GPU memory and doorbells are replaced with faulting
    mappings. This is required, not a courtesy: otherwise the child shares the
    source's live channels.
*   NVIDIA's libraries then report `VK_ERROR_DEVICE_LOST`, or a CUDA error that
    is permanent for the process, so a CUDA program restarts.

**nvproxy, `snapshot`:**

*   Before capture, toggle the family's CUDA processes off with
    `cuda-checkpoint`, as `state_cuda.go` does. Their GPU state is then in
    their own memory, which the fork copies on write.
*   After capture, toggle the source's processes back on.
*   In the child, nvproxy's restore path re-creates the driver objects, and the
    child's processes are toggled back on.
*   If `cuda-checkpoint` fails, or an object that cannot be re-created remains,
    the fork fails and the source is toggled back on.
*   Remote forks need GPUs of the same model.

### Coexistence

A container may have both `/dev/nvidia*` and the virtual GPU's render node:

*   The Vulkan loader lists two devices, and glvnd chooses per EGL device.
    CUDA sees only NVIDIA.
*   Buffers cannot move between the two without a copy.
*   On a Linux host with an NVIDIA GPU, both may end at the same GPU: nvproxy
    directly, and the virtual GPU through its renderer.

### Security

*   The Sentry validates every ioctl argument.
*   Command buffers are opaque to the Sentry, and only the confined renderer
    parses them.
*   Each family is limited in contexts, resources, and blob memory.

## Plan

### Phase A: macOS, this host

**A0, spike (days).** Answer before building:

*   Does gfxstream's host renderer build on macOS against MoltenVK, and run
    headless through `stream_renderer_*`? Does a `snapshot` and `restore`
    round trip work?
*   Which ioctls, sysfs reads, and `mmap`s do `vulkaninfo` and an offscreen
    render make with Mesa's gfxstream driver? Use a guest image whose Mesa is
    built with `-Dvulkan-drivers=gfxstream` and has vulkan-tools. Distributions
    may not build that driver.
*   How does blob memory cross from the renderer to the Sentry on macOS?
    Candidates: host-pointer import into memory the Sentry allocates, IOSurface,
    or Mach memory entries.
*   What does Mesa's gfxstream driver do when its host context is lost?

**A0 findings (2026-09-30)**, with gfxstream at 62c7716, Mesa at 49162a0, and
MoltenVK 1.4.2 on an Apple M5 Pro:

*   **The renderer runs on MoltenVK.**
    *   gfxstream's host side builds on macOS with meson
        (`-Dgfxstream-build=host`). The Vulkan-only configuration
        (`-Ddecoders=vulkan`) does not compile: `frame_buffer.h` includes a
        `gles_compat.h` that does not exist. The default decoders build, and
        initializing with only `STREAM_RENDERER_FLAGS_USE_VK_BIT` runs Vulkan
        only.
    *   It needs `ANDROID_EMU_VK_LOADER_PATH` to find the Vulkan loader: it
        looks only beside the program, then for `libvulkan.dylib`.
    *   If initialization fails, `stream_renderer_init` hangs instead of
        returning an error: `GetRenderer` waits for a frame buffer that will
        never exist. Our build must patch that.
    *   Its host compositor cannot create its pipelines under MoltenVK (a
        SPIR-V to MSL argument-buffer error). Headless rendering does not use
        it.
*   **What the renderer offers the guest:** capset 3 is 112 bytes, protocol
    1, a 12 KiB command ring and 1 MiB buffer, 16 KiB blob alignment, and
    deferred mapping.
*   **Blob memory crosses processes as POSIX shared memory.** With
    `STREAM_RENDERER_FLAGS_USE_EXTERNAL_BLOB | USE_SYSTEM_BLOB`:
    *   the command ring is `shm_open` memory, which `stream_renderer_export_blob`
        returns as a `STREAM_HANDLE_TYPE_MEM_SHM` descriptor that another
        process can map (verified);
    *   host-visible Vulkan memory is such shared memory too, imported into
        Vulkan with `VK_EXT_external_memory_host` (read, not yet run; gfxstream
        warns that `SystemBlob` "has only been tested on Windows");
    *   MoltenVK imports shared memory by host pointer at 16 KiB alignment, and
        a second mapping of it, as the Sentry's would be, sees the GPU's writes
        and the GPU sees its writes (verified with a probe).
*   **Snapshots need a snapshot-enabled build.** `stream_renderer_snapshot` and
    `stream_renderer_restore` return `-EINVAL` unless
    `GFXSTREAM_BUILD_WITH_SNAPSHOT_FRONTEND_SUPPORT` is defined, which only
    the Bazel and Android builds do. It needs protobuf for five host `.proto`
    files. A round trip means something only with guest traffic, so it waits
    for A2.
*   **No distribution packages the guest driver.** Debian sid, Ubuntu 26.04,
    Fedora, and Alpine edge on arm64 ship venus and lavapipe, not Mesa's
    gfxstream driver. So `--gfx` should provide it: build Mesa with
    `-Dvulkan-drivers=gfxstream`, and inject the driver into the container, as
    NVIDIA's container toolkit injects NVIDIA's libraries; one build per libc
    and architecture. Mesa's driver builds in a Debian sid arm64 TTC container
    (`libvulkan_gfxstream.so` and `gfxstream_vk_icd.aarch64.json`). Without a
    device, `vulkaninfo` with it fails cleanly after it finds no `/dev/dri`.
*   **What the guest driver needs** (from Mesa's source; A2 confirms with
    traces):
    *   discovery: libdrm's `drmGetDevices2` over `/dev/dri` and sysfs finds a
        PCI device `1af4:1050`, or any platform device, with a render node;
        `DRM_IOCTL_VERSION` must say `virtio_gpu`, major version 0;
    *   `GETPARAM` 1 to 8, and ChromeOS's 9 (fence passing) and 10 (create
        guest handle). Upstream Linux defines 9 as blob alignment, so the
        Sentry answers 9 and 10 as unknown (`EINVAL`);
    *   `GET_CAPS` for capset 3, and `CONTEXT_INIT` with two rings, capset 3,
        and an optional debug name;
    *   `RESOURCE_CREATE`, `RESOURCE_CREATE_BLOB` (host blobs, mappable, or
        shareable and cross-device), `RESOURCE_INFO`, `MAP` and `mmap` of the
        device, `EXECBUFFER` with at most one buffer, a ring index, and a
        fence out, `WAIT`, `TRANSFER_TO_HOST`, `TRANSFER_FROM_HOST`,
        `GEM_CLOSE`, and PRIME export and import;
    *   sync_file descriptors that `poll` reports, with `SYNC_IOC_FILE_INFO`
        for debugging.
*   **The command transport is a ring in shared memory.** The guest writes
    commands into a host blob with blob ID 0, which it maps, and pings with
    `EXECBUFFER` only when the host is not consuming. It busy-waits for the
    host, falling back to `usleep` after 50 million spins. Its writes give up
    when the ring's `in_error` field is set, but its reads never do (A4 found
    that), and a failed read aborts the process. Its Vulkan driver reports
    `VK_ERROR_DEVICE_LOST` from fence waits.

**A1, the Sentry's device**, independent of the platform, with a fake renderer
for unit tests:

*   the DRM core subset and the virtio-gpu ioctls;
*   dma-buf and sync_file;
*   sysfs and the `/dev/dri` nodes;
*   save and load of all state.

*Status (2026-09-30): done.* `pkg/sentry/devices/virtgpu`:

*   The device keeps what Linux's driver keeps: per-file contexts, whose IDs
    it assigns at open, and GEM handles; device-wide resources, which
    handles, mappings, and dma-bufs hold; and fences on each context's rings
    and on a global timeline, which sync_files and each resource's implicit
    synchronization (`WAIT`, dma-buf `poll`, `EXPORT_SYNC_FILE`) wait for.
*   It follows Linux's behavior where Mesa or libdrm can see it: the
    zero-extended ioctl argument sizes of `drm_ioctl`, `EACCES` for the core
    ioctls that render nodes refuse, exact-offset whole-blob `mmap`, no
    private mappings, dma-bufs exported once per resource, `lseek(SEEK_END)`
    for a dma-buf's size.
*   Differences from Linux: a classic resource's backing is the renderer's
    memory, like a blob's; no DRM sync objects; no fence events on the file
    (`POLL_RINGS_MASK`); no cross-device resources (`CROSS_DEVICE` is 0).
*   sysfs presents a virtio-gpu PCI function `00ff:00:00.0` (1af4:1050, a
    domain unlikely to collide with mirrored host trees), its `virtio0`, and
    `drm/renderD128`, as Linux lays them out, which `drmGetDevices2` walks.
*   A load keeps the Sentry's state and loses the renderer's: contexts and
    resources of the old renderer fail with `ENODEV`, and their pending
    fences signal with it. `ValidateLiveFork` refuses forks while the GPU is
    in use unless the family's policy allows them.

**A2, the renderer:**

*   the process, its protocol, and its macOS backend;
*   ttcd launching renderers and donating their connections;
*   vmplatform's mapping of blobs.

End to end, `vulkaninfo` and an offscreen render's checksum in
`ttc run --gfx --waive-live-fork`.

*Status (2026-09-30): done on macOS.* `ttc run --gfx --waive-live-fork` gives
a Debian container a GPU whose `vulkaninfo` reports "Virtio-GPU GFXStream
(Apple M5 Pro)". `runsc/ttc/test/e2e_gfx.sh` checks that, and an offscreen
compute and render whose every pixel it checks (`test/gfx/vkoffscreen.c`),
with the daemon's host isolation on.

*   `ttc-gfx-renderer` (`runsc/ttcgfx`, `runsc/ttc/gfxrenderer`) is its own
    binary because ttcd is pure Go on Linux. It loads gfxstream with `dlopen`;
    `probe` prints the capability sets. Its README records the gfxstream build
    and TTC's patches to it, and how to build the guest's driver.
*   `pkg/gfxproto` is the protocol: framed messages on a Unix stream socket,
    descriptors with `SCM_RIGHTS`. The renderer answers only context creation
    and resource mapping, and forwards gfxstream's fences, which it signals on
    its own threads, through a pipe.
*   `rendererclient` is the Sentry's `virtgpu.Renderer`. Its `Broker` asks the
    daemon's GPU service for a renderer for each branch whose GPU is in use:
    runsc connects the sandbox to the service's socket, in the daemon's
    `run/`, and passes the connection to the Sentry at boot. The service
    probes the renderer for its capability sets once, and passes them to
    runsc (`--gfx-capsets`).
*   vmplatform maps a `memmap.HostMemoryFile` in place, in GuestRAM's external
    slabs: at IPA of their own, readable and writable, never executable, whole
    host slabs within the file's own mapping, unmapped from stage 2 before the
    Sentry unmaps the memory. It maps them through `vmm.SharedStage2Mapper`:
    mapping touches the pages, which the Sentry's continuation cannot do at
    guest EL1 (the ring0 engine mirrors only memory that the Sentry owns, or
    that stage 2 already maps), so HVF returns it to host EL0 first. The first
    end-to-end runs found that crash.
*   **Confinement.** Renderers decode untrusted command streams, so each
    confines itself with the macOS sandbox before it loads gfxstream
    (`seatbelt.RendererProfile`). It may read only system files, the package
    prefixes where the Vulkan loader and MoltenVK live, and its own and
    gfxstream's files. It gets the GPU's IOKit user clients and POSIX shared
    memory of its own names; no writes, network, or processes. Metal needs no
    Mach service beyond Apple's `system.sb`. macOS lets no process change its
    profile, and ttcd's bars the GPU, so ttcd starts a launcher,
    `ttc-gfx-renderer launch`, before it confines itself. The launcher, which
    stays unconfined so that its children can confine themselves, starts the
    renderers and probes. Findings on the way:
    *   gfxstream named its POSIX shared memory after resource IDs and
        per-process counters, and opened an existing object of the name.
        macOS's names are global to the user, so two renderers shared
        command rings and memory: guests could read each other's, and their
        rings deadlocked. TTC's gfxstream patch 0002 makes the objects
        unnamed.
    *   A family's branches share one Sentry, so a removed branch's renderer
        ran on until the family exited. The device now closes its renderer
        once nothing uses it.
    *   CoreFoundation resolves the executable's path, and the Vulkan loader
        crashes without a main bundle, so the profile admits the metadata of
        the admitted paths' directories.
*   The Linux side of A2 (Phase B) needs: the renderer built with cgo, and
    gfxstream on the host's Vulkan driver; blobs that gfxstream exports as
    shared memory or dma-bufs, which the Sentry maps through their
    descriptors, as `hostMemory.DataFD` offers; and seccomp rules for the
    Sentry's broker and renderer connections.

**A3, the CLI and family:**

*   `--gfx`, `--gfx-fork`, `--nvproxy`, and `--nvproxy-fork`, parsed and
    validated on every platform; `--nvproxy` is rejected on macOS as
    unsupported;
*   the family record and the waiver rule;
*   validation tests.

*Status (2026-09-30): done for `--gfx`.* `ttcbundle.GPUOptions` is the
family's record, which `ValidateGPUOptions` checks on creation, and
`checkGPURecord` on load. `--gfx-fork=snapshot` is refused until A5, and
`--nvproxy` until Phase B (on macOS, as needing NVIDIA GPUs).

**A4**, `--gfx-fork=device-lost`.

*Status (2026-09-30): done.* A fork's child loads the device with every
inherited context lost. Their submissions fail with `ENODEV`, their pending
fences signal with it, and the child's mappings of the source's blob memory
raise `SIGBUS`. The source keeps its renderer. A fork invalidates the source's
translations of blob memory, which it cannot save, and the source faults them
back in. `e2e_gfx.sh` forks a container while it renders:

*   in the child, the process that was rendering dies of `SIGBUS` at its next
    use of the GPU;
*   a new process in the child renders with a renderer of the child's own;
*   the source renders on, unaffected.

A first version gave the child copies of its command rings with `in_error`
set. That left the process spinning forever: gfxstream's guest driver cannot
survive the loss of its renderer. Its reads spin until data arrives, whatever
`in_error` says, and a failed read aborts the process.

**A5**, `--gfx-fork=snapshot`: local forks first, then dehydration and remote
forks with renderer-profile checks.

*Status (2026-09-30): done for local forks on macOS.* `e2e_gfx.sh` forks a
snapshot family's container while it renders. In the child, the process
that was rendering continues its loop, 300 checked frames, on a renderer
restored from the source's snapshot, and so does the source on its own. The
fork of that 5.5 MB snapshot took 0.2 s.

*   **Device.** A save prepares at `vfs.PrepareSave`, through
    `vfs.DeviceSaveRestoreExtension`: it waits for the renderer's fences, for
    up to 10 s, then takes the snapshot. A failure there fails the fork
    cleanly, not in `beforeSave`. The snapshot is part of the device's saved
    state. The loaded device keeps the snapshot's contexts and resources. Its
    first use of a renderer, an ioctl or a fault on GPU memory, starts one
    that restores the snapshot. A failed restore loses them, as
    `device-lost` does.
*   **Renderer.** `--snapshots` starts gfxstream with `VulkanSnapshots`, and
    gives it a scratch directory. A snapshot archives gfxstream's snapshot
    files, the renderer's resources, and the backings of classic resources,
    which gfxstream does not save. Snapshots travel through pipes: the Sentry
    cannot create POSIX shared memory under ttcd's sandbox, and mapping
    memory on a task goroutine is the EL1 hazard.
*   **gfxstream patches:** the meson build gains snapshot support (0003); a
    snapshot pauses the rings' consumers for all of its duration, not only
    while the ASG device saves (0004); and a snapshot that cannot open its
    files fails, where it crashed the source's renderer (0005).
*   **Limits:**
    *   gfxstream rebuilds command buffers, but not the commands recorded in
        them, so guests must record them again after a fork. Android's
        renderers do so each frame; `vkoffscreen` has a `rerecord` mode.
    *   Batched descriptor set updates are off in snapshot families: their
        replay fails to restore descriptor sets. The daemon probes such
        renderers' capability sets separately.
    *   Checkpoints of a family with the virtual GPU remain host-bound, since
        nothing checks another host's renderer profile.

**A6 (optional)**, Zink for OpenGL in Linux guests, and a way to see the output:
a VNC server in the guest, or a scanout sink.

*Finding (2026-09-30):* Zink refuses the device, since it requires the
`nullDescriptor` feature of `VK_EXT_robustness2`, and OpenGL falls back to
llvmpipe. Mesa's gfxstream guest driver offers guests only the device
extensions of its allowlist (`ResourceTracker.cpp`), which lacks
`VK_EXT_robustness2`.

*Status (2026-10-01):* real workloads run on the virtual GPU, and live forks
of them (`--gfx-fork=snapshot`) resume on the child's restored renderer:

*   **Guest driver:** ttcd injects TTC's build of Mesa's gfxstream driver
    into `--gfx` containers (mounts and `VK_LOADER_DRIVERS_DISABLE`, which
    hides the gfxstream driver that Debian's Mesa now packages; see
    `runsc/ttc/gfxrenderer/README.md`). The guest's patch allows
    robustness2.
*   **OpenGL:** Zink renders OpenGL 2.1 and OpenGL ES 2.0 on MoltenVK
    (gfxstream patch `0006` reports `nullDescriptor`); every pixel checks, and
    a render loop survives a fork. MoltenVK lacks transform feedback and
    geometry shaders, which OpenGL 3 needs, so on macOS Zink is opt-in
    (`MESA_LOADER_DRIVER_OVERRIDE=zink`): forcing it broke applications that
    llvmpipe serves with OpenGL 4.6. Elsewhere a driconf file selects it.
*   **Chromium:** ANGLE on Vulkan (`--use-angle=vulkan`) renders WebGL on the
    GPU once the device offers `VK_EXT_provoking_vertex` (patch `0007`;
    MoltenVK lacks it and ANGLE requires it for OpenGL ES 3; flat varyings
    take the first vertex). A headless Chromium drawing and checking WebGL
    frames continues in both branches of a fork. WebGPU falls back to
    SwiftShader: Chromium matches adapters to the GPU it finds in sysfs, a
    virtio PCI device, and the guest driver cannot export non-dedicated
    memory as file descriptors. Chromium's own Vulkan compositing
    (`--enable-features=Vulkan`) hangs or crashes now and then; it is off by
    default on Linux.
*   **llama.cpp:** its Vulkan backend runs once the device offers the
    extensions Vulkan 1.1 and 1.2 promoted (patch `0008`): qwen2.5-0.5b
    Q4_K_M processes prompts at 6600 tokens/s and generates 257 tokens/s on
    an M5 Pro, against 250 tokens/s generation on 8 CPU threads. A fork in
    the middle of greedy generation gives both branches the text of an
    unforked run. The fork took 38 s: the snapshot carries the model's
    470 MB of device memory, and the source waits for it.
*   **Found in passing:** the Sentry's CPU generation with as many threads as
    host cores (18) collapses to 3 tokens/s, ggml's spinning thread pool
    starving the Sentry; with 8 threads it is fine.

### Handing off to Phase B

Phase A is done on macOS (A0-A5); A6 is optional. What Phase B inherits:

*   **Code map** for `--gfx`:
    *   the Sentry's device, `pkg/sentry/devices/virtgpu`: `device.go`,
        `render_fd.go` (the ioctls), `resource.go`, `fence.go`,
        `sync_file.go`, `dmabuf.go`, and `snapshot.go`; its sysfs entries in
        `pkg/sentry/fsimpl/sys/virtgpu.go`;
    *   the renderer's client, the broker's client, and the renderer's memory
        in the Sentry, `.../virtgpu/rendererclient`; the protocol,
        `pkg/gfxproto`;
    *   the renderer, `runsc/ttcgfx` and `runsc/ttc/gfxrenderer`: the cgo shim
        over gfxstream, the broker, the launcher, confinement, and snapshots;
        TTC's gfxstream patches in `patches/`, and the guest driver's build in
        `guest/`;
    *   the daemon and runsc: `runsc/ttc/cmd/gfx.go` (`gfxHost`,
        `gfxService`), `runsc/ttc/bundle/gpu.go` (the flags and the family
        record), `runsc/boot/gfx.go`, and `runsc/sandbox/gfx.go`;
    *   the save hook for devices, `vfs.DeviceSaveRestoreExtension`;
    *   tests: `virtgpu_test.go`, against a fake renderer; the
        `rendererclient` tests, against a real one (`TTC_GFXSTREAM_LIBRARY`);
        and `runsc/ttc/test/e2e_gfx.sh`.
*   **B1 needs, on Linux:**
    *   the renderer built with cgo: `runsc/ttcgfx`'s `pure` is `auto` on
        Linux, which builds `nocgo.go`, a renderer that fails;
    *   gfxstream built on the host's Vulkan driver, with TTC's patches.
        Patch 0002 matters only on macOS, where shared memory has names;
        gfxstream uses `memfd_create` on Linux;
    *   blob memory that the Sentry maps through its descriptor: `SystemBlob`
        memory is a memfd. The Sentry's `hostMemory` offers `DataFD`, for
        systrap; KVM maps what the Sentry maps;
    *   seccomp rules for the Sentry's broker and renderer connections:
        `sendmsg` and `recvmsg` with descriptors, `mmap` of the memfds they
        carry, and `read` of a snapshot's pipe. The client already avoids
        `pipe2` and the runtime's poller;
    *   confinement for renderers: `gfxrenderer.Confine` does nothing on
        Linux, so renderers run as ttcd's children, with its privileges.
        Linux's namespaces and seccomp filters nest, so no launcher is needed:
        renderers can confine themselves under a confined ttcd;
    *   the guest driver for x86-64 guests: `guest/build_mesa_gfxstream.sh`
        builds for the architecture of the container it runs in;
    *   `e2e_gfx.sh` on x86-64 and arm64 hosts.
*   **Lessons that carry over:**
    *   gfxstream's guest driver spins, rather than fails, when its renderer
        is gone; `device-lost` ends such processes with `SIGBUS`;
    *   a family's branches share one Sentry, so a branch's host resources
        need explicit release: the device closes renderers that nothing uses;
    *   a fork fails cleanly at `vfs.PrepareSave`, not in `beforeSave`, which
        is where a device captures state outside the sandbox;
    *   gfxstream's snapshots rebuild Vulkan objects and memory, but not
        recorded commands. They need `VulkanSnapshots` from the renderer's
        start, and batched descriptor set updates off;
    *   nvproxy can follow the same structure: a fork policy on the device,
        its checks in `ValidateLiveFork` and `ValidateLiveForkMapping`, and
        `cuda-checkpoint` in a `DeviceSaveRestoreExtension`.

### Phase B: Linux (x86-64 and arm64, NVIDIA GPUs)

**B1, `--gfx` on Linux:**

*   the renderer on the host's Vulkan driver, NVIDIA's and others' where
    available;
*   blobs exported as file descriptors;
*   mappings on systrap and KVM;
*   Phase A's end-to-end tests on x86-64 and arm64.

**B2, `--nvproxy[=CAPS]` in TTC:**

*   the Sentry's nvproxy configuration and the device nodes;
*   NVIDIA's driver libraries in the container, as `nvidia-container-toolkit`
    mounts them (decide whether TTC injects them or images carry them);
*   end-to-end tests: a CUDA sample, headless Vulkan, and NVENC.

**B3, `--nvproxy-fork=device-lost`:** the child is cut off, its old descriptors
fail, its new ones work, and the source is unaffected.

**B4, `--nvproxy-fork=snapshot`:**

*   `cuda-checkpoint` at fork, and a clean refusal on failure;
*   remote forks on GPUs of one model;
*   an end-to-end test: device memory that the source wrote before the fork
    reads back the same in the source and the child;
*   confirm arm64 support.

**B5**, families with both features, and placement of snapshot families on
compatible nodes.

**B6, redroid:**

*   binder in the Sentry, through binderfs (`binder`, `hwbinder`, `vndbinder`);
*   ashmem or memfd, and whatever else Android's init requires;
*   an image whose Vulkan driver is Mesa's gfxstream driver and whose GLES is
    ANGLE, as Cuttlefish images are;
*   display through scrcpy over adb, or VNC;
*   the device-lost experiment: SurfaceFlinger is expected to crash and
    Android's framework to restart. Measure the restart, and what survives it.

redroid's arm64 images could then run on macOS too.

*Status (2026-10-01): redroid runs on macOS, ahead of Phase B, renders on
the virtual GPU, shows on the host through scrcpy, and forks live.* redroid
14's arm64 image (`redroid/redroid:14.0.0_64only-latest`) boots with its own
init as the container's in about 40 s; SurfaceFlinger and apps render with
ANGLE on gfxstream's Vulkan driver; Android's network comes up and
validates; apps from F-Droid install and run, F-Droid itself downloading its
repository; scrcpy, on the host, shows and controls Android over adb; and a
live fork's branches both run on. `runsc/ttc/test/e2e_redroid.sh` checks
that.

*   **Sentry:** what Android's init and daemons required:
    *   binder, and binderfs for `binder`, `hwbinder`, and `vndbinder`;
        memfd serves as ashmem (`androidboot.use_memfd=true`);
    *   a kernel log (`/dev/kmsg`, `syslog(2)`), `reboot(2)` as in a PID
        namespace, securebits, `security.*` extended attributes, and cgroup2
        always;
    *   `bpf(2)` maps, the bpf filesystem, and cgroup attachments, for
        bpfloader and netd. The Sentry runs no BPF programs, so Android's
        traffic accounting and firewall see nothing;
    *   netd's netlink sockets and options, raw sockets for containers that
        add `CAP_NET_RAW`, alarm clocks for AlarmManager, `/proc/pressure`
        for lmkd, and real-time scheduling policies, recorded without effect,
        for Bluetooth;
    *   for Android's network: TTC's interface named `eth0` by default, as
        Docker's is (`--interface-name` names it otherwise), which Android's
        Ethernet service manages; `/sys/class/net`, where
        netd lists interfaces; the per-interface settings of
        `/proc/sys/net/ipv{4,6}/{conf,neigh}`, which netd and IpClient
        write; `SIOCSIFADDR` and `SIOCSIFNETMASK`, with which IpClient sets
        the address; and rtnetlink's address events, from which it learns
        the address. Policy routing remains unsupported: netd fails to add
        rules and per-network routes, but the Sentry routes every socket by
        its main table, which is right for one interface;
    *   the DMA-BUF system heap, `/dev/dma_heap/system`, from which Codec2's
        software video codecs allocate: Android encodes its screen for
        scrcpy with them, on the CPU, while it renders on the GPU;
    *   fixes: arm64 signal frames laid out as Linux lays them out, which ART's
        unwinder reads; `setsid(2)` deadlocked with children in an orphaned
        process group; `SOCK_SEQPACKET` peer credentials; and container roots
        reported a disk of 8 EiB, which overflowed Android's storage
        arithmetic.
*   **Graphics:** gfxstream's Vulkan driver for Android, built with the NDK
    alone (guest patch `0002`), and `guest/redroid/gpu_config.sh`, which
    selects it, redroid's minigbm, and ANGLE. SurfaceFlinger crashed while
    MoltenVK's four identical queue families showed; gfxstream patch `0009`
    hides the duplicates.
*   **Display:** scrcpy runs on the host, not in TTC, against adb, which
    `-p 127.0.0.1:5555:5555` publishes (a child's, `ttc port publish`).
    redroid's image turns Codec2's software codecs off, and has no OMX
    service; `gpu_config.sh` turns them on, and has encoders take their
    input surface in the client's process.
*   **Forks:** with `--gfx-fork=snapshot`, Android forks live. The child
    restores the renderer's snapshot, and both branches run on, with the same
    processes and apps in the same state, a network each, and their own
    screens. Live forks needed, as the family's eligibility allows: tmpfs
    mappings on devtmpfs clonable (Android maps `/dev/__properties__`); the
    iptables reaper started by `Stack.Restore`, since its start while the
    state loaded deadlocked on the clock; binder transaction memory per
    branch, which the branches had shared; and restored connection tracking
    that reinstalls what it does not save, its TCP state handlers and random
    source. After a fork, every thread of both branches restarts its system
    call at once; the Sentry restarts them itself, rather than through the
    application, which would need a vCPU each, so the branches answer within
    about ten seconds. The HVF monitor's table of address spaces, one per
    process, holds 4096, and a family of six Androids forks; with 320, a
    fifth Android could not fork.
*   **Not done:** VNC, and the device-lost experiment.
*   **Found in the image:** its simulated Bluetooth HAL aborts on any
    kernel ("Invalid address"): it links two different
    `bluetooth::hci::Address` classes, and tests the parsed address's
    `std::optional` at the wrong offset. The Bluetooth service restarts
    until Android gives up.

## Testing

*   **Unit tests:** the Sentry's device against a fake renderer:
    *   ioctl validation;
    *   handle and file lifetimes;
    *   dma-buf passing and fence polling;
    *   save and load round trips;
    *   each fork semantics.
*   **End-to-end tests:** `runsc/ttc/test/e2e_gfx_*.sh` and
    `e2e_nvproxy_*.sh`:
    *   an offscreen render's checksum;
    *   `device-lost`: the child's old context fails, a new one works, and the
        source is unaffected;
    *   `snapshot`: after the fork, the source and the child render the same
        next frame, and state created before the fork is intact in both;
    *   dehydration and remote forks of snapshot families;
    *   the flags' validation and the waiver rule.

## Risks and open questions

*   Stage-2 mappings of the renderer's memory are new to TTC's HVF engine.
    *Resolved:* they work, read-write and never executable (A2). The Sentry
    must return to host EL0 before it touches the renderer's memory.
*   MoltenVK's coverage limits what Zink and ANGLE can offer on macOS.
*   gfxstream's Vulkan snapshots are young, and gated by a feature flag.
    *Found (A5):* they restore Vulkan objects and memory, but not recorded
    commands, and fail to restore batched descriptor set updates.
*   No distribution packaged Mesa's gfxstream driver, so TTC builds and
    injects it. *Update:* Debian's Mesa now packages one, without
    robustness2; TTC's containers hide it.
*   gfxstream's `SystemBlob` path, which TTC's renderer relies on for
    cross-process blobs, is tested upstream only on Windows. *Resolved on
    macOS,* with patch 0002, which keeps renderers' memory apart.
*   It is unknown how Mesa's gfxstream driver, and Android, behave when a
    device is lost. *Found (A4):* the driver spins forever, so `device-lost`
    ends its processes with `SIGBUS`. Android's behavior is for B6.
*   A renderer per branch costs memory. Starting them lazily, and closing
    them once nothing uses them, limits that.
*   nvproxy's save hook panics on objects it cannot re-create. TTC must refuse
    those forks first.
*   `cuda-checkpoint` does not support UVM memory or exported IPC memory, and
    needs R550 or later.
*   Command buffers pass through the Sentry. Shared-memory rings keep them off
    the socket.

## Code pointers

*   Live-fork policy: `runsc/ttc/cmd/cmd.go`: `validateLiveForkCreationPolicy`,
    `liveForkWaiverReason`, and the `--waive-live-fork` flag. The family
    record's `LiveForkEligible` and `LiveForkWaiver`, and their checks in
    `runsc/ttc/cmd/distributed.go`.
*   Forks: `runsc/boot/sandbox_family.go` (`ForkMany`),
    `pkg/sentry/kernel/live_fork.go` (`LiveForkMany`, `streamLiveForkState`),
    and `pkg/sentry/kernel/dehydrate.go` (`CaptureDehydration`,
    `LoadDehydration`).
*   nvproxy:
    *   `pkg/sentry/devices/nvproxy`: `handlers.go` (capabilities),
        `save_restore.go` (restore and remapping), and `frontend_mmap.go` and
        `uvm_mmap.go` (mappings);
    *   `runsc/boot/nvproxy.go`;
    *   `pkg/sentry/control/state_cuda.go`;
    *   `g3doc/user_guide/gpu.md` and `checkpoint_restore.md`.
*   sysfs PCI trees: `pkg/sentry/fsimpl/sys/pci.go` and `rdma.go`.
*   HVF mappings: `pkg/sentry/platform/vmplatform/platform.go` (`MapFile`), and
    [hvf_memory_footprint.md](hvf_memory_footprint.md).

## References

*   gfxstream: [source](https://github.com/google/gfxstream), including its
    [renderer API](https://github.com/google/gfxstream/blob/main/host/include/gfxstream/virtio-gpu-gfxstream-renderer.h),
    [unstable API](https://github.com/google/gfxstream/blob/main/host/include/gfxstream/virtio-gpu-gfxstream-renderer-unstable.h),
    [VkReconstruction](https://github.com/google/gfxstream/blob/main/host/vulkan/vk_reconstruction.h),
    and
    [snapshot utilities](https://github.com/google/gfxstream/blob/main/host/vulkan/vk_decoder_snapshot_utils.cpp).
*   [Mesa's gfxstream driver](https://www.phoronix.com/news/Mesa-Gfxstream-Merged).
*   [Linux's virtio-gpu interface](https://github.com/torvalds/linux/blob/master/include/uapi/drm/virtgpu_drm.h).
*   [libkrun](https://github.com/libkrun/libkrun), and
    [GPU for containers on macOS](https://sinrega.org/2024-03-06-enabling-containers-gpu-macos/).
*   [Android Emulator snapshots](https://developer.android.com/studio/run/emulator-snapshots).
*   [cuda-checkpoint](https://github.com/NVIDIA/cuda-checkpoint).
*   [Cuttlefish GPU modes](https://source.android.com/docs/devices/cuttlefish/gpu).
