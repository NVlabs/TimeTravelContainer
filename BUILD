# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

load("@rules_license//rules:license.bzl", "license")
load("//tools:defs.bzl", "build_test", "gazelle", "go_path")
load("//tools:release.bzl", "RELEASE_RUNSC", "RELEASE_SIDECARS", "release_files")
load("//tools/nogo:defs.bzl", "nogo_config")
load("//tools/yamltest:defs.bzl", "yaml_test")

package(
    default_applicable_licenses = [":license"],
    licenses = ["notice"],
)

license(
    name = "license",
    package_name = "TimeTravelContainer",
)

exports_files([
    "LICENSE",
    "README.md",
    "SECURITY.md",
])

# Upstream integration tests expect runsc and its sidecars in an installed
# release layout. TTC's runsc target is a test-only compatibility launcher for
# ttcd, so this layout is likewise test-only and includes ttcd explicitly.
release_files(
    name = "release",
    bins = [
        "//runsc/ttc",
        "//runsc/ttcd",
    ],
    runsc = RELEASE_RUNSC,
    sidecars = RELEASE_SIDECARS,
    testonly = True,
    visibility = ["//visibility:public"],
)

nogo_config(
    name = "nogo_config",
    srcs = ["nogo.yaml"],
    visibility = [
        "//visibility:public",
    ],
)

yaml_test(
    name = "nogo_config_test",
    srcs = glob(["nogo*.yaml"]),
    schema = "//tools/nogo/config:schema.json",
)

yaml_test(
    name = "github_workflows_test",
    srcs = glob([".github/workflows/*.yml"]),
    schema = "@github_workflow_schema//file",
)

# The sandbox filegroup is used for sandbox-internal dependencies.
package_group(
    name = "sandbox",
    packages = ["//..."],
)

# For targets that will not normally build internally, we ensure that they are
# least build by a static BUILD test.
build_test(
    name = "build_test",
    targets = [
        "//test/e2e:integration_test",
        "//test/image:image_test",
        "//test/root:crictl_test",
        "//test/root:root_test",
        "//test/benchmarks/base:startup_test",
        "//test/benchmarks/base:size_test",
        "//test/benchmarks/base:sysbench_test",
        "//test/benchmarks/database:redis_test",
        "//test/benchmarks/fs:bazel_test",
        "//test/benchmarks/fs:fio_test",
        "//test/benchmarks/media:ffmpeg_test",
        "//test/benchmarks/ml:tensorflow_test",
        "//test/benchmarks/network:httpd_test",
        "//test/benchmarks/network:nginx_test",
        "//test/benchmarks/network:node_test",
        "//test/benchmarks/network:ruby_test",
    ],
)

# gopath defines a directory that is structured in a way that is compatible
# with standard Go tools. Things like godoc, editors and refactor tools should
# work as expected.
#
# The files in this tree are symlinks to the true sources.
go_path(
    name = "gopath",
    mode = "archive",
    visibility = ["//:sandbox"],
    deps = [
        # Main binaries.
        #
        # For reasons related to reproducibility of the generated
        # files, in order to ensure that :gopath produces only a
        # a single "pure" version of all files, we can only depend
        # on go_library targets here, and not go_binary. Thus the
        # binaries have been factored into a cli package, which is
        # a good practice in any case.
        "//runsc/cli/maincli",
        "//tools/checklocks",

        # Packages that are not dependencies of the above.
        "//pkg/sentry/kernel/memevent",
        "//pkg/sentry/socket/plugin/stack",
        "//pkg/tcpip/adapters/gonet",
        "//pkg/tcpip/faketime",
        "//pkg/tcpip/link/channel",
        "//pkg/tcpip/link/ethernet",
        "//pkg/tcpip/link/muxed",
        "//pkg/tcpip/link/pipe",
        "//pkg/tcpip/link/sharedmem",
        "//pkg/tcpip/link/sharedmem/pipe",
        "//pkg/tcpip/link/sharedmem/queue",
        "//pkg/tcpip/link/tun",
        "//pkg/tcpip/link/waitable",
        "//pkg/tcpip/sample/tun_tcp_connect",
        "//pkg/tcpip/sample/tun_tcp_echo",
        "//pkg/tcpip/transport/tcpconntrack",
        "//sandboxexec/sandbox",
    ],
)

# CC toolchain targets for cross-compilation.
# Required to be explicitly specified in bazel >= 5.
toolchain(
    name = "cc_toolchain_k8",
    target_compatible_with = [
        "@platforms//os:linux",
        "@platforms//cpu:x86_64",
    ],
    toolchain = "@crosstool//:cc-compiler-k8",
    toolchain_type = "@bazel_tools//tools/cpp:toolchain_type",
)

toolchain(
    name = "cc_toolchain_aarch64",
    target_compatible_with = [
        "@platforms//os:linux",
        "@platforms//cpu:aarch64",
    ],
    toolchain = "@crosstool//:cc-compiler-aarch64",
    toolchain_type = "@bazel_tools//tools/cpp:toolchain_type",
)

# gazelle is a set of build tools.
#
# To update the WORKSPACE from go.mod, use:
#   bazel run //:gazelle -- update-repos -from_file=go.mod
gazelle(name = "gazelle")

exports_files([
    "go.sum",
    "go.mod",
])
