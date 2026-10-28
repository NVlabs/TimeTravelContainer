# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "util",
    srcs = [
        "host_mmap_darwin.go",
        "host_mmap_linux.go",
        "ioctl_unsafe.go",
        "util.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/sentry/devices/tpuproxy/util",
    visibility = [
        "//pkg/sentry:internal",
    ],
    deps = [
        "//pkg/abi/linux",
        "@org_golang_x_exp//constraints:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
