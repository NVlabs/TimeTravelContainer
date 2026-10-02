# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_binary", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "ttc-gfx-renderer",
    srcs = ["main.go"],
    # The renderer calls Vulkan, which it loads with dlopen, through cgo.
    # Like ttcd's, Linux configurations keep the repository's default.
    pure = select_system(darwin = "off", linux = "auto"),
    visibility = ["//visibility:public"],
    deps = ["//runsc/ttc/gfxrenderer"],
)
