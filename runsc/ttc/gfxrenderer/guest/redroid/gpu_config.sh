#!/system/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# redroid's /vendor/bin/gpu_config.sh, which its init runs at boot to pick
# its graphics stack, for TTC's virtual GPU (--gfx): the image's minigbm
# allocates buffers on the GPU's virtio-gpu render node, the renderer's
# Vulkan driver drives it, and the image's ANGLE implements OpenGL ES on the
# driver. The driver is Mesa's Venus driver (build_mesa_venus_android.sh),
# /vendor/lib64/hw/vulkan.virtio.so.
echo "use TTC's virtual GPU"
chmod 666 /dev/dri/renderD128
setprop ro.hardware.gralloc cros
setprop ro.hardware.vulkan virtio
setprop ro.hardware.egl angle

# Video encoding, which screen mirroring such as scrcpy's uses: redroid turns
# Codec2's software codecs off, since they allocate from a DMA-BUF heap,
# which TTC has. Without an OMX service, which the image lacks, encoders
# take their input surface in the client's process.
setprop debug.stagefright.ccodec 4
setprop debug.stagefright.c2inputsurface -1
