load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "monitorabi",
    srcs = ["abi.go"],
    visibility = ["//pkg/sentry:internal"],
)

go_test(
    name = "monitorabi_test",
    size = "small",
    srcs = ["abi_test.go"],
    library = ":monitorabi",
)
