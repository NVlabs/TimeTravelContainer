load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "cluster",
    srcs = [
		"authority_raft.go",
		"authority_state.go",
        "capabilities.go",
        "control.go",
		"fixed_terminal_planner.go",
        "placement.go",
		"planner.go",
		"policy.go",
		"protocol_mux.go",
		"raft_catalog.go",
        "registry.go",
		"resource_window.go",
        "store.go",
        "transaction.go",
    ],
    deps = [
		"@com_github_hashicorp_raft//:raft",
		"@com_github_hashicorp_raft_boltdb_v2//:raft-boltdb",
		"//pkg/hostarch",
		"//pkg/ttcremote",
		"//pkg/ttcsnapshot",
	],
    visibility = ["//visibility:public"],
)

go_test(
    name = "cluster_test",
    size = "small",
    srcs = [
		"authority_raft_test.go",
		"authority_state_test.go",
        "cluster_test.go",
        "control_test.go",
		"fixed_terminal_planner_test.go",
        "placement_test.go",
		"planner_test.go",
		"policy_test.go",
		"protocol_mux_test.go",
        "registry_test.go",
		"resource_window_test.go",
    ],
    library = ":cluster",
	deps = ["//pkg/ttcremote"],
)
