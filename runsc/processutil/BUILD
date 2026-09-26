load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "processutil",
    srcs = [
        "process_state.go",
        "process_state_darwin.go",
        "process_state_linux.go",
        "process_state_other.go",
        "process_table.go",
        "process_table_darwin.go",
        "process_table_linux.go",
    ],
    visibility = ["//runsc:__subpackages__"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
