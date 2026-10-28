# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "flipcall",
    srcs = [
        "ctrl_futex.go",
        "flipcall.go",
        "flipcall_unsafe.go",
        "futex_darwin.go",
        "futex_linux.go",
        "io.go",
        "packet_window.go",
        "packet_window_host_darwin.go",
        "packet_window_host_linux.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/flipcall",
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/log",
        "//pkg/memutil",
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "flipcall_test",
    size = "small",
    srcs = [
        "flipcall_example_test.go",
        "flipcall_test.go",
    ],
    embed = [":flipcall"],
    deps = ["//pkg/sync"],
)
