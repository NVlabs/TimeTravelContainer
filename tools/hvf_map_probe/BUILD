load("//tools:defs.bzl", "cc_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

# This is an intentionally Darwin-only qualification tool. It is not a test:
# observations about undocumented host-VM mutation semantics must not become a
# release contract accidentally.
cc_binary(
    name = "hvf_map_probe",
    srcs = ["hvf_map_probe.c"],
    copts = [
        "-Wall",
        "-Wextra",
        "-Werror",
        "-mmacosx-version-min=15.0",
    ],
    linkopts = [
        "-framework Hypervisor",
        "-mmacosx-version-min=15.0",
    ],
    target_compatible_with = [
        "@platforms//cpu:arm64",
        "@platforms//os:macos",
    ],
    visibility = ["//visibility:public"],
)
