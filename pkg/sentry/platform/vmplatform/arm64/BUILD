load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "arm64",
    srcs = ["arm64.go"],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/sentry/arch",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/vmplatform",
        "//pkg/sentry/platform/vmplatform/vmm",
    ],
)
