load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcremote",
    srcs = ["tcp.go"],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/hostarch",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/state/stateio",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "ttcremote_test",
    size = "small",
    srcs = ["tcp_test.go"],
    library = ":ttcremote",
    deps = [
        "//pkg/hostarch",
        "//pkg/safemem",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/usage",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
