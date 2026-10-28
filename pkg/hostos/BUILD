# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hostos",
    srcs = [
        "hostos.go",
        "memory_darwin.go",
        "memory_linux.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/hostos",
    visibility = ["//:sandbox"],
    deps = [
        "@org_golang_x_mod//semver:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "hostos_test",
    srcs = ["hostos_test.go"],
    embed = [":hostos"],
)
