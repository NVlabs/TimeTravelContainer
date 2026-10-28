# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "maincli",
    srcs = [
        "maincli.go",
    ],
    visibility = [
        "//:__pkg__",
        "//runsc:__pkg__",
        "//runsc/ttcd:__pkg__",
    ],
    deps = [
        "//runsc/cli",
        "//runsc/cmd",
        "//runsc/cmd/alias",
        "//runsc/cmd/nvproxy",
        "//runsc/cmd/profile",
        "//runsc/cmd/sentry/sentrycmd",
        "//runsc/cmd/trace",
        "//runsc/cmd/util",
        "@com_github_google_subcommands//:go_default_library",
    ],
)

go_test(
    name = "maincli_test",
    srcs = ["maincli_test.go"],
    embed = [":maincli"],
    deps = [
        "//runsc/cmd",
        "//runsc/flag",
        "@com_github_google_subcommands//:go_default_library",
    ],
)
