# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_binary", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_test(
    name = "safemount_test",
    size = "small",
    srcs = ["safemount_test.go"],
    data = [":safemount_runner"],
    deps = [
        "//pkg/test/testutil",
        "@org_golang_x_sys//unix:go_default_library",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)

go_binary(
    name = "safemount_runner",
    srcs = ["safemount_runner.go"],
    deps = [
        "//runsc/specutils",
        "@org_golang_x_sys//unix:go_default_library",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)
