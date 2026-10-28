# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")
load("//tools/go_generics:defs.bzl", "go_template_instance")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_template_instance(
    name = "waiter_list",
    out = "waiter_list.go",
    package = "waiter",
    prefix = "waiter",
    template = "//pkg/ilist:generic_list",
    types = {
        "Element": "*Entry",
        "Linker": "*Entry",
    },
)

go_library(
    name = "waiter",
    srcs = [
        "waiter.go",
        "waiter_list.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/state",
        "//pkg/sync",
    ],
)

go_test(
    name = "waiter_test",
    size = "small",
    srcs = [
        "waiter_test.go",
    ],
    embed = [":waiter"],
    deps = [
        "//pkg/atomicbitops",
        "//pkg/state",
    ],
)
