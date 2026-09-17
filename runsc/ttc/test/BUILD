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

# Manual because it deliberately SIGKILLs every daemon in a two-voter cluster,
# then verifies sequential quorum recovery and live-runtime reauthorization.
sh_binary(
    name = "e2e_cluster_restart",
    srcs = ["e2e_cluster_restart.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it starts a three-node cluster, creates a configurable
# high-fanout family across nodes, and retires a node through bulk migration.
sh_binary(
    name = "e2e_distributed_fanout",
    srcs = ["e2e_distributed_fanout.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it pulls Python and runs a sustained page-checked heap plus a
# synchronous SQLite WAL workload through remote fork, migration, and cold
# lifecycle operations.
sh_binary(
    name = "e2e_distributed_workload",
    srcs = ["e2e_distributed_workload.sh"],
    data = ["workload_fixture.py"],
    tags = [
        "manual",
    ],
)

# Manual because it pulls an image and composes all of the lower-frequency CLI
# lifecycle paths, including checkpoint/restore and dehydration.
sh_binary(
    name = "e2e_cli_commands",
    srcs = ["e2e_cli_commands.sh"],
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

# Manual because it starts two compute daemons plus an outbound-only
# login-node gateway, and binds a host port to test placement-aware routing.
sh_binary(
    name = "e2e_control_only",
    srcs = ["e2e_control_only.sh"],
    tags = [
        "manual",
    ],
)

# Manual multi-daemon pressure/rebalance qualification. It intentionally
# allocates hundreds of MiB and performs live migrations.
sh_binary(
    name = "e2e_elastic_pressure",
    srcs = ["e2e_elastic_pressure.sh"],
    tags = [
        "manual",
    ],
)

# Manual three-daemon qualification that kills a draining daemon after its
# durable intent is visible, restarts it, and verifies automatic evacuation.
sh_binary(
    name = "e2e_elastic_drain_recovery",
    srcs = ["e2e_elastic_drain_recovery.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it kills the active shared-GC coordinator and verifies that
# a shared-root retain waits through fenced lease takeover and ledger rebuild.
sh_binary(
    name = "e2e_shared_gc_turnover",
    srcs = ["e2e_shared_gc_turnover.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it starts a clustered daemon and deliberately races fresh
# start and rehydration against asynchronous cold publication.
sh_binary(
    name = "e2e_publication_restart",
    srcs = ["e2e_publication_restart.sh"],
    tags = [
        "manual",
    ],
)

# Manual because it starts three clustered daemons and composes branch state
# transitions with graceful node and daemon retirement policies.
sh_binary(
    name = "e2e_lifecycle_state_matrix",
    srcs = ["e2e_lifecycle_state_matrix.sh"],
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
