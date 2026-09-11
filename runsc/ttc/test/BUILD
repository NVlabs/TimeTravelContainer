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

# Manual because it starts and mutates a configurable high-fanout branch
# family against an already running TTC daemon.
sh_binary(
    name = "e2e_fanout_lifecycle",
    srcs = ["e2e_fanout_lifecycle.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it starts two clustered daemons and binds two host TCP ports.
sh_binary(
    name = "e2e_distributed_multihop",
    srcs = ["e2e_distributed_multihop.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it starts a three-voter cluster and deliberately kills the
# source-node daemon and every runtime in its host session.
sh_binary(
    name = "e2e_distributed_source_loss",
    srcs = ["e2e_distributed_source_loss.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it starts a three-node cluster, migrates live branches, and
# retires a node through scheduler-selected evacuation.
sh_binary(
    name = "e2e_elastic_lifecycle",
    srcs = ["e2e_elastic_lifecycle.sh"],
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
