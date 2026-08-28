load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "copyhelper",
    srcs = ["helper.go"],
    visibility = [
        "//runsc/ttc/cmd:__pkg__",
        "//runsc/ttc/copyguest/cmd:__pkg__",
    ],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "copyhelper_test",
    size = "small",
    srcs = ["helper_test.go"],
    library = ":copyhelper",
)
