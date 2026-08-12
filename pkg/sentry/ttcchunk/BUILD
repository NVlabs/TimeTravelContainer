load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ttcchunk",
    srcs = [
		"assemble.go",
        "chunk.go",
		"donated.go",
		"install.go",
		"lifecycle.go",
		"manifest.go",
		"repository.go",
        "store.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
		"@org_golang_x_sys//unix",
        "//pkg/hostarch",
		"//pkg/sentry/checkpoint",
		"//pkg/safemem",
		"//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
		"//pkg/sentry/state/stateio",
		"//pkg/sentry/usage",
    ],
)

go_test(
    name = "ttcchunk_test",
    size = "small",
    srcs = ["chunk_test.go"],
    library = ":ttcchunk",
    deps = [
        "//pkg/hostarch",
		"//pkg/sentry/checkpoint",
        "//pkg/sentry/pgalloc",
    ],
)
