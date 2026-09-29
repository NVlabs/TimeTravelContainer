# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcchunk",
    srcs = [
        "chunk.go",
        "graph.go",
        "graph_stream.go",
        "install.go",
        "manifest.go",
        "packed_reader.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/hostarch",
        "//pkg/sentry/checkpoint",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/state/stateio",
        "//pkg/sentry/usage",
        "//pkg/ttcsnapshot",
    ],
)

go_test(
    name = "ttcchunk_test",
    size = "small",
    srcs = [
        "chunk_test.go",
        "graph_test.go",
    ],
    library = ":ttcchunk",
    deps = [
        "//pkg/hostarch",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/ttcsnapshot",
    ],
)
