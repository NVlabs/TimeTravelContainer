#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Manual end-to-end coverage for the virtual GPU, --gfx: vulkaninfo, an
# offscreen render's checksum (test/gfx/vkoffscreen.c), and a fork with
# --gfx-fork=device-lost, in a container whose renderer is ttc-gfx-renderer
# and gfxstream on the host's GPU.
#
# It needs:
#   TTC and TTCD, the binaries;
#   TTC_GFX_RENDERER, ttc-gfx-renderer, and TTC_GFXSTREAM_LIBRARY, gfxstream's
#     host library (see runsc/ttc/gfxrenderer/README.md);
#   TTC_GFX_GUEST_DRIVER, a directory with Mesa's libvulkan_gfxstream.so for
#     the guest's architecture (runsc/ttc/gfxrenderer/guest/);
#   network access, to install vulkan-tools and a compiler in a Debian image.
#
# The daemon runs with host isolation unless TTC_UNSAFE_NO_HOST_ISOLATION=1.
# On macOS, it then starts renderers through a launcher, and each renderer
# confines itself with the macOS sandbox.
set -eu

TTC=${TTC:-}
TTCD=${TTCD:-}
TTC_GFX_RENDERER=${TTC_GFX_RENDERER:-}
TTC_GFXSTREAM_LIBRARY=${TTC_GFXSTREAM_LIBRARY:-}
TTC_GFX_GUEST_DRIVER=${TTC_GFX_GUEST_DRIVER:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
DEBIAN_IMAGE=${DEBIAN_IMAGE:-debian:sid}
# The checksum of vkoffscreen's image, whose every pixel it checks.
WANT_CHECKSUM=98d8b7be2dda8383

fail() {
  echo "ttc gfx e2e: $*" >&2
  exit 1
}

[ -x "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$TTCD" ] || fail "set TTCD to the matching ttcd binary"
[ -x "$TTC_GFX_RENDERER" ] || fail "set TTC_GFX_RENDERER to ttc-gfx-renderer"
[ -f "$TTC_GFXSTREAM_LIBRARY" ] || fail "set TTC_GFXSTREAM_LIBRARY to gfxstream's host library"
[ -f "$TTC_GFX_GUEST_DRIVER/libvulkan_gfxstream.so" ] ||
  fail "set TTC_GFX_GUEST_DRIVER to a directory with Mesa's libvulkan_gfxstream.so"
owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  # macOS gives TMPDIR a trailing slash; a doubled one would differ from
  # the cleaned paths TTC reports.
  tmp=${TMPDIR:-/tmp}
  TTC_E2E_ROOT=$(mktemp -d "${tmp%/}/ttc-gfx.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTC_E2E_ROOT"
fi
TEST_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BIN_DIR="$TTC_E2E_ROOT/bin"
DRIVER_DIR="$TTC_E2E_ROOT/driver"
mkdir -p "$BIN_DIR" "$DRIVER_DIR"
install -m 0755 "$(readlink -f "$TTC")" "$BIN_DIR/ttc"
install -m 0755 "$(readlink -f "$TTCD")" "$BIN_DIR/ttcd"
# The daemon finds the renderer beside itself, unless TTC_GFX_RENDERER names
# another.
install -m 0755 "$(readlink -f "$TTC_GFX_RENDERER")" "$BIN_DIR/ttc-gfx-renderer"
unset TTC_GFX_RENDERER
if [ "$(uname -s)" = Darwin ]; then
  # A copy loses its ad-hoc signature; Hypervisor.framework requires the
  # entitlement on the executable at its final path.
  /usr/bin/codesign --force --sign - \
    --entitlements "$TEST_DIR/../../hvf.entitlements" "$BIN_DIR/ttcd" >/dev/null
fi
export TTC_GFXSTREAM_LIBRARY
install -m 0644 "$TTC_GFX_GUEST_DRIVER/libvulkan_gfxstream.so" "$DRIVER_DIR/"
cat >"$DRIVER_DIR/gfxstream_icd.json" <<'EOF'
{
    "ICD": {
        "api_version": "1.1.363",
        "library_arch": "64",
        "library_path": "/opt/ttc-gfx/libvulkan_gfxstream.so"
    },
    "file_format_version": "1.0.1"
}
EOF

ttc() {
  "$BIN_DIR/ttc" "$@"
}

# The client finds the daemon through TTC_ROOT, which a node's TTC_LOCAL_ROOT
# would override.
unset TTC_LOCAL_ROOT
export TTC_ROOT="$TTC_E2E_ROOT/data"
NAME="gfx-$$"

cleanup() {
  ttc daemon stop --stop-all >/dev/null 2>&1 || true
  if [ "$owned_root" = true ]; then
    rm -rf "$TTC_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

set -- daemon start
if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
  set -- "$@" --unsafe-no-host-isolation
fi
ttc "$@"

# A GPU refuses live forks unless its family says what a fork does with it.
if output=$(ttc run --rm --gfx "$DEBIAN_IMAGE" true 2>&1); then
  fail "--gfx without --gfx-fork or --waive-live-fork was accepted"
fi
case "$output" in
  *--gfx-fork=device-lost*) ;;
  *) fail "--gfx without fork semantics: $output" ;;
esac

ttc run -d --name "$NAME" --gfx --gfx-fork=device-lost --network ttc --nat \
  -v "$DRIVER_DIR:/opt/ttc-gfx:ro" -v "$TEST_DIR/gfx:/src:ro" \
  "$DEBIAN_IMAGE" sleep infinity >/dev/null
ttc exec "$NAME@main" test -c /dev/dri/renderD128 || fail "no render node"
[ "$(ttc exec "$NAME@main" cat /sys/dev/char/226:128/device/../device)" = 0x1050 ] ||
  fail "the render node is not a virtio-gpu PCI device in sysfs"
ttc exec "$NAME@main" sh -c '
  set -e
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq vulkan-tools libdrm2 libexpat1 \
    gcc libc6-dev libvulkan-dev glslang-tools >/dev/null 2>&1
  mkdir -p /work
  cd /work
  glslangValidator -V /src/compute.comp -o compute.spv >/dev/null
  glslangValidator -V /src/square.vert -o square.spv >/dev/null
  glslangValidator -V /src/red.frag -o red.spv >/dev/null
  gcc -O2 -Wall -o vkoffscreen /src/vkoffscreen.c -lvulkan
' || fail "preparing the container"

VK_DRIVER_FILES=VK_DRIVER_FILES=/opt/ttc-gfx/gfxstream_icd.json
summary=$(ttc exec -e "$VK_DRIVER_FILES" "$NAME@main" vulkaninfo --summary 2>&1) ||
  fail "vulkaninfo: $summary"
case "$summary" in
  *"Virtio-GPU GFXStream"*) ;;
  *) fail "vulkaninfo found no virtual GPU: $summary" ;;
esac

# Repeat the render: each run creates a context of the branch's renderer.
for i in 1 2 3; do
  result=$(ttc exec -e "$VK_DRIVER_FILES" "$NAME@main" sh -c \
    'cd /work && ./vkoffscreen compute.spv square.spv red.spv' 2>&1) ||
    fail "vkoffscreen run $i: $result"
  case "$result" in
    *"vkoffscreen: ok checksum=$WANT_CHECKSUM"*) ;;
    *) fail "vkoffscreen run $i: $result" ;;
  esac
done

# wait_log BRANCH PATTERN SECONDS waits for the render loop's log in BRANCH to
# match PATTERN, an extended regular expression.
wait_log() {
  deadline=$(($(date +%s) + $3))
  until ttc exec "$NAME@$1" grep -Eq "$2" /tmp/loop.log 2>/dev/null; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      fail "$1's render loop has no '$2' after $3s: $(ttc exec "$NAME@$1" cat /tmp/loop.log 2>&1)"
    fi
    sleep 0.5
  done
}

# A fork with --gfx-fork=device-lost while the source renders, for at least
# 30s. The child's copy of the rendering process loses the GPU: it must end,
# not hang. A new process in the child renders with a renderer of the
# child's own, and the source renders on, unaffected.
ttc exec -d -e "$VK_DRIVER_FILES" "$NAME@main" sh -c \
  'cd /work && ./vkoffscreen compute.spv square.spv red.spv 600 50 >/tmp/loop.log 2>&1
   echo "exit=$?" >>/tmp/loop.log' >/dev/null
wait_log main "iteration 3 ok" 60
ttc fork "$NAME@main" lost >/dev/null
wait_log lost "^exit=" 60
lost_log=$(ttc exec "$NAME@lost" cat /tmp/loop.log)
case "$lost_log" in
  *exit=0) fail "the child's render loop survived the fork: $lost_log" ;;
esac
echo "the child's render loop ended: $(echo "$lost_log" | tail -n 2 | tr '\n' ' ')"
result=$(ttc exec -e "$VK_DRIVER_FILES" "$NAME@lost" sh -c \
  'cd /work && ./vkoffscreen compute.spv square.spv red.spv' 2>&1) ||
  fail "vkoffscreen in the fork's child: $result"
case "$result" in
  *"vkoffscreen: ok checksum=$WANT_CHECKSUM"*) ;;
  *) fail "vkoffscreen in the fork's child: $result" ;;
esac
wait_log main "^exit=" 180
source_log=$(ttc exec "$NAME@main" cat /tmp/loop.log)
case "$source_log" in
  *"iteration 600 ok"*"vkoffscreen: ok checksum=$WANT_CHECKSUM"*exit=0) ;;
  *) fail "the source's render loop: $source_log" ;;
esac

# Two new branches render at once, each with a renderer of its own. Their
# resource IDs start alike, which once made their renderers share command
# rings through macOS's global shared memory names.
ttc fork "$NAME@main" one two >/dev/null
for branch in one two; do
  ttc exec -d -e "$VK_DRIVER_FILES" "$NAME@$branch" sh -c \
    'cd /work && ./vkoffscreen compute.spv square.spv red.spv 300 20 >/tmp/pair.log 2>&1
     echo "exit=$?" >>/tmp/pair.log' >/dev/null
done
# wait_pair BRANCH PATTERN waits for the pair's log in BRANCH to match
# PATTERN, an extended regular expression.
wait_pair() {
  deadline=$(($(date +%s) + 120))
  until ttc exec "$NAME@$1" grep -Eq "$2" /tmp/pair.log 2>/dev/null; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      fail "branch $1, rendering beside another, has no '$2': $(ttc exec "$NAME@$1" cat /tmp/pair.log 2>&1)"
    fi
    sleep 0.5
  done
}
wait_pair one "iteration 2 ok"
wait_pair two "iteration 2 ok"
# Renderers that a daemon with host isolation starts on macOS confine
# themselves. python3 asks the kernel's sandbox.
if [ "$(uname -s)" = Darwin ] && [ "$TTC_UNSAFE_NO_HOST_ISOLATION" != 1 ] &&
  command -v python3 >/dev/null; then
  renderers=$(pgrep -f "$BIN_DIR/ttc-gfx-renderer serve" || true)
  [ -n "$renderers" ] || fail "no renderer runs while two branches render"
  for pid in $renderers; do
    python3 -c '
import ctypes, sys
check = ctypes.CDLL("/usr/lib/libSystem.dylib").sandbox_check
check.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int]
sys.exit(0 if check(int(sys.argv[1]), None, 0) == 1 else 1)
' "$pid" || fail "renderer $pid is not confined"
  done
fi
for branch in one two; do
  wait_pair "$branch" "^exit="
  pair_log=$(ttc exec "$NAME@$branch" cat /tmp/pair.log)
  case "$pair_log" in
    *"iteration 300 ok"*"vkoffscreen: ok checksum=$WANT_CHECKSUM"*exit=0) ;;
    *) fail "branch $branch, rendering beside another: $pair_log" ;;
  esac
done

# Nothing uses the GPU now, so no renderer runs.
deadline=$(($(date +%s) + 30))
while pgrep -f "$BIN_DIR/ttc-gfx-renderer serve" >/dev/null; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    fail "renderers outlive their GPU's use: $(pgrep -fl "$BIN_DIR/ttc-gfx-renderer serve")"
  fi
  sleep 0.5
done

ttc rm -f "$NAME" >/dev/null
echo "ttc gfx e2e: ok"
