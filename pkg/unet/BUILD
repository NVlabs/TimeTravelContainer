# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "unet",
    srcs = [
        "socketpair_darwin.go",
        "socketpair_linux.go",
        "unet.go",
        "unet_darwin.go",
        "unet_unsafe.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/atomicbitops",
        "//pkg/eventfd",
        "//pkg/hostthread",
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "unet_test",
    size = "small",
    srcs = [
        "fd_count_darwin_test.go",
        "fd_count_linux_test.go",
        "unet_darwin_test.go",
        "unet_test.go",
    ],
    embed = [":unet"],
    deps = [
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
