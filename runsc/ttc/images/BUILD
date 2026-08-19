load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "images",
    srcs = [
        "images.go",
        "credentials.go",
        "rootfs.go",
        "native_store.go",
    ],
    visibility = ["//runsc/ttc:__subpackages__"],
    deps = [
        "//runsc/ttc/ownership",
        "@com_github_containerd_containerd_v2//core/content",
        "@com_github_containerd_errdefs//:go_default_library",
        "@com_github_containerd_containerd_v2//core/images",
        "@com_github_containerd_containerd_v2//core/images/archive",
        "@com_github_containerd_containerd_v2//core/remotes",
        "@com_github_containerd_containerd_v2//core/remotes/docker",
        "@com_github_containerd_containerd_v2//pkg/archive",
        "@com_github_containerd_containerd_v2//pkg/archive/compression",
        "@com_github_containerd_containerd_v2//plugins/content/local",
        "@com_github_containerd_platforms//:platforms",
        "@com_github_distribution_reference//:go_default_library",
        "@com_github_opencontainers_image_spec//specs-go/v1:specs-go",
    ],
)

go_test(
    name = "images_test",
    size = "small",
    srcs = [
        "images_test.go",
        "rootfs_test.go",
    ],
    library = ":images",
    deps = [
        "@com_github_containerd_containerd_v2//core/content",
        "@com_github_containerd_containerd_v2//core/images",
        "@com_github_containerd_containerd_v2//core/images/archive",
        "@com_github_containerd_platforms//:platforms",
        "@com_github_opencontainers_image_spec//specs-go/v1:specs-go",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
