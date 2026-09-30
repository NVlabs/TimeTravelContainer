# The GPU renderer

`ttc-gfx-renderer` (`runsc/ttcgfx`) renders what a sandbox's virtual GPU
(`pkg/sentry/devices/virtgpu`) submits, with [gfxstream]. The design is in
`g3doc/proposals/ttc_gpu_gfx_nvproxy.md`.

*   `serve` renders for the Sentry connected to its fd 3, speaking
    `pkg/gfxproto`. The daemon starts one per branch whose GPU is in use,
    through the broker (`ServeBroker`), which the Sentry asks. A renderer
    exits when its Sentry closes it: once no context or resource of the
    branch's GPU is left.
*   `probe` prints the renderer's capability sets as JSON. The Sentry needs
    them before any renderer runs.
*   `launch` starts renderers for the ttcd connected to its fd 3
    (`ServeLauncher`), on macOS.

`--snapshots` makes a renderer, for a family with `--gfx-fork=snapshot`,
that takes snapshots and restores them (`MsgSnapshot`, `MsgRestore`). It
records how to rebuild its Vulkan objects from its start, which costs
memory, and its capability sets differ: it does not offer guests batched
descriptor set updates, whose replay fails to restore. A snapshot is an
archive of gfxstream's snapshot files and of what gfxstream does not save:
the renderer's resources, and its classic resources' backings. Snapshots
travel through pipes, so that the Sentry neither creates nor maps memory for
them. gfxstream rebuilds command buffers but not the commands recorded in
them: a guest must record them again after a fork, as Android's renderers
do each frame.

## Confinement

A renderer decodes a guest's command streams with gfxstream, a large C++
library, so it confines itself before it loads gfxstream or reads anything
from its Sentry (`Confine`). On macOS it applies
`seatbelt.RendererProfile`: reading only Apple's system files, the package
prefixes `/opt/homebrew` and `/usr/local`, and its own, gfxstream's, and the
Vulkan loader's files; the GPU's IOKit user clients; and POSIX shared memory
of its own names. It cannot write files, use the network, or start
processes. So the Vulkan loader and MoltenVK must be installed under a
package prefix. `--unconfined` skips this, as ttcd with
`--unsafe-no-host-isolation` does.

macOS lets no process change its sandbox profile, and ttcd's bars the GPU,
so ttcd's children cannot confine themselves to the renderer's. A ttcd with
host isolation therefore starts `ttc-gfx-renderer launch` before it confines
itself, and has it start the renderers and probes. The launcher stays
unconfined, so that its children can confine themselves, and does nothing
but start the renderer's executable with fixed arguments.

On Linux, renderers do not confine themselves yet (plan item B1).

## gfxstream

The renderer loads gfxstream's host library with `dlopen`: by `--library`,
beside its executable, or from the dynamic linker's search path. TTC builds
it from upstream [gfxstream] at `62c7716251c56619cdaf2419de4b53ee6035937f`
with the patches in `patches/`:

```sh
git clone https://github.com/google/gfxstream.git && cd gfxstream
git checkout 62c7716251c56619cdaf2419de4b53ee6035937f
git am /path/to/ttc/runsc/ttc/gfxrenderer/patches/*.patch
meson setup build -Ddefault_library=shared -Dgfxstream-build=host -Dbuildtype=release \
  -Dsnapshots=enabled
ninja -C build   # build/host/libgfxstream_backend.{dylib,so}
```

The patches:

*   `0001`: `stream_renderer_init` fails, instead of hanging, when the
    renderer does not start.
*   `0002`: on macOS, shared memory is created unnamed. POSIX shared memory
    names are global to the user there, and gfxstream's repeat in every
    process, so two renderers shared their command rings and memory.
*   `0003`: the meson build has snapshot support (`-Dsnapshots`), which needs
    protobuf; only Android's build had it.
*   `0004`: a snapshot pauses the rings' consumers, which decode what guests
    write, for all of the snapshot, not only while the rings are saved.
*   `0005`: a snapshot whose files cannot be opened fails, instead of
    crashing the renderer.

*   **macOS**: Homebrew's `molten-vk`, `vulkan-loader`, and
    `vulkan-headers`, and `meson` and `ninja`. gfxstream looks for the Vulkan
    loader only beside the program, so the renderer sets
    `ANDROID_EMU_VK_LOADER_PATH` to Homebrew's if it is unset.
*   **Linux**: the host's Vulkan driver and loader.
*   The Vulkan-only configuration (`-Ddecoders=vulkan`) does not compile at
    this commit; the default decoders build, and the renderer runs Vulkan only.
*   Snapshots (`--gfx-fork=snapshot`) need protobuf: Homebrew's
    `protobuf` on macOS.

To test the renderer on the host's GPU:

```sh
bazel test //pkg/sentry/devices/virtgpu/rendererclient:rendererclient_test \
  --test_env=TTC_GFXSTREAM_LIBRARY=/path/to/libgfxstream_backend.dylib
```

## The guest's driver

Guests use Mesa's gfxstream Vulkan driver, which no distribution packages.
`guest/build_mesa_gfxstream.sh` builds it in a Debian container.

[gfxstream]: https://github.com/google/gfxstream
