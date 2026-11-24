# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "hostfd",
    srcs = [
        "hostfd.go",
        "hostfd_darwin.go",
        "hostfd_linux.go",
        "hostfd_unsafe.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/sentry/hostfd",
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/safemem",
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "hostfd_test",
    size = "small",
    srcs = ["hostfd_test.go"],
    embed = [":hostfd"],
    deps = [
        "@org_golang_x_sys//unix:go_default_library",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)
