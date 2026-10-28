# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//pkg/sync/locking:locking.bzl", "declare_rwmutex")
load("//tools:defs.bzl", "go_library", "go_test", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

declare_rwmutex(
    name = "endpoint_mutex",
    out = "endpoint_mutex.go",
    package = "xdp",
    prefix = "endpoint",
)

go_library(
    name = "xdp",
    srcs = [
        "endpoint.go",
        "endpoint_darwin.go",
        "endpoint_mutex.go",
    ],
    force_add_state_pkg = True,
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/buffer",
        "//pkg/rawfile",
        "//pkg/sync",
        "//pkg/sync/locking",
        "//pkg/tcpip",
        "//pkg/tcpip/header",
        "//pkg/tcpip/link/qdisc/fifo",
        "//pkg/tcpip/link/stopfd",
        "//pkg/tcpip/stack",
        "@org_golang_x_sys//unix:go_default_library",
    ] + select_system(linux = ["//pkg/xdp"]),
)

go_test(
    name = "xdp_test",
    srcs = ["endpoint_test.go"],
    embed = [":xdp"],
    deps = ["//pkg/tcpip"],
    target_compatible_with = ["@platforms//os:linux"],
)
