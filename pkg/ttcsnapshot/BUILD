# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcsnapshot",
    srcs = [
        "btree.go",
		"catalog.go",
        "data.go",
		"deferred.go",
        "filesystem.go",
        "filesystem_json.go",
        "graph_binary.go",
        "host_ops_darwin.go",
        "host_ops_linux.go",
		"hosttree.go",
        "json.go",
		"ledger.go",
        "legacy.go",
        "memory.go",
        "pack.go",
        "pack_binary.go",
        "pack_json.go",
		"relay.go",
        "resolver.go",
        "store.go",
        "types.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/hostthread",
        "@org_golang_x_sync//errgroup:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "ttcsnapshot_test",
    size = "small",
    srcs = [
		"catalog_test.go",
		"deferred_test.go",
		"filesystem_json_test.go",
        "graph_binary_test.go",
		"hosttree_test.go",
		"ledger_test.go",
		"memory_bench_test.go",
		"names_test.go",
        "pack_binary_test.go",
		"pack_json_test.go",
		"pack_test.go",
		"relay_test.go",
        "resolver_test.go",
        "store_test.go",
    ],
    library = ":ttcsnapshot",
)
