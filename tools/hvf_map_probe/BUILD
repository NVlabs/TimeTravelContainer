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

# hvf_mem_probe characterizes the primitives of a demand-paged HVF memory
# design. Like hvf_map_probe, it is a qualification tool, not a test. It never
# maps host-executable pages: that panics the host (see its header).
cc_binary(
    name = "hvf_mem_probe",
    srcs = ["hvf_mem_probe.c"],
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
