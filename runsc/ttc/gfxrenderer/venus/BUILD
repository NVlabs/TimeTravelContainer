# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "venus",
    srcs = [
        "clone_unsafe.go",
        "context.go",
        "crossdomain_unsafe.go",
        "device_unsafe.go",
        "dispatch_unsafe.go",
        "dmabuf_unsafe.go",
        "hostcopy_unsafe.go",
        "hostimport_unsafe.go",
        "instance_unsafe.go",
        "journal_unsafe.go",
        "layout_unsafe.go",
        "memory_unsafe.go",
        "modifier_unsafe.go",
        "nocgo.go",
        "objects_unsafe.go",
        "queue_unsafe.go",
        "quirks_unsafe.go",
        "ring_unsafe.go",
        "server_unsafe.go",
        "stream_unsafe.go",
        "transport_unsafe.go",
        "venus.go",
    ],
    bazel_cgo = True,
    stateify = False,
    visibility = ["//runsc/ttc/gfxrenderer:__subpackages__"],
    deps = [
        "//pkg/gfxproto",
        "//runsc/ttc/gfxrenderer/venus/vk",
        "//runsc/ttc/gfxrenderer/venus/vn",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
