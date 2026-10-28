# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Description:
#   This VDSO is a shared library that provides the same interfaces as the
#   normal system VDSO (time, gettimeofday, clock_gettimeofday) but which uses
#   timekeeping parameters managed by the sandbox kernel.

load("@rules_python//python:py_test.bzl", "py_test")
load("//tools:arch.bzl", "select_arch")
load("//tools:defs.bzl", "cc_flags_supplier", "cc_toolchain")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

exports_files(["check_vdso.py"])

genrule(
    name = "vdso",
    srcs = [
        "barrier.h",
        "compiler.h",
        "cycle_clock.h",
        "seqlock.h",
        "syscalls.h",
        "vdso.cc",
        "vdso_amd64.lds",
        "vdso_arm64.lds",
        "vdso_time.h",
        "vdso_time.cc",
    ],
    outs = [
        "vdso.so",
    ],
    cmd = "if [ \"$$(uname -s)\" = Darwin ]; then " +
          "$(location //vdso/minimal:minimal_vdso) " +
          select_arch(
              amd64 = "-arch=amd64 ",
              arm64 = "-arch=arm64 ",
              no_match_error = "unsupported architecture",
          ) +
          "-output=$(location vdso.so); " +
          "else " +
          "$(CC) $(CC_FLAGS) " +
          "-I. " +
          "-O2 " +
          "-std=c++11 " +
          "-fPIC " +
          "-fno-sanitize=all " +
          # Some toolchains enable stack protector by default. Disable it, the
          # VDSO has no hooks to handle failures.
          "-fno-stack-protector " +
          "-shared " +
          "-nostdlib " +
          "-Wl,-soname=linux-vdso.so.1 " +
          "-Wl,--hash-style=sysv " +
          "-Wl,--no-undefined " +
          "-Wl,-Bsymbolic " +
          "-Wl,-z,max-page-size=4096 " +
          "-Wl,-z,common-page-size=4096 " +
          select_arch(
              amd64 = "-Wl,-T$(location vdso_amd64.lds) ",
              arm64 = "-Wl,-T$(location vdso_arm64.lds) ",
              no_match_error = "unsupported architecture",
          ) +
          "-o $(location vdso.so) " +
          "$(location vdso.cc) " +
          "$(location vdso_time.cc); " +
          "fi",
    features = ["-pie"],
    toolchains = [
        cc_toolchain,
        ":no_pie_cc_flags",
    ],
    tools = ["//vdso/minimal:minimal_vdso"],
    visibility = ["//:sandbox"],
)

cc_flags_supplier(
    name = "no_pie_cc_flags",
    features = ["-pie"],
)

py_test(
    name = "vdso_test",
    srcs = ["check_vdso.py"],
    args = [
        "--check-data",
        "--vdso=$(location :vdso)",
    ],
    data = [":vdso"],
    main = "check_vdso.py",
)
