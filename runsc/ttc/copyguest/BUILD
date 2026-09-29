# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "copyguest",
    srcs = [
        "copyguest.go",
        "guest_darwin_arm64.go",
        "guest_other.go",
    ],
    embedsrcs = select_system(
        darwin = ["//runsc/ttc/copyguest/cmd:copy_helper_linux_arm64"],
        linux = [],
    ),
    visibility = [
        "//runsc/ttc:__pkg__",
        "//runsc/ttcd:__pkg__",
    ],
)
