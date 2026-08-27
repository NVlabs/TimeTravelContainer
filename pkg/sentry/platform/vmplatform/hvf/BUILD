load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hvf",
    srcs = ["platform_darwin_arm64.go"],
    stateify = False,
    target_compatible_with = [
        "@platforms//cpu:arm64",
        "@platforms//os:osx",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/fd",
        "//pkg/hostarch",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/hvf",
        "//pkg/sentry/platform/vmplatform",
        "//pkg/sentry/platform/vmplatform/arm64",
    ],
)

go_test(
    name = "hvf_test",
    size = "small",
    pure = "off",
    srcs = ["platform_darwin_arm64_test.go"],
    library = ":hvf",
    target_compatible_with = [
        "@platforms//cpu:arm64",
        "@platforms//os:osx",
    ],
    deps = ["//pkg/sentry/platform"],
)
