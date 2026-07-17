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
        "//pkg/abi/linux",
        "//runsc/ttos/console",
        "//runsc/ttos/images",
        "//runsc/ttos/ownership",
        "@com_github_docker_go_units//:go_default_library",
        "@com_github_moby_sys_signal//:go_default_library",
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
