# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "gfxrenderer",
    srcs = [
        "gfxstream.go",
        "nocgo.go",
        "options.go",
        "renderer.go",
        "shm_darwin.go",
        "shm_linux.go",
    ],
    bazel_cgo = True,
    stateify = False,
    visibility = ["//runsc:__subpackages__"],
    deps = [
        "//pkg/gfxproto",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
