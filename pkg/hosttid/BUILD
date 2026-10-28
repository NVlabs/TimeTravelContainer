# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hosttid",
    srcs = [
        "hosttid.go",
        "hosttid_amd64.tmpl.s",
        "hosttid_arm64.tmpl.s",
    ],
    visibility = ["//visibility:public"],
)

go_test(
    name = "hosttid_test",
    size = "small",
    srcs = [
        "hosttid_exact_test.go",
        "hosttid_inexact_test.go",
        "hosttid_test.go",
    ],
    embed = [":hosttid"],
    deps = [
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "hosttid_net_test",
    size = "small",
    srcs = [
        "hosttid_exact_test.go",
        "hosttid_inexact_test.go",
        "hosttid_net_test.go",
        "hosttid_test.go",
    ],
    embed = [":hosttid"],
    deps = [
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
