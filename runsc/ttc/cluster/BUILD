# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "cluster",
    srcs = [
		"authority_catalog.go",
		"authority_log.go",
		"authority_node.go",
		"authority_query.go",
		"authority_state.go",
        "capabilities.go",
        "control.go",
		"fixed_terminal_planner.go",
        "placement.go",
		"planner.go",
		"policy.go",
        "registry.go",
		"resource_window.go",
		"shared_authority.go",
		"shared_authority_darwin.go",
		"shared_authority_linux.go",
        "store.go",
        "transaction.go",
    ],
    deps = [
		"@org_golang_x_sys//unix:go_default_library",
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
		"authority_log_test.go",
		"authority_node_test.go",
		"authority_query_test.go",
		"authority_state_test.go",
        "cluster_test.go",
        "control_test.go",
		"fixed_terminal_planner_test.go",
        "placement_test.go",
		"planner_test.go",
		"policy_test.go",
        "registry_test.go",
		"resource_window_test.go",
		"shared_authority_test.go",
    ],
    library = ":cluster",
	deps = ["//pkg/ttcremote"],
)
