# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "memutil",
    srcs = [
        "memfd_darwin.go",
        "memfd_linux_unsafe.go",
        "memutil_unsafe.go",
        "mmap.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/memutil",
    visibility = ["//visibility:public"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
