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

TTC=${TTC:-}
RUNSC=${RUNSC:-}
REGISTRY_FIXTURE=${REGISTRY_FIXTURE:-}
FIXTURE_BINARY=${FIXTURE_BINARY:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
FIXTURE_GUEST_ARCH=${FIXTURE_GUEST_ARCH:-$(uname -m)}

case "$FIXTURE_GUEST_ARCH" in
  aarch64) FIXTURE_GUEST_ARCH=arm64 ;;
  x86_64) FIXTURE_GUEST_ARCH=amd64 ;;
esac

fail() {
  echo "ttc local registry: $*" >&2
  exit 1
}

[ -n "$TTC" ] || fail "set TTC"
[ -n "$RUNSC" ] || fail "set RUNSC"
[ -n "$REGISTRY_FIXTURE" ] || fail "set REGISTRY_FIXTURE"
[ -n "$FIXTURE_BINARY" ] || fail "set FIXTURE_BINARY"
[ -x "$TTC" ] || fail "TTC is not executable: $TTC"
[ -x "$RUNSC" ] || fail "RUNSC is not executable: $RUNSC"
[ -x "$REGISTRY_FIXTURE" ] || fail "REGISTRY_FIXTURE is not executable: $REGISTRY_FIXTURE"
[ -x "$FIXTURE_BINARY" ] || fail "FIXTURE_BINARY is not executable: $FIXTURE_BINARY"

if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" != 1 ] && ! unshare -Ur /bin/true >/dev/null 2>&1; then
  fail "rootless UID/GID mapping is unavailable; set TTC_UNSAFE_NO_HOST_ISOLATION=1 only for trusted test workloads"
fi

owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  TTC_E2E_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/ttc-local-registry.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTC_E2E_ROOT"
fi
ADDRESS_FILE="$TTC_E2E_ROOT/registry-address"
REGISTRY_PID=

ttc() {
  "$TTC" \
    --data-root "$TTC_E2E_ROOT/data" \
    --daemon-socket "$TTC_E2E_ROOT/ttcd.sock" \
    --root "$TTC_E2E_ROOT/runsc" \
    --runsc "$RUNSC" \
    "$@"
}

cleanup() {
  if [ -n "$REGISTRY_PID" ]; then
    kill "$REGISTRY_PID" >/dev/null 2>&1 || true
    wait "$REGISTRY_PID" >/dev/null 2>&1 || true
  fi
  ttc daemon stop >/dev/null 2>&1 || true
  if [ "$owned_root" = true ]; then
    rm -rf "$TTC_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
  ttc daemon start --unsafe-no-host-isolation
else
  ttc daemon start
fi

"$REGISTRY_FIXTURE" \
  --binary "$FIXTURE_BINARY" \
  --address-file "$ADDRESS_FILE" \
  --guest-os linux \
  --guest-arch "$FIXTURE_GUEST_ARCH" &
REGISTRY_PID=$!
i=0
while [ ! -s "$ADDRESS_FILE" ] && [ "$i" -lt 100 ]; do
  kill -0 "$REGISTRY_PID" >/dev/null 2>&1 || fail "registry fixture exited before publishing its address"
  i=$((i + 1))
  sleep 0.05
done
[ -s "$ADDRESS_FILE" ] || fail "registry fixture did not publish its address"
IMAGE=$(cat "$ADDRESS_FILE")

FIRST_PULL=$(ttc pull "$IMAGE")
echo "$FIRST_PULL" | grep -Eq 'Content: [1-9][0-9]* bytes downloaded' ||
  fail "first pull did not download fixture content: $FIRST_PULL"
SECOND_PULL=$(ttc pull "$IMAGE")
echo "$SECOND_PULL" | grep -Eq 'Content: 0 bytes downloaded, [1-9][0-9]* bytes reused' ||
  fail "second pull did not reuse fixture content: $SECOND_PULL"

kill "$REGISTRY_PID"
wait "$REGISTRY_PID" >/dev/null 2>&1 || true
REGISTRY_PID=

[ "$(ttc run --rm --pull never "$IMAGE")" = "ttc-local-registry-ok" ] ||
  fail "cached fixture did not run after the registry stopped"
ttc image inspect "$IMAGE" >/dev/null
ttc image rm --force "$IMAGE" >/dev/null

echo "TTC hermetic local-registry integration passed"
