load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_test(
    name = "sighandling_test",
    size = "small",
    srcs = ["sighandling_darwin_test.go"],
    library = ":sighandling",
    deps = [
        "//pkg/abi/linux",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_library(
    name = "sighandling",
    srcs = [
        "sighandling.go",
        "sighandling_darwin.go",
        "sighandling_linux_unsafe.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/abi/linux",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
