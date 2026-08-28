load("//tools:defs.bzl", "go_library", "go_test", "select_system")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

genrule(
    name = "monitor_image_go",
    srcs = select_system(
        darwin = ["//pkg/sentry/platform/hvf/monitor:monitor_object"],
        linux = [],
    ),
    outs = ["monitor_image_generated.go"],
    cmd = select_system(
        darwin = "$(location //pkg/sentry/platform/hvf/monitor/imagegen) -input $(location //pkg/sentry/platform/hvf/monitor:monitor_object) -output $@",
        linux = "printf '// Code generated for a non-Darwin build; DO NOT EDIT.\\n\\npackage hvf\\n' > $@",
    ),
    tools = select_system(
        darwin = ["//pkg/sentry/platform/hvf/monitor/imagegen"],
        linux = [],
    ),
)

go_library(
    name = "hvf",
    srcs = [
        "backend_darwin_arm64.go",
        "backend_unsupported.go",
        ":monitor_image_go",
        "monitor_darwin_arm64.go",
        "ring0_darwin_arm64.go",
        "ring0_engine_darwin_arm64.go",
        "ring0_runtime_unsafe.go",
    ],
    bazel_cgo = True,
    bazel_cdeps = select_system(
        darwin = ["//pkg/sentry/platform/hvf/monitor:monitor_layout"],
        linux = [],
    ),
    bazel_clinkopts = [
        "-framework Hypervisor",
        "-framework CoreFoundation",
        "-mmacosx-version-min=15.0",
    ],
    stateify = False,
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/hostarch",
        "//pkg/hosttid",
        "//pkg/sentry/platform/hvf/ring0asm",
        "//pkg/sentry/platform/vmplatform",
        "//pkg/sentry/platform/vmplatform/monitorabi",
        "//pkg/sentry/platform/vmplatform/vmm",
    ],
)

go_test(
    name = "hvf_test",
    size = "small",
    pure = "off",
    srcs = select_system(
        darwin = [
            "backend_darwin_arm64_test.go",
            "ring0_engine_darwin_arm64_test.go",
        ],
        linux = [],
    ),
    library = ":hvf",
    deps = [
        "//pkg/context",
        "//pkg/hostarch",
        "//pkg/safemem",
        "//pkg/sentry/arch",
        "//pkg/sentry/memmap",
        "//pkg/sentry/pgalloc",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/vmplatform",
        "//pkg/sentry/platform/hvf/ring0asm",
        "//pkg/sentry/platform/vmplatform/arm64",
        "//pkg/sentry/platform/vmplatform/monitorabi",
        "//pkg/sentry/platform/vmplatform/vmm",
        "//pkg/sentry/time",
        "//pkg/sentry/usage",
    ],
)
