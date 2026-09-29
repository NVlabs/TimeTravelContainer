# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "seatbelt",
    srcs = [
        "profile_darwin.go",
        "seatbelt_darwin.go",
        "seatbelt_darwin_nocgo.go",
    ],
    bazel_cgo = True,
    stateify = False,
    visibility = ["//runsc/ttc:__subpackages__"],
)

go_test(
    name = "seatbelt_test",
    size = "small",
    # The Darwin test confines a child process through cgo. Linux
    # configurations, such as nogo analysis, have no test sources.
    pure = select_system(darwin = "off", linux = "auto"),
    srcs = select_system(
        darwin = ["seatbelt_darwin_test.go"],
        linux = [],
    ),
    library = ":seatbelt",
    # Bazel's Darwin sandbox is a Seatbelt profile too, which does not admit
    # applying another.
    tags = ["no-sandbox"],
)
