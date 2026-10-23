# Mesa's Zink, for TTC's GPU renderer

On macOS, `ttcd`, which is TTC's GPU renderer there, renders for guests'
virgl OpenGL drivers with Zink, Mesa's Gallium driver on Vulkan, on the
MoltenVK that the Venus renderer links (see
`runsc/ttc/gfxrenderer/virgl`), unless `TTC_VIRGL_BACKEND=vkgl` has it
render with vkgl, the renderer's own Gallium on Vulkan, in Go, as Linux's
renderer does, which links no Mesa. Mesa leaves the tree once macOS's
renderer renders with vkgl too (`g3doc/proposals/ttc_virgl_go_backend.md`).
Bazel builds Zink, and what it needs of Mesa, from Mesa's release
(`MODULE.bazel`, `@mesa`) with `tools/mesa.BUILD`.

Mesa builds with meson, which Bazel cannot run. `gen_mesa_build.py` writes
`tools/mesa.BUILD` from a meson build of Mesa: the same compilations, as
`cc_library` targets, and Mesa's code generators, which are Python scripts,
as genrules, which run them by `run.py` with Mako, MarkupSafe and PyYAML,
which `MODULE.bazel` pins too. The libraries are those that Zink links:
Zink itself, Gallium's auxiliary code, NIR and the compiler, Vulkan's
utilities, and Mesa's utilities.

`gallium_api` is what the renderer includes: Gallium's interface, Zink's,
and virgl's protocol (`src/virtio/virtio-gpu`), which Mesa's guest driver
shares.

## Patches

* `0001-zink-take-Vulkan-s-entry-points-from-an-embedder.patch`: Zink loads
  Vulkan with `dlopen`; the renderer links MoltenVK, and gives Zink its
  entry points instead, through which it also works around MoltenVK's lack
  of robustness2's `nullDescriptor`, as the Venus renderer does.
* `0002-tgsi-count-sanity-errors-whether-they-are-printed-or-not.patch`:
  `tgsi_sanity_check` counted errors only where it printed them, and so
  passed every shader; the renderer checks guests' TGSI with it, as
  `tgsi_to_nir` dereferences the declarations of the registers used.
* `0004-tgsi-read-floats-printed-as-hex-as-their-bits.patch`: virgl's
  guests print their immediates' floats as their bits, in hex, which
  `tgsi_text_translate` read with `strtof`, as hexadecimal floats.
* `0005-tgsi_to_nir-read-an-input-FACE-as-TGSI-s-float.patch`: TGSI's
  `FACE` is a float where shaders read it from an input, as virgl's guests
  do, and an integer where they read it from a system value; `tgsi_to_nir`
  gave Zink, whose face is an integer system value, the integer of either,
  of which guests took front faces for back faces.
* `0006-tgsi-bound-the-vertices-that-the-sanity-check-declares.patch`:
  `tgsi_sanity_check`, which `tgsi_text_translate` runs, declared each of
  as many vertices of a tessellation control shader's outputs as a guest's
  shader said, 2^31 of them.

## The release's mirrors

`MODULE.bazel` fetches the release from distributions' mirrors before
`archive.mesa3d.org`, which serves a connection some 15 KiB/s: Gentoo's,
`distfiles/XX/mesa-VERSION.tar.xz`, where `XX` is the first byte, in hex,
of the BLAKE2b of the file's name, and Fedora's lookaside cache, by the
file's SHA-512. A new release names its own, once both serve the SHA-256
of Mesa's:

```sh
python3 -c "import hashlib; print(hashlib.blake2b(b'mesa-VERSION.tar.xz').hexdigest()[:2])"
shasum -a 512 mesa-VERSION.tar.xz
```

## Regenerating `tools/mesa.BUILD`

Regenerate it for a new release of Mesa, or other libraries or options. On
macOS, with the release's source in `$MESA` and a scratch directory `$Z`:

```sh
python3 -m venv $Z/venv
$Z/venv/bin/pip install meson ninja mako pyyaml packaging
# MoltenVK's headers, which Bazel fetched.
MVK=$(bazel info output_base)/external/+_repo_rules+moltenvk
PATH=$Z/venv/bin:$PATH meson setup $Z/build $MESA \
  -Dbuildtype=release -Ddefault_library=static -Db_ndebug=true \
  -Dplatforms=macos -Dgallium-drivers=zink -Dvulkan-drivers= -Dopengl=true \
  -Dgles1=disabled -Dgles2=disabled -Dglx=disabled -Degl=disabled -Dgbm=disabled \
  -Dllvm=disabled -Dvalgrind=disabled -Dlibunwind=disabled -Dzstd=disabled \
  -Dxmlconfig=disabled -Dexpat=disabled -Dshader-cache=disabled \
  -Dgallium-va=disabled -Dgallium-rusticl=false -Dbuild-tests=false -Dtools= \
  -Dspirv-tools=disabled -Ddisplay-info=disabled -Dteflon=false \
  -Dmoltenvk-dir=$MVK
# The libraries, so that their generated sources exist.
PATH=$Z/venv/bin:$PATH ninja -C $Z/build \
  src/gallium/drivers/zink/libzink.a src/gallium/auxiliary/libgallium.a \
  src/compiler/nir/libnir.a src/compiler/libcompiler.a \
  src/vulkan/util/libvulkan_util.a src/util/libxmlconfig.a \
  src/util/libmesa_util.a src/util/libmesa_util_simd.a \
  src/util/blake3/libblake3.a src/c11/impl/libmesa_util_c11.a
PATH=$Z/venv/bin:$PATH python3 tools/mesa/gen_mesa_build.py \
  --source $MESA --build $Z/build --out tools/mesa.BUILD --version 26.2.4
```

`-Dplatforms=macos` matters: without it, MoltenVK's headers define
Vulkan's platform macros after Mesa includes Vulkan's headers. `-Dopengl`
builds NIR, which Mesa builds only for a driver of OpenGL or Vulkan.
`-Dshader-cache=disabled` keeps Zink from writing a cache in the home
directory, which the renderer's sandbox denies.

Then check that the renderer builds and renders:

```sh
bazel test //runsc/ttc/gfxrenderer/virgl:virgl_test
```
