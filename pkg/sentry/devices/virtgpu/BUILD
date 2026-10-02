load("//pkg/sync/locking:locking.bzl", "declare_mutex")
load("//tools:defs.bzl", "go_library", "go_test")
load("//tools/go_generics:defs.bzl", "go_template_instance")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

declare_mutex(
    name = "device_mutex",
    out = "device_mutex.go",
    package = "virtgpu",
    prefix = "device",
)

declare_mutex(
    name = "submit_mutex",
    out = "submit_mutex.go",
    package = "virtgpu",
    prefix = "submit",
)

declare_mutex(
    name = "render_fd_mutex",
    out = "render_fd_mutex.go",
    package = "virtgpu",
    prefix = "renderFD",
)

declare_mutex(
    name = "resource_maps_mutex",
    out = "resource_maps_mutex.go",
    package = "virtgpu",
    prefix = "resourceMaps",
)

go_template_instance(
    name = "resource_refs",
    out = "resource_refs.go",
    package = "virtgpu",
    prefix = "resource",
    template = "//pkg/refs:refs_template",
    types = {
        "T": "resource",
    },
)

go_library(
    name = "virtgpu",
    srcs = [
        "device.go",
        "device_mutex.go",
        "dmabuf.go",
        "fence.go",
        "ids.go",
        "render_fd.go",
        "render_fd_mutex.go",
        "renderer.go",
        "resource.go",
        "resource_maps_mutex.go",
        "resource_refs.go",
        "snapshot.go",
        "submit_mutex.go",
        "sync_file.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/context",
        "//pkg/errors",
        "//pkg/errors/linuxerr",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/marshal",
        "//pkg/marshal/primitive",
        "//pkg/refs",
        "//pkg/sentry/arch",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/ktime",
        "//pkg/sentry/memmap",
        "//pkg/sentry/vfs",
        "//pkg/sync",
        "//pkg/sync/locking",
        "//pkg/usermem",
        "//pkg/waiter",
    ],
)

go_test(
    name = "virtgpu_test",
    size = "small",
    srcs = ["virtgpu_test.go"],
    library = ":virtgpu",
    deps = [
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/errors",
        "//pkg/errors/linuxerr",
        "//pkg/fspath",
        "//pkg/hostarch",
        "//pkg/marshal",
        "//pkg/marshal/primitive",
        "//pkg/safemem",
        "//pkg/sentry/arch",
        "//pkg/sentry/fsimpl/testutil",
        "//pkg/sentry/fsimpl/tmpfs",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/limits",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/usage",
        "//pkg/sentry/vfs",
        "//pkg/state",
        "//pkg/waiter",
    ],
)
