#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds Mesa's Venus Vulkan driver (virtio) for Android (arm64), the guest
# side of --gfx for Android containers such as redroid's with TTC's Venus
# renderer, into $PREFIX/lib64/hw/vulkan.virtio.so (default PREFIX:
# ./out-android-venus). Run it on macOS or x86-64 Linux, with the Android
# NDK in $ANDROID_NDK, and meson, ninja, and Python's mako and PyYAML in the
# PATH:
#
#   python3 -m venv venv && venv/bin/pip install meson ninja mako pyyaml packaging
#   PATH=$PWD/venv/bin:$PATH ANDROID_NDK=~/android-ndk-r27c sh build_mesa_venus_android.sh
#
# The driver drives the render node of virtio-gpu, and takes the layouts of
# Android's buffers from minigbm's gralloc, which Android images for
# virtio-gpu have. It links libdrm and the C++ runtime statically, and needs
# only libraries that Android gives vendor drivers. Android loads it as the
# Vulkan HAL of ro.hardware.vulkan=virtio.
set -eu
: "${ANDROID_NDK:?set ANDROID_NDK to the Android NDK}"
API=${API:-34}
PREFIX=${PREFIX:-$PWD/out-android-venus}
# The commit that the virtual GPU was developed against, and the version of
# the Venus protocol that it pins.
MESA_COMMIT=${MESA_COMMIT:-49162a0ba8f3647917bc4c94d13eea6b51a1e49f}
VENUS_PROTOCOL_TAG=${VENUS_PROTOCOL_TAG:-v1.1.3}
BUILD=${BUILD:-$PWD/build-android-venus}

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
fi
if [ ! -d mesa/subprojects/venus-protocol ]; then
    git clone -q --depth 1 --branch "$VENUS_PROTOCOL_TAG" \
        https://gitlab.freedesktop.org/virgl/venus-protocol.git mesa/subprojects/venus-protocol
fi
cat >android-aarch64.cross <<CROSS
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
CROSS
cd mesa
meson setup build --cross-file ../android-aarch64.cross -Dplatforms=android \
    -Dplatform-sdk-version="$API" -Dandroid-stub=true -Dandroid-libbacktrace=disabled \
    -Dvulkan-drivers=virtio -Dgallium-drivers= -Dglx=disabled -Degl=disabled -Dgbm=disabled \
    -Dopengl=false -Dgles1=disabled -Dgles2=disabled -Dllvm=disabled -Dvideo-codecs= \
    -Dxmlconfig=disabled -Dzstd=disabled -Dshader-cache=disabled \
    -Dallow-fallback-for=libdrm --force-fallback-for=libdrm -Dlibdrm:default_library=static \
    "-Dcpp_link_args=['-static-libstdc++']" \
    -Dbuildtype=release >"$BUILD/meson-setup.log" 2>&1 || { tail -30 "$BUILD/meson-setup.log"; exit 1; }
ninja -C build src/virtio/vulkan/libvulkan_virtio.so >"$BUILD/ninja.log" 2>&1 ||
    { grep -m5 -B2 -A8 'error:' "$BUILD/ninja.log"; exit 1; }
mkdir -p "$PREFIX/lib64/hw"
"$TOOLCHAIN/llvm-strip" -o "$PREFIX/lib64/hw/vulkan.virtio.so" \
    build/src/virtio/vulkan/libvulkan_virtio.so
ls -la "$PREFIX/lib64/hw/vulkan.virtio.so"
