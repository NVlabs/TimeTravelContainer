load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcsnapshot",
    srcs = [
        "btree.go",
        "data.go",
        "filesystem.go",
        "memory.go",
        "resolver.go",
        "store.go",
        "types.go",
    ],
    visibility = ["//:sandbox"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "ttcsnapshot_test",
    size = "small",
    srcs = [
        "resolver_test.go",
        "store_test.go",
    ],
    library = ":ttcsnapshot",
)
