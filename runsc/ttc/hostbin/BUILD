# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "bool_flag", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

# carry_helpers has ttcd carry, on Linux, the GPU renderer and
# ttc-cuda-checkpoint, which link the C library, as ttcd does not. A ttcd that
# carries neither looks for them beside itself.
bool_flag(
    name = "carry_helpers",
    build_setting_default = True,
)

config_setting(
    name = "carried",
    constraint_values = ["@platforms//os:linux"],
    flag_values = {":carry_helpers": "true"},
)

# rules_go's platforms without cgo, which cross-builds (--config=aarch64) and
# nogo's analyses of other architectures use, build no helper, and carry none.
config_setting(
    name = "carried_without_cgo",
    constraint_values = [
        "@platforms//os:linux",
        "@io_bazel_rules_go//go/toolchain:cgo_off",
    ],
    flag_values = {":carry_helpers": "true"},
)

# Each helper, gzip-compressed; empty where ttcd carries none.
genrule(
    name = "gfx_renderer_gz",
    srcs = select({
        ":carried_without_cgo": [],
        ":carried": ["//runsc/ttcgfx:ttc-gfx-renderer"],
        "//conditions:default": [],
    }),
    outs = ["ttc-gfx-renderer.gz"],
    cmd = select({
        ":carried_without_cgo": ": > $@",
        ":carried": "gzip -9 -n -c $(SRCS) > $@",
        "//conditions:default": ": > $@",
    }),
)

genrule(
    name = "cuda_checkpoint_gz",
    srcs = select({
        ":carried_without_cgo": [],
        ":carried": ["//runsc/ttc/cudacheckpoint:ttc-cuda-checkpoint"],
        "//conditions:default": [],
    }),
    outs = ["ttc-cuda-checkpoint.gz"],
    cmd = select({
        ":carried_without_cgo": ": > $@",
        ":carried": "gzip -9 -n -c $(SRCS) > $@",
        "//conditions:default": ": > $@",
    }),
)

# The C sidecars that the Sentry and runsc execute (see
# //runsc/gvisorbinaries), static and freestanding, gzip-compressed; empty
# where ttcd carries none. ttcd is the Sentry sidecar itself.
genrule(
    name = "prewarmer_gz",
    srcs = select({
        ":carried_without_cgo": [],
        ":carried": ["//runsc/prewarmer:prewarmer_binary"],
        "//conditions:default": [],
    }),
    outs = ["gvisor-sentry-prewarmer.gz"],
    cmd = select({
        ":carried_without_cgo": ": > $@",
        ":carried": "gzip -9 -n -c $(SRCS) > $@",
        "//conditions:default": ": > $@",
    }),
)

genrule(
    name = "fdparking_gz",
    srcs = select({
        ":carried_without_cgo": [],
        ":carried": ["//runsc/fdparking:fdparking_binary"],
        "//conditions:default": [],
    }),
    outs = ["runsc-fd-parking.gz"],
    cmd = select({
        ":carried_without_cgo": ": > $@",
        ":carried": "gzip -9 -n -c $(SRCS) > $@",
        "//conditions:default": ": > $@",
    }),
)

genrule(
    name = "procumounter_gz",
    srcs = select({
        ":carried_without_cgo": [],
        ":carried": ["//runsc/procumounter:procumounter_binary"],
        "//conditions:default": [],
    }),
    outs = ["gvisor-proc-umounter.gz"],
    cmd = select({
        ":carried_without_cgo": ": > $@",
        ":carried": "gzip -9 -n -c $(SRCS) > $@",
        "//conditions:default": ": > $@",
    }),
)

go_library(
    name = "hostbin",
    srcs = ["hostbin.go"],
    embedsrcs = [
        "gvisor-proc-umounter.gz",
        "gvisor-sentry-prewarmer.gz",
        "runsc-fd-parking.gz",
        "ttc-cuda-checkpoint.gz",
        "ttc-gfx-renderer.gz",
    ],
    visibility = ["//runsc/ttcd:__pkg__"],
)

go_test(
    name = "hostbin_test",
    size = "small",
    srcs = ["hostbin_test.go"],
    library = ":hostbin",
)
