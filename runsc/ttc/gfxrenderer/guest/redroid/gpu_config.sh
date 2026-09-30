#!/system/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# redroid's /vendor/bin/gpu_config.sh, which its init runs at boot to pick
# its graphics stack, for TTC's virtual GPU (--gfx): the image's minigbm
# allocates buffers on the GPU's virtio-gpu render node, gfxstream's Vulkan
# driver (build_mesa_gfxstream_android.sh), which /vendor/lib64/hw/
# vulkan.gfxstream.so must be, drives it, and the image's ANGLE implements
# OpenGL ES on the driver.
echo "use TTC's virtual GPU"
chmod 666 /dev/dri/renderD128
setprop ro.hardware.gralloc cros
setprop ro.hardware.vulkan gfxstream
setprop ro.hardware.egl angle
