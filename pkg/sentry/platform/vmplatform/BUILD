# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "vmplatform",
    srcs = [
		"application_context.go",
        "doc.go",
        "engine.go",
        "external.go",
        "guest_ram.go",
        "monitor.go",
        "platform.go",
        "staging_unsafe.go",
        "vmm.go",
        "worker_pool.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/errors/linuxerr",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/safemem",
        "//pkg/sentry/arch",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/vmplatform/monitorabi",
        "//pkg/sentry/platform/vmplatform/vmm",
        "@com_github_google_btree//:go_default_library",
    ],
)

go_test(
    name = "vmplatform_test",
    size = "small",
    srcs = [
		"application_context_test.go",
        "engine_test.go",
        "external_test.go",
        "guest_ram_test.go",
        "platform_test.go",
        "staging_test.go",
        "worker_pool_test.go",
    ],
    library = ":vmplatform",
    deps = [
        "//pkg/hostarch",
        "//pkg/sentry/fsutil",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/state/stateio",
        "//pkg/sentry/platform/vmplatform/monitorabi",
        "//pkg/sentry/usage",
        "//pkg/safemem",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
