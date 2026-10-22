# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "tgsi",
    srcs = glob(
        ["*.go"],
        exclude = ["*_test.go"],
    ),
    stateify = False,
    visibility = ["//runsc/ttc/gfxrenderer:__subpackages__"],
)

go_test(
    name = "tgsi_test",
    size = "small",
    srcs = ["tgsi_test.go"],
    library = ":tgsi",
)
