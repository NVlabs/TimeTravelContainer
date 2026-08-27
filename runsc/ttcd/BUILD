load("//tools:defs.bzl", "go_binary", "go_test", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "ttcd",
    srcs = ["main.go"],
    # Native macOS links Hypervisor.framework through the HVF backend. Linux
    # retains the repository's pure-Go default.
    pure = select_system(darwin = "off", linux = "auto"),
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
