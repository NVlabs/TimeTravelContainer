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
        "backend_linux.go",
        "clone_unsafe.go",
        "context_unsafe.go",
        "formats.c",
        "pipe.c",
        "protocol.go",
        "screen.c",
        "screen_darwin.c",
        "tgsi_check.c",
        "virgl.go",
        "virgl.h",
        "virgl_int.h",
        "virgl_other.go",
        "virgl_unsafe.go",
        "vkgl_unsafe.go",
        "zink_unsafe.go",
    ],
    # The renderer renders with vkgl, of no Mesa, and on macOS links Zink
    # too (see tools/mesa/README.md), on the MoltenVK that the Venus
    # renderer links, until vkgl is verified on MoltenVK. Zink's C files,
    # and zink_unsafe.go, are of macOS's builds only.
    bazel_cdeps = select({
        "@platforms//os:macos": [
            "@mesa//:gallium_api",
            "@mesa//:zink",
            "@moltenvk",
        ],
        "//conditions:default": [],
    }),
    bazel_cgo = True,
    stateify = False,
    visibility = ["//runsc/ttc/gfxrenderer:__subpackages__"],
    deps = [
        "//runsc/ttc/gfxrenderer/virgl/tgsi",
        "//runsc/ttc/gfxrenderer/virgl/tospirv",
        "//runsc/ttc/gfxrenderer/virgl/vkgl",
    ],
)

go_test(
    name = "virgl_test",
    size = "small",
    srcs = ["virgl_test.go"],
    library = ":virgl",
    # The tests render with the renderer's backend, through cgo, and skip
    # where it finds no Vulkan device.
    pure = "off",
)
