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
    # ttcd carries it: without Go's symbol table and DWARF, it is smaller.
    gc_linkopts = [
        "-s",
        "-w",
    ],
    # The renderer calls Vulkan, which it loads with dlopen, through cgo, on
    # every platform: a pure build of it renders nothing.
    pure = "off",
    visibility = ["//visibility:public"],
    deps = [
        "//runsc/ttc/gfxrenderer",
        "//runsc/ttc/glibccompat",
    ],
)
