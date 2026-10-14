load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "cputopology",
    srcs = [
        "cputopology.go",
        "host_linux.go",
        "host_other.go",
    ],
    visibility = ["//:sandbox"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "cputopology_test",
    size = "small",
    srcs = ["cputopology_test.go"],
    library = ":cputopology",
)
