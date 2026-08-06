load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcvolume",
    srcs = [
        "metadata.go",
        "repository.go",
        "snapshot.go",
        "tree.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "ttcvolume_test",
    size = "small",
    srcs = ["repository_test.go"],
    library = ":ttcvolume",
)
