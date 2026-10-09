# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("@rules_cc//cc:defs.bzl", "cc_library")
load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

# Vulkan's headers, of venus-protocol's vk.xml, and the declarations that
# venus-protocol generates for renderers. See ../gen/venusgen.py.
cc_library(
    name = "headers",
    hdrs = glob(["include/**/*.h"]) + [
        "hostmem.h",
        "metal.h",
        "vn_protocol_renderer_cs.h",
        "vn_protocol_renderer_defines.h",
        "vn_protocol_renderer_info.h",
        "vn_protocol_renderer_util.h",
        "zcall.h",
    ],
    defines = [
        # The renderer calls Vulkan through procedure tables.
        "VK_NO_PROTOTYPES",
        "VK_USE_PLATFORM_METAL_EXT",
    ],
    includes = [
        ".",
        "include",
    ],
)

go_library(
    name = "vk",
    srcs = [
        "doc.go",
        "hostcopy_unsafe.go",
        "hostmem.c",
        "hostmem_unsafe.go",
        "linked_darwin.go",
        "linked_other.go",
        "metal.c",
        "metal_unsafe.go",
        "shm.go",
        "vk_unsafe.go",
        "zcall.c",
    ],
    # On macOS the renderer links MoltenVK (see Linked).
    bazel_cdeps = [":headers"] + select({
        "@platforms//os:macos": ["@moltenvk"],
        "//conditions:default": [],
    }),
    bazel_cgo = True,
    stateify = False,
    visibility = ["//runsc/ttc/gfxrenderer:__subpackages__"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
