# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "machexc",
    srcs = ["machexc_darwin.go"],
    bazel_cgo = True,
    stateify = False,
    visibility = ["//pkg/safecopy:__pkg__"],
)
