# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//pkg/seccomp/precompiledseccomp:defs.bzl", "precompiled_seccomp_rules")
load("//tools:defs.bzl", "go_library", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

precompiled_seccomp_rules(
    name = "filter_precompiled",
    out = "filter_precompiled.go",
    exclude_in_fastbuild = True,
    out_package_name = "filter",
    programs_to_compile_go_import = "gvisor.dev/gvisor/runsc/gofer/fs/filter/config",
    programs_to_compile_go_library = "//runsc/gofer/fs/filter/config",
)

# The system's part of the filter: on Linux, its precompiled programs; on
# macOS, which installs no seccomp filter, filter_darwin.go, which needs
# neither the gofer's rules, which build only for Linux, nor their programs.
filegroup(
    name = "filter_system",
    srcs = select_system(
        darwin = ["filter_darwin.go"],
        linux = [":filter_precompiled"],
    ),
)

go_library(
    name = "filter",
    srcs = [
        "filter.go",
        ":filter_system",
    ],
    # The dependencies differ by system.
    force_add_state_pkg = True,
    visibility = [
        "//runsc:__subpackages__",
    ],
    deps = [
        "//pkg/seccomp",
        "//pkg/seccomp/precompiledseccomp",
        "//pkg/sync",
    ] + select_system(
        darwin = [],
        linux = [
            "//runsc/gofer/filter",
            "//runsc/gofer/fs/filter/config",
        ],
    ),
)
