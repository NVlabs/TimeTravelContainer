# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "cc_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

cc_binary(
    name = "server_cc",
    srcs = ["server.cc"],
    visibility = ["//:sandbox"],
    deps = [
        # any_cc_proto placeholder,
        "//pkg/sentry/seccheck/points:points_cc_proto",
        "@com_google_absl//absl/cleanup",
        "@com_google_absl//absl/strings",
        "@com_google_protobuf//:protobuf",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)
