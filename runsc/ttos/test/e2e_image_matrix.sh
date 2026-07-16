#!/bin/sh
# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# This is a manual, networked end-to-end matrix. It intentionally uses public,
# unmodified Docker images and a real containerd content service.
set -eu

TTOS=${TTOS:-}
RUNSC=${RUNSC:-}
CONTAINERD_ADDRESS=${CONTAINERD_ADDRESS:-}
TTOS_NAMESPACE=${TTOS_NAMESPACE:-ttos-e2e}
TTOS_E2E_ROOT=${TTOS_E2E_ROOT:-}
TTOS_UNSAFE_NO_HOST_ISOLATION=${TTOS_UNSAFE_NO_HOST_ISOLATION:-0}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-busybox:1.36}
ALPINE_IMAGE=${ALPINE_IMAGE:-alpine:3.20}
PYTHON_IMAGE=${PYTHON_IMAGE:-python:3.12-alpine}
NGINX_IMAGE=${NGINX_IMAGE:-nginx:1.27-alpine}

fail() {
  echo "ttos e2e: $*" >&2
  exit 1
}

[ -n "$TTOS" ] || fail "set TTOS to the ttos binary"
[ -x "$TTOS" ] || fail "TTOS is not executable: $TTOS"
[ -n "$RUNSC" ] || fail "set RUNSC to the TimeTravel-enabled runsc binary"
[ -x "$RUNSC" ] || fail "RUNSC is not executable: $RUNSC"
[ -n "$CONTAINERD_ADDRESS" ] || fail "set CONTAINERD_ADDRESS to an accessible containerd socket"
[ -S "$CONTAINERD_ADDRESS" ] || fail "containerd socket does not exist: $CONTAINERD_ADDRESS"
if [ "$TTOS_UNSAFE_NO_HOST_ISOLATION" != 1 ]; then
  command -v unshare >/dev/null 2>&1 || fail "unshare is required for the rootless prerequisite probe"
  if ! unshare -Ur /bin/true >/dev/null 2>&1; then
    fail "rootless UID/GID mapping is unavailable; enable unprivileged user namespaces or set TTOS_UNSAFE_NO_HOST_ISOLATION=1 for trusted workloads"
  fi
fi

owned_root=false
if [ -z "$TTOS_E2E_ROOT" ]; then
  TTOS_E2E_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/ttos-e2e.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTOS_E2E_ROOT"
fi
DATA_ROOT="$TTOS_E2E_ROOT/data"
RUNSC_ROOT="$TTOS_E2E_ROOT/runsc"
HOST_FIXTURE="$TTOS_E2E_ROOT/host-fixture"
mkdir -p "$HOST_FIXTURE"
echo host-readonly >"$HOST_FIXTURE/value"
PREFIX="e2e-$$"
LIFECYCLE="$PREFIX-lifecycle"
CONSOLE_STRESS="$PREFIX-console-stress"
VOLUME="$PREFIX-state"

ttos() {
  if [ "$TTOS_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
    "$TTOS" \
      --unsafe-no-host-isolation \
      --containerd "$CONTAINERD_ADDRESS" \
      --namespace "$TTOS_NAMESPACE" \
      --data-root "$DATA_ROOT" \
      --root "$RUNSC_ROOT" \
      --runsc "$RUNSC" \
      "$@"
    return
  fi
  "$TTOS" \
    --containerd "$CONTAINERD_ADDRESS" \
    --namespace "$TTOS_NAMESPACE" \
    --data-root "$DATA_ROOT" \
    --root "$RUNSC_ROOT" \
    --runsc "$RUNSC" \
    "$@"
}

cleanup() {
  for container in "$PREFIX-busybox" "$PREFIX-alpine" "$PREFIX-python" "$PREFIX-nginx" "$CONSOLE_STRESS" "$LIFECYCLE"; do
    ttos rm --force "$container" >/dev/null 2>&1 || true
  done
  ttos volume rm "$VOLUME" >/dev/null 2>&1 || true
  if $owned_root; then
    rm -rf "$TTOS_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

echo "== pulling public image matrix =="
for image in "$BUSYBOX_IMAGE" "$ALPINE_IMAGE" "$PYTHON_IMAGE" "$NGINX_IMAGE"; do
  ttos pull "$image"
done

echo "== image inventory =="
IMAGE_LIST="$TTOS_E2E_ROOT/images.json"
ttos images --json >"$IMAGE_LIST"
for reference in docker.io/library/busybox:1.36 docker.io/library/alpine:3.20 docker.io/library/python:3.12-alpine docker.io/library/nginx:1.27-alpine; do
  grep -q "\"reference\": \"$reference\"" "$IMAGE_LIST" || fail "image inventory omitted $reference"
done
[ "$(ttos images -q | wc -l)" -ge 4 ] || fail "quiet image inventory omitted an image ID"

echo "== one-shot compatibility probes =="
ttos create --name "$PREFIX-busybox" "$BUSYBOX_IMAGE" sh -c \
  'test -r /proc/self/status && test -r /sys/kernel && echo busybox-ok'
[ "$(ttos inspect -f '{{.State.Status}}|{{.Config.Image}}|{{.Path}}|{{len .TTOS.Branches}}' "$PREFIX-busybox")" = \
  "created|docker.io/library/busybox:1.36|sh|1" ] ||
  fail "created-container inspect did not expose Docker config and TTOS branch state"
ttos start --attach "$PREFIX-busybox"
[ "$(ttos inspect -f '{{.State.Status}}|{{.State.ExitCode}}|{{.State.Running}}' "$PREFIX-busybox")" = \
  "exited|0|false" ] ||
  fail "exited-container inspect did not expose exit state"
ttos rm "$PREFIX-busybox"
ttos image inspect "$BUSYBOX_IMAGE" |
  grep -q '"Materialized": true' ||
  fail "image inspection did not report the materialized BusyBox root"

ttos run --name "$PREFIX-alpine" "$ALPINE_IMAGE" sh -c \
  'grep -q Alpine /etc/os-release && apk --version >/dev/null && echo alpine-ok'
ttos rm "$PREFIX-alpine"

ttos run --name "$PREFIX-python" "$PYTHON_IMAGE" python3 -c \
  'import json, sqlite3, threading; db=sqlite3.connect(":memory:"); db.execute("create table t(v)"); db.execute("insert into t values (7)"); assert db.execute("select v from t").fetchone()[0] == 7; t=threading.Thread(target=lambda: json.dumps({"ok": True})); t.start(); t.join(); print("python-ok")'
ttos rm "$PREFIX-python"

ttos run --name "$PREFIX-nginx" "$NGINX_IMAGE" nginx -t
ttos rm "$PREFIX-nginx"

echo "== quick-exit console integrity =="
STRESS_BYTES=262144
STRESS_RUN_STDOUT="$TTOS_E2E_ROOT/console-stress-run.stdout"
STRESS_RUN_STDERR="$TTOS_E2E_ROOT/console-stress-run.stderr"
STRESS_LOG_STDOUT="$TTOS_E2E_ROOT/console-stress-log.stdout"
STRESS_LOG_STDERR="$TTOS_E2E_ROOT/console-stress-log.stderr"
ttos run --name "$CONSOLE_STRESS" "$PYTHON_IMAGE" python3 -c \
  'import sys; n=262144; sys.stdout.buffer.write(b"A"*n+b"\n"); sys.stdout.buffer.flush(); sys.stderr.buffer.write(b"B"*n+b"\n"); sys.stderr.buffer.flush()' \
  >"$STRESS_RUN_STDOUT" 2>"$STRESS_RUN_STDERR"
ttos logs "$CONSOLE_STRESS" >"$STRESS_LOG_STDOUT" 2>"$STRESS_LOG_STDERR"

EXPECTED_A=$( \
  { head -c "$STRESS_BYTES" /dev/zero | tr '\0' A; printf '\n'; } | \
    sha256sum | awk '{print $1}' \
)
EXPECTED_B=$( \
  { head -c "$STRESS_BYTES" /dev/zero | tr '\0' B; printf '\n'; } | \
    sha256sum | awk '{print $1}' \
)
[ "$(wc -c <"$STRESS_RUN_STDOUT")" -eq $((STRESS_BYTES + 1)) ] || fail "foreground stdout was truncated"
[ "$(sed '/^ttos: warning:/d' "$STRESS_RUN_STDERR" | wc -c)" -eq $((STRESS_BYTES + 1)) ] || fail "foreground stderr was truncated"
[ "$(wc -c <"$STRESS_LOG_STDOUT")" -eq $((STRESS_BYTES + 1)) ] || fail "persisted stdout was truncated"
[ "$(sed '/^ttos: warning:/d' "$STRESS_LOG_STDERR" | wc -c)" -eq $((STRESS_BYTES + 1)) ] || fail "persisted stderr was truncated"
[ "$(sha256sum "$STRESS_RUN_STDOUT" | awk '{print $1}')" = "$EXPECTED_A" ] || fail "foreground stdout content changed"
[ "$(sed '/^ttos: warning:/d' "$STRESS_RUN_STDERR" | sha256sum | awk '{print $1}')" = "$EXPECTED_B" ] || fail "foreground stderr content changed"
[ "$(sha256sum "$STRESS_LOG_STDOUT" | awk '{print $1}')" = "$EXPECTED_A" ] || fail "persisted stdout content changed"
[ "$(sed '/^ttos: warning:/d' "$STRESS_LOG_STDERR" | sha256sum | awk '{print $1}')" = "$EXPECTED_B" ] || fail "persisted stderr content changed"
ttos rm "$CONSOLE_STRESS"

echo "== detached lifecycle, managed-volume CoW, and live fork =="
ttos volume create "$VOLUME"
ttos run --name "$LIFECYCLE" --detach \
  --log-opt max-size=64k --log-opt max-file=3 \
  --volume "$VOLUME:/state" \
  --volume "$HOST_FIXTURE:/host-fixture:ro" \
  "$ALPINE_IMAGE" sh -c \
  'echo parent >/state/value; i=0; while :; do echo "tick:$i"; i=$((i+1)); sleep 1; done'

deadline=$(( $(date +%s) + 15 ))
while ! ttos logs --tail 10 "$LIFECYCLE" 2>/dev/null | grep -q 'tick:'; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "detached init did not produce console output"
  sleep 1
done

[ "$(ttos exec "$LIFECYCLE" cat /state/value)" = "parent" ] || fail "parent managed volume was not initialized"
[ "$(ttos exec "$LIFECYCLE" cat /host-fixture/value)" = "host-readonly" ] || fail "read-only host bind was not readable"
if ttos exec "$LIFECYCLE" sh -c 'echo forbidden >/host-fixture/value' >/dev/null 2>&1; then
  fail "read-only host bind accepted a write"
fi
ttos fork --check "$LIFECYCLE"
ttos fork "$LIFECYCLE" child

RUNNING_INSPECT=$(ttos inspect -f '{{.State.Status}}|{{.Config.Image}}|{{len .TTOS.Branches}}' "$LIFECYCLE")
[ "$RUNNING_INSPECT" = "running|docker.io/library/alpine:3.20|2" ] ||
  fail "running forked inspect returned $RUNNING_INSPECT"
INSPECT_JSON="$TTOS_E2E_ROOT/lifecycle-inspect.json"
ttos inspect --format json "$LIFECYCLE" >"$INSPECT_JSON"
grep -q "\"Name\":\"$VOLUME\"" "$INSPECT_JSON" || fail "inspect omitted the managed volume"
grep -q "\"Source\":\"$HOST_FIXTURE\"" "$INSPECT_JSON" || fail "inspect omitted the read-only host bind"
grep -q '"processes":\[' "$INSPECT_JSON" || fail "inspect omitted live branch processes"
FORK_INSPECT=$(ttos inspect --fork-check -f '{{range .TTOS.Branches}}{{.Metadata.ID}}={{if .ForkEligibility}}{{.ForkEligibility.Eligible}}{{else}}error{{end}} {{end}}' "$LIFECYCLE")
echo "$FORK_INSPECT" | grep -q 'main=true' || fail "inspect fork check omitted eligible main branch: $FORK_INSPECT"
echo "$FORK_INSPECT" | grep -q 'child=true' || fail "inspect fork check omitted eligible child branch: $FORK_INSPECT"

PS_CHILD=$(ttos ps --filter branch=child --filter status=running --filter ancestor="$ALPINE_IMAGE" --format '{{.ID}}')
[ "$PS_CHILD" = "$LIFECYCLE@child" ] || fail "filtered ps selected $PS_CHILD instead of the child branch"
[ "$(ttos ps --latest --quiet)" = "$LIFECYCLE@child" ] || fail "ps --latest did not select the forked child"
ttos ps --filter branch=child --format json |
  grep -q '"created_at":"' ||
  fail "ps JSON omitted the child creation timestamp"

ttos exec "$LIFECYCLE@child" sh -c 'echo child >/state/value'
[ "$(ttos exec "$LIFECYCLE" cat /state/value)" = "parent" ] || fail "child write leaked into parent managed volume"
[ "$(ttos exec "$LIFECYCLE@child" cat /state/value)" = "child" ] || fail "child managed-volume write was not retained"

ttos pause "$LIFECYCLE" "$LIFECYCLE@child"
PAUSED_INSPECT=$(ttos inspect -f '{{range .TTOS.Branches}}{{.Metadata.ID}}={{.Metadata.State}} {{end}}' "$LIFECYCLE")
echo "$PAUSED_INSPECT" | grep -q 'main=paused' || fail "inspect did not report paused main branch: $PAUSED_INSPECT"
echo "$PAUSED_INSPECT" | grep -q 'child=paused' || fail "inspect did not report paused child branch: $PAUSED_INSPECT"
ttos resume "$LIFECYCLE" "$LIFECYCLE@child"
ttos ps --all
ttos top "$LIFECYCLE"
ttos ls --tree
ttos logs --tail 2 "$LIFECYCLE"
ttos logs --tail 2 "$LIFECYCLE@child"

ttos stop --time 2 "$LIFECYCLE@child" "$LIFECYCLE"
[ "$(ttos inspect -f '{{.State.Status}}|{{.State.Running}}' "$LIFECYCLE")" = "exited|false" ] ||
  fail "inspect did not report the stopped family as exited"
ttos branch rm "$LIFECYCLE@child"
ttos rm "$LIFECYCLE"
ttos volume rm "$VOLUME"

trap - EXIT HUP INT TERM
if $owned_root; then
  rm -rf "$TTOS_E2E_ROOT"
fi
echo "TTOS end-to-end image matrix passed"
