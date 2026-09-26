load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "minimal_vdso",
    srcs = ["main.go"],
    pure = True,
    visibility = ["//vdso:__pkg__"],
)
