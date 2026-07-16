load("//tools:defs.bzl", "go_library")
load("//tools/go_generics:defs.bzl", "go_template_instance")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_template_instance(
    name = "shm_refs",
    out = "shm_refs.go",
    consts = {
        "enableLogging": "true",
    },
    package = "shm",
    prefix = "Shm",
    template = "//pkg/refs:refs_template",
    types = {
        "T": "Shm",
    },
)

go_template_instance(
    name = "shm_file_range_set_impl",
    out = "shm_file_range_set_impl.go",
    imports = {
        "memmap": "gvisor.dev/gvisor/pkg/sentry/memmap",
    },
    package = "shm",
    prefix = "ShmFileRange",
    template = "//pkg/segment:generic_set",
    types = {
        "Key": "uint64",
        "Range": "memmap.MappableRange",
        "Value": "uint64",
        "Functions": "ShmFileRangeSetFunctions",
    },
)

go_library(
    name = "shm",
    srcs = [
        "context.go",
        "range_set.go",
        "shm.go",
        "shm_file_range_set_impl.go",
        "shm_refs.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/context",
        "//pkg/errors/linuxerr",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/refs",
        "//pkg/safemem",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/kernel/ipc",
        "//pkg/sentry/ktime",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/usage",
        "//pkg/sentry/vfs",
        "//pkg/sync",
        "//pkg/usermem",
    ],
)
