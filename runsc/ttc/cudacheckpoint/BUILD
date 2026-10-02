# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "cc_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

# ttc-cuda-checkpoint checkpoints and restores CUDA processes in containers
# of --nvproxy-fork=snapshot families, for their forks. It calls the host's
# CUDA driver, which containers have, by dlopen: it builds without CUDA.
cc_binary(
    name = "ttc-cuda-checkpoint",
    srcs = ["cuda_checkpoint.c"],
    copts = [
        "-Wall",
        "-Wextra",
        "-Werror",
    ],
    linkopts = ["-ldl"],
    target_compatible_with = ["@platforms//os:linux"],
    visibility = ["//visibility:public"],
)
