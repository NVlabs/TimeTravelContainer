# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("@rules_shell//shell:sh_binary.bzl", "sh_binary")
load("@rules_shell//shell:sh_test.bzl", "sh_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

exports_files([
    "hvf.entitlements",
    "run_entitled_test.sh",
])

# Test-only compatibility launcher for inherited gVisor tests that expect a
# //runsc executable in runfiles. TTC releases do not contain this launcher;
# ttcd embeds the runtime entry point.
sh_binary(
    name = "runsc",
    srcs = ["runsc_compat.sh"],
    data = ["//runsc/ttcd"],
    testonly = True,
    visibility = ["//visibility:public"],
)

sh_test(
    name = "version_test",
    size = "small",
    srcs = ["version_test.sh"],
    args = ["$(location :runsc)"],
    data = [":runsc"],
)
