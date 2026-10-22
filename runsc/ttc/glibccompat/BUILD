# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("@rules_cc//cc:defs.bzl", "cc_library")
load("//tools:defs.bzl", "go_library", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

# An older glibc has the dl functions in libdl, and the pthread ones in
# libpthread, which a newer one keeps as empty shared libraries; their -l
# names are empty archives there, which link no library.
GLIBC_LIBRARIES = [
    "-Wl,--no-as-needed",
    "-l:libdl.so.2",
    "-l:libpthread.so.0",
    "-Wl,--as-needed",
]

# For C binaries: the start and dl functions.
cc_library(
    name = "start_dl",
    srcs = [
        "glibccompat.h",
        "start_dl.c",
    ],
    linkopts = GLIBC_LIBRARIES,
    target_compatible_with = ["@platforms//os:linux"],
    visibility = ["//runsc/ttc/cudacheckpoint:__pkg__"],
    alwayslink = True,
)

# For Go binaries with cgo, as the GPU renderer: the start, dl, pthread, file
# status and math functions.
go_library(
    name = "glibccompat",
    srcs = [
        "cgo.go",
        "glibccompat.go",
        "glibccompat.h",
        "pthread.c",
        "start_dl.c",
        "stat_math.c",
    ],
    bazel_cgo = True,
    bazel_clinkopts = select_system(
        darwin = [],
        linux = GLIBC_LIBRARIES,
    ),
    stateify = False,
    visibility = ["//runsc/ttcgfx:__pkg__"],
)
