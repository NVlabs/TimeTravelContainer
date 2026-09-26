load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "protocol",
    srcs = [
        "layout.go",
        "protocol.go",
    ],
    visibility = [
        "//runsc/ttc/client:__pkg__",
        "//runsc/ttc/cmd:__pkg__",
    ],
)

go_test(
    name = "protocol_test",
    size = "small",
    srcs = ["layout_test.go"],
    library = ":protocol",
)

