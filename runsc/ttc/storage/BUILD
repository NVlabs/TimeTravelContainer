# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_test(
    name = "storage_test",
    size = "small",
    srcs = ["queue_test.go"],
    library = ":storage",
)

go_library(
    name = "storage",
    srcs = ["queue.go"],
    visibility = ["//runsc/ttc:__subpackages__"],
)
