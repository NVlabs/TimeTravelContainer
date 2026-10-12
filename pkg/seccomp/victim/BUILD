# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "victim",
    testonly = 1,
    srcs = [
        "seccomp_test_victim.go",
        "seccomp_test_victim_amd64.go",
        "seccomp_test_victim_arm64.go",
    ],
    nogo = False,
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/seccomp",
        "@org_golang_x_sys//unix:go_default_library",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)
