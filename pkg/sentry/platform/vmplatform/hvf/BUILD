load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hvf",
    srcs = ["platform_darwin_arm64.go"],
    stateify = False,
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
