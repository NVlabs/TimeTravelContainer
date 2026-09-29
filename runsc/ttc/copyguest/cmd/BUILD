# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_binary")
load("@io_bazel_rules_go//go:def.bzl", "go_cross_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "copy_helper_guest",
    srcs = ["main.go"],
    pure = True,
    deps = ["//runsc/ttc/copyhelper"],
)

go_cross_binary(
    name = "copy_helper_linux_arm64",
    target = ":copy_helper_guest",
    platform = "@io_bazel_rules_go//go/toolchain:linux_arm64",
    visibility = ["//runsc/ttc/copyguest:__pkg__"],
)
