# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "console",
    srcs = [
        "follower.go",
        "follower_darwin.go",
        "follower_linux.go",
        "frame.go",
        "rotating.go",
        "status.go",
    ],
    visibility = ["//runsc:__subpackages__"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "console_test",
    size = "small",
    srcs = [
        "follower_test.go",
        "frame_test.go",
        "rotating_test.go",
        "status_test.go",
    ],
    library = ":console",
)
