# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_binary", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "registry_fixture",
    testonly = 1,
    srcs = ["main.go"],
    visibility = ["//runsc/ttc/test:__pkg__"],
)

go_test(
    name = "registry_fixture_test",
    srcs = [
        "main.go",
        "main_test.go",
    ],
)
