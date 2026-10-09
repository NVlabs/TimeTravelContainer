# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "client",
    srcs = [
        "client.go",
        "host_darwin.go",
        "host_linux.go",
        "terminal.go",
    ],
    visibility = ["//runsc/ttc:__pkg__"],
    deps = [
        "//runsc/ttc/protocol",
        "//runsc/version",
        "@org_golang_x_term//:go_default_library",
    ],
)

go_test(
    name = "client_test",
    size = "small",
    srcs = ["client_test.go"],
    library = ":client",
)
