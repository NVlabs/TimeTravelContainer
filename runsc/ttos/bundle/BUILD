load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "bundle",
    srcs = [
        "spec.go",
        "store.go",
    ],
    visibility = ["//runsc/ttos:__subpackages__"],
    deps = [
        "//runsc/ttos/images",
        "@com_github_moby_sys_user//:user",
        "@com_github_opencontainers_image_spec//specs-go/v1:specs-go",
        "@com_github_opencontainers_runtime_spec//specs-go",
    ],
)

go_test(
    name = "bundle_test",
    size = "small",
    srcs = ["spec_test.go"],
    library = ":bundle",
)
