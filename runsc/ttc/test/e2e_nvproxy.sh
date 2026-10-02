#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Manual end-to-end coverage for the host's NVIDIA GPUs, --nvproxy: the
# driver's device nodes and libraries in containers, nvidia-smi, CUDA
# (test/nvproxy/cudaloop.cu), and live forks of a process that counts on the
# GPU, with --nvproxy-fork=device-lost, whose child's copy loses the GPU and
# ends while new processes in the child use the GPU, and with
# --nvproxy-fork=snapshot, whose children count on from the fork.
#
# It needs:
#   TTC and TTCD, the binaries;
#   TTC_CUDA_CHECKPOINT, ttc-cuda-checkpoint;
#   CUDALOOP, cudaloop built for the host's GPU, for instance with
#     nvcc -O2 -o cudaloop runsc/ttc/test/nvproxy/cudaloop.cu;
#   a host with NVIDIA GPUs and a driver that nvproxy supports, or
#     TTC_NVPROXY_DRIVER_VERSION naming one whose interface the host's
#     driver has;
#   network access, to pull IMAGE (default debian:sid), whose C library
#     runs cudaloop.
#
# The daemon runs with host isolation unless TTC_UNSAFE_NO_HOST_ISOLATION=1.
set -eu

TTC=${TTC:-}
TTCD=${TTCD:-}
TTC_CUDA_CHECKPOINT=${TTC_CUDA_CHECKPOINT:-}
CUDALOOP=${CUDALOOP:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
IMAGE=${IMAGE:-debian:sid}

fail() {
  echo "ttc nvproxy e2e: $*" >&2
  exit 1
}

[ -x "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$TTCD" ] || fail "set TTCD to the matching ttcd binary"
[ -x "$TTC_CUDA_CHECKPOINT" ] || fail "set TTC_CUDA_CHECKPOINT to ttc-cuda-checkpoint"
[ -x "$CUDALOOP" ] || fail "set CUDALOOP to cudaloop, built for the host's GPU"
[ -c /dev/nvidiactl ] || fail "the host has no NVIDIA driver"
owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  tmp=${TMPDIR:-/tmp}
  TTC_E2E_ROOT=$(mktemp -d "${tmp%/}/ttc-nvproxy.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTC_E2E_ROOT"
fi
BIN_DIR="$TTC_E2E_ROOT/bin"
WORK_DIR="$TTC_E2E_ROOT/work"
mkdir -p "$BIN_DIR" "$WORK_DIR"
install -m 0755 "$(readlink -f "$TTC")" "$BIN_DIR/ttc"
install -m 0755 "$(readlink -f "$TTCD")" "$BIN_DIR/ttcd"
# The daemon finds ttc-cuda-checkpoint beside itself.
install -m 0755 "$(readlink -f "$TTC_CUDA_CHECKPOINT")" "$BIN_DIR/ttc-cuda-checkpoint"
unset TTC_CUDA_CHECKPOINT
install -m 0755 "$(readlink -f "$CUDALOOP")" "$WORK_DIR/cudaloop"

ttc() {
  "$BIN_DIR/ttc" "$@"
}

unset TTC_LOCAL_ROOT
export TTC_ROOT="$TTC_E2E_ROOT/data"
NAME="nvproxy-$$"

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

# GPUs refuse live forks unless their family says what a fork does with them.
if output=$(ttc run --rm --nvproxy "$IMAGE" true 2>&1); then
  fail "--nvproxy without --nvproxy-fork or --waive-live-fork was accepted"
fi
case "$output" in
  *--nvproxy-fork=device-lost*) ;;
  *) fail "--nvproxy without fork semantics: $output" ;;
esac

# wait_log CONTAINER@BRANCH PATTERN SECONDS [LOG] waits for the loop's log in
# the branch, LOG or /tmp/loop.log, to match PATTERN, an extended regular
# expression.
wait_log() {
  log=${4:-/tmp/loop.log}
  deadline=$(($(date +%s) + $3))
  until ttc exec "$1" grep -Eq "$2" "$log" 2>/dev/null; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      fail "$1's loop has no '$2' in $log after $3s: $(ttc exec "$1" cat "$log" 2>&1)"
    fi
    sleep 0.5
  done
}

# start_loop CONTAINER@BRANCH STEPS starts cudaloop, of STEPS steps of 50ms
# apart, in the background, logging to /tmp/loop.log and then its status.
start_loop() {
  ttc exec -d "$1" sh -c \
    "/work/cudaloop -n $2 -i 50 >/tmp/loop.log 2>&1; echo \"exit=\$?\" >>/tmp/loop.log" >/dev/null
}

ttc run -d --name "$NAME" --nvproxy --nvproxy-fork=device-lost \
  -v "$WORK_DIR:/work:ro" "$IMAGE" sleep infinity >/dev/null
for dev in /dev/nvidiactl /dev/nvidia-uvm /dev/nvidia0; do
  ttc exec "$NAME@main" test -c "$dev" || fail "no $dev"
done
smi=$(ttc exec "$NAME@main" nvidia-smi -L 2>&1) || fail "nvidia-smi: $smi"
case "$smi" in
  "GPU 0: "*) ;;
  *) fail "nvidia-smi -L: $smi" ;;
esac
result=$(ttc exec "$NAME@main" /work/cudaloop -n 3 -i 0 2>&1) || fail "cudaloop: $result"
case "$result" in
  *"step 3 ok"*"cudaloop: done"*) ;;
  *) fail "cudaloop: $result" ;;
esac
echo "$result" | head -n 1

# A fork with --nvproxy-fork=device-lost while the source counts. The
# child's copy of the counting process loses the GPU: it must end, not hang
# or count on. A new process in the child counts on the GPU, and the source
# counts on, unaffected.
start_loop "$NAME@main" 600
wait_log "$NAME@main" "step 20 ok" 60
ttc fork "$NAME@main" lost >/dev/null
wait_log "$NAME@lost" "^exit=" 60
lost_log=$(ttc exec "$NAME@lost" cat /tmp/loop.log)
case "$lost_log" in
  *exit=0) fail "the child's loop survived the fork: $lost_log" ;;
esac
echo "the child's loop ended: $(echo "$lost_log" | tail -n 2 | tr '\n' ' ')"
result=$(ttc exec "$NAME@lost" /work/cudaloop -n 3 -i 0 2>&1) ||
  fail "cudaloop in the fork's child: $result"
case "$result" in
  *"step 3 ok"*"cudaloop: done"*) ;;
  *) fail "cudaloop in the fork's child: $result" ;;
esac
wait_log "$NAME@main" "^exit=" 180
source_log=$(ttc exec "$NAME@main" cat /tmp/loop.log)
case "$source_log" in
  *"step 600 ok"*"cudaloop: done"*exit=0) ;;
  *) fail "the source's loop: $source_log" ;;
esac
ttc rm -f "$NAME" >/dev/null

# A family with --nvproxy-fork=snapshot: a fork's children count on from the
# count at the fork, as the source does. The family forks thrice: a child,
# two children at once, and a child's child.
SNAP="$NAME-snap"
SNAP_BRANCHES="kid one two grandkid"
ttc run -d --name "$SNAP" --nvproxy --nvproxy-fork=snapshot \
  -v "$WORK_DIR:/work:ro" "$IMAGE" sleep infinity >/dev/null
start_loop "$SNAP@main" 600
wait_log "$SNAP@main" "step 20 ok" 60
ttc fork "$SNAP@main" kid >/dev/null
sleep 1
ttc fork "$SNAP@main" one two >/dev/null
sleep 1
ttc fork "$SNAP@one" grandkid >/dev/null
for target in "$SNAP@main" $(for b in $SNAP_BRANCHES; do echo "$SNAP@$b"; done); do
  wait_log "$target" "^exit=" 240
  loop_log=$(ttc exec "$target" cat /tmp/loop.log)
  case "$loop_log" in
    *"step 600 ok"*"cudaloop: done"*exit=0) ;;
    *) fail "$target's loop, across a snapshot fork: $(echo "$loop_log" | tail -n 5)" ;;
  esac
done
ttc rm -f "$SNAP" >/dev/null
echo "ttc nvproxy e2e: ok"
