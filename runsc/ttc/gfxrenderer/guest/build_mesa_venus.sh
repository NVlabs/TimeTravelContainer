#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds Mesa's Venus Vulkan driver (virtio), the guest side of --gfx, into
# $PREFIX (default /out). Run it in a Debian container of the guest's
# architecture, for example:
#
#   ttc run -d --name mesabuild --network ttc --nat -v $PWD:/work:ro debian:sid sleep infinity
#   ttc exec mesabuild@main sh /work/build_mesa_venus.sh
#   ttc cp mesabuild@main:/out - | tar -x
#
# It installs lib/<triple>/libvulkan_virtio.so and a Vulkan ICD manifest
# under share/vulkan/icd.d, with TTC's patches (patches/, beside this
# script) applied. The driver links libdrm and the C++ runtime
# statically, and needs no expat, so it needs only the C library in the
# guest.
set -eu
PATCHES=$(cd "$(dirname "$0")" && pwd)/patches
PREFIX=${PREFIX:-/out}
# The commit that the virtual GPU was developed against, and the version of
# the Venus protocol that it pins.
MESA_COMMIT=${MESA_COMMIT:-49162a0ba8f3647917bc4c94d13eea6b51a1e49f}
VENUS_PROTOCOL_TAG=${VENUS_PROTOCOL_TAG:-v1.1.3}
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq git meson ninja-build pkg-config python3-mako python3-yaml \
    glslang-tools bison flex g++ libdrm-dev libvulkan-dev libexpat1-dev zlib1g-dev \
    libzstd-dev >/dev/null
BUILD=${BUILD:-/build}
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
for patch in "$PATCHES"/*.patch; do
    git -C mesa apply --reverse --check "$patch" 2>/dev/null || git -C mesa apply "$patch"
done
cd mesa
meson setup build -Dprefix="$PREFIX" -Dvulkan-drivers=virtio -Dgallium-drivers= \
    -Dplatforms= -Dglx=disabled -Degl=disabled -Dgbm=disabled -Dopengl=false \
    -Dgles1=disabled -Dgles2=disabled -Dllvm=disabled -Dvideo-codecs= \
    -Dxmlconfig=disabled -Dallow-fallback-for=libdrm --force-fallback-for=libdrm \
    -Dlibdrm:default_library=static \
    "-Dcpp_link_args=['-static-libstdc++', '-static-libgcc']" \
    -Dbuildtype=release >"$BUILD/meson-setup.log" 2>&1 || { tail -30 "$BUILD/meson-setup.log"; exit 1; }
ninja -C build >"$BUILD/ninja.log" 2>&1 || { grep -m5 -B2 -A8 'error:' "$BUILD/ninja.log"; exit 1; }
ninja -C build install >/dev/null
ls -la "$PREFIX"/lib/*/libvulkan_virtio.so* "$PREFIX"/share/vulkan/icd.d/
