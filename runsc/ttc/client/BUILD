load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "client",
    srcs = [
        "client.go",
        "terminal.go",
    ],
    visibility = ["//runsc/ttc:__pkg__"],
    deps = [
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
