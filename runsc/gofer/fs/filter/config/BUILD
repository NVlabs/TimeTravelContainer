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
        "config.go",
        "config_amd64.go",
        "config_arm64.go",
        "config_main.go",
        "config_precompiled.go",
    ],
    importpath = "gvisor.dev/gvisor/runsc/gofer/fs/filter/config",
    visibility = [
        "//runsc/gofer/fs/filter:__subpackages__",
    ],
    deps = [
        "//pkg/abi/linux",
        "//pkg/seccomp",
        "//pkg/seccomp/precompiledseccomp",
        "//pkg/sync",
        "//runsc/gofer/filter",
        "@org_golang_x_sync//errgroup:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
    # The gofer's seccomp rules: macOS installs no filter.
    target_compatible_with = ["@platforms//os:linux"],
)

go_test(
    name = "config_test",
    size = "small",
    srcs = [
        "clone_test.go",
        "config_test.go",
    ],
    embed = [":config"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/bpf",
        "//pkg/seccomp",
        "//pkg/seccomp/precompiledseccomp",
        "@org_golang_x_sys//unix:go_default_library",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)
