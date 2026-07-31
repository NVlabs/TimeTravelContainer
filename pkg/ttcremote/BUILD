load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcremote",
    srcs = [
        "model.go",
        "policy.go",
        "simulator.go",
        "trace.go",
    ],
    visibility = ["//visibility:public"],
)

go_test(
    name = "ttcremote_test",
    size = "small",
    srcs = [
        "model_test.go",
        "simulator_test.go",
    ],
    library = ":ttcremote",
)
