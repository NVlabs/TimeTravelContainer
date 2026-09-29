# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "stress",
    testonly = 1,
    srcs = ["stress.go"],
    features = ["fully_static_link"],
    visibility = ["//visibility:public"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
    target_compatible_with = ["@platforms//os:linux"],
)

filegroup(
    name = "stress_source",
    srcs = ["stress.go"],
    visibility = ["//visibility:public"],
)
