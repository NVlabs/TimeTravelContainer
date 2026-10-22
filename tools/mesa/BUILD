# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("@rules_python//python:py_binary.bzl", "py_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

# Runs Mesa's code generators for tools/mesa.BUILD's genrules.
py_binary(
    name = "run",
    srcs = ["run.py"],
    visibility = ["//visibility:public"],
    deps = [
        "@mako",
        "@markupsafe",
        "@pyyaml//:yaml",
    ],
)

exports_files(glob(["*.patch"]))
