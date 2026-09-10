load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcchunk",
    srcs = [
		"assemble.go",
        "consumption.go",
        "consumption_tracker.go",
        "chunk.go",
        "donated.go",
        "graph.go",
		"graph_stream.go",
        "install.go",
        "lifecycle.go",
        "manifest.go",
        "packed_reader.go",
        "repository.go",
        "remote.go",
        "store.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "@org_golang_x_sys//unix",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/safemem",
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
    srcs = ["chunk_test.go"],
    library = ":ttcchunk",
    deps = [
        "//pkg/hostarch",
        "//pkg/sentry/checkpoint",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/ttcsnapshot",
    ],
)
