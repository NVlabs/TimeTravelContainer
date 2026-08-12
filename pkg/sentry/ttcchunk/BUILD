load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcchunk",
    srcs = [
		"assemble.go",
        "chunk.go",
        "store.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/hostarch",
        "//pkg/sentry/pgalloc",
    ],
)

go_test(
    name = "ttcchunk_test",
    size = "small",
    srcs = ["chunk_test.go"],
    library = ":ttcchunk",
    deps = [
        "//pkg/hostarch",
        "//pkg/sentry/pgalloc",
    ],
)
