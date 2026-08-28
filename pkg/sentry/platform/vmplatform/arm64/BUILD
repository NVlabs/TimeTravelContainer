load("//tools:defs.bzl", "go_library", "go_test")

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
        "//pkg/cpuid",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/sentry/arch",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/vmplatform",
        "//pkg/sentry/platform/vmplatform/vmm",
    ],
)

go_test(
    name = "arm64_test",
    size = "small",
    srcs = ["arm64_test.go"],
    library = ":arm64",
    deps = [
        "//pkg/abi/linux",
        "//pkg/hostarch",
        "//pkg/sentry/arch",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/vmplatform",
    ],
)
