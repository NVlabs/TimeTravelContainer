# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "checkpoint",
    srcs = ["checkpoint.go"],
    visibility = ["//visibility:public"],
    deps = [
        "//runsc/ttc/bundle",
        "@com_github_opencontainers_runtime_spec//specs-go:go_default_library",
    ],
)

go_test(
    name = "checkpoint_test",
    size = "small",
    srcs = ["checkpoint_test.go"],
    library = ":checkpoint",
)
