# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    default_visibility = ["//visibility:public"],
    licenses = ["notice"],
)

go_library(
    name = "p9",
    srcs = [
        "host_stat_darwin.go",
        "host_stat_linux.go",
        "p9.go",
    ],
    deps = [
        "//pkg/abi/linux",
    ],
)

go_test(
    name = "p9_test",
    size = "small",
    srcs = ["p9_test.go"],
    embed = [":p9"],
)
