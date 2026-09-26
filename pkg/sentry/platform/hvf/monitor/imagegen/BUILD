load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "imagegen",
    srcs = ["main.go"],
    pure = True,
    visibility = ["//pkg/sentry/platform/hvf:__pkg__"],
)
