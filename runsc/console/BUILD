# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "console",
    srcs = [
        "console.go",
        "pty_darwin.go",
        "pty_linux.go",
    ],
    importpath = "gvisor.dev/gvisor/runsc/console",
    visibility = [
        "//runsc:__subpackages__",
    ],
    deps = [
        "@com_github_creack_pty//:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
