# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "gfxproto",
    srcs = ["gfxproto.go"],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/unet",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "gfxproto_test",
    size = "small",
    srcs = ["gfxproto_test.go"],
    library = ":gfxproto",
    deps = [
        "//pkg/unet",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
