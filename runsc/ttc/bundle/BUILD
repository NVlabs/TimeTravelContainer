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
    visibility = ["//runsc/ttc:__subpackages__"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/log",
        "//pkg/ttcvolume",
        "//runsc/specutils",
        "//runsc/ttc/console",
        "//runsc/ttc/images",
        "//runsc/ttc/ownership",
        "@com_github_docker_go_units//:go_default_library",
        "@com_github_moby_sys_signal//:go_default_library",
        "@com_github_moby_sys_user//:user",
        "@com_github_opencontainers_image_spec//specs-go/v1:specs-go",
        "@com_github_opencontainers_runtime_spec//specs-go",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "bundle_test",
    size = "small",
    srcs = [
        "spec_test.go",
        "store_test.go",
	],
    library = ":bundle",
    deps = [
        "//pkg/ttcvolume",
        "//runsc/specutils",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
