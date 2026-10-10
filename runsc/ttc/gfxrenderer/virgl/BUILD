# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "virgl",
    srcs = [
        "context_unsafe.go",
        "formats_darwin.c",
        "pipe_darwin.c",
        "protocol.go",
        "screen_darwin.c",
        "virgl.go",
        "virgl.h",
        "virgl_int.h",
        "virgl_other.go",
        "virgl_unsafe.go",
    ],
    # On macOS the renderer links Zink (see tools/mesa/README.md), on the
    # MoltenVK that the Venus renderer links.
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
)

go_test(
    name = "virgl_test",
    size = "small",
    srcs = ["virgl_darwin_test.go"],
    library = ":virgl",
    # The tests render with Zink, through cgo, as the renderer does.
    pure = select_system(
        darwin = "off",
        linux = "auto",
    ),
)
