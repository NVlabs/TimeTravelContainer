# The GPU renderer

`ttc-gfx-renderer` (`runsc/ttcgfx`) renders what the virtual GPUs
(`pkg/sentry/devices/virtgpu`) of a family's branches submit, with TTC's
Venus renderer (`venus/`), for guests whose Vulkan driver is Mesa's Venus
driver, and, on macOS, with its virgl renderer (`virgl/`), for guests whose
OpenGL driver is Mesa's virgl driver. The design is in
`g3doc/proposals/ttc_venus_renderer.md`, and the virtual GPU's in
`g3doc/proposals/ttc_gpu_gfx_nvproxy.md`.

*   `serve-family` renders for the branches of a family, a connection to each,
    speaking `pkg/gfxproto`. The daemon's broker (`ServeBroker`), which each
    Sentry asks for a branch's renderer, gives it the connections, on the
    control connection that is its fd 3, and starts it when the family has
    none. It exits once it serves no branch and keeps nothing for a fork's
    children. The broker keeps its copies of a connection's ends until the
    renderer and the Sentry say that they received them: macOS may flush a
    Unix socket whose every reference is in a message in flight, and the
    renderer then read the end of a connection whose Sentry was there, as
    one boot of redroid in about sixteen did.
*   `probe` prints the renderer's capability sets as JSON. The Sentry needs
    them before any renderer runs.
*   `launch` starts renderers for the ttcd connected to its fd 3
    (`ServeLauncher`), on macOS.

ttcd is the renderer, or carries it, so that a host needs no file of it.
On macOS, where ttcd links C and loads the renderer's graphics libraries only
when it renders, ttcd is the renderer when it runs as `ttc-gfx-renderer`
(`Name`): the daemon gives it that argv[0], as it gives runsc's gofer
`runsc-gofer`, and `ttcd _gfx-renderer MODE` runs it by hand
(`GFXRendererArg` in `runsc/ttc/cmd`). Every renderer runs as
`ttc-gfx-renderer`, whatever its executable, so that process listings name
it alike on every host. On Linux, where ttcd links no C library and the
renderer does, ttcd carries the renderer, gzip-compressed
(`runsc/ttc/hostbin`), and runs it from a sealed memory file, or from a file
in its root where memory files cannot run. Its C library functions are
bound to versions that every glibc since 2.17 has (`runsc/ttc/glibccompat`).
`TTC_GFX_RENDERER` names a renderer to run instead; a ttcd that carries
none, as one cross-built for another architecture (`--config=aarch64`) or
built with `--//runsc/ttc/hostbin:carry_helpers=false`, runs the one beside
it.

A fork's child shares its parent's renderer: the renderer clones the
branch's GPU contexts for it, sharing their compiled objects, so that no
shader is compiled again and nothing is serialized. A snapshot is a
reference to the clone, which the children restore.

On macOS the renderer links MoltenVK, of its release (MODULE.bazel), so
that ttcd loads no library but Apple's, as a signature of a developer's
certificate requires; it calls MoltenVK without the Vulkan loader. On Linux
it loads the host's loader with `dlopen`, which loads the host's driver.

Guests map host-visible memory, which is the renderer's shared memory. On
macOS the renderer imports it into Vulkan as `MTLBuffer`s
(`VK_EXT_external_memory_metal`); on Linux, by its address
(`VK_EXT_external_memory_host`), into the memory types that the host's
driver imports it into, which it probes once per GPU: the others guests see
as memory they cannot map. Guests see no software rasterizer, such as
llvmpipe, where the host has a GPU.

## Confinement

A renderer decodes guests' command streams, so it confines itself before it
loads Vulkan or reads anything from its Sentry (`Confine`). On macOS it
applies `seatbelt.RendererProfile`: reading only Apple's system files and
its own files; the GPU's IOKit user clients; and POSIX shared memory of its
own names. It cannot write files, use the network, or start processes.
`--unconfined` skips this, as ttcd with `--unsafe-no-host-isolation` does.

macOS lets no process change its sandbox profile, and ttcd's bars the GPU,
so ttcd's children cannot confine themselves to the renderer's. A ttcd with
host isolation therefore starts its renderer's `launch` before it confines
itself, and has it start the renderers and probes. The launcher stays
unconfined, so that its children can confine themselves, and does nothing
but start the renderer's executable with fixed arguments.

On Linux, renderers do not confine themselves yet (plan item B1).

To test the renderer on the host's GPU:

```sh
bazel test //pkg/sentry/devices/virtgpu/rendererclient:rendererclient_test
```

## The guest's driver

Guests use Mesa's Venus Vulkan driver. `guest/build_mesa_venus.sh` builds it
in a Debian container of the guest's architecture, with TTC's patches
(`guest/patches/`): on NVIDIA hosts, Mesa hides the device's DRM properties,
for X11's and Wayland's presentation, which the driver has none of, and
Zink then finds no device for the render node.

ttcd gives it to the containers of `--gfx` families: it mounts, read-only,

*   the driver, from `gfx-guest/ARCH/libvulkan_virtio.so` beside ttcd
    (`TTC_GFX_GUEST_DRIVER` names another directory), at
    `/usr/lib/ttc-gfx/libvulkan_virtio.so`;
*   its manifest, which the Vulkan loader finds it by, at
    `/usr/share/vulkan/icd.d/ttc_venus_icd.json`;
*   on hosts other than macOS, a configuration of Mesa's OpenGL, at
    `/usr/share/drirc.d/50-ttc-gfx.conf`, which has it drive the virtual GPU
    by Zink, OpenGL on Vulkan: the GPU has no OpenGL driver. If Zink fails,
    Mesa renders in software, as it would without the configuration.

It also sets `VK_LOADER_DRIVERS_DISABLE=virtio_icd*` unless the image or the
container sets it: the Venus driver that distributions package (Debian's
`mesa-vulkan-drivers`, for one) would drive the GPU as a second device.
Applications need no change: Vulkan ones find the GPU, and OpenGL ones that
use Mesa's EGL on its surfaceless platform render on it by Zink. On macOS,
Zink on MoltenVK has only OpenGL 2.1 and OpenGL ES 2.0, as MoltenVK has
neither transform feedback nor geometry shaders: there, Mesa's OpenGL drives
the GPU with its virgl driver, which the renderer serves with Zink on the
host (see [virgl](#virgl)), with OpenGL 3.1 and OpenGL ES 3.1, unless an
application sets `MESA_LOADER_DRIVER_OVERRIDE=zink`, for Zink in the guest,
or `LIBGL_ALWAYS_SOFTWARE=1`, for Mesa's llvmpipe, which has OpenGL 4.6 in
software. Chromium renders on the GPU with ANGLE
on Vulkan (`--use-angle=vulkan`), which offers OpenGL ES 3, and llama.cpp's
Vulkan backend computes on it. The driver has no window systems
(`-Dplatforms=`), which would link libxcb and libwayland: X11 and Wayland
applications render in software.

### Android

`guest/build_mesa_venus_android.sh` builds the driver for Android (arm64, or
x86-64 with `ARCH=x86_64`) with the NDK, on macOS or Linux, into
`vulkan.virtio.so`, which Android loads as the Vulkan HAL of
`ro.hardware.vulkan=virtio`. Android's buffers are minigbm's, the gralloc of
Android images for virtio-gpu: minigbm asks the renderer, by a cross-domain
context, how to lay out each image, and the renderer answers with the layout
of the host's linear images, so that the images that the guest's Vulkan
creates of a buffer share its memory.

Where the host's driver cannot give a linear image what the guest uses an
image of a buffer for, as NVIDIA's cannot render to linear images, the image
is a shadow (`venus/shadow_unsafe.go`): an optimal image in GPU memory, which
the guest's commands use, and the linear image in the buffer's memory, which
the guest sees, kept in step by the renderer's copies. They copy the linear
image to the shadow where the guest takes the image from a foreign or
external queue family, and the shadow to the linear image where it gives the
image to one, and at the end of each command buffer that rendered or copied
to it. Images that are only sampled, and every image where linear images
have what optimal ones have, as with MoltenVK on Apple silicon, are not
shadows.

redroid, an Android image for containers, runs in TTC with the GPU: its init
is the container's, and `guest/redroid/gpu_config.sh`, which replaces the
image's `/vendor/bin/gpu_config.sh`, has it use the image's minigbm
(`ro.hardware.gralloc=cros`), the driver, and the image's ANGLE, OpenGL ES
on Vulkan, for SurfaceFlinger and apps; it also turns on Codec2's software
video codecs, which encode the screen for screen mirroring. With
`--gfx-fork=snapshot`, Android forks live, and its adb, on port 5555, can be
published to the host:

```sh
ttc create --name android --gfx --gfx-fork snapshot -p 127.0.0.1:5555:5555 \
  --cap-add ALL --network ttc --nat \
  --entrypoint /init redroid/redroid:14.0.0_64only-latest \
  qemu=1 androidboot.hardware=redroid androidboot.use_memfd=true
ttc cp out-android-venus/lib64/hw/vulkan.virtio.so android@main:/vendor/lib64/hw/
ttc cp guest/redroid/gpu_config.sh android@main:/vendor/bin/gpu_config.sh
ttc start android
ttc exec android@main /system/bin/dumpsys SurfaceFlinger | grep GLES
```

[scrcpy] shows and controls Android from the host, over adb; neither runs in
TTC. Android renders on the GPU, and its software encoder compresses the
screen to H.264, on the CPU:

```sh
adb connect 127.0.0.1:5555
scrcpy -s 127.0.0.1:5555 --no-audio
```

A fork's child is another Android, with the same apps in the same state,
whose adb a port of its own publishes:

```sh
ttc fork android@main child
ttc port publish android@child 127.0.0.1:5556:5555
adb connect 127.0.0.1:5556
scrcpy -s 127.0.0.1:5556 --no-audio
```

A fork pauses a booted Android for about 2 s, and both branches answer
within a second after.

`runsc/ttc/test/e2e_redroid.sh` boots it so, checks that SurfaceFlinger and
apps render with ANGLE on the GPU, that the screen records to video, and
that both branches of a live fork run on, and installs and runs an APK.

redroid 14's own Venus driver, redroid's build of Mesa 24.0.8, does not do:
the device and the renderer serve it, as they serve Venus drivers before
Mesa 24.2 (`CROSS_DEVICE`, and sync files that `venus/syncfd_unsafe.go`
emulates), and SurfaceFlinger starts on it, but the build offers no
`VK_ANDROID_external_memory_android_hardware_buffer`, without which ANGLE
has no `EGL_ANDROID_image_native_buffer` and SurfaceFlinger composes no
app's buffers. Android 14's Vulkan loader also offers ANGLE
`VK_EXT_swapchain_maintenance1` on it, whose feature the driver refuses at
`vkCreateDevice` unless ANGLE leaves it off
(`debug.angle.feature_overrides_disabled=supportsSwapchainMaintenance1`).

## virgl

On macOS the renderer also serves Mesa's virgl driver, the OpenGL driver for
virtio-gpu that distributions' Mesa and Android images ship: it offers
virgl's capability sets, 1 and 2, and executes the contexts of those, or of
none, which guests create without asking for one, with Zink, Mesa's Gallium
driver on Vulkan, which ttcd links on the MoltenVK that the Venus renderer
links (`tools/mesa/README.md`). virgl's commands are Gallium's calls, of
Gallium's state objects and TGSI shaders, which Zink takes as they are:
`virgl/context_unsafe.go` checks each command and resolves its handles, and
`virgl/pipe_darwin.c` checks what Zink trusts of its fields and makes the
call, as virglrenderer's vrend does on the host's OpenGL. A command that a
guest's driver would not send, on which Zink could trap, ends its stream.

A virgl context's classic resources are Gallium resources, and the
renderer's shared memory, which the guest maps, is the guest's memory of
them: transfers copy between the two. Fences of the global timeline, which
virgl's submissions use, signal once Zink's work before them is done, and
queries' results are in the guest's memory by then.

Guests need no configuration: Debian's Mesa reports `virgl (zink Vulkan
1.4(Apple M5 Pro (MOLTENVK)))`, with OpenGL 3.1 and OpenGL ES 3.1, more than
Zink in the guest has, and redroid's image runs in its own host mode, with
no file of TTC's: its Mesa's virgl driver renders SurfaceFlinger and apps,
and its GBM allocates their buffers.

```sh
ttc create --name android --gfx --gfx-fork device-lost --cap-add ALL \
  --network ttc --nat --entrypoint /init redroid/redroid:14.0.0_64only-latest \
  qemu=1 androidboot.hardware=redroid androidboot.use_memfd=true \
  androidboot.redroid_gpu_mode=host androidboot.redroid_gpu_node=/dev/dri/renderD128
```

The node names the render node, which redroid otherwise finds through
debugfs, which TTC has none of; without debugfs, redroid offers apps no
Vulkan. Applications that need more OpenGL than virgl has here fail rather
than render in software: they can ask for Mesa's llvmpipe with
`LIBGL_ALWAYS_SOFTWARE=1`.

OpenGL 3.0 and OpenGL ES 3.0, which Android 14's HWUI requires, require
transform feedback, which MoltenVK, and so Zink, has none of: the renderer
emulates it, as ANGLE does on Vulkan without it. A vertex shader with a
stream output captures it itself: at its end it stores its outputs to the
targets' buffers, which the renderer binds as shader buffers, at its
vertex's index among the draw's, which a uniform buffer of the draw's
parameters gives. The renderer keeps those slots of the vertex shader's
state from the guest, and counts the queries of transform feedback and of
primitives generated on the CPU, which knows each draw's size. It captures
what OpenGL ES 3.0 allows to: lists of points, lines and triangles, of no
indexed draw; of OpenGL's indexed and strip draws, it captures nothing, and
logs so.

TGSI's shaders differ from what Zink's own frontends give it, in ways that
MoltenVK refuses:

*   TGSI's vertex inputs have no types, and Metal requires that shaders read
    integer vertex formats as integers: draws bind a variant of the vertex
    shader whose inputs have the types of the vertex elements bound, as
    virglrenderer's vrend makes for OpenGL.
*   Zink names SPIR-V's variables by TGSI's, which MoltenVK names Metal's
    by, and Metal fails to compile a texture named `sampler`: the
    renderer's Vulkan, for Zink, drops the names, which are for debuggers
    only.

Where MoltenVK still fails to create a pipeline, Zink binds none, and the
renderer drops the draws that would use it, as virglrenderer drops draws
that it cannot draw. The renderer's Vulkan for Zink logs only MoltenVK's
errors, not the warning that MoltenVK logs of each of Zink's pipelines.

The renderer does not clone virgl's contexts yet: `--gfx-fork=snapshot`
fails while a branch has a virgl context or resource, and the child of a
`--gfx-fork=device-lost` fork loses them, as it loses Venus's. On Linux the
renderer offers no virgl capability sets yet.

[scrcpy]: https://github.com/Genymobile/scrcpy
