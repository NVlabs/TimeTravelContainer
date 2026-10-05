# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_test(
    name = "ttcpack_test",
    size = "small",
    srcs = ["pack_test.go"],
    library = ":ttcpack",
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_library(
    name = "ttcpack",
    srcs = [
        "lazy.go",
        "lazy_linux.go",
        "lazy_other.go",
        "pack.go",
    ],
    visibility = ["//visibility:public"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
