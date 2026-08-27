load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "machexc",
    srcs = ["machexc_darwin.go"],
    bazel_cgo = True,
    stateify = False,
    visibility = ["//pkg/safecopy:__pkg__"],
)
