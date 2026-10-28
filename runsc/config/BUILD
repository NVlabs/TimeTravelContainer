# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "config",
    srcs = [
        "cgo_disabled.go",
        "cgo_enabled.go",
        "config.go",
        "config_bundles.go",
        "config_host_darwin.go",
        "config_host_linux.go",
        "flags.go",
        "flags_graveyard.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/log",
        "//pkg/refs",
        "//pkg/sentry/devices/nvproxy/nvconf",
        "//pkg/sync",
        "//runsc/flag",
        "//runsc/version",
    ],
)

go_test(
    name = "config_test",
    size = "small",
    srcs = [
        "config_host_darwin_test.go",
        "config_test.go",
        "flags_test.go",
    ],
    library = ":config",
    deps = [
        "//runsc/flag",
        "@com_github_google_go_cmp//cmp:go_default_library",
    ],
)
