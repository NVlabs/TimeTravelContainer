# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "fdbudget",
    srcs = ["fdbudget.go"],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/atomicbitops",
        "//pkg/log",
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "fdbudget_test",
    size = "small",
    srcs = ["fdbudget_test.go"],
    library = ":fdbudget",
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
