# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ebpf",
    srcs = [
        "ebpf.go",
        "map.go",
        "registry.go",
        "types.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/ebpf",
    imports = ["gvisor.dev/gvisor/pkg/abi/linux"],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/errors/linuxerr",
        "//pkg/sync",
    ],
)

go_test(
    name = "ebpf_test",
    size = "small",
    srcs = ["map_test.go"],
    library = ":ebpf",
    deps = [
        "//pkg/abi/linux",
        "//pkg/errors/linuxerr",
    ],
)
