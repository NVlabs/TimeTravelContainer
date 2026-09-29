# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcremote",
    srcs = [
		"doc.go",
		"donated_conn.go",
        "metadata.go",
        "push.go",
        "tcp.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/hostarch",
        "//pkg/hostthread",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/state/stateio",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "ttcremote_test",
    size = "small",
    srcs = [
		"donated_conn_test.go",
        "metadata_test.go",
        "tcp_test.go",
    ],
    library = ":ttcremote",
    deps = [
        "//pkg/hostarch",
        "//pkg/safemem",
        "//pkg/sentry/contexttest",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/state/stateio",
        "//pkg/sentry/usage",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
