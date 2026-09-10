load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcvolume",
    srcs = [
        "gc.go",
        "metadata.go",
        "remote.go",
        "repository.go",
        "snapshot.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
		"//pkg/ttcsnapshot",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "ttcvolume_test",
    size = "small",
    srcs = [
        "remote_test.go",
        "repository_test.go",
    ],
    library = ":ttcvolume",
)
