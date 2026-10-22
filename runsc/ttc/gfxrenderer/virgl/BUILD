# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "virgl",
    srcs = [
        "backend.go",
        "clone_unsafe.go",
        "context_unsafe.go",
        "formats.c",
        "pipe.c",
        "protocol.go",
        "screen.c",
        "screen_darwin.c",
        "screen_linux.c",
        "tgsi_check.c",
        "virgl.go",
        "virgl.h",
        "virgl_int.h",
        "virgl_other.go",
        "virgl_unsafe.go",
        "zink_unsafe.go",
    ],
    # The renderer links Zink (see tools/mesa/README.md): on macOS, on the
    # MoltenVK that the Venus renderer links; on Linux, on the host's Vulkan
    # loader, which it loads.
    bazel_cdeps = select({
        "@platforms//os:macos": [
            "@mesa//:gallium_api",
            "@mesa//:zink",
            "@moltenvk",
        ],
        "@platforms//os:linux": [
            "@mesa//:gallium_api",
            "@mesa//:zink",
        ],
        "//conditions:default": [],
    }),
    bazel_cgo = True,
    stateify = False,
    visibility = ["//runsc/ttc/gfxrenderer:__subpackages__"],
)

go_test(
    name = "virgl_test",
    size = "small",
    srcs = ["virgl_test.go"],
    library = ":virgl",
    # The tests render with Zink, through cgo, as the renderer does, and skip
    # where Zink finds no Vulkan device.
    pure = "off",
)
