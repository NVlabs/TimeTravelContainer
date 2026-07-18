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

# Manual PTY integration for the versioned ttos/ttosd terminal transport and
# Sentry-owned branch-local consoles. It uses an unmodified public image.
set -eu

TTOS=${TTOS:-}
RUNSC=${RUNSC:-}
TTOS_E2E_ROOT=${TTOS_E2E_ROOT:-}
TTOS_UNSAFE_NO_HOST_ISOLATION=${TTOS_UNSAFE_NO_HOST_ISOLATION:-0}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-busybox:1.36}

fail() {
  echo "ttos terminal e2e: $*" >&2
  exit 1
}

[ -n "$TTOS" ] || fail "set TTOS to the ttos binary"
[ -x "$TTOS" ] || fail "TTOS is not executable: $TTOS"
[ -n "$RUNSC" ] || fail "set RUNSC to the TimeTravel-enabled runsc binary"
[ -x "$RUNSC" ] || fail "RUNSC is not executable: $RUNSC"
command -v script >/dev/null 2>&1 || fail "util-linux script is required"
command -v timeout >/dev/null 2>&1 || fail "timeout is required"
if [ "$TTOS_UNSAFE_NO_HOST_ISOLATION" != 1 ]; then
  command -v unshare >/dev/null 2>&1 || fail "unshare is required"
  unshare -Ur /bin/true >/dev/null 2>&1 || fail "rootless user namespaces are unavailable; set TTOS_UNSAFE_NO_HOST_ISOLATION=1 only for trusted workloads"
fi

owned_root=false
if [ -z "$TTOS_E2E_ROOT" ]; then
  TTOS_E2E_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/ttos-terminal-e2e.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTOS_E2E_ROOT"
fi
DATA_ROOT="$TTOS_E2E_ROOT/data"
RUNSC_ROOT="$TTOS_E2E_ROOT/runsc"
DAEMON_SOCKET="$TTOS_E2E_ROOT/ttosd.sock"
NAME="terminal-e2e-$$"
UNSAFE=
if [ "$TTOS_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
  UNSAFE=--unsafe-no-host-isolation
fi

for value in "$TTOS" "$RUNSC" "$TTOS_E2E_ROOT"; do
  case "$value" in
    *[[:space:]]*) fail "PTY test paths must not contain whitespace: $value" ;;
  esac
done

TTOS_COMMAND="$TTOS $UNSAFE --data-root $DATA_ROOT --daemon-socket $DAEMON_SOCKET --root $RUNSC_ROOT --runsc $RUNSC"

ttos() {
  "$TTOS" $UNSAFE \
    --data-root "$DATA_ROOT" \
    --daemon-socket "$DAEMON_SOCKET" \
    --root "$RUNSC_ROOT" \
    --runsc "$RUNSC" \
    "$@"
}

run_pty() {
  transcript=$1
  command=$2
  timeout 60 script -qec "$TTOS_COMMAND $command" "$transcript"
}

cleanup() {
  ttos rm --force "$NAME" >/dev/null 2>&1 || true
  ttos daemon stop >/dev/null 2>&1 || true
  if $owned_root; then
    rm -rf "$TTOS_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

# Repetition makes the original post-start follower race deterministic on an
# unfixed runtime. Every run also checks that stdin and stdout are real TTYs.
i=1
while [ "$i" -le 5 ]; do
  run_pty "$TTOS_E2E_ROOT/quick-$i.typescript" \
    "run --rm -it $BUSYBOX_IMAGE sh -c \"test -t 0 && test -t 1 && echo tty-quick-$i\"" \
    >"$TTOS_E2E_ROOT/quick-$i.out"
  grep -a -q "tty-quick-$i" "$TTOS_E2E_ROOT/quick-$i.out" || fail "quick terminal run $i lost output"
  i=$((i + 1))
done

ttos run -dit --name "$NAME" "$BUSYBOX_IMAGE" sh >/dev/null
ttos fork "$NAME" "$NAME@child" >/dev/null

{
  sleep 1
  printf "echo parent-terminal-marker\r\020\021"
} | run_pty "$TTOS_E2E_ROOT/parent.typescript" "attach -i $NAME" >"$TTOS_E2E_ROOT/parent.out"
{
  sleep 1
  printf "echo child-terminal-marker\r\020\021"
} | run_pty "$TTOS_E2E_ROOT/child.typescript" "attach -i $NAME@child" >"$TTOS_E2E_ROOT/child.out"

ttos pause "$NAME@child" >/dev/null
ttos resume "$NAME@child" >/dev/null
run_pty "$TTOS_E2E_ROOT/exec.typescript" \
  "exec -it $NAME@child sh -c \"test -t 0 && test -t 1 && echo tty-exec-ok\"" \
  >"$TTOS_E2E_ROOT/exec.out"

ttos logs "$NAME" >"$TTOS_E2E_ROOT/parent.logs"
ttos logs "$NAME@child" >"$TTOS_E2E_ROOT/child.logs"
ttos ls --tree >"$TTOS_E2E_ROOT/tree.out"

grep -a -q "parent-terminal-marker" "$TTOS_E2E_ROOT/parent.logs" || fail "parent attach output was not persisted"
grep -a -q "child-terminal-marker" "$TTOS_E2E_ROOT/child.logs" || fail "child attach output was not persisted"
grep -a -q "tty-exec-ok" "$TTOS_E2E_ROOT/exec.out" || fail "terminal exec failed"
grep -q "child running" "$TTOS_E2E_ROOT/tree.out" || fail "branch tree does not show the running child"

echo "TTOS terminal and live-fork integration passed"
