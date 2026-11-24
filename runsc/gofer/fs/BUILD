# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "fs",
    srcs = [
        "budget.go",
        "host_ops_darwin.go",
        "host_ops_linux.go",
        "lisafs.go",
    ],
    visibility = ["//runsc:__subpackages__"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/cleanup",
        "//pkg/fd",
        "//pkg/fdbudget",
        "//pkg/fsutil",
        "//pkg/lisafs",
        "//pkg/log",
        "//pkg/marshal/primitive",
        "//runsc/config",
        "//runsc/ttc/ownership",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "lisafs_test",
    size = "small",
    srcs = ["lisafs_test.go"],
    deps = [
        ":fs",
        "//pkg/lisafs",
        "//pkg/lisafs/testsuite",
        "//pkg/log",
    ],
)

go_test(
    name = "budget_test",
    size = "small",
    srcs = ["budget_test.go"],
    deps = [
        ":fs",
        "//pkg/context",
        "//pkg/errors/linuxerr",
        "//pkg/fdbudget",
        "//pkg/lisafs",
        "//pkg/lisafs/testsuite",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "budget_bench_test",
    size = "small",
    srcs = ["budget_bench_test.go"],
    library = ":fs",
    deps = [
        "//pkg/fdbudget",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "lend_test",
    size = "small",
    srcs = ["lend_test.go"],
    library = ":fs",
    deps = [
        "//pkg/abi/linux",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "virtual_ownership_test",
    size = "small",
    srcs = ["virtual_ownership_test.go"],
    library = ":fs",
    deps = [
        "//runsc/ttc/ownership",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "trusted_ownership_test",
    size = "small",
    srcs = ["trusted_ownership_test.go"],
    library = ":fs",
    deps = ["//pkg/lisafs"],
)

go_test(
    name = "groups_test",
    size = "small",
    srcs = ["groups_test.go"],
    embed = [":fs"],
    deps = [
        "//pkg/lisafs",
        "@org_golang_x_sys//unix:go_default_library",
    ],
    target_compatible_with = ["@platforms//os:linux"],
)
