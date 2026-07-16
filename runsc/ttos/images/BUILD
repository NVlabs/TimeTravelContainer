load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "images",
    srcs = ["images.go"],
    visibility = ["//runsc/ttos:__subpackages__"],
    deps = [
        "@com_github_containerd_containerd_v2//client",
        "@com_github_distribution_reference//:go_default_library",
    ],
)

go_test(
    name = "images_test",
    size = "small",
    srcs = ["images_test.go"],
    library = ":images",
)
