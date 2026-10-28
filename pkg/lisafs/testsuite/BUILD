# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    default_visibility = ["//visibility:public"],
    licenses = ["notice"],
)

go_library(
    name = "testsuite",
    testonly = True,
    srcs = ["testsuite.go"],
    importpath = "gvisor.dev/gvisor/pkg/lisafs/testsuite",
    deps = [
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/errors/linuxerr",
        "//pkg/lisafs",
        "//pkg/rand",
        "//pkg/refs",
        "//pkg/unet",
        "@com_github_moby_sys_capability//:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
