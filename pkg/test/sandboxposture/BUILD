# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "sandboxposture",
    testonly = 1,
    srcs = [
        "expected.go",
        "sandboxposture.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/test/sandboxposture",
    visibility = ["//:sandbox"],
    deps = [
        "@com_github_google_go_cmp//cmp:go_default_library",
        "@com_github_google_go_cmp//cmp/cmpopts:go_default_library",
        "@com_github_moby_sys_capability//:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)
