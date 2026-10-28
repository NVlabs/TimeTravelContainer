# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "select_system")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "testutil",
    testonly = 1,
    # Nothing needs to be stateified, and stateify has trouble when select is
    # used to choose deps.
    stateify = False,
    srcs = [
        "kernel.go",
        "kernel_darwin.go",
        "kernel_linux.go",
        "testutil.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil",
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/cpuid",
        "//pkg/fspath",
        "//pkg/hostarch",
        "//pkg/memutil",
        "//pkg/sentry/fsimpl/cgroup2fs",
        "//pkg/sentry/fsimpl/tmpfs",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/kernel/sched",
        "//pkg/sentry/limits",
        "//pkg/sentry/loader",
        "//pkg/sentry/mm",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/platformdesc",
        "//pkg/sentry/seccheck",
        "//pkg/sentry/time",
        "//pkg/sentry/usage",
        "//pkg/sentry/vfs",
        "//pkg/sync",
        "//pkg/usermem",
        "@com_github_google_go_cmp//cmp:go_default_library",
    ] + select_system(
        darwin = ["//pkg/sentry/platform/platformtest"],
        linux = [
            "//pkg/sentry/platform/kvm",
            "//pkg/sentry/platform/ptrace",
        ],
    ),
)
