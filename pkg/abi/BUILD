# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "abi",
    srcs = [
        "abi.go",
        "abi_darwin.go",
        "abi_linux.go",
        "flag.go",
    ],
    hostlayout = True,
    importpath = "gvisor.dev/gvisor/pkg/abi",
    visibility = ["//:sandbox"],
)
