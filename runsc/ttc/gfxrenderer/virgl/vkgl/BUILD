# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "vkgl",
    srcs = glob(
        ["*.go"],
        exclude = ["*_test.go"],
    ),
    bazel_cgo = True,
    stateify = False,
    visibility = ["//runsc/ttc/gfxrenderer:__subpackages__"],
    deps = [
        "//runsc/ttc/gfxrenderer/venus/vk",
        "//runsc/ttc/gfxrenderer/venus/vn",
        "//runsc/ttc/gfxrenderer/virgl/tgsi",
        "//runsc/ttc/gfxrenderer/virgl/tospirv",
    ],
)

go_test(
    name = "vkgl_test",
    size = "small",
    srcs = glob(["*_test.go"]),
    library = ":vkgl",
    # The tests render with Vulkan, through cgo, and skip where it has no
    # device.
    pure = "off",
    deps = [
        "//runsc/ttc/gfxrenderer/virgl/tgsi",
        "//runsc/ttc/gfxrenderer/virgl/tospirv",
    ],
)
