# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "xdp",
    srcs = [
        "completionqueue.go",
        "fillqueue.go",
        "pin.go",
        "rxqueue.go",
        "txqueue.go",
        "umem.go",
        "xdp.go",
        "xdp_unsafe.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/atomicbitops",
        "//pkg/cleanup",
        "//pkg/log",
        "//pkg/memutil",
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)
