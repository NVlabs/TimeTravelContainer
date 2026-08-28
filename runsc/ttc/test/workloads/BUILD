load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "stress",
    testonly = 1,
    srcs = ["stress.go"],
    features = ["fully_static_link"],
    visibility = ["//visibility:public"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

filegroup(
    name = "stress_source",
    srcs = ["stress.go"],
    visibility = ["//visibility:public"],
)
