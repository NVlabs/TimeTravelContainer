# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hostmm",
    srcs = [
        "cgroup.go",
        "hostmm.go",
        "max_map_count.go",
        "membarrier.go",
        "membarrier_darwin.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/sentry/hostmm",
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/eventfd",
        "//pkg/hostarch",
        "//pkg/log",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
