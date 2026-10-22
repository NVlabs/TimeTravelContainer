# Mesa's Zink, for TTC's GPU renderer

TTC's GPU renderer renders for guests' virgl OpenGL drivers with Zink,
Mesa's Gallium driver on Vulkan (see `runsc/ttc/gfxrenderer/virgl`): on
macOS, where `ttcd` is the renderer, on the MoltenVK that the Venus renderer
links; on Linux, where `ttcd` carries the renderer, on the host's Vulkan
loader. Bazel builds Zink, and what it needs of Mesa, from Mesa's release
(`MODULE.bazel`, `@mesa`) with `tools/mesa.BUILD`.

Mesa builds with meson, which Bazel cannot run. `gen_mesa_build.py` writes
`tools/mesa.BUILD` from a meson build of Mesa for each platform, macOS, Linux
on x86-64, and Linux on arm64: each platform's compilations, as
`cc_library` targets of their own, and Mesa's code generators, which are
Python scripts, as genrules, which run them by `run.py` with Mako,
MarkupSafe and PyYAML, which `MODULE.bazel` pins too. The libraries are
those that Zink links: Zink itself, Gallium's auxiliary code, NIR and the
compiler, Vulkan's utilities, and Mesa's utilities. The code generators are
the same on every platform, as every platform's build has Vulkan's beta
extensions, which macOS's requires.

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
* `0003-zink-let-an-embedder-render-on-a-CPU-s-Vulkan-device.patch`: Zink
  renders on a CPU's Vulkan device, such as lavapipe, only where
  `LIBGL_ALWAYS_SOFTWARE` asks it to; on Linux, the renderer renders on one
  where the host has no GPU, as the Venus renderer does, and says so without
  changing its environment, which its other threads read.

On Linux, the renderer runs on every glibc since 2.17
(`runsc/ttc/glibccompat`): the Linux builds drop the defines of functions
that glibc 2.17 lacks, of which Mesa has its own (`DROPPED` in
`gen_mesa_build.py`), and Zink's C++ links libstdc++ statically.

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

Regenerate it for a new release of Mesa, or other libraries or options,
from each platform's meson build in turn: the generator replaces the
targets of the platform whose build it reads, and keeps the other
platforms'. For a new release, regenerate every platform's: their code
generators must agree.

On macOS, with the release's source in `$MESA` and a scratch directory `$Z`:

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

On Linux, in the image that `make build` builds in, whose gcc is Bazel's
toolchain's, configure a build for each architecture, x86-64 natively and
arm64 with the image's cross compiler, run its code generators, and
regenerate, with the release's source in `$MESA`, a scratch directory `$Z`,
and a meson cross file `$Z/aarch64.txt` that names `aarch64-linux-gnu-gcc`,
`aarch64-linux-gnu-g++` and `aarch64-linux-gnu-ar`, and no `pkg-config`:

```sh
apt-get install -y bison flex
python3 -m pip install --target $Z/py meson ninja mako pyyaml packaging
export PATH=$Z/py/bin:$PATH PYTHONPATH=$Z/py
for arch in x86_64 aarch64; do
  if [ $arch = aarch64 ]; then cross="--cross-file $Z/aarch64.txt"; else cross=; fi
  CC=x86_64-linux-gnu-gcc CXX=x86_64-linux-gnu-g++ meson setup $Z/build-$arch $MESA $cross \
    -Dbuildtype=release -Ddefault_library=static -Db_ndebug=true \
    -Dplatforms= -Dgallium-drivers=zink -Dvulkan-drivers= -Dopengl=true \
    -Dgles1=disabled -Dgles2=disabled -Dglx=disabled -Degl=disabled -Dgbm=disabled \
    -Dllvm=disabled -Dvalgrind=disabled -Dlibunwind=disabled -Dzstd=disabled \
    -Dxmlconfig=disabled -Dexpat=disabled -Dshader-cache=disabled \
    -Dgallium-va=disabled -Dgallium-rusticl=false -Dbuild-tests=false -Dtools= \
    -Dspirv-tools=disabled -Ddisplay-info=disabled -Dteflon=false \
    -Dzlib=disabled -Dglvnd=disabled -Dvulkan-beta=true
  # The code generators' outputs, which the generator reads.
  ninja -C $Z/build-$arch $(ninja -C $Z/build-$arch -t targets rule CUSTOM_COMMAND | grep -v '^meson-')
  python3 tools/mesa/gen_mesa_build.py \
    --source $MESA --build $Z/build-$arch --out tools/mesa.BUILD --version 26.2.4
done
```

The Linux builds leave zlib out, which macOS's links from the system: Mesa
compresses only its shader cache with it, and has a CRC-32 of its own.

Then check that the renderer builds and renders, on each platform:

```sh
bazel test //runsc/ttc/gfxrenderer/virgl:virgl_test
```
