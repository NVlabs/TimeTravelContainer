load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "cmd",
    srcs = ["cmd.go"],
    visibility = ["//runsc/ttos:__subpackages__"],
    deps = [
        "//runsc/flag",
        "//runsc/version",
    ],
)

go_test(
    name = "cmd_test",
    size = "small",
    srcs = ["cmd_test.go"],
    library = ":cmd",
)
