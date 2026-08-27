load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ring0asm",
    srcs = [
        "bridge_darwin_arm64.go",
        "bridge_darwin_arm64.s",
    ],
    stateify = False,
    visibility = ["//pkg/sentry/platform/hvf:__pkg__"],
)
