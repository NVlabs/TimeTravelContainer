load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "fsgofer",
    srcs = [
        "lisafs.go",
    ],
    visibility = ["//runsc:__subpackages__"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/cleanup",
        "//pkg/fd",
        "//pkg/fsutil",
        "//pkg/lisafs",
        "//pkg/log",
        "//pkg/marshal/primitive",
        "//runsc/config",
        "//runsc/ttos/ownership",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "lisafs_test",
    size = "small",
    srcs = ["lisafs_test.go"],
    deps = [
        ":fsgofer",
        "//pkg/lisafs",
        "//pkg/lisafs/testsuite",
        "//pkg/log",
    ],
)

go_test(
    name = "virtual_ownership_test",
    size = "small",
    srcs = ["virtual_ownership_test.go"],
    library = ":fsgofer",
    deps = [
        "//runsc/ttos/ownership",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
