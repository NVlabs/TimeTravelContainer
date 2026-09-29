#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# This is a manual, networked compatibility test. It runs an isolated
# privileged Docker-in-Docker daemon so the host Docker configuration is never
# modified. The test validates the ordinary OCI main-container path; Docker
# does not know about TimeTravel branches.
set -eu

RUNSC=${RUNSC:-}
DIND_IMAGE=${DIND_IMAGE:-docker:29-dind}
TEST_IMAGE=${TEST_IMAGE:-busybox:1.36}
TEST_NAME=${TEST_NAME:-ttc-runsc-dind-$$}

fail() {
  echo "ttc Docker compatibility: $*" >&2
  exit 1
}

[ -n "$RUNSC" ] || fail "set RUNSC to the TimeTravel-enabled runsc binary"
[ -x "$RUNSC" ] || fail "RUNSC is not executable: $RUNSC"
command -v docker >/dev/null 2>&1 || fail "docker is required"

# ttcd selects its embedded runsc entry point from argv[0]. Preserve a
# runsc-prefixed basename when the release ttcd binary is supplied here.
RUNTIME_COPY=$(mktemp "${TMPDIR:-/tmp}/runsc-ttc.XXXXXX")
cleanup() {
  docker rm --force "$TEST_NAME" >/dev/null 2>&1 || true
  rm -f "$RUNTIME_COPY"
}
trap cleanup EXIT HUP INT TERM
cp "$RUNSC" "$RUNTIME_COPY"
chmod 0755 "$RUNTIME_COPY"

docker run --privileged --detach --name "$TEST_NAME" \
  --volume "$RUNTIME_COPY:/usr/local/bin/runsc:ro" \
  --entrypoint /bin/sh "$DIND_IMAGE" -c \
  '/usr/local/bin/runsc install --config_file=/etc/docker/daemon.json --runtime=runsc-ttc -- --network=none --platform=systrap && exec dockerd-entrypoint.sh' \
  >/dev/null

ready=false
i=0
while [ "$i" -lt 45 ]; do
  if docker exec "$TEST_NAME" docker info >/dev/null 2>&1; then
    ready=true
    break
  fi
  i=$((i + 1))
  sleep 1
done
if [ "$ready" != true ]; then
  docker logs "$TEST_NAME" >&2
  fail "nested Docker daemon did not become ready"
fi

docker exec "$TEST_NAME" docker info --format '{{json .Runtimes}}' |
  grep -q 'runsc-ttc' || fail "Docker did not register runsc-ttc"
docker exec "$TEST_NAME" docker pull "$TEST_IMAGE" >/dev/null
docker exec "$TEST_NAME" docker create --runtime=runsc-ttc --name oci-main \
  "$TEST_IMAGE" sh -c 'echo docker-runsc-started; while true; do sleep 1; done' \
  >/dev/null
docker exec "$TEST_NAME" docker start oci-main >/dev/null
[ "$(docker exec "$TEST_NAME" docker inspect --format '{{.HostConfig.Runtime}} {{.State.Status}}' oci-main)" = "runsc-ttc running" ] ||
  fail "Docker did not start oci-main with runsc-ttc"
[ "$(docker exec "$TEST_NAME" docker logs oci-main)" = "docker-runsc-started" ] ||
  fail "Docker logs did not return init output"
[ "$(docker exec "$TEST_NAME" docker exec oci-main sh -c 'echo docker-runsc-exec-ok')" = "docker-runsc-exec-ok" ] ||
  fail "Docker exec failed"
docker exec "$TEST_NAME" docker pause oci-main >/dev/null
[ "$(docker exec "$TEST_NAME" docker inspect --format '{{.State.Status}} {{.State.Paused}}' oci-main)" = "paused true" ] ||
  fail "Docker pause state was not preserved"
docker exec "$TEST_NAME" docker unpause oci-main >/dev/null
[ "$(docker exec "$TEST_NAME" docker inspect --format '{{.State.Status}} {{.State.Paused}}' oci-main)" = "running false" ] ||
  fail "Docker unpause state was not preserved"
docker exec "$TEST_NAME" docker stop --timeout 2 oci-main >/dev/null
[ "$(docker exec "$TEST_NAME" docker inspect --format '{{.State.Status}}' oci-main)" = "exited" ] ||
  fail "Docker stop did not produce an exited container"
docker exec "$TEST_NAME" docker rm oci-main >/dev/null

echo "TTC runsc Docker main-container compatibility passed"
