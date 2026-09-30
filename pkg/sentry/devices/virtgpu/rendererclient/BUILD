load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "rendererclient",
    srcs = [
        "client.go",
        "memory.go",
        "memory_unsafe.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/gfxproto",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/safemem",
        "//pkg/sentry/devices/virtgpu",
        "//pkg/sentry/fsutil",
        "//pkg/sentry/memmap",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "rendererclient_test",
    size = "small",
    srcs = [
        "client_test.go",
        "renderer_test.go",
    ],
    data = ["//runsc/ttcgfx:ttc-gfx-renderer"],
    library = ":rendererclient",
    # TestRealRenderer uses the host's GPU, which Bazel's Darwin sandbox
    # denies.
    tags = ["no-sandbox"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/gfxproto",
        "//pkg/hostarch",
        "//pkg/safemem",
        "//pkg/sentry/devices/virtgpu",
        "//pkg/sentry/memmap",
        "//pkg/test/testutil",
        "//pkg/unet",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
