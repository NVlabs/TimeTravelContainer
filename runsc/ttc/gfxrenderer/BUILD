# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "gfxrenderer",
    srcs = [
        "broker.go",
        "confine_darwin.go",
        "confine_linux.go",
        "gfxstream.go",
        "launcher.go",
        "nocgo.go",
        "options.go",
        "renderer.go",
        "shm_darwin.go",
        "shm_linux.go",
        "snapshot.go",
    ],
    bazel_cgo = True,
    stateify = False,
    visibility = [
        "//pkg/sentry/devices/virtgpu/rendererclient:__pkg__",
        "//runsc:__subpackages__",
    ],
    deps = [
        "//pkg/gfxproto",
        "//runsc/ttc/gfxrenderer/venus",
        "//runsc/ttc/seatbelt",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "gfxrenderer_test",
    size = "small",
    srcs = [
        "family_test.go",
        "launcher_test.go",
        "snapshot_test.go",
    ],
    library = ":gfxrenderer",
    # The renderer of a family serves with the Venus backend, which calls
    # Vulkan through cgo, as the renderer's binary does.
    pure = select_system(darwin = "off", linux = "auto"),
    deps = [
        "//pkg/gfxproto",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

# redroid's graphics configuration for the virtual GPU, for e2e_redroid.
exports_files(["guest/redroid/gpu_config.sh"])
