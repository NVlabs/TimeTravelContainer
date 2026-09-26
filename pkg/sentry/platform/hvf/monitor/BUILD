load("//tools:defs.bzl", "cc_binary", "cc_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

cc_library(
    name = "monitor_layout",
    hdrs = ["monitor_layout.h"],
    visibility = ["//pkg/sentry/platform/hvf:__pkg__"],
)

cc_binary(
    name = "monitor_object",
    srcs = [
        "monitor.c",
        "monitor_arm64.S",
        "monitor_layout.h",
    ],
    copts = [
        "-ffreestanding",
        "-fno-builtin",
        "-fno-stack-protector",
        "-fno-unwind-tables",
        "-fno-asynchronous-unwind-tables",
        "-fno-jump-tables",
        "-fno-vectorize",
        "-fno-slp-vectorize",
        "-Oz",
        "-mmacosx-version-min=15.0",
    ],
    linkopts = [
        "-Wl,-e,_ttc_monitor_bootstrap",
        "-mmacosx-version-min=15.0",
    ],
    visibility = ["//pkg/sentry/platform/hvf:__pkg__"],
)
