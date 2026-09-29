# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hostmem",
    srcs = ["hostmem.go"],
    visibility = ["//:sandbox"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "hostmem_test",
    size = "small",
    srcs = ["hostmem_test.go"],
    library = ":hostmem",
)
