load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hostthread",
    srcs = [
        "hostthread.go",
        "hostthread_darwin.go",
        "hostthread_other.go",
    ],
    visibility = ["//:sandbox"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "hostthread_test",
    size = "small",
    srcs = ["hostthread_darwin_test.go"],
    library = ":hostthread",
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
