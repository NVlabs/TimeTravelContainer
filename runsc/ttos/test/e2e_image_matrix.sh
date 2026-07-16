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
command -v unshare >/dev/null 2>&1 || fail "unshare is required for the rootless prerequisite probe"
if ! unshare -Ur /bin/true >/dev/null 2>&1; then
  fail "rootless UID/GID mapping is unavailable; configure unprivileged user namespaces and subordinate-ID policy (some AppArmor configurations also restrict this)"
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
PREFIX="e2e-$$"
LIFECYCLE="$PREFIX-lifecycle"
VOLUME="$PREFIX-state"

ttos() {
  "$TTOS" \
    --containerd "$CONTAINERD_ADDRESS" \
    --namespace "$TTOS_NAMESPACE" \
    --data-root "$DATA_ROOT" \
    --root "$RUNSC_ROOT" \
    --runsc "$RUNSC" \
    "$@"
}

cleanup() {
  for container in "$PREFIX-busybox" "$PREFIX-alpine" "$PREFIX-python" "$PREFIX-nginx" "$LIFECYCLE"; do
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

echo "== one-shot compatibility probes =="
ttos run --name "$PREFIX-busybox" "$BUSYBOX_IMAGE" sh -c \
  'test -r /proc/self/status && test -r /sys/kernel && echo busybox-ok'
ttos rm "$PREFIX-busybox"

ttos run --name "$PREFIX-alpine" "$ALPINE_IMAGE" sh -c \
  'grep -q Alpine /etc/os-release && apk --version >/dev/null && echo alpine-ok'
ttos rm "$PREFIX-alpine"

ttos run --name "$PREFIX-python" "$PYTHON_IMAGE" python3 -c \
  'import json, sqlite3, threading; db=sqlite3.connect(":memory:"); db.execute("create table t(v)"); db.execute("insert into t values (7)"); assert db.execute("select v from t").fetchone()[0] == 7; t=threading.Thread(target=lambda: json.dumps({"ok": True})); t.start(); t.join(); print("python-ok")'
ttos rm "$PREFIX-python"

ttos run --name "$PREFIX-nginx" "$NGINX_IMAGE" nginx -t
ttos rm "$PREFIX-nginx"

echo "== detached lifecycle, managed-volume CoW, and live fork =="
ttos volume create "$VOLUME"
ttos run --name "$LIFECYCLE" --detach \
  --log-opt max-size=64k --log-opt max-file=3 \
  --volume "$VOLUME:/state" \
  "$ALPINE_IMAGE" sh -c \
  'echo parent >/state/value; i=0; while :; do echo "tick:$i"; i=$((i+1)); sleep 1; done'

deadline=$(( $(date +%s) + 15 ))
while ! ttos logs --tail 10 "$LIFECYCLE" 2>/dev/null | grep -q 'tick:'; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "detached init did not produce console output"
  sleep 1
done

[ "$(ttos exec "$LIFECYCLE" cat /state/value)" = "parent" ] || fail "parent managed volume was not initialized"
ttos fork --check "$LIFECYCLE"
ttos fork "$LIFECYCLE" child

ttos exec "$LIFECYCLE@child" sh -c 'echo child >/state/value'
[ "$(ttos exec "$LIFECYCLE" cat /state/value)" = "parent" ] || fail "child write leaked into parent managed volume"
[ "$(ttos exec "$LIFECYCLE@child" cat /state/value)" = "child" ] || fail "child managed-volume write was not retained"

ttos pause "$LIFECYCLE" "$LIFECYCLE@child"
ttos resume "$LIFECYCLE" "$LIFECYCLE@child"
ttos ps --all
ttos top "$LIFECYCLE"
ttos ls --tree
ttos logs --tail 2 "$LIFECYCLE"
ttos logs --tail 2 "$LIFECYCLE@child"

ttos stop --time 2 "$LIFECYCLE@child" "$LIFECYCLE"
ttos branch rm "$LIFECYCLE@child"
ttos rm "$LIFECYCLE"
ttos volume rm "$VOLUME"

trap - EXIT HUP INT TERM
if $owned_root; then
  rm -rf "$TTOS_E2E_ROOT"
fi
echo "TTOS end-to-end image matrix passed"
