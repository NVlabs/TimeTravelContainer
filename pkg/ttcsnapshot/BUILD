load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcsnapshot",
    srcs = [
        "btree.go",
		"catalog.go",
        "data.go",
        "filesystem.go",
		"hosttree.go",
        "memory.go",
		"pack.go",
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
		"catalog_test.go",
		"hosttree_test.go",
		"pack_test.go",
        "resolver_test.go",
        "store_test.go",
    ],
    library = ":ttcsnapshot",
)
