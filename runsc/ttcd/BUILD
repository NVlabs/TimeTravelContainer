load("//tools:defs.bzl", "go_binary", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "ttcd",
    srcs = ["main.go"],
    pure = True,
    tags = ["staging"],
    visibility = ["//visibility:public"],
    x_defs = {"gvisor.dev/gvisor/runsc/version.version": "{STABLE_VERSION}"},
    deps = [
        "//runsc/cli/maincli",
        "//runsc/ttc/cmd",
        "//runsc/version",
    ],
)

go_test(
    name = "ttcd_test",
    size = "small",
    srcs = [
        "main.go",
        "main_test.go",
    ],
    deps = [
        "//runsc/cli/maincli",
        "//runsc/ttc/cmd",
        "//runsc/version",
    ],
)
