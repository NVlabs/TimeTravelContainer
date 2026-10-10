#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Manual end-to-end coverage for the virtual GPU, --gfx: vulkaninfo, an
# offscreen render's checksum (test/gfx/vkoffscreen.c), OpenGL ES by Zink
# (test/gfx/gloffscreen.c), on macOS by Debian's virgl driver too, and forks
# with --gfx-fork=device-lost and --gfx-fork=snapshot, in containers whose
# renderer is ttc-gfx-renderer on the host's GPU, and whose Vulkan driver is
# the daemon's.
#
# It needs:
#   TTC and TTCD, the binaries;
#   optionally TTC_GFX_RENDERER, a ttc-gfx-renderer to run instead of the
#     daemon's;
#   TTC_GFX_GUEST_DRIVER, a directory with Mesa's Venus driver,
#     libvulkan_virtio.so, for the guest's architecture
#     (runsc/ttc/gfxrenderer/guest/build_mesa_venus.sh);
#   network access, to install vulkan-tools, Mesa, and a compiler in a
#     Debian image. Debian's Mesa has a Venus driver of its own, which the
#     daemon's containers skip.
#
# The daemon runs with host isolation unless TTC_UNSAFE_NO_HOST_ISOLATION=1,
# on its default platform unless TTC_PLATFORM names another, such as kvm.
# On macOS, it then starts renderers through a launcher, and each renderer
# confines itself with the macOS sandbox.
set -eu

TTC=${TTC:-}
TTCD=${TTCD:-}
TTC_GFX_RENDERER=${TTC_GFX_RENDERER:-}
TTC_GFX_GUEST_DRIVER=${TTC_GFX_GUEST_DRIVER:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
TTC_PLATFORM=${TTC_PLATFORM:-}
DEBIAN_IMAGE=${DEBIAN_IMAGE:-debian:sid}
# The checksums of vkoffscreen's and gloffscreen's images, whose every pixel
# they check.
WANT_CHECKSUM=98d8b7be2dda8383
WANT_GL_CHECKSUM=1175f5d01ae9db25

fail() {
  echo "ttc gfx e2e: $*" >&2
  exit 1
}

[ -x "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$TTCD" ] || fail "set TTCD to the matching ttcd binary"
[ -z "$TTC_GFX_RENDERER" ] || [ -x "$TTC_GFX_RENDERER" ] ||
  fail "TTC_GFX_RENDERER names no ttc-gfx-renderer"
GUEST_DRIVER=libvulkan_virtio.so
DEVICE="Virtio-GPU Venus"
[ -f "$TTC_GFX_GUEST_DRIVER/$GUEST_DRIVER" ] ||
  fail "set TTC_GFX_GUEST_DRIVER to a directory with Mesa's $GUEST_DRIVER"
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
# The test programs, which one container builds for all.
WORK_DIR="$TTC_E2E_ROOT/work"
mkdir -p "$BIN_DIR" "$DRIVER_DIR"
install -m 0755 "$(readlink -f "$TTC")" "$BIN_DIR/ttc"
install -m 0755 "$(readlink -f "$TTCD")" "$BIN_DIR/ttcd"
# The daemon runs the renderer that it carries on Linux, and is it on macOS,
# unless TTC_GFX_RENDERER names another.
if [ -n "$TTC_GFX_RENDERER" ]; then
  TTC_GFX_RENDERER=$(readlink -f "$TTC_GFX_RENDERER")
  export TTC_GFX_RENDERER
else
  unset TTC_GFX_RENDERER
fi
if [ "$(uname -s)" = Darwin ]; then
  # A copy loses its ad-hoc signature; Hypervisor.framework requires the
  # entitlement on the executable at its final path.
  /usr/bin/codesign --force --sign - \
    --entitlements "$TEST_DIR/../../hvf.entitlements" "$BIN_DIR/ttcd" >/dev/null
fi
# The daemon gives --gfx containers the guest's driver.
install -m 0644 "$TTC_GFX_GUEST_DRIVER/$GUEST_DRIVER" "$DRIVER_DIR/"
export TTC_GFX_GUEST_DRIVER="$DRIVER_DIR"

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
if [ -n "$TTC_PLATFORM" ]; then
  set -- "$@" --platform "$TTC_PLATFORM"
fi
ttc "$@"
DAEMON_PID=$(ttc daemon status | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
[ -n "$DAEMON_PID" ] || fail "the daemon's status has no PID"
if [ -n "$TTC_PLATFORM" ]; then
  case "$(ttc daemon status)" in
    *"\"runtime_platform\":\"$TTC_PLATFORM\""*) ;;
    *) fail "the daemon does not run on $TTC_PLATFORM" ;;
  esac
fi

# A GPU refuses live forks unless its family says what a fork does with it.
if output=$(ttc run --rm --gfx "$DEBIAN_IMAGE" true 2>&1); then
  fail "--gfx without --gfx-fork or --waive-live-fork was accepted"
fi
case "$output" in
  *--gfx-fork=device-lost*) ;;
  *) fail "--gfx without fork semantics: $output" ;;
esac

ttc run -d --name "$NAME" --gfx --gfx-fork=device-lost --network ttc --nat \
  -v "$TEST_DIR/gfx:/src:ro" \
  "$DEBIAN_IMAGE" sleep infinity >/dev/null
ttc exec "$NAME@main" test -c /dev/dri/renderD128 || fail "no render node"
[ "$(ttc exec "$NAME@main" cat /sys/dev/char/226:128/device/../device)" = 0x1050 ] ||
  fail "the render node is not a virtio-gpu PCI device in sysfs"
ttc exec "$NAME@main" sh -c '
  set -e
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq vulkan-tools libdrm2 libexpat1 \
    gcc libc6-dev libvulkan-dev glslang-tools mesa-vulkan-drivers libegl-mesa0 \
    libgl1-mesa-dri libegl-dev libgles-dev >/dev/null 2>&1
  mkdir -p /work
  cd /work
  glslangValidator -V /src/compute.comp -o compute.spv >/dev/null
  glslangValidator -V /src/square.vert -o square.spv >/dev/null
  glslangValidator -V /src/red.frag -o red.spv >/dev/null
  gcc -O2 -Wall -o vkoffscreen /src/vkoffscreen.c -lvulkan
  gcc -O2 -Wall -o gloffscreen /src/gloffscreen.c -lEGL -lGLESv2
' || fail "preparing the container"
ttc cp "$NAME@main:/work" "$WORK_DIR" >/dev/null || fail "copying the test programs out"

summary=$(ttc exec "$NAME@main" vulkaninfo --summary 2>&1) ||
  fail "vulkaninfo: $summary"
case "$summary" in
  *"$DEVICE"*) ;;
  *) fail "vulkaninfo found no virtual GPU: $summary" ;;
esac
# Debian's driver for the backend would be a second device of the GPU.
[ "$(echo "$summary" | grep -c "deviceName *= $DEVICE")" = 1 ] ||
  fail "the virtual GPU is not one device: $summary"

# OpenGL ES renders on the GPU, by Zink. On macOS, where Zink has only
# OpenGL 2.1 and OpenGL ES 2.0, applications opt in to it; elsewhere, the
# daemon's configuration of Mesa has them use it.
ZINK=MESA_LOADER_DRIVER_OVERRIDE=zink
result=$(ttc exec -e "$ZINK" "$NAME@main" /work/gloffscreen 2>&1) || fail "gloffscreen: $result"
case "$result" in
  *"renderer zink"*"$DEVICE"*"gloffscreen: ok checksum=$WANT_GL_CHECKSUM"*) ;;
  *) fail "gloffscreen: $result" ;;
esac
# On macOS, Debian's Mesa drives the GPU with its virgl driver by default,
# which the renderer serves with Zink on the host: the same render.
if [ "$(uname -s)" = Darwin ]; then
  result=$(ttc exec "$NAME@main" /work/gloffscreen 2>&1) || fail "gloffscreen by virgl: $result"
  case "$result" in
    *"renderer virgl (zink"*"gloffscreen: ok checksum=$WANT_GL_CHECKSUM"*) ;;
    *) fail "gloffscreen by virgl: $result" ;;
  esac
fi

# Repeat the render: each run creates a context of the branch's renderer.
for i in 1 2 3; do
  result=$(ttc exec "$NAME@main" sh -c \
    'cd /work && ./vkoffscreen compute.spv square.spv red.spv' 2>&1) ||
    fail "vkoffscreen run $i: $result"
  case "$result" in
    *"vkoffscreen: ok checksum=$WANT_CHECKSUM"*) ;;
    *) fail "vkoffscreen run $i: $result" ;;
  esac
done

# wait_log CONTAINER@BRANCH PATTERN SECONDS [LOG] waits for the render
# loop's log in the branch, LOG or /tmp/loop.log, to match PATTERN, an
# extended regular expression.
wait_log() {
  log=${4:-/tmp/loop.log}
  deadline=$(($(date +%s) + $3))
  until ttc exec "$1" grep -Eq "$2" "$log" 2>/dev/null; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      fail "$1's render loop has no '$2' in $log after $3s: $(ttc exec "$1" cat "$log" 2>&1)"
    fi
    sleep 0.5
  done
}

# A fork with --gfx-fork=device-lost while the source renders, for at least
# 30s. The child's copy of the rendering process loses the GPU: it must end,
# not hang. A new process in the child renders with a renderer of the
# child's own, and the source renders on, unaffected.
ttc exec -d "$NAME@main" sh -c \
  'cd /work && ./vkoffscreen compute.spv square.spv red.spv 600 50 >/tmp/loop.log 2>&1
   echo "exit=$?" >>/tmp/loop.log' >/dev/null
wait_log "$NAME@main" "iteration 3 ok" 60
ttc fork "$NAME@main" lost >/dev/null
wait_log "$NAME@lost" "^exit=" 60
lost_log=$(ttc exec "$NAME@lost" cat /tmp/loop.log)
case "$lost_log" in
  *exit=0) fail "the child's render loop survived the fork: $lost_log" ;;
esac
echo "the child's render loop ended: $(echo "$lost_log" | tail -n 2 | tr '\n' ' ')"
result=$(ttc exec "$NAME@lost" sh -c \
  'cd /work && ./vkoffscreen compute.spv square.spv red.spv' 2>&1) ||
  fail "vkoffscreen in the fork's child: $result"
case "$result" in
  *"vkoffscreen: ok checksum=$WANT_CHECKSUM"*) ;;
  *) fail "vkoffscreen in the fork's child: $result" ;;
esac
wait_log "$NAME@main" "^exit=" 180
source_log=$(ttc exec "$NAME@main" cat /tmp/loop.log)
case "$source_log" in
  *"iteration 600 ok"*"vkoffscreen: ok checksum=$WANT_CHECKSUM"*exit=0) ;;
  *) fail "the source's render loop: $source_log" ;;
esac

# Two new branches render at once, with the family's renderer, which keeps
# their contexts and resources apart: their IDs start alike.
ttc fork "$NAME@main" one two >/dev/null
for branch in one two; do
  ttc exec -d "$NAME@$branch" sh -c \
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
# renderers prints the PIDs of the daemon's renderers: its descendants that
# run as ttc-gfx-renderer, whatever their executable (ttcd on macOS), and
# serve a family.
renderers() {
  ps -A -o pid= -o ppid= -o args= | awk -v daemon="$DAEMON_PID" '
    {
      pid = $1
      parent[pid] = $2
      sub(/^[ \t]*[0-9]+[ \t]+[0-9]+[ \t]*/, "")
      args[pid] = $0
    }
    END {
      for (pid in args) {
        if (args[pid] !~ /^ttc-gfx-renderer serve-family/) continue
        for (p = parent[pid]; p in parent && p != parent[p]; p = parent[p]) {
          if (p == daemon) { print pid; break }
        }
      }
    }'
}
# Renderers that a daemon with host isolation starts on macOS confine
# themselves. python3 asks the kernel's sandbox.
if [ "$(uname -s)" = Darwin ] && [ "$TTC_UNSAFE_NO_HOST_ISOLATION" != 1 ] &&
  command -v python3 >/dev/null; then
  renderers=$(renderers)
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
while [ -n "$(renderers)" ]; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    fail "renderers outlive their GPU's use: $(renderers)"
  fi
  sleep 0.5
done

ttc rm -f "$NAME" >/dev/null

# A family with --gfx-fork=snapshot: a fork's child resumes the GPU's state,
# which the family's renderer clones, recorded command buffers too, so the
# Vulkan render loop records them once; Zink records its own for each draw.
# The family forks thrice: a child, two children at once, and a child's
# child.
SNAP="$NAME-snap"
SNAP_BRANCHES="kid one two grandkid"
ttc run -d --name "$SNAP" --gfx --gfx-fork=snapshot --network ttc --nat \
  -v "$WORK_DIR:/work:ro" \
  "$DEBIAN_IMAGE" sleep infinity >/dev/null
ttc exec "$SNAP@main" sh -c '
  set -e
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq libvulkan1 libdrm2 libexpat1 \
    libegl1 libgles2 libegl-mesa0 libgl1-mesa-dri >/dev/null 2>&1
' || fail "preparing the snapshot family's container"
ttc exec -d "$SNAP@main" sh -c \
  "cd /work && ./vkoffscreen compute.spv square.spv red.spv 300 20 >/tmp/loop.log 2>&1
   echo \"exit=\$?\" >>/tmp/loop.log" >/dev/null
ttc exec -d -e "$ZINK" "$SNAP@main" sh -c \
  'cd /work && ./gloffscreen 300 20 >/tmp/gl.log 2>&1
   echo "exit=$?" >>/tmp/gl.log' >/dev/null
wait_log "$SNAP@main" "iteration 3 ok" 60
wait_log "$SNAP@main" "iteration 3 ok" 60 /tmp/gl.log
ttc fork "$SNAP@main" kid >/dev/null
sleep 1
ttc fork "$SNAP@main" one two >/dev/null
sleep 1
ttc fork "$SNAP@one" grandkid >/dev/null
for target in "$SNAP@main" $(for b in $SNAP_BRANCHES; do echo "$SNAP@$b"; done); do
  wait_log "$target" "^exit=" 180
  loop_log=$(ttc exec "$target" cat /tmp/loop.log)
  case "$loop_log" in
    *"iteration 300 ok"*"vkoffscreen: ok checksum=$WANT_CHECKSUM"*exit=0) ;;
    *) fail "$target's render loop, across a snapshot fork: $loop_log" ;;
  esac
  wait_log "$target" "^exit=" 180 /tmp/gl.log
  gl_log=$(ttc exec "$target" cat /tmp/gl.log)
  case "$gl_log" in
    *"iteration 300 ok"*"gloffscreen: ok checksum=$WANT_GL_CHECKSUM"*exit=0) ;;
    *) fail "$target's OpenGL loop, across a snapshot fork: $gl_log" ;;
  esac
done
ttc rm -f "$SNAP" >/dev/null
echo "ttc gfx e2e: ok"
