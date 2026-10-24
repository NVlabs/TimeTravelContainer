# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "images",
    srcs = [
        "images.go",
        "credentials.go",
        "rootfs.go",
        "native_store.go",
        "registry_client.go",
    ],
    visibility = ["//runsc/ttc:__subpackages__"],
    deps = [
        "//runsc/ttc/ownership",
        "//runsc/ttc/pflate",
        "@com_github_containerd_containerd_v2//core/content",
        "@com_github_containerd_errdefs//:go_default_library",
        "@com_github_containerd_containerd_v2//core/images",
        "@com_github_containerd_containerd_v2//core/images/archive",
        "@com_github_containerd_containerd_v2//core/remotes",
        "@com_github_containerd_containerd_v2//core/remotes/docker",
        "@com_github_containerd_containerd_v2//pkg/archive",
        "@com_github_containerd_containerd_v2//pkg/archive/compression",
        "@com_github_containerd_containerd_v2//plugins/content/local",
        "@com_github_containerd_platforms//:platforms",
        "@com_github_distribution_reference//:go_default_library",
        "@com_github_klauspost_compress//flate:go_default_library",
        "@com_github_opencontainers_image_spec//specs-go/v1:specs-go",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "images_test",
    size = "small",
    srcs = [
        "images_test.go",
        "registry_client_test.go",
        "rootfs_test.go",
        "xattr_test_darwin.go",
        "xattr_test_linux.go",
    ],
    library = ":images",
    deps = [
        "//pkg/ttcvolume",
        "@com_github_containerd_containerd_v2//core/content",
        "@com_github_containerd_containerd_v2//core/images",
        "@com_github_containerd_containerd_v2//core/images/archive",
        "@com_github_containerd_containerd_v2//core/remotes/docker",
        "@com_github_containerd_platforms//:platforms",
        "@com_github_opencontainers_image_spec//specs-go/v1:specs-go",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
