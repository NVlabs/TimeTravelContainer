load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "ttosd",
    srcs = ["main.go"],
    pure = True,
    tags = ["staging"],
    visibility = ["//visibility:public"],
    x_defs = {"gvisor.dev/gvisor/runsc/version.version": "{STABLE_VERSION}"},
    deps = ["//runsc/ttos/cmd"],
)
