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
TTCD=${TTCD:-}
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
[ -n "$TTCD" ] || fail "set TTCD"
[ -n "$REGISTRY_FIXTURE" ] || fail "set REGISTRY_FIXTURE"
[ -n "$FIXTURE_BINARY" ] || fail "set FIXTURE_BINARY"
[ -x "$TTC" ] || fail "TTC is not executable: $TTC"
[ -x "$TTCD" ] || fail "TTCD is not executable: $TTCD"
[ -x "$REGISTRY_FIXTURE" ] || fail "REGISTRY_FIXTURE is not executable: $REGISTRY_FIXTURE"
[ -x "$FIXTURE_BINARY" ] || fail "FIXTURE_BINARY is not executable: $FIXTURE_BINARY"

# Rootless Linux sandboxes need user namespaces; macOS confines ttcd with
# Seatbelt instead.
if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" != 1 ] && [ "$(uname -s)" = Linux ] &&
  ! unshare -Ur /bin/true >/dev/null 2>&1; then
  fail "rootless UID/GID mapping is unavailable; set TTC_UNSAFE_NO_HOST_ISOLATION=1 only for trusted test workloads"
fi

owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  # macOS gives TMPDIR a trailing slash; a doubled one would differ from
  # the cleaned paths TTC reports.
  tmp=${TMPDIR:-/tmp}
  TTC_E2E_ROOT=$(mktemp -d "${tmp%/}/ttc-registry.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTC_E2E_ROOT"
fi
ADDRESS_FILE="$TTC_E2E_ROOT/registry-address"
REGISTRY_PID=
BIN_DIR="$TTC_E2E_ROOT/bin"
mkdir -p "$BIN_DIR"
install -m 0755 "$(readlink -f "$TTC")" "$BIN_DIR/ttc"
install -m 0755 "$(readlink -f "$TTCD")" "$BIN_DIR/ttcd"
if [ "$(uname -s)" = Darwin ]; then
  # A copy loses its ad-hoc signature; Hypervisor.framework requires the
  # entitlement on the executable at its final path.
  /usr/bin/codesign --force --sign - \
    --entitlements "$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)/hvf.entitlements" \
    "$BIN_DIR/ttcd" >/dev/null
fi
TTC="$BIN_DIR/ttc"
# The client finds the daemon through TTC_ROOT, which a node's TTC_LOCAL_ROOT
# would override.
unset TTC_LOCAL_ROOT
export TTC_ROOT="$TTC_E2E_ROOT/data"

ttc() {
  "$TTC" "$@"
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
