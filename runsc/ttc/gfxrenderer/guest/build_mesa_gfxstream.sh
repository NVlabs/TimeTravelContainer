#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds Mesa's gfxstream Vulkan driver, the guest side of --gfx, into
# $PREFIX (default /out). Run it in a Debian container of the guest's
# architecture, for example:
#
#   ttc run -d --name mesabuild --network ttc --nat -v $PWD:/work:ro debian:sid sleep infinity
#   ttc exec mesabuild@main sh /work/build_mesa_gfxstream.sh
#   ttc cp mesabuild@main:/out - | tar -x
#
# It installs lib/<triple>/libvulkan_gfxstream.so and a Vulkan ICD manifest
# under share/vulkan/icd.d. The driver needs libdrm2, libexpat1, and
# libstdc++6 in the guest.
set -eu
PREFIX=${PREFIX:-/out}
# The commit that the virtual GPU was developed against.
MESA_COMMIT=${MESA_COMMIT:-49162a0ba8f3647917bc4c94d13eea6b51a1e49f}
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
cd mesa
meson setup build -Dprefix="$PREFIX" -Dvulkan-drivers=gfxstream -Dgallium-drivers= \
    -Dplatforms= -Dglx=disabled -Degl=disabled -Dgbm=disabled -Dopengl=false \
    -Dgles1=disabled -Dgles2=disabled -Dllvm=disabled -Dvideo-codecs= \
    -Dbuildtype=release >"$BUILD/meson-setup.log" 2>&1 || { tail -30 "$BUILD/meson-setup.log"; exit 1; }
ninja -C build >"$BUILD/ninja.log" 2>&1 || { grep -m5 -B2 -A8 'error:' "$BUILD/ninja.log"; exit 1; }
ninja -C build install >/dev/null
ls -la "$PREFIX"/lib/*/libvulkan_gfxstream.so* "$PREFIX"/share/vulkan/icd.d/
