# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""BUILD rule for embedded binaries."""

load("//tools:defs.bzl", "go_library")
load("//tools/go_generics:defs.bzl", "go_template_instance")

_EMBEDDED_BINARY_TEMPLATE = "//tools/embeddedbinary:embeddedbinary_template"
_EMBEDDED_BINARY_TEMPLATE_DARWIN = "//tools/embeddedbinary:embeddedbinary_template_darwin"
_FLATECOMPRESS = "//tools/embeddedbinary:flatecompress"

def embedded_binary_go_library(
        name,
        binary,
        binary_name = None,
        out = None,
        go_package_name = None,
        visibility = None):
    """Embed a binary and generate a go_library target that can execute it.

    The binary will be compressed, and needs temporary space to be available
    when executing.

    Args:
        name: The name of the go_library rule.
        binary: Binary BUILD target that should be embedded.
        binary_name: The name (i.e. typical argv[0]) of the binary being
          embedded, defaults to `name`.
        out: Output filename of the go_library rule, defaults to `name + ".go"`.
        go_package_name: Package name of the go_library, defaults to `name`.
        visibility: Visibility of the go_library rule.
    """
    if binary_name == None:
        binary_name = name
    if out == None:
        out = name + ".go"
    if go_package_name == None:
        go_package_name = name
    compressed_binary = binary_name + ".flate"
    uncompressed_binary = binary_name + ".bin"
    native.genrule(
        name = name + "_flate",
        outs = [compressed_binary],
        cmd = "$(location %s) < $(SRCS) > $(OUTS)" % (_FLATECOMPRESS,),
        srcs = [binary],
        tools = [_FLATECOMPRESS],
    )
    native.genrule(
        name = name + "_noflate",
        outs = [uncompressed_binary],
        cmd = "cat < $(SRCS) > $(OUTS)",
        srcs = [binary],
    )
    darwin_out = out[:-3] + "_darwin.go"
    go_template_instance(
        name = name + "_lib_linux",
        template = _EMBEDDED_BINARY_TEMPLATE,
        package = go_package_name,
        out = out,
        substrs = select({
            "//tools/embeddedbinary:compilation_mode_opt": {
                "embedded.bin.name": binary_name,
                "//go:embed embedded.bin.flate": "//go:embed %s" % (compressed_binary,),
            },
            "//conditions:default": {
                "embedded.bin.name": binary_name,
                "//go:embed embedded.bin.flate": "//go:embed %s" % (uncompressed_binary,),
                "flate.NewReader": "io.Reader",
            },
        }),
    )
    go_template_instance(
        name = name + "_lib_darwin",
        template = _EMBEDDED_BINARY_TEMPLATE_DARWIN,
        package = go_package_name,
        out = darwin_out,
        substrs = select({
            "//tools/embeddedbinary:compilation_mode_opt": {
                "embedded.bin.name": binary_name,
                "//go:embed embedded.bin.flate": "//go:embed %s" % (compressed_binary,),
            },
            "//conditions:default": {
                "embedded.bin.name": binary_name,
                "//go:embed embedded.bin.flate": "//go:embed %s" % (uncompressed_binary,),
                "flate.NewReader": "io.Reader",
            },
        }),
    )
    go_library(
        name = name,
        srcs = [out, darwin_out],
        embedsrcs = select({
            "//tools/embeddedbinary:compilation_mode_opt": [compressed_binary],
            "//conditions:default": [uncompressed_binary],
        }),
        deps = ["@org_golang_x_sys//unix:go_default_library"],
        visibility = visibility,
    )
