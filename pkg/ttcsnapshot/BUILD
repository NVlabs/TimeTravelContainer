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
        "checkpoint_json.go",
        "data.go",
		"deferred.go",
        "filesystem.go",
        "filesystem_json.go",
        "host_ops_darwin.go",
        "host_ops_linux.go",
		"hosttree.go",
		"ledger.go",
        "memory.go",
        "pack.go",
		"relay.go",
        "resolver.go",
        "store.go",
        "types.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "@org_golang_x_sync//errgroup:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "ttcsnapshot_test",
    size = "small",
    srcs = [
		"catalog_test.go",
        "checkpoint_json_test.go",
		"deferred_test.go",
		"filesystem_json_test.go",
		"hosttree_test.go",
		"ledger_test.go",
		"names_test.go",
		"pack_test.go",
		"relay_test.go",
        "resolver_test.go",
        "store_test.go",
    ],
    library = ":ttcsnapshot",
)
