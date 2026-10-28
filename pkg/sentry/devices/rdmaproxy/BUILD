# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "rdmaproxy",
    srcs = [
        "driver.go",
        "host_mmap_darwin.go",
        "host_mmap_linux.go",
        "ioctl.go",
        "ioctl_unsafe.go",
        "rdmaproxy.go",
        "schema.go",
        "seccomp_filter.go",
        "seccomp_filter_darwin.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/sentry/devices/rdmaproxy",
    visibility = [
        "//pkg/sentry:internal",
        "//runsc:__subpackages__",
    ],
    deps = [
        "//pkg/abi/ib",
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/cleanup",
        "//pkg/context",
        "//pkg/devutil",
        "//pkg/errors/linuxerr",
        "//pkg/fdnotifier",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/seccomp",
        "//pkg/sentry/arch",
        "//pkg/sentry/fsutil",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/memmap",
        "//pkg/sentry/mm",
        "//pkg/sentry/vfs",
        "//pkg/usermem",
        "//pkg/waiter",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
