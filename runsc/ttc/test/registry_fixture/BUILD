load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "registry_fixture",
    testonly = 1,
    srcs = ["main.go"],
    visibility = ["//runsc/ttc/test:__pkg__"],
)
