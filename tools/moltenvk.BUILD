# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# MoltenVK, Khronos' Vulkan on Metal, of its release for macOS: the GPU
# renderer, which ttcd is on macOS, links its static library, so that ttcd
# loads no library but Apple's.

load("@rules_cc//cc:defs.bzl", "cc_import", "cc_library")

package(default_visibility = ["//visibility:public"])

licenses(["notice"])

# MoltenVK's license, Apache 2.0, which a binary that links it carries.
exports_files(["LICENSE"])

# For arm64 and x86-64, in one object that exports only Vulkan's commands
# and MoltenVK's: SPIRV-Cross, SPIRV-Tools and cereal, which it holds, stay
# its own.
cc_import(
    name = "static_library",
    static_library = "MoltenVK/static/MoltenVK.xcframework/macos-arm64_x86_64/libMoltenVK.a",
)

cc_library(
    name = "moltenvk",
    # What it links of Apple's.
    linkopts = [
        "-framework AppKit",
        "-framework CoreGraphics",
        "-framework Foundation",
        "-framework IOKit",
        "-framework IOSurface",
        "-framework Metal",
        "-framework QuartzCore",
        "-lc++",
        "-lobjc",
    ],
    target_compatible_with = ["@platforms//os:macos"],
    deps = [":static_library"],
)

# MoltenVK's own headers, without its copy of Vulkan's, for Zink (@mesa).
cc_library(
    name = "mvk_headers",
    hdrs = glob(["MoltenVK/include/MoltenVK/*.h"]),
    strip_include_prefix = "MoltenVK/include",
    target_compatible_with = ["@platforms//os:macos"],
)
