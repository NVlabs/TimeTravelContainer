# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//pkg/sync/locking:locking.bzl", "declare_mutex")
load("//tools:defs.bzl", "go_library", "go_test", "proto_library")
load("//tools/go_generics:defs.bzl", "go_template_instance")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

declare_mutex(
    name = "amfls_mutex",
    out = "amfls_mutex.go",
    package = "pgalloc",
    prefix = "amfls",
)

declare_mutex(
    name = "apfl_mutex",
    out = "apfl_mutex.go",
    package = "pgalloc",
    prefix = "apfl",
)

declare_mutex(
    name = "apfs_mutex",
    out = "apfs_mutex.go",
    package = "pgalloc",
    prefix = "apfs",
)

go_template_instance(
    name = "amfl_list",
    out = "amfl_list.go",
    package = "pgalloc",
    prefix = "asyncMemoryFileLoad",
    template = "//pkg/ilist:generic_list",
    types = {
        "Element": "*asyncMemoryFileLoad",
        "Linker": "*asyncMemoryFileLoad",
    },
)

go_template_instance(
    name = "apl_unloaded_set",
    out = "apl_unloaded_set.go",
    imports = {
        "memmap": "gvisor.dev/gvisor/pkg/sentry/memmap",
    },
    package = "pgalloc",
    prefix = "aplUnloaded",
    template = "//pkg/segment:generic_set",
    types = {
        "Key": "uint64",
        "Range": "memmap.FileRange",
        "Value": "aplUnloadedInfo",
        "Functions": "aplUnloadedSetFunctions",
    },
)

go_template_instance(
    name = "evictable_range",
    out = "evictable_range.go",
    package = "pgalloc",
    prefix = "Evictable",
    template = "//pkg/segment:generic_range",
    types = {
        "T": "uint64",
    },
)

go_template_instance(
    name = "evictable_range_set",
    out = "evictable_range_set.go",
    package = "pgalloc",
    prefix = "evictableRange",
    template = "//pkg/segment:generic_set",
    types = {
        "Key": "uint64",
        "Range": "EvictableRange",
        "Value": "evictableRangeSetValue",
        "Functions": "evictableRangeSetFunctions",
    },
)

go_template_instance(
    name = "logical_page_set",
    out = "logical_page_set.go",
    imports = {
        "memmap": "gvisor.dev/gvisor/pkg/sentry/memmap",
    },
    package = "pgalloc",
    prefix = "logicalPage",
    template = "//pkg/segment:generic_set",
    types = {
        "Key": "uint64",
        "Range": "memmap.FileRange",
        "Value": "logicalPageInfo",
        "Functions": "logicalPageSetFunctions",
    },
)

go_template_instance(
    name = "sparse_relocation_set",
    out = "sparse_relocation_set.go",
    imports = {
        "memmap": "gvisor.dev/gvisor/pkg/sentry/memmap",
    },
    package = "pgalloc",
    prefix = "sparseRelocation",
    template = "//pkg/segment:generic_set",
    types = {
        "Key": "uint64",
        "Range": "memmap.FileRange",
        "Value": "uint64",
        "Functions": "sparseRelocationSetFunctions",
    },
)

go_template_instance(
    name = "memacct_set",
    out = "memacct_set.go",
    consts = {
        "minDegree": "10",
    },
    imports = {
        "memmap": "gvisor.dev/gvisor/pkg/sentry/memmap",
    },
    package = "pgalloc",
    prefix = "memAcct",
    template = "//pkg/segment:generic_set",
    types = {
        "Key": "uint64",
        "Range": "memmap.FileRange",
        "Value": "memAcctInfo",
        "Functions": "memAcctSetFunctions",
    },
)

go_template_instance(
    name = "unfree_set",
    out = "unfree_set.go",
    consts = {
        "minDegree": "10",
        "trackGaps": "1",
    },
    imports = {
        "memmap": "gvisor.dev/gvisor/pkg/sentry/memmap",
    },
    package = "pgalloc",
    prefix = "unfree",
    template = "//pkg/segment:generic_set",
    types = {
        "Key": "uint64",
        "Range": "memmap.FileRange",
        "Value": "unfreeInfo",
        "Functions": "unfreeSetFunctions",
    },
)

go_template_instance(
    name = "unwaste_set",
    out = "unwaste_set.go",
    consts = {
        "minDegree": "10",
        "trackGaps": "1",
    },
    imports = {
        "memmap": "gvisor.dev/gvisor/pkg/sentry/memmap",
    },
    package = "pgalloc",
    prefix = "unwaste",
    template = "//pkg/segment:generic_set",
    types = {
        "Key": "uint64",
        "Range": "memmap.FileRange",
        "Value": "unwasteInfo",
        "Functions": "unwasteSetFunctions",
    },
)

go_library(
    name = "pgalloc",
    srcs = [
        "backing.go",
        "amfl_list.go",
        "amfls_mutex.go",
        "apfl_mutex.go",
        "apfs_mutex.go",
        "apl_unloaded_set.go",
        "chunk_mapping.go",
        "collapse.go",
        "commit_limit.go",
        "context.go",
        "debug.go",
        "dma_pin.go",
        "evictable_range.go",
        "evictable_range_set.go",
        "fold.go",
        "home.go",
        "home_pages.go",
        "host_ops_darwin.go",
        "host_ops_linux.go",
        "host_ops_linux_unsafe.go",
        "logical_capture.go",
        "logical_page_set.go",
        "memacct_set.go",
        "memory_file_mutex.go",
        "numa.go",
        "numa_darwin.go",
        "numa_linux_unsafe.go",
        "pgalloc.go",
        "pgalloc_unsafe.go",
        "remote_snapshot.go",
        "save_restore.go",
        "sparse_import.go",
        "sparse_relocation_set.go",
        "unfree_set.go",
        "unwaste_set.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/bitmap",
        "//pkg/context",
        "//pkg/errors/linuxerr",
        "//pkg/gohacks",
        "//pkg/goid",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/rand",
        "//pkg/ringdeque",
        "//pkg/safemem",
        "//pkg/sentry/arch",
        "//pkg/sentry/checkpoint",
        "//pkg/sentry/hostmm",
        "//pkg/sentry/memmap",
        "//pkg/sentry/state/stateio",
        "//pkg/sentry/usage",
        "//pkg/state",
        "//pkg/state/wire",
        "//pkg/sync",
        "//pkg/sync/locking",
        "//pkg/syncevent",
        "//pkg/timing",
        "//pkg/usermem",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "pgalloc_test",
    size = "small",
    srcs = [
        "chunk_mapping_test.go",
        "collapse_test.go",
        "commit_limit_test.go",
        "fold_test.go",
        "home_pages_test.go",
        "home_test.go",
        "huge_page_test.go",
        "numa_test.go",
        "pgalloc_64k_test.go",
        "pgalloc_test.go",
        "remote_snapshot_test.go",
        "save_restore_test.go",
        "slot_identity_test.go",
        "sparse_import_test.go",
    ],
    embed = [":pgalloc"],
    deps = [
        "//pkg/context",
        "//pkg/hostarch",
        "//pkg/memutil",
        "//pkg/safemem",
        "//pkg/sentry/memmap",
        "//pkg/sentry/state/stateio",
        "//pkg/sentry/usage",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

proto_library(
    name = "pgalloc_metadata",
    srcs = ["pgalloc.proto"],
)
