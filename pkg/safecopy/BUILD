# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("//tools:defs.bzl", "go_library", "go_test", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "safecopy",
    force_add_state_pkg = True,
    srcs = [
        "atomic_amd64.s",
        "atomic_arm64.s",
        "mach_fault_addr_darwin.s",
        "memclr_amd64.s",
        "memclr_arm64.s",
        "memcpy_amd64.s",
        "memcpy_arm64.s",
        "safecopy.go",
        "safecopy_amd64_unsafe.go",
        "safecopy_arm64.go",
        "safecopy_darwin.go",
        "safecopy_darwin_nocgo.go",
        "safecopy_linux.go",
        "safecopy_unsafe.go",
        "sighandler_amd64.s",
        "sighandler_arm64.s",
        "xrstor_amd64.s",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/errors",
        "//pkg/errors/linuxerr",
        "//pkg/sighandling",
        "@org_golang_x_sys//unix:go_default_library",
    ] + select_system(linux = [], darwin = ["//pkg/safecopy/machexc"]),
)

go_test(
    name = "safecopy_test",
    srcs = [
        "safecopy_darwin_test.go",
        "safecopy_test.go",
    ],
    # Darwin fault recovery uses the Mach exception server in machexc.
    pure = select_system(darwin = "off", linux = "auto"),
    embed = [":safecopy"],
    deps = [
        "@org_golang_x_sys//unix:go_default_library",
    ] + select_system(linux = [], darwin = ["//pkg/safecopy/machexc"]),
)

go_test(
    name = "safecopy_x_test",
    srcs = [
        "safecopy_amd64_test.go",
        "safecopy_arm64_test.go",
    ],
    # NOTE: It seems that bazel code generation does not properly parse tags
    # when run via the architecture transition for nogo. This should be fixed
    # at some point in the future, but for now we can simply skip nogo analysis
    # on the test itself. It still applies to the core library.
    nogo = False,
    tags = ["not_run:arm"],
    deps = [
        "//pkg/cpuid",
        "//pkg/safecopy",
        "//pkg/sentry/arch/fpu",
    ],
)
