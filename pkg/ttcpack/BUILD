load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_test(
    name = "ttcpack_test",
    size = "small",
    srcs = ["pack_test.go"],
    library = ":ttcpack",
)

go_library(
    name = "ttcpack",
    srcs = ["pack.go"],
    visibility = ["//visibility:public"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
