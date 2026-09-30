#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds Mesa's gfxstream Vulkan driver for Android (arm64), the guest side
# of --gfx for Android containers such as redroid's, into
# $PREFIX/lib64/hw/vulkan.gfxstream.so (default PREFIX: ./out-android). Run
# it on macOS or x86-64 Linux, with the Android NDK in $ANDROID_NDK, and
# meson, ninja, and Python's mako and PyYAML in the PATH:
#
#   python3 -m venv venv && venv/bin/pip install meson ninja mako pyyaml packaging
#   PATH=$PWD/venv/bin:$PATH ANDROID_NDK=~/android-ndk-r27c sh build_mesa_gfxstream_android.sh
#
# The driver needs the NDK alone (patches/0002): it drives the render node
# of virtio-gpu, and uses minigbm's gralloc, which Android images for
# virtio-gpu have. It links libdrm and the C++ runtime statically, and
# needs only libraries that Android gives vendor drivers (libhardware,
# liblog, libnativewindow, libsync). Android loads it as the Vulkan HAL of
# ro.hardware.vulkan=gfxstream.
set -eu
: "${ANDROID_NDK:?set ANDROID_NDK to the Android NDK}"
API=${API:-34}
PREFIX=${PREFIX:-$PWD/out-android}
PATCHES=${PATCHES:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/patches}
# The commit that the virtual GPU was developed against.
MESA_COMMIT=${MESA_COMMIT:-49162a0ba8f3647917bc4c94d13eea6b51a1e49f}
BUILD=${BUILD:-$PWD/build-android}

case "$(uname -s)" in
Darwin) HOST_TAG=darwin-x86_64 ;;
Linux) HOST_TAG=linux-x86_64 ;;
*) echo "unsupported host: $(uname -s)" >&2; exit 1 ;;
esac
TOOLCHAIN=$ANDROID_NDK/toolchains/llvm/prebuilt/$HOST_TAG/bin
[ -x "$TOOLCHAIN/aarch64-linux-android$API-clang" ] || {
    echo "no NDK compiler for API $API in $TOOLCHAIN" >&2
    exit 1
}

mkdir -p "$BUILD"
cd "$BUILD"
if [ ! -d mesa ]; then
    git init -q mesa
    git -C mesa fetch -q --depth 1 https://gitlab.freedesktop.org/mesa/mesa.git "$MESA_COMMIT"
    git -C mesa checkout -q FETCH_HEAD
    for patch in "$PATCHES"/*.patch; do
        [ -e "$patch" ] || continue
        git -C mesa apply "$patch"
    done
fi
cat >android-aarch64.cross <<EOF
[binaries]
c = '$TOOLCHAIN/aarch64-linux-android$API-clang'
cpp = '$TOOLCHAIN/aarch64-linux-android$API-clang++'
ar = '$TOOLCHAIN/llvm-ar'
strip = '$TOOLCHAIN/llvm-strip'
pkg-config = 'false'

[properties]
needs_exe_wrapper = true

[host_machine]
system = 'android'
cpu_family = 'aarch64'
cpu = 'armv8-a'
endian = 'little'
EOF
cd mesa
meson setup build --cross-file ../android-aarch64.cross -Dplatforms=android \
    -Dplatform-sdk-version="$API" -Dandroid-stub=true -Dandroid-libbacktrace=disabled \
    -Dvulkan-drivers=gfxstream -Dgallium-drivers= -Dglx=disabled -Degl=disabled -Dgbm=disabled \
    -Dopengl=false -Dgles1=disabled -Dgles2=disabled -Dllvm=disabled -Dvideo-codecs= \
    -Dxmlconfig=disabled -Dzstd=disabled -Dshader-cache=disabled \
    -Dallow-fallback-for=libdrm --force-fallback-for=libdrm -Dlibdrm:default_library=static \
    "-Dcpp_link_args=['-static-libstdc++']" \
    -Dbuildtype=release >"$BUILD/meson-setup.log" 2>&1 || { tail -30 "$BUILD/meson-setup.log"; exit 1; }
ninja -C build src/gfxstream/guest/vulkan/libvulkan_gfxstream.so >"$BUILD/ninja.log" 2>&1 ||
    { grep -m5 -B2 -A8 'error:' "$BUILD/ninja.log"; exit 1; }
mkdir -p "$PREFIX/lib64/hw"
"$TOOLCHAIN/llvm-strip" -o "$PREFIX/lib64/hw/vulkan.gfxstream.so" \
    build/src/gfxstream/guest/vulkan/libvulkan_gfxstream.so
ls -la "$PREFIX/lib64/hw/vulkan.gfxstream.so"
