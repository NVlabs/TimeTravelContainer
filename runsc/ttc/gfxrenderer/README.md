# The GPU renderer

`ttc-gfx-renderer` (`runsc/ttcgfx`) renders what a sandbox's virtual GPU
(`pkg/sentry/devices/virtgpu`) submits, with [gfxstream]. The design is in
`g3doc/proposals/ttc_gpu_gfx_nvproxy.md`.

*   `serve` renders for the Sentry connected to its fd 3, speaking
    `pkg/gfxproto`. The daemon starts one per branch whose GPU is in use,
    through the broker (`ServeBroker`), which the Sentry asks.
*   `probe` prints the renderer's capability sets as JSON. The Sentry needs
    them before any renderer runs.

## gfxstream

The renderer loads gfxstream's host library with `dlopen`: by `--library`,
beside its executable, or from the dynamic linker's search path. TTC builds
it from upstream [gfxstream] at `62c7716251c56619cdaf2419de4b53ee6035937f`
with the patches in `patches/`:

```sh
git clone https://github.com/google/gfxstream.git && cd gfxstream
git checkout 62c7716251c56619cdaf2419de4b53ee6035937f
git am /path/to/ttc/runsc/ttc/gfxrenderer/patches/*.patch
meson setup build -Ddefault_library=shared -Dgfxstream-build=host -Dbuildtype=release
ninja -C build   # build/host/libgfxstream_backend.{dylib,so}
```

The patches:

*   `0001`: `stream_renderer_init` fails, instead of hanging, when the
    renderer does not start.
*   `0002`: on macOS, shared memory is created unnamed. POSIX shared memory
    names are global to the user there, and gfxstream's repeat in every
    process, so two renderers shared their command rings and memory.

*   **macOS**: Homebrew's `molten-vk`, `vulkan-loader`, and
    `vulkan-headers`, and `meson` and `ninja`. gfxstream looks for the Vulkan
    loader only beside the program, so the renderer sets
    `ANDROID_EMU_VK_LOADER_PATH` to Homebrew's if it is unset.
*   **Linux**: the host's Vulkan driver and loader.
*   The Vulkan-only configuration (`-Ddecoders=vulkan`) does not compile at
    this commit; the default decoders build, and the renderer runs Vulkan only.
*   Snapshots (`--gfx-fork=snapshot`) need
    `GFXSTREAM_BUILD_WITH_SNAPSHOT_FRONTEND_SUPPORT` and protobuf, which the
    meson build does not set up yet.

To test the renderer on the host's GPU:

```sh
bazel test //pkg/sentry/devices/virtgpu/rendererclient:rendererclient_test \
  --test_env=TTC_GFXSTREAM_LIBRARY=/path/to/libgfxstream_backend.dylib
```

## The guest's driver

Guests use Mesa's gfxstream Vulkan driver, which no distribution packages.
`guest/build_mesa_gfxstream.sh` builds it in a Debian container.

[gfxstream]: https://github.com/google/gfxstream
