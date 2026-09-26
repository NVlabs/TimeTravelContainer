load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "protocol",
    srcs = ["protocol.go"],
    visibility = [
        "//runsc/ttc/client:__pkg__",
        "//runsc/ttc/cmd:__pkg__",
    ],
)
