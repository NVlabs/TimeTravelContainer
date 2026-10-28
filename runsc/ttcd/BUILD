# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_binary", "go_test", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "ttcd",
    srcs = [
        "bundle_darwin.go",
        "main.go",
    ],
    # Native macOS links Hypervisor.framework through the HVF backend. Linux
    # retains the repository's pure-Go default.
    pure = select_system(darwin = "off", linux = "auto"),
    tags = ["staging"],
    visibility = ["//visibility:public"],
    x_defs = {"gvisor.dev/gvisor/runsc/version.version": "{STABLE_VERSION}"},
    deps = [
        "//runsc/cli/maincli",
        "//runsc/cli/sentrycli",
        "//runsc/gvisorbinaries",
        "//runsc/processutil",
        "//runsc/ttc/cmd",
        "//runsc/ttc/copyguest",
        "//runsc/ttc/gfxrenderer",
        "//runsc/ttc/hostbin",
        "//runsc/version",
    ] + select_system(
        darwin = ["//runsc/ttcd/bundle"],
        linux = [],
    ),
)

go_test(
    name = "ttcd_test",
    size = "small",
    srcs = [
        "main.go",
        "main_test.go",
    ] + select_system(
        darwin = ["info_plist_darwin_test.go"],
        linux = [],
    ),
    data = select_system(
        darwin = [":ttcd"],
        linux = [],
    ),
    deps = [
        "//runsc/cli/maincli",
        "//runsc/cli/sentrycli",
        "//runsc/gvisorbinaries",
        "//runsc/processutil",
        "//runsc/ttc/cmd",
        "//runsc/ttc/copyguest",
        "//runsc/ttc/gfxrenderer",
        "//runsc/ttc/hostbin",
        "//runsc/version",
    ],
)
