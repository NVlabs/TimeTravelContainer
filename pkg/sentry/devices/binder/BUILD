load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "binder",
    srcs = [
        "abi.go",
        "alloc.go",
        "core.go",
        "driver.go",
        "read.go",
        "refs.go",
        "release.go",
        "save_restore.go",
        "write.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/errors/linuxerr",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/safemem",
        "//pkg/sentry/arch",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/usage",
        "//pkg/sentry/vfs",
        "//pkg/sync",
        "//pkg/usermem",
        "//pkg/waiter",
    ],
)

go_test(
    name = "binder_test",
    size = "small",
    srcs = ["binder_test.go"],
    library = ":binder",
    deps = [
        "//pkg/hostarch",
        "//pkg/sentry/contexttest",
        "//pkg/sentry/kernel",
        "//pkg/sentry/memmap",
    ],
)
