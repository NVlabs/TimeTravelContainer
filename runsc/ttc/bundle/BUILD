# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "bundle",
    srcs = [
        "clone_darwin.go",
        "clone_linux.go",
        "gpu.go",
        "spec.go",
        "store.go",
    ],
    visibility = ["//runsc/ttc:__subpackages__"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/log",
        "//pkg/ttcpack",
        "//pkg/ttcsnapshot",
        "//pkg/ttcvolume",
        "//runsc/config",
        "//runsc/specutils",
        "//runsc/ttc/console",
        "//runsc/ttc/images",
        "//runsc/ttc/ownership",
        "@com_github_docker_go_units//:go_default_library",
        "@com_github_moby_sys_signal//:go_default_library",
        "@com_github_moby_sys_user//:user",
        "@com_github_opencontainers_image_spec//specs-go/v1:specs-go",
        "@com_github_opencontainers_runtime_spec//specs-go",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "bundle_test",
    size = "small",
    srcs = [
        "clone_test.go",
        "gpu_test.go",
        "spec_test.go",
        "store_test.go",
    ],
    library = ":bundle",
    deps = [
        "//pkg/ttcpack",
        "//pkg/ttcvolume",
        "//runsc/specutils",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
