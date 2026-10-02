# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "pflate",
    srcs = [
        "pflate.go",
        "reader.go",
    ],
    visibility = ["//runsc:__subpackages__"],
    deps = ["@com_github_klauspost_compress//flate:go_default_library"],
)

go_test(
    name = "pflate_test",
    size = "small",
    srcs = ["pflate_test.go"],
    library = ":pflate",
)
