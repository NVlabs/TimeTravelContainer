load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "checkpoint",
    srcs = [
        "checkpoint.go",
        "portable.go",
    ],
    visibility = ["//pkg/sentry:internal"],
)

go_test(
    name = "checkpoint_test",
    size = "small",
    srcs = ["portable_test.go"],
    library = ":checkpoint",
)
