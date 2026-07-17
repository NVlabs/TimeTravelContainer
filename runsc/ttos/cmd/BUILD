load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "cmd",
    srcs = ["cmd.go", "completion.go", "daemon.go"],
    visibility = ["//runsc/ttos:__subpackages__", "//runsc/ttosd:__pkg__"],
    deps = [
        "//pkg/abi/linux",
        "//runsc/flag",
        "//runsc/ttos/bundle",
        "//runsc/ttos/console",
        "//runsc/ttos/images",
        "//runsc/version",
        "@com_github_moby_sys_signal//:go_default_library",
        "@com_github_opencontainers_runtime_spec//specs-go",
        "@org_golang_x_term//:go_default_library",
    ],
)

go_test(
    name = "cmd_test",
    size = "small",
    srcs = ["cmd_test.go", "daemon_test.go"],
    library = ":cmd",
    deps = [
        "//runsc/ttos/console",
        "@com_github_opencontainers_runtime_spec//specs-go",
    ],
)
