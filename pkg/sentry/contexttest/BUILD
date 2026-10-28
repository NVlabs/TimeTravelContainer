# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "contexttest",
    testonly = 1,
    # Nothing needs to be stateified, and stateify has trouble when select is
    # used to choose deps.
    stateify = False,
    srcs = [
        "contexttest.go",
        "contexttest_darwin.go",
        "contexttest_linux.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/sentry/contexttest",
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/atomicbitops",
        "//pkg/context",
        "//pkg/memutil",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/ktime",
        "//pkg/sentry/limits",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/platform",
        "//pkg/sentry/uniqueid",
    ] + select_system(
        darwin = ["//pkg/sentry/platform/platformtest"],
        linux = ["//pkg/sentry/platform/ptrace"],
    ),
)
