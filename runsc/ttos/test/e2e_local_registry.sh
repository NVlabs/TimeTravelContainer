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

# This is a manual, hermetic registry integration test. The registry and guest
# image are repository-built fixtures; the test makes no public network request.
set -eu

TTOS=${TTOS:-}
RUNSC=${RUNSC:-}
REGISTRY_FIXTURE=${REGISTRY_FIXTURE:-}
FIXTURE_BINARY=${FIXTURE_BINARY:-}
TTOS_E2E_ROOT=${TTOS_E2E_ROOT:-}
TTOS_UNSAFE_NO_HOST_ISOLATION=${TTOS_UNSAFE_NO_HOST_ISOLATION:-0}

fail() {
  echo "ttos local registry: $*" >&2
  exit 1
}

[ -n "$TTOS" ] || fail "set TTOS"
[ -n "$RUNSC" ] || fail "set RUNSC"
[ -n "$REGISTRY_FIXTURE" ] || fail "set REGISTRY_FIXTURE"
[ -n "$FIXTURE_BINARY" ] || fail "set FIXTURE_BINARY"
[ -x "$TTOS" ] || fail "TTOS is not executable: $TTOS"
[ -x "$RUNSC" ] || fail "RUNSC is not executable: $RUNSC"
[ -x "$REGISTRY_FIXTURE" ] || fail "REGISTRY_FIXTURE is not executable: $REGISTRY_FIXTURE"
[ -x "$FIXTURE_BINARY" ] || fail "FIXTURE_BINARY is not executable: $FIXTURE_BINARY"

if [ "$TTOS_UNSAFE_NO_HOST_ISOLATION" != 1 ] && ! unshare -Ur /bin/true >/dev/null 2>&1; then
  fail "rootless UID/GID mapping is unavailable; set TTOS_UNSAFE_NO_HOST_ISOLATION=1 only for trusted test workloads"
fi

owned_root=false
if [ -z "$TTOS_E2E_ROOT" ]; then
  TTOS_E2E_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/ttos-local-registry.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTOS_E2E_ROOT"
fi
ADDRESS_FILE="$TTOS_E2E_ROOT/registry-address"
REGISTRY_PID=

ttos() {
  "$TTOS" \
    --data-root "$TTOS_E2E_ROOT/data" \
    --daemon-socket "$TTOS_E2E_ROOT/ttosd.sock" \
    --root "$TTOS_E2E_ROOT/runsc" \
    --runsc "$RUNSC" \
    "$@"
}

cleanup() {
  if [ -n "$REGISTRY_PID" ]; then
    kill "$REGISTRY_PID" >/dev/null 2>&1 || true
    wait "$REGISTRY_PID" >/dev/null 2>&1 || true
  fi
  ttos daemon stop >/dev/null 2>&1 || true
  if [ "$owned_root" = true ]; then
    rm -rf "$TTOS_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

if [ "$TTOS_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
  ttos daemon start --unsafe-no-host-isolation
else
  ttos daemon start
fi

"$REGISTRY_FIXTURE" --binary "$FIXTURE_BINARY" --address-file "$ADDRESS_FILE" &
REGISTRY_PID=$!
i=0
while [ ! -s "$ADDRESS_FILE" ] && [ "$i" -lt 100 ]; do
  kill -0 "$REGISTRY_PID" >/dev/null 2>&1 || fail "registry fixture exited before publishing its address"
  i=$((i + 1))
  sleep 0.05
done
[ -s "$ADDRESS_FILE" ] || fail "registry fixture did not publish its address"
IMAGE=$(cat "$ADDRESS_FILE")

FIRST_PULL=$(ttos pull "$IMAGE")
echo "$FIRST_PULL" | grep -Eq 'Content: [1-9][0-9]* bytes downloaded' ||
  fail "first pull did not download fixture content: $FIRST_PULL"
SECOND_PULL=$(ttos pull "$IMAGE")
echo "$SECOND_PULL" | grep -Eq 'Content: 0 bytes downloaded, [1-9][0-9]* bytes reused' ||
  fail "second pull did not reuse fixture content: $SECOND_PULL"

kill "$REGISTRY_PID"
wait "$REGISTRY_PID" >/dev/null 2>&1 || true
REGISTRY_PID=

[ "$(ttos run --rm --pull never "$IMAGE")" = "ttos-local-registry-ok" ] ||
  fail "cached fixture did not run after the registry stopped"
ttos image inspect "$IMAGE" >/dev/null
ttos image rm --force "$IMAGE" >/dev/null

echo "TTOS hermetic local-registry integration passed"
