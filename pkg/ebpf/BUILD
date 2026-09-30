load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ebpf",
    srcs = [
        "ebpf.go",
        "map.go",
        "registry.go",
        "types.go",
    ],
    imports = ["gvisor.dev/gvisor/pkg/abi/linux"],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/errors/linuxerr",
        "//pkg/sync",
    ],
)

go_test(
    name = "ebpf_test",
    size = "small",
    srcs = ["map_test.go"],
    library = ":ebpf",
    deps = [
        "//pkg/abi/linux",
        "//pkg/errors/linuxerr",
    ],
)
