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
        "protocol.go",
        "virgl.go",
        "virgl_other.go",
        "virgl_unsafe.go",
        "vkgl_unsafe.go",
    ],
    # The renderer renders with vkgl, of no Mesa, on the Vulkan that the
    # Venus renderer calls: package vkgl's cgo.
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
