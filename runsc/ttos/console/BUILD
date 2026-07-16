load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "console",
    srcs = ["frame.go"],
    visibility = ["//runsc:__subpackages__"],
)

go_test(
    name = "console_test",
    size = "small",
    srcs = ["frame_test.go"],
    library = ":console",
)
