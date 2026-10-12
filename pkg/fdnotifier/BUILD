# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "fdnotifier",
    srcs = [
        "fdnotifier_darwin.go",
        "fdnotifier.go",
        "poll_darwin.go",
        "poll_unsafe.go",
        "readiness_darwin.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/sync",
        "//pkg/waiter",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "fdnotifier_test",
    size = "small",
    srcs = ["fdnotifier_darwin_test.go"],
    library = ":fdnotifier",
    deps = [
        "//pkg/waiter",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
