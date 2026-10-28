# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "sandboxsetup",
    srcs = [
        "caps.go",
        "caps_unsafe.go",
        "caps_unsafe_darwin.go",
        "cpu_dma_latency.go",
        "cpu_dma_latency_amd64.go",
        "cpu_dma_latency_arm64.go",
        "fdmappings.go",
        "flags.go",
        "fs.go",
        "fs_darwin.go",
        "fs_linux.go",
        "gofer_mount.go",
        "gofer_mount_darwin.go",
        "process.go",
        "process_darwin.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/cpuid",
        "//pkg/fd",
        "//pkg/log",
        "//pkg/marshal/primitive",
        "//pkg/timing",
        "//pkg/unet",
        "//runsc/boot",
        "//runsc/cmd/util",
        "//runsc/config",
        "//runsc/flag",
        "//runsc/specutils",
        "//runsc/starttime",
        "@com_github_moby_sys_capability//:go_default_library",
        "@com_github_opencontainers_runtime_spec//specs-go:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "sandboxsetup_test",
    size = "small",
    srcs = ["gofer_mount_test.go"],
    embed = [":sandboxsetup"],
)
