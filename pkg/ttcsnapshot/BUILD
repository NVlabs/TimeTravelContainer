load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcsnapshot",
    srcs = [
        "btree.go",
        "memory.go",
        "store.go",
        "types.go",
    ],
    visibility = ["//:sandbox"],
)

go_test(
    name = "ttcsnapshot_test",
    size = "small",
    srcs = ["store_test.go"],
    library = ":ttcsnapshot",
)
