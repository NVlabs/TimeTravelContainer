# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "coretag",
    srcs = [
        "coretag.go",
        "coretag_darwin.go",
        "coretag_unsafe.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/abi/linux",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "coretag_test",
    size = "small",
    srcs = [
        "coretag_test.go",
    ],
    embed = [":coretag"],
    deps = [
        "//pkg/hostos",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)
