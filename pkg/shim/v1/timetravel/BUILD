# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "timetravel",
    srcs = ["service.go"],
    visibility = ["//visibility:public"],
    deps = [
        "@com_github_containerd_ttrpc//:go_default_library",
        "@org_golang_google_protobuf//types/known/anypb:go_default_library",
    ],
)

go_test(
    name = "timetravel_test",
    srcs = ["service_test.go"],
    library = ":timetravel",
    deps = [
        "@com_github_containerd_ttrpc//:go_default_library",
        "@org_golang_google_protobuf//types/known/anypb:go_default_library",
    ],
)
