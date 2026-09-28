load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hostthread",
    srcs = [
        "hostthread.go",
        "hostthread_darwin.go",
        "hostthread_other.go",
    ],
    visibility = ["//:sandbox"],
)
