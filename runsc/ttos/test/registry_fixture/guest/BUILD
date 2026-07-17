load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "guest",
    testonly = 1,
    srcs = ["main.go"],
    features = ["fully_static_link"],
    visibility = ["//runsc/ttos/test:__pkg__"],
)
