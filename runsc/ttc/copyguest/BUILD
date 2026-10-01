# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "select_arch")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "copyguest",
    srcs = [
        "copyguest.go",
        "guest_amd64.go",
        "guest_arm64.go",
        "guest_other.go",
    ],
    # The guest's ISA is the host's.
    embedsrcs = select_arch(
        amd64 = ["//runsc/ttc/copyguest/cmd:copy_helper_linux_amd64"],
        arm64 = ["//runsc/ttc/copyguest/cmd:copy_helper_linux_arm64"],
        default = [],
    ),
    visibility = [
        "//runsc/ttc:__pkg__",
        "//runsc/ttcd:__pkg__",
    ],
)
