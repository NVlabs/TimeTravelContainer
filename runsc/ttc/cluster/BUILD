load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "cluster",
    srcs = [
        "capabilities.go",
        "control.go",
		"fixed_terminal_planner.go",
        "placement.go",
		"planner.go",
        "registry.go",
        "store.go",
        "transaction.go",
    ],
    deps = ["//pkg/ttcremote"],
    visibility = ["//visibility:public"],
)

go_test(
    name = "cluster_test",
    size = "small",
    srcs = [
        "cluster_test.go",
        "control_test.go",
		"fixed_terminal_planner_test.go",
        "placement_test.go",
		"planner_test.go",
        "registry_test.go",
    ],
    library = ":cluster",
    deps = ["//pkg/ttcremote"],
)
