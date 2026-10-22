# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "tospirv",
    srcs = glob(
        ["*.go"],
        exclude = ["*_test.go"],
    ),
    stateify = False,
    visibility = ["//runsc/ttc/gfxrenderer:__subpackages__"],
    deps = [
        "//runsc/ttc/gfxrenderer/virgl/spirv",
        "//runsc/ttc/gfxrenderer/virgl/tgsi",
    ],
)

go_test(
    name = "tospirv_test",
    size = "small",
    srcs = ["tospirv_test.go"],
    library = ":tospirv",
    deps = [
        "//runsc/ttc/gfxrenderer/virgl/spirv",
        "//runsc/ttc/gfxrenderer/virgl/tgsi",
    ],
)
