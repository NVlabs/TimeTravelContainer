load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "guest_fixture",
    srcs = ["main.go"],
    pure = "on",
    visibility = ["//runsc/ttc/test:__pkg__"],
)
