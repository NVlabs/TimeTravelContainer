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

# Manual PTY integration for the versioned ttc/ttcd terminal transport and
# Sentry-owned branch-local consoles. It uses an unmodified public image.
set -eu

TTC=${TTC:-}
TTCD=${TTCD:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-busybox:1.36}

fail() {
  echo "ttc terminal e2e: $*" >&2
  exit 1
}

[ -n "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$TTC" ] || fail "TTC is not executable: $TTC"
[ -n "$TTCD" ] || fail "set TTCD to the matching ttcd binary"
[ -x "$TTCD" ] || fail "TTCD is not executable: $TTCD"
command -v script >/dev/null 2>&1 || fail "script is required"
if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" != 1 ]; then
  command -v unshare >/dev/null 2>&1 || fail "unshare is required"
  unshare -Ur /bin/true >/dev/null 2>&1 || fail "rootless user namespaces are unavailable; set TTC_UNSAFE_NO_HOST_ISOLATION=1 only for trusted workloads"
fi

owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  # macOS gives TMPDIR a trailing slash; a doubled one would differ from
  # the cleaned paths TTC reports.
  tmp=${TMPDIR:-/tmp}
  TTC_E2E_ROOT=$(mktemp -d "${tmp%/}/ttc-terminal.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTC_E2E_ROOT"
fi
DATA_ROOT="$TTC_E2E_ROOT/data"
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
export TTC_ROOT="$DATA_ROOT"
NAME="terminal-e2e-$$"
CLIENT_TERM=${TERM:-xterm}
for value in "$TTC" "$TTCD" "$TTC_E2E_ROOT"; do
  case "$value" in
    *[[:space:]]*) fail "PTY test paths must not contain whitespace: $value" ;;
  esac
done

TTC_COMMAND="$TTC"

ttc() {
  "$TTC" "$@"
}

run_with_timeout() {
  seconds=$1
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$seconds" "$@"
    return
  fi
  if command -v gtimeout >/dev/null 2>&1; then
    gtimeout "$seconds" "$@"
    return
  fi
  command -v perl >/dev/null 2>&1 || fail "timeout, gtimeout, or perl is required"
  perl -e '
    use POSIX qw(:sys_wait_h);
    my $seconds = shift @ARGV;
    my $pid = fork();
    die "fork: $!\n" unless defined $pid;
    if ($pid == 0) {
      exec @ARGV;
      die "exec $ARGV[0]: $!\n";
    }
    $SIG{ALRM} = sub {
      kill "TERM", $pid;
      select undef, undef, undef, 1;
      kill "KILL", $pid;
      exit 124;
    };
    alarm $seconds;
    waitpid($pid, 0);
    my $status = $?;
    alarm 0;
    exit(WIFEXITED($status) ? WEXITSTATUS($status) : 128 + WTERMSIG($status));
  ' "$seconds" "$@"
}

run_pty() {
  transcript=$1
  command=$2
  case "$(uname -s)" in
    Darwin)
      run_with_timeout 60 script -q "$transcript" /bin/sh -c "$TTC_COMMAND $command"
      ;;
    *)
      run_with_timeout 60 script -qec "$TTC_COMMAND $command" "$transcript"
      ;;
  esac
}

cleanup() {
  ttc rm --force "$NAME" >/dev/null 2>&1 || true
  ttc daemon stop >/dev/null 2>&1 || true
  if $owned_root; then
    rm -rf "$TTC_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

set -- daemon start
if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
  set -- "$@" --unsafe-no-host-isolation
fi
ttc "$@"

# Repetition makes the original post-start follower race deterministic on an
# unfixed runtime. Every run also checks that stdin and stdout are real TTYs.
i=1
while [ "$i" -le 5 ]; do
  run_pty "$TTC_E2E_ROOT/quick-$i.typescript" \
    "run --rm -it $BUSYBOX_IMAGE sh -c \"test -t 0 && test -t 1 && echo tty-quick-$i\"" \
    >"$TTC_E2E_ROOT/quick-$i.out"
  grep -a -q "tty-quick-$i" "$TTC_E2E_ROOT/quick-$i.out" || fail "quick terminal run $i lost output"
  i=$((i + 1))
done

ttc run -dit --name "$NAME" "$BUSYBOX_IMAGE" sh >/dev/null
ttc fork "$NAME@main" child >/dev/null

{
  sleep 1
  printf "echo parent-terminal-marker\r\020\021"
} | run_pty "$TTC_E2E_ROOT/parent.typescript" "attach -i $NAME@main" >"$TTC_E2E_ROOT/parent.out"
{
  sleep 1
  printf "echo child-terminal-marker\r\020\021"
} | run_pty "$TTC_E2E_ROOT/child.typescript" "attach -i $NAME@child" >"$TTC_E2E_ROOT/child.out"

ttc pause "$NAME@child" >/dev/null
ttc resume "$NAME@child" >/dev/null
run_pty "$TTC_E2E_ROOT/exec.typescript" \
  "exec -it $NAME@child sh -c 'test -t 0 && test -t 1 && test -t 2 && stty -a >/dev/null && echo term:\$TERM && echo tty-exec-ok'" \
  >"$TTC_E2E_ROOT/exec.out"

ttc logs "$NAME@main" >"$TTC_E2E_ROOT/parent.logs"
ttc logs "$NAME@child" >"$TTC_E2E_ROOT/child.logs"
ttc ls --tree >"$TTC_E2E_ROOT/tree.out"

grep -a -q "parent-terminal-marker" "$TTC_E2E_ROOT/parent.logs" || fail "parent attach output was not persisted"
grep -a -q "child-terminal-marker" "$TTC_E2E_ROOT/child.logs" || fail "child attach output was not persisted"
grep -a -q "tty-exec-ok" "$TTC_E2E_ROOT/exec.out" || fail "terminal exec failed"
grep -a -q "term:$CLIENT_TERM" "$TTC_E2E_ROOT/exec.out" || fail "terminal exec did not inherit client TERM=$CLIENT_TERM"
grep -q "child running" "$TTC_E2E_ROOT/tree.out" || fail "branch tree does not show the running child"

echo "TTC terminal and live-fork integration passed"
