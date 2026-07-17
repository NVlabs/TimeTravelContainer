package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

# Manual because it pulls public images and requires a real containerd socket
# plus host support for rootless UID/GID mappings.
sh_binary(
    name = "e2e_image_matrix",
    srcs = ["e2e_image_matrix.sh"],
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
