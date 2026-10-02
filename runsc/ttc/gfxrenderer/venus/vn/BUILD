# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "vn",
    # Most of the package is generated: see ../gen/venusgen.py.
    srcs = glob(
        ["*.go"],
        exclude = ["*_test.go"],
    ),
    stateify = False,
    visibility = ["//runsc/ttc/gfxrenderer:__subpackages__"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
