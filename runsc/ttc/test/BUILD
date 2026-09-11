package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

# Manual because it pulls public images and requires host support for rootless
# UID/GID mappings.
sh_binary(
    name = "e2e_image_matrix",
    srcs = ["e2e_image_matrix.sh"],
    tags = [
        "manual",
    ],
)

sh_binary(
    name = "e2e_managed_volumes",
    srcs = ["e2e_managed_volumes.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it pulls a public image and starts a rootless sandbox.
sh_binary(
    name = "e2e_cp",
    srcs = ["e2e_cp.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it pulls public images and starts a privileged, isolated
# Docker-in-Docker daemon. The host Docker configuration is not modified.
sh_binary(
    name = "e2e_docker_main",
    srcs = ["e2e_docker_main.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it requires a real PTY and a public image.
sh_binary(
    name = "e2e_terminal",
    srcs = ["e2e_terminal.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it installs fuse-overlayfs from a public Ubuntu repository,
# then exercises an in-sandbox FUSE daemon across TTC lifecycle operations.
sh_binary(
    name = "e2e_fuse_lifecycle",
    srcs = ["e2e_fuse_lifecycle.sh"],
    tags = [
        "manual",
    ],
)

# Hermetic registry integration: both the server and OCI guest are repository-built.
sh_binary(
    name = "e2e_local_registry",
    srcs = ["e2e_local_registry.sh"],
    tags = [
        "manual",
    ],
)
