# The GPU renderer

`ttc-gfx-renderer` (`runsc/ttcgfx`) renders what the virtual GPUs
(`pkg/sentry/devices/virtgpu`) of a family's branches submit, with TTC's
Venus renderer (`venus/`), for guests whose Vulkan driver is Mesa's Venus
driver. The design is in `g3doc/proposals/ttc_venus_renderer.md`, and the
virtual GPU's in `g3doc/proposals/ttc_gpu_gfx_nvproxy.md`.

*   `serve-family` renders for the branches of a family, a connection to each,
    speaking `pkg/gfxproto`. The daemon's broker (`ServeBroker`), which each
    Sentry asks for a branch's renderer, gives it the connections, on the
    control connection that is its fd 3, and starts it when the family has
    none. It exits once it serves no branch and keeps nothing for a fork's
    children.
*   `probe` prints the renderer's capability sets as JSON. The Sentry needs
    them before any renderer runs.
*   `launch` starts renderers for the ttcd connected to its fd 3
    (`ServeLauncher`), on macOS.

A fork's child shares its parent's renderer: the renderer clones the
branch's GPU contexts for it, sharing their compiled objects, so that no
shader is compiled again and nothing is serialized. A snapshot is a
reference to the clone, which the children restore.

The renderer loads the Vulkan loader with `dlopen`: on macOS, Homebrew's
`vulkan-loader` and `molten-vk`; on Linux, the host's loader and driver.

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
applies `seatbelt.RendererProfile`: reading only Apple's system files, the
package prefixes `/opt/homebrew` and `/usr/local`, and its own files; the
GPU's IOKit user clients; and POSIX shared memory of its own names. It
cannot write files, use the network, or start processes. So the Vulkan
loader and MoltenVK must be installed under a package prefix.
`--unconfined` skips this, as ttcd with `--unsafe-no-host-isolation` does.

macOS lets no process change its sandbox profile, and ttcd's bars the GPU,
so ttcd's children cannot confine themselves to the renderer's. A ttcd with
host isolation therefore starts `ttc-gfx-renderer launch` before it confines
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
neither transform feedback nor geometry shaders, and applications that need
more would fail rather than render in software (Mesa's llvmpipe has OpenGL
4.6): there, OpenGL renders in software unless an application sets
`MESA_LOADER_DRIVER_OVERRIDE=zink`. Chromium renders on the GPU with ANGLE
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

[scrcpy]: https://github.com/Genymobile/scrcpy
