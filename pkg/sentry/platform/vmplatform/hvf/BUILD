# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hvf",
    srcs = ["platform_darwin_arm64.go"],
    stateify = False,
    target_compatible_with = [
        "@platforms//cpu:arm64",
        "@platforms//os:osx",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/hostarch",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/hvf",
        "//pkg/sentry/platform/vmplatform",
        "//pkg/sentry/platform/vmplatform/arm64",
    ],
)

go_test(
    name = "hvf_test",
    size = "small",
    pure = "off",
    srcs = ["platform_darwin_arm64_test.go"],
    library = ":hvf",
    target_compatible_with = [
        "@platforms//cpu:arm64",
        "@platforms//os:osx",
    ],
    deps = ["//pkg/sentry/platform"],
)

# hvf_test skips unless its binary is signed with the hypervisor entitlement.
sh_test(
    name = "hvf_entitled_test",
    size = "medium",
    srcs = ["//runsc:run_entitled_test.sh"],
    args = [
        "$(rootpath :hvf_test)",
        "$(rootpath //runsc:hvf.entitlements)",
    ],
    data = [
        ":hvf_test",
        "//runsc:hvf.entitlements",
    ],
    target_compatible_with = [
        "@platforms//cpu:arm64",
        "@platforms//os:osx",
    ],
)
