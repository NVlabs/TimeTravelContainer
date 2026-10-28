# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "eventfd",
    srcs = [
        "eventfd.go",
        "eventfd_darwin.go",
        "eventfd_linux.go",
        "eventfd_mmio_darwin.go",
        "eventfd_mmio_linux_unsafe.go",
        "eventfd_unsafe.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/eventfd",
    visibility = [
        "//:sandbox",
        "//cloud/cluster/node/network/client/go:__pkg__",
    ],
    deps = [
        "//pkg/atomicbitops",
        "//pkg/hostarch",
        "//pkg/rawfile",
        "//pkg/safecopy",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "eventfd_test",
    srcs = ["eventfd_test.go"],
    embed = [":eventfd"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
