# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")
load("//tools/go_generics:defs.bzl", "go_template_instance")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_template_instance(
    name = "inode_refs",
    out = "inode_refs.go",
    package = "host",
    prefix = "inode",
    template = "//pkg/refs:refs_template",
    types = {
        "T": "inode",
    },
)

go_library(
    name = "host",
    srcs = [
        "host.go",
        "host_unsafe.go",
        "inode_refs.go",
        "ioctl_unsafe.go",
        "platform_darwin.go",
        "platform_linux.go",
        "save_restore.go",
        "tty.go",
        "util.go",
        "util_unsafe.go",
        "util_unsafe_darwin.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/context",
        "//pkg/errors/linuxerr",
        "//pkg/fdnotifier",
        "//pkg/fspath",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/marshal/primitive",
        "//pkg/refs",
        "//pkg/safemem",
        "//pkg/sentry/arch",
        "//pkg/sentry/checkpoint",
        "//pkg/sentry/fsimpl/eventfd",
        "//pkg/sentry/fsimpl/kernfs",
        "//pkg/sentry/fsimpl/lock",
        "//pkg/sentry/fsutil",
        "//pkg/sentry/hostfd",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/memmap",
        "//pkg/sentry/socket/control",
        "//pkg/sentry/socket/unix",
        "//pkg/sentry/socket/unix/transport",
        "//pkg/sentry/unimpl",
        "//pkg/sentry/uniqueid",
        "//pkg/sentry/vfs",
        "//pkg/sync",
        "//pkg/syserr",
        "//pkg/tcpip",
        "//pkg/unet",
        "//pkg/usermem",
        "//pkg/waiter",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "host_test",
    size = "small",
    srcs = ["util_test.go"],
    library = ":host",
    deps = [
        "//pkg/errors/linuxerr",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
