load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ownership",
    srcs = ["manifest.go"],
    visibility = ["//runsc:__subpackages__"],
)

go_test(
    name = "ownership_test",
    size = "small",
    srcs = ["manifest_test.go"],
    library = ":ownership",
)
