load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "client",
    srcs = [
        "client.go",
        "host_darwin.go",
        "host_linux.go",
        "terminal.go",
    ],
    visibility = ["//runsc/ttc:__pkg__"],
    deps = [
        "//runsc/processutil",
        "//runsc/ttc/protocol",
        "//runsc/version",
        "@org_golang_x_term//:go_default_library",
    ],
)

go_test(
    name = "client_test",
    size = "small",
    srcs = ["client_test.go"],
    library = ":client",
)
