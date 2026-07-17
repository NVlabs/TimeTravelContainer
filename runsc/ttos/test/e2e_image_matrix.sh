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
REDIS_IMAGE=${REDIS_IMAGE:-redis:7-alpine}

fail() {
  echo "ttos e2e: $*" >&2
  exit 1
}

container_id_valid() {
  [ "${#1}" -eq 64 ] || return 1
  case "$1" in
    *[!0-9a-f]*) return 1 ;;
  esac
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
HOST_FIXTURE_TWO="$TTOS_E2E_ROOT/host-fixture-two"
HOST_COW_FIXTURE="$TTOS_E2E_ROOT/host-cow-fixture"
mkdir -p "$HOST_FIXTURE" "$HOST_FIXTURE_TWO" "$HOST_COW_FIXTURE"
echo host-readonly >"$HOST_FIXTURE/value"
echo host-readonly-two >"$HOST_FIXTURE_TWO/value"
echo host-cow-lower >"$HOST_COW_FIXTURE/value"
ENV_FILE="$TTOS_E2E_ROOT/container.env"
LABEL_FILE="$TTOS_E2E_ROOT/container.labels"
TTOS_E2E_HOST_IMPORT=host-import
export TTOS_E2E_HOST_IMPORT
printf "%s\n" "# environment file" "FROM_FILE=file" "TTOS_E2E_HOST_IMPORT" >"$ENV_FILE"
printf "%s\n" "# label file" "suite=file" "file-only=value" "empty" >"$LABEL_FILE"
PREFIX="e2e-$$"
LIFECYCLE="$PREFIX-lifecycle"
ATTACH_INPUT="$PREFIX-attach-input"
AUTO_FOREGROUND="$PREFIX-auto-foreground"
AUTO_DETACHED="$PREFIX-auto-detached"
AUTO_CREATED="$PREFIX-auto-created"
CONSOLE_STRESS="$PREFIX-console-stress"
RETENTION="$PREFIX-retention"
NOLOG="$PREFIX-nolog"
MULTI_A="$PREFIX-multi-a"
MULTI_B="$PREFIX-multi-b"
HOSTNET="$PREFIX-hostnet"
STOP_POLICY="$PREFIX-stop-policy"
PIDS_LIMIT="$PREFIX-pids-limit"
ANON_VOLUME="$PREFIX-anon-volume"
ANON_NOCOPY="$PREFIX-anon-nocopy"
HOSTNET_PORT=$((20000 + ($$ % 30000)))
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
  for container in "$PREFIX-busybox" "$PREFIX-alpine" "$PREFIX-python" "$PREFIX-nginx" "$PREFIX-redis" "$CONSOLE_STRESS" "$RETENTION" "$NOLOG" "$MULTI_A" "$MULTI_B" "$HOSTNET" "$STOP_POLICY" "$PIDS_LIMIT" "$ANON_VOLUME" "$ANON_NOCOPY" "$LIFECYCLE" "$ATTACH_INPUT" "$AUTO_FOREGROUND" "$AUTO_DETACHED" "$AUTO_CREATED"; do
    ttos rm --force "$container" >/dev/null 2>&1 || true
  done
  ttos volume rm "$VOLUME" >/dev/null 2>&1 || true
  if $owned_root; then
    rm -rf "$TTOS_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

echo "== pulling public image matrix =="
for image in "$BUSYBOX_IMAGE" "$ALPINE_IMAGE" "$PYTHON_IMAGE" "$NGINX_IMAGE" "$REDIS_IMAGE"; do
  ttos pull "$image"
done
PULL_REUSE="$TTOS_E2E_ROOT/pull-reuse.out"
ttos pull "$BUSYBOX_IMAGE" >"$PULL_REUSE"
grep -Eq '^Content: 0 bytes downloaded, [1-9][0-9]* bytes reused$' "$PULL_REUSE" || fail "repeat pull did not report content reuse"
PULL_QUIET="$TTOS_E2E_ROOT/pull-quiet.out"
ttos pull -q "$BUSYBOX_IMAGE" >"$PULL_QUIET"
[ "$(wc -l <"$PULL_QUIET")" -eq 1 ] || fail "quiet pull emitted multiple lines"
grep -Eq '^sha256:[0-9a-f]{64}$' "$PULL_QUIET" || fail "quiet pull emitted more than the digest"

echo "== image inventory =="
IMAGE_LIST="$TTOS_E2E_ROOT/images.json"
ttos images --json >"$IMAGE_LIST"
for reference in docker.io/library/busybox:1.36 docker.io/library/alpine:3.20 docker.io/library/python:3.12-alpine docker.io/library/nginx:1.27-alpine docker.io/library/redis:7-alpine; do
  grep -q "\"reference\": \"$reference\"" "$IMAGE_LIST" || fail "image inventory omitted $reference"
done
[ "$(ttos images -q | wc -l)" -ge 5 ] || fail "quiet image inventory omitted an image ID"

echo "== automatic container removal =="
ttos run --rm --pull always --name "$AUTO_FOREGROUND" "$BUSYBOX_IMAGE" true
if ttos inspect "$AUTO_FOREGROUND" >/dev/null 2>&1; then
  fail "foreground --rm retained its container"
fi
AUTO_DETACHED_ID=$(ttos run --rm --detach --name "$AUTO_DETACHED" --volume /auto "$BUSYBOX_IMAGE" sh -c "sleep 2")
container_id_valid "$AUTO_DETACHED_ID" || fail "detached --rm returned invalid ID: $AUTO_DETACHED_ID"
AUTO_DETACHED_VOLUME=$(ttos inspect -f "{{(index .Mounts 0).Name}}" "$AUTO_DETACHED_ID")
container_id_valid "$AUTO_DETACHED_VOLUME" || fail "detached --rm generated invalid anonymous volume name: $AUTO_DETACHED_VOLUME"
[ "$(ttos inspect -f "{{.HostConfig.AutoRemove}}" "$AUTO_DETACHED_ID")" = true ] ||
  fail "inspect omitted detached auto-remove policy"
deadline=$(( $(date +%s) + 15 ))
while ttos inspect "$AUTO_DETACHED" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "detached --rm did not remove its container"
  sleep 1
done
if ttos volume inspect "$AUTO_DETACHED_VOLUME" >/dev/null 2>&1; then
  fail "detached --rm retained its anonymous volume"
fi
AUTO_CREATED_ID=$(ttos create --rm --name "$AUTO_CREATED" "$BUSYBOX_IMAGE" sh -c "sleep 1")
[ "$(ttos inspect -f "{{.HostConfig.AutoRemove}}" "$AUTO_CREATED_ID")" = true ] ||
  fail "create --rm did not persist its policy"
ttos start "$AUTO_CREATED_ID" >/dev/null
deadline=$(( $(date +%s) + 15 ))
while ttos inspect "$AUTO_CREATED" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "started create --rm container was retained"
  sleep 1
done

echo "== one-shot compatibility probes =="
if ttos create --pull never --name "$PREFIX-never-missing" "registry.invalid/ttos/missing:latest" true >/dev/null 2>&1; then
  fail "--pull never unexpectedly created a missing image"
fi

BUSYBOX_ID=$(ttos create --pull never --name "$PREFIX-busybox" \
  --env-file "$ENV_FILE" --env FROM_FILE=direct \
  --label-file "$LABEL_FILE" --label suite=create \
  "$BUSYBOX_IMAGE" sh -c \
  'test "$FROM_FILE" = direct && test "$TTOS_E2E_HOST_IMPORT" = host-import && test -r /proc/self/status && test -r /sys/kernel && echo busybox-ok && echo busybox-err >&2')
container_id_valid "$BUSYBOX_ID" || fail "create returned invalid container ID: $BUSYBOX_ID"
BUSYBOX_PREFIX=$(printf %.16s "$BUSYBOX_ID")
[ "$(ttos inspect -f "{{.Id}}|{{.Name}}" "$PREFIX-busybox")" = "$BUSYBOX_ID|/$PREFIX-busybox" ] ||
  fail "inspect by name did not expose the stable ID and name"
[ "$(ttos inspect -f "{{.Id}}|{{.Name}}" "$BUSYBOX_PREFIX")" = "$BUSYBOX_ID|/$PREFIX-busybox" ] ||
  fail "inspect did not resolve an unambiguous ID prefix"
[ "$(ttos inspect -f '{{.State.Status}}|{{.Config.Image}}|{{.Path}}|{{len .TTOS.Branches}}' "$PREFIX-busybox")" = \
  "created|docker.io/library/busybox:1.36|sh|1" ] ||
  fail "created-container inspect did not expose Docker config and TTOS branch state"
[ "$(ttos inspect -f '{{index .Config.Labels "suite"}}|{{index .Config.Labels "file-only"}}|{{index .Config.Labels "empty"}}' "$PREFIX-busybox")" = \
  "create|value|" ] || fail "created-container inspect did not retain merged labels"
BUSYBOX_ENV=$(ttos inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$PREFIX-busybox")
echo "$BUSYBOX_ENV" | grep -qx 'FROM_FILE=direct' || fail "created-container inspect omitted direct environment override"
echo "$BUSYBOX_ENV" | grep -qx 'TTOS_E2E_HOST_IMPORT=host-import' || fail "created-container inspect omitted imported host environment"
ttos start --attach "$BUSYBOX_PREFIX"
[ "$(ttos inspect -f '{{.State.Status}}|{{.State.ExitCode}}|{{.State.Running}}' "$PREFIX-busybox")" = \
  "exited|0|false" ] ||
  fail "exited-container inspect did not expose exit state"
ATTACH_STDOUT="$TTOS_E2E_ROOT/attach.stdout"
ATTACH_STDERR="$TTOS_E2E_ROOT/attach.stderr"
ttos attach --no-stderr "$PREFIX-busybox" >"$ATTACH_STDOUT" 2>"$ATTACH_STDERR"
grep -qx 'busybox-ok' "$ATTACH_STDOUT" || fail "stdout-only attach omitted stdout"
[ -z "$(sed '/^ttos: warning:/d' "$ATTACH_STDERR")" ] || fail "stdout-only attach leaked stderr"
ttos attach --no-stdout "$PREFIX-busybox" >"$ATTACH_STDOUT" 2>"$ATTACH_STDERR"
[ ! -s "$ATTACH_STDOUT" ] || fail "stderr-only attach leaked stdout"
[ "$(sed '/^ttos: warning:/d' "$ATTACH_STDERR")" = busybox-err ] || fail "stderr-only attach omitted stderr"

ttos run --name "$ATTACH_INPUT" --detach --interactive "$BUSYBOX_IMAGE" sh -c \
  'IFS= read -r value; test "$value" = stdin-ok; echo hidden-stdout; echo hidden-stderr >&2' >/dev/null
printf 'stdin-ok\n' | ttos attach --interactive --no-stdout --no-stderr "$ATTACH_INPUT" >"$ATTACH_STDOUT" 2>"$ATTACH_STDERR"
[ ! -s "$ATTACH_STDOUT" ] || fail "stdin-only attach leaked stdout"
[ -z "$(sed '/^ttos: warning:/d' "$ATTACH_STDERR")" ] || fail "stdin-only attach leaked stderr"
[ "$(ttos inspect -f '{{.State.ExitCode}}' "$ATTACH_INPUT")" = 0 ] || fail "stdin-only attach did not deliver input"
ttos rm "$ATTACH_INPUT"
ttos container restart --time 1 "$PREFIX-busybox"
[ "$(ttos container wait "$PREFIX-busybox")" = "0" ] ||
  fail "restart/wait did not preserve the BusyBox command exit status"
ttos rm "$BUSYBOX_ID"
ttos image inspect "$BUSYBOX_IMAGE" |
  grep -q '"Materialized": true' ||
  fail "image inspection did not report the materialized BusyBox root"

echo "== anonymous Docker volumes and live-fork isolation =="
ttos run --name "$ANON_VOLUME" --detach --volume /etc "$BUSYBOX_IMAGE" sh -c 'test -s /etc/passwd; echo parent >/etc/branch; sleep 300' >/dev/null
ANON_NAME=$(ttos inspect -f "{{(index .Mounts 0).Name}}" "$ANON_VOLUME")
container_id_valid "$ANON_NAME" || fail "anonymous volume name is not a 64-character ID: $ANON_NAME"
[ "$(ttos inspect -f '{{(index .Mounts 0).Type}}|{{(index .Mounts 0).Destination}}' "$ANON_VOLUME")" = "volume|/etc" ] ||
  fail "anonymous short volume was not exposed through Docker inspect"
ttos exec "$ANON_VOLUME" sh -c 'test -s /etc/passwd; test "$(cat /etc/branch)" = parent' || fail "anonymous volume did not copy image contents"
ttos fork "$ANON_VOLUME" "$ANON_VOLUME@child" >/dev/null
ttos exec "$ANON_VOLUME@child" sh -c 'test -s /etc/passwd; echo child >/etc/branch'
[ "$(ttos exec "$ANON_VOLUME" cat /etc/branch)" = parent ] || fail "anonymous-volume child write leaked into parent"
[ "$(ttos exec "$ANON_VOLUME@child" cat /etc/branch)" = child ] || fail "anonymous-volume child write was not retained"
if ttos rm --volumes "$ANON_VOLUME" >"$TTOS_E2E_ROOT/descendant-rm.out" 2>"$TTOS_E2E_ROOT/descendant-rm.err"; then
  fail "unforced rm accepted a live family with a descendant"
fi
grep -q "stop it or use --force" "$TTOS_E2E_ROOT/descendant-rm.err" ||
  fail "unforced descendant removal lacked a force diagnostic"
ttos rm --force --volumes "$ANON_VOLUME"
if ttos volume inspect "$ANON_NAME" >/dev/null 2>&1; then
  fail "rm --volumes retained an anonymous volume"
fi

ttos run --name "$ANON_NOCOPY" --volume /etc:volume-nocopy "$BUSYBOX_IMAGE" sh -c 'test ! -e /etc/passwd'
NOCOPY_NAME=$(ttos inspect -f "{{(index .Mounts 0).Name}}" "$ANON_NOCOPY")
container_id_valid "$NOCOPY_NAME" || fail "volume-nocopy generated invalid anonymous volume name"
ttos rm -v "$ANON_NOCOPY"

ttos container create --name "$MULTI_A" "$BUSYBOX_IMAGE" sh -c 'exit 3'
ttos container create --name "$MULTI_B" "$BUSYBOX_IMAGE" sh -c 'exit 4'
MULTI_STARTED=$(ttos container start "$MULTI_A" "$MULTI_B")
echo "$MULTI_STARTED" | grep -qx "$MULTI_A" || fail "multi-start omitted $MULTI_A"
echo "$MULTI_STARTED" | grep -qx "$MULTI_B" || fail "multi-start omitted $MULTI_B"
MULTI_WAIT=$(ttos wait "$MULTI_A" "$MULTI_B")
[ "$MULTI_WAIT" = "$(printf '3\n4')" ] || fail "multi-wait returned: $MULTI_WAIT"
ttos container rm "$MULTI_A" "$MULTI_B"

echo "== persisted host networking and fork rejection =="
command -v curl >/dev/null 2>&1 || fail "curl is required for the host-network service probe"
ttos container create --network host --name "$HOSTNET" "$BUSYBOX_IMAGE" sh -c \
  "mkdir -p /tmp/www; echo hostnet-ok >/tmp/www/index.html; exec httpd -f -p $HOSTNET_PORT -h /tmp/www"
ttos container start "$HOSTNET"
deadline=$(( $(date +%s) + 15 ))
HOSTNET_RESPONSE=
while [ "$HOSTNET_RESPONSE" != hostnet-ok ]; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "host-network BusyBox service did not become reachable"
  HOSTNET_RESPONSE=$(curl -fsS --max-time 1 "http://127.0.0.1:$HOSTNET_PORT/" 2>/dev/null || true)
  [ "$HOSTNET_RESPONSE" = hostnet-ok ] || sleep 1
done
[ "$(ttos inspect -f '{{.HostConfig.NetworkMode}}' "$HOSTNET")" = host ] ||
  fail "inspect did not retain host network mode"
if ttos fork --check "$HOSTNET" >/dev/null 2>&1; then
  fail "host-network container was incorrectly eligible for a live fork"
fi
ttos stop --time 1 "$HOSTNET"
ttos rm "$HOSTNET"

ttos run --name "$PREFIX-alpine" "$ALPINE_IMAGE" sh -c \
  'grep -q Alpine /etc/os-release && apk --version >/dev/null && echo alpine-ok'
ttos rm "$PREFIX-alpine"
ttos run --name "$PREFIX-alpine" --user 123:456 "$ALPINE_IMAGE" sh -c \
  'test "$(stat -c %a /)" = 755 && test -r /etc/os-release && \
   touch /dev/shm/nonroot-ok && test "$(stat -c %u:%g /dev/shm/nonroot-ok)" = 123:456 && \
   echo alpine-nonroot-ok'
ttos rm "$PREFIX-alpine"

ttos run --name "$PREFIX-python" --ulimit nofile=64:128 --ulimit core=0 "$PYTHON_IMAGE" python3 -c \
  'import json, resource, sqlite3, threading; assert resource.getrlimit(resource.RLIMIT_NOFILE) == (64, 128); assert resource.getrlimit(resource.RLIMIT_CORE) == (0, 0); db=sqlite3.connect(":memory:"); db.execute("create table t(v)"); db.execute("insert into t values (7)"); assert db.execute("select v from t").fetchone()[0] == 7; t=threading.Thread(target=lambda: json.dumps({"ok": True})); t.start(); t.join(); print("python-ok")'
[ "$(ttos inspect -f '{{len .HostConfig.Ulimits}}|{{(index .HostConfig.Ulimits 0).Name}}|{{(index .HostConfig.Ulimits 0).Soft}}|{{(index .HostConfig.Ulimits 0).Hard}}|{{(index .HostConfig.Ulimits 1).Name}}|{{(index .HostConfig.Ulimits 1).Soft}}|{{(index .HostConfig.Ulimits 1).Hard}}' "$PREFIX-python")" = "2|core|0|0|nofile|64|128" ] ||
  fail "inspect did not retain sorted Docker ulimits"
ttos rm "$PREFIX-python"

echo "== rootless Sentry PID limit and branch-local inheritance =="
ttos run --name "$PIDS_LIMIT" --detach --pids-limit 3 "$PYTHON_IMAGE" python3 -c "
import errno, os, signal
children = []
def release(signum, frame):
    if children:
        pid = children.pop()
        os.kill(pid, signal.SIGTERM)
        os.waitpid(pid, 0)
        print(\"pids-released\", flush=True)
signal.signal(signal.SIGUSR1, release)
for _ in range(2):
    pid = os.fork()
    if pid == 0:
        signal.pause()
        os._exit(0)
    children.append(pid)
try:
    pid = os.fork()
except OSError as error:
    assert error.errno == errno.EAGAIN, error
    print(\"pids-ready\", flush=True)
else:
    if pid == 0:
        os._exit(99)
    raise RuntimeError(\"third fork unexpectedly succeeded\")
while True:
    signal.pause()
"
deadline=$(( $(date +%s) + 15 ))
until ttos logs "$PIDS_LIMIT" | grep -q pids-ready; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "PID-limited container did not reach its cap"
  sleep 1
done
[ "$(ttos inspect -f "{{.HostConfig.PidsLimit}}" "$PIDS_LIMIT")" = 3 ] ||
  fail "inspect did not retain --pids-limit"
if ttos exec "$PIDS_LIMIT" true >/dev/null 2>&1; then
  fail "exec bypassed a full PID limit"
fi
ttos fork --check "$PIDS_LIMIT"
ttos fork "$PIDS_LIMIT" "$PIDS_LIMIT@child"
if ttos exec "$PIDS_LIMIT@child" true >/dev/null 2>&1; then
  fail "live-forked child lost its full PID limit"
fi
ttos kill --signal SIGUSR1 "$PIDS_LIMIT" >/dev/null
deadline=$(( $(date +%s) + 15 ))
until ttos logs "$PIDS_LIMIT" | grep -q pids-released; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "parent branch did not release a PID slot"
  sleep 1
done
ttos exec "$PIDS_LIMIT" true
if ttos exec "$PIDS_LIMIT@child" true >/dev/null 2>&1; then
  fail "parent PID release leaked into the child branch"
fi
ttos stop --time 1 "$PIDS_LIMIT@child" "$PIDS_LIMIT"
ttos branch rm "$PIDS_LIMIT@child"
ttos rm "$PIDS_LIMIT"

ttos create --name "$PREFIX-nginx" "$NGINX_IMAGE" nginx -t
[ "$(ttos inspect -f '{{.Config.StopSignal}}' "$PREFIX-nginx")" = SIGQUIT ] ||
  fail "nginx image STOPSIGNAL was not inherited"
ttos start --attach "$PREFIX-nginx"
ttos rm "$PREFIX-nginx"

ttos run --name "$PREFIX-redis" "$REDIS_IMAGE" sh -c \
  'test "$(stat -c %u:%g /data)" = 999:1000 && echo redis-owner-ok'
REDIS_VOLUME=$(ttos inspect -f '{{(index .Mounts 0).Name}}|{{(index .Mounts 0).Destination}}' "$PREFIX-redis")
case "$REDIS_VOLUME" in
  *"|/data") ;;
  *) fail "Redis image VOLUME was not materialized: $REDIS_VOLUME" ;;
esac
ttos rm --volumes "$PREFIX-redis"

echo "== persisted stop signal and timeout policy =="
ttos run --name "$STOP_POLICY" --detach --stop-signal SIGUSR1 --stop-timeout 3 \
  "$ALPINE_IMAGE" sh -c "trap \"exit 42\" USR1; echo stop-policy-ready; while :; do sleep 0.05; done"
deadline=$(( $(date +%s) + 15 ))
until ttos logs "$STOP_POLICY" | grep -q stop-policy-ready; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "custom-stop container did not become ready"
  sleep 1
done
[ "$(ttos inspect -f '{{.Config.StopSignal}}|{{.HostConfig.StopTimeout}}' "$STOP_POLICY")" = "SIGUSR1|3" ] ||
  fail "inspect did not retain custom stop policy"
ttos stop "$STOP_POLICY"
[ "$(ttos inspect -f '{{.State.ExitCode}}' "$STOP_POLICY")" = 42 ] ||
  fail "ttos stop did not deliver the configured SIGUSR1"
ttos rm "$STOP_POLICY"

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

echo "== bounded log retention accounting =="
ttos run --name "$RETENTION" --detach --log-opt max-size=128 --log-opt max-file=1 "$PYTHON_IMAGE" python3 -c 'import sys,time; [(sys.stdout.write("X"*96+"\n"),sys.stdout.flush(),time.sleep(0.02)) for _ in range(8)]' >/dev/null
ttos wait "$RETENTION" >/dev/null
RETENTION_INSPECT="$TTOS_E2E_ROOT/retention-inspect.json"
ttos inspect "$RETENTION" >"$RETENTION_INSPECT"
grep -Eq '"evicted_bytes": [1-9][0-9]*' "$RETENTION_INSPECT" || fail "inspect omitted evicted output bytes"
grep -Eq '"evicted_generations": [1-9][0-9]*' "$RETENTION_INSPECT" || fail "inspect omitted evicted output generations"
ttos logs "$RETENTION" >/dev/null
RETENTION_EVENTS="$TTOS_E2E_ROOT/retention-events.jsonl"
ttos events --json --filter action=logs >"$RETENTION_EVENTS"
grep -q "\"subject\":\"$RETENTION\"" "$RETENTION_EVENTS" || fail "events omitted retention log subject"
grep -q '"output_evicted_bytes":"' "$RETENTION_EVENTS" || fail "events omitted output eviction snapshot"
ttos rm "$RETENTION"

echo "== explicit logging drivers =="
ttos create --name "$NOLOG" --log-driver none "$BUSYBOX_IMAGE" sh -c "echo hidden" >/dev/null
[ "$(ttos inspect -f '{{.HostConfig.LogConfig.Type}}' "$NOLOG")" = "none" ] ||
  fail "inspect omitted the none log driver"
ttos start "$NOLOG" >/dev/null
if ttos logs "$NOLOG" >"$TTOS_E2E_ROOT/nolog.out" 2>"$TTOS_E2E_ROOT/nolog.err"; then
  fail "logs accepted a container configured with the none log driver"
fi
grep -q "does not support reading" "$TTOS_E2E_ROOT/nolog.err" ||
  fail "none log-driver rejection lacked a compatibility diagnostic"
ttos rm --force "$NOLOG"

echo "== detached lifecycle, managed-volume CoW, and live fork =="
ttos volume create "$VOLUME"
LIFECYCLE_ID=$(ttos run --name "$LIFECYCLE" --detach --read-only \
  --ulimit nofile=256:256 \
  --shm-size 32m \
  --group-add 4242 --cap-add sys_ptrace --cap-drop net_raw \
  --add-host ttos.local:192.0.2.10 --add-host v6.local=2001:db8::10 \
  --dns 203.0.113.53 --dns-search example.test --dns-option ndots:2 \
  --env-file "$ENV_FILE" --env FROM_FILE=lifecycle \
  --label-file "$LABEL_FILE" --label suite=lifecycle \
  --log-driver json-file --log-opt max-size=64k --log-opt max-file=3 --log-opt labels=suite --log-opt env=FROM_FILE \
  --volume "$VOLUME:/state" \
  --volume "$VOLUME:/state-view:ro" \
  --volume "$HOST_FIXTURE:/host-fixture:ro" \
  --mount "type=bind,source=$HOST_FIXTURE_TWO,target=/host-fixture-two,readonly,immutable-source" \
  --mount "type=bind,source=$HOST_COW_FIXTURE,target=/cow-host,cow,immutable-source" \
  --tmpfs /scratch:mode=0700,size=1m \
  --mount type=proc,target=/alt-proc,readonly \
  --mount type=proc,target=/repeat-proc,readonly \
  --mount type=proc,target=/nested/views/proc,readonly \
  --mount type=sysfs,target=/alt-sys,readonly \
  --mount type=sysfs,target=/repeat-sys,readonly \
  --mount type=sysfs,target=/nested/views/sys,readonly \
  "$ALPINE_IMAGE" sh -c \
  'grep -Eq "^192\.0\.2\.10[[:space:]]+ttos\.local$" /etc/hosts; grep -Eq "^2001:db8::10[[:space:]]+v6\.local$" /etc/hosts; grep -qx "nameserver 203.0.113.53" /etc/resolv.conf; grep -qx "search example.test" /etc/resolv.conf; grep -qx "options ndots:2" /etc/resolv.conf; echo parent >/state/value; echo scratch-parent >/scratch/value; test "$(cat /cow-host/value)" = host-cow-lower; echo parent >/cow-host/value; echo parent-created >/cow-host/created; id -G | grep -qw 4242; set -- $(grep ^CapEff: /proc/self/status); [ $((0x$2 & 0x80000)) -ne 0 ]; [ $((0x$2 & 0x2000)) -eq 0 ]; test -r /state-view/value; test -r /alt-proc/self/status; test -r /repeat-proc/1/status; test -r /nested/views/proc/self/status; test -r /alt-sys/kernel; test -r /repeat-sys/kernel; test -r /nested/views/sys/kernel; i=0; while :; do echo "tick:$i"; i=$((i+1)); sleep 1; done')
container_id_valid "$LIFECYCLE_ID" || fail "detached run returned invalid container ID: $LIFECYCLE_ID"
LIFECYCLE_PREFIX=$(printf %.16s "$LIFECYCLE_ID")
LIFECYCLE_SHORT=$(printf %.12s "$LIFECYCLE_ID")
[ "$(ttos inspect -f "{{.Id}}|{{.Name}}" "$LIFECYCLE_PREFIX")" = "$LIFECYCLE_ID|/$LIFECYCLE" ] ||
  fail "detached container was not addressable by ID prefix"

deadline=$(( $(date +%s) + 15 ))
while ! ttos logs --tail 10 "$LIFECYCLE" 2>/dev/null | grep -q 'tick:'; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "detached init did not produce console output"
  sleep 1
done

echo "== daemon restart reconciliation =="
ttos daemon status | grep -q '"version":2' || fail "ttosd did not report API version 2"
ttos daemon stop
if ttos daemon status >/dev/null 2>&1; then
  fail "ttosd remained reachable after stop"
fi
[ "$(ttos inspect -f '{{.State.Status}}' "$LIFECYCLE")" = running ] ||
  fail "autostarted ttosd did not reconcile the running container"
ttos daemon status | grep -q '"version":2' || fail "ttosd did not autostart after restart"
deadline=$(( $(date +%s) + 15 ))
while ! ttos logs --tail 10 "$LIFECYCLE" 2>/dev/null | grep -q 'tick:'; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "console draining did not survive daemon restart"
  sleep 1
done
echo "== console sidecar crash recovery =="
SIDECAR_PID_FILE="$DATA_ROOT/containers/$LIFECYCLE_ID/console/main.pid"
deadline=$(( $(date +%s) + 15 ))
while [ ! -s "$SIDECAR_PID_FILE" ]; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "active console sidecar did not publish its PID"
  sleep 1
done
OLD_SIDECAR_PID=$(cat "$SIDECAR_PID_FILE")
kill -KILL "$OLD_SIDECAR_PID"
deadline=$(( $(date +%s) + 15 ))
while :; do
  ttos logs --tail 10 "$LIFECYCLE" >/dev/null 2>&1 || true
  NEW_SIDECAR_PID=$(cat "$SIDECAR_PID_FILE" 2>/dev/null || true)
  if [ -n "$NEW_SIDECAR_PID" ] && [ "$NEW_SIDECAR_PID" != "$OLD_SIDECAR_PID" ] && kill -0 "$NEW_SIDECAR_PID" 2>/dev/null; then
    break
  fi
  [ "$(date +%s)" -lt "$deadline" ] || fail "console sidecar was not replaced after SIGKILL"
  sleep 1
done
deadline=$(( $(date +%s) + 15 ))
while ! ttos logs --tail 10 "$LIFECYCLE" 2>/dev/null | grep -q "tick:"; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "console output did not resume after sidecar replacement"
  sleep 1
done

ttos container restart --force --time 2 "$LIFECYCLE"
deadline=$(( $(date +%s) + 15 ))
while ! ttos logs --tail 10 "$LIFECYCLE" 2>/dev/null | grep -q 'tick:'; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "restarted detached init did not produce console output"
  sleep 1
done

ttos exec "$LIFECYCLE" grep -qx "$LIFECYCLE" /etc/hostname ||
  fail "restarted container did not retain /etc/hostname"

ttos exec "$LIFECYCLE" sh -c "grep -Eq \"^192\.0\.2\.10[[:space:]]+ttos\.local$\" /etc/hosts; grep -Eq \"^2001:db8::10[[:space:]]+v6\.local$\" /etc/hosts; grep -qx \"nameserver 203.0.113.53\" /etc/resolv.conf; grep -qx \"search example.test\" /etc/resolv.conf; grep -qx \"options ndots:2\" /etc/resolv.conf" ||
  fail "restarted container did not retain generated network files"

EXEC_GROUPS=$(ttos exec --user 123:456 --additional-gids 4242 --additional-gids 4243 "$LIFECYCLE" /bin/busybox id)
echo "$EXEC_GROUPS" | grep -q "4242,4243" || fail "exec supplementary groups missing: $EXEC_GROUPS"
ttos exec --cap CAP_SYS_ADMIN "$LIFECYCLE" sh -c \
  'set -- $(grep ^CapEff: /proc/self/status); value=$2; [ $((0x$value & 0x200000)) -ne 0 ]'
PROCESS_JSON="$TTOS_E2E_ROOT/process.json"
printf '%s\n' \
  '{"terminal":false,"user":{"uid":123,"gid":456,"additionalGids":[4342,4343]},"args":["sh","-c","id; echo mark:$MARK"],"env":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin","MARK=process-ok"],"cwd":"/"}' \
  >"$PROCESS_JSON"
PROCESS_OUTPUT=$(ttos exec --process "$PROCESS_JSON" "$LIFECYCLE")
echo "$PROCESS_OUTPUT" | grep -q 'groups=4342,4343' || fail "process JSON supplementary groups missing: $PROCESS_OUTPUT"
echo "$PROCESS_OUTPUT" | grep -q 'mark:process-ok' || fail "process JSON environment missing: $PROCESS_OUTPUT"

[ "$(ttos inspect -f "{{.HostConfig.ShmSize}}" "$LIFECYCLE")" = 33554432 ] || fail "inspect omitted shared memory size"
[ "$(ttos inspect -f '{{.HostConfig.LogConfig.Type}}|{{index .HostConfig.LogConfig.Config "max-size"}}|{{index .HostConfig.LogConfig.Config "max-file"}}|{{index .HostConfig.LogConfig.Config "labels"}}|{{index .HostConfig.LogConfig.Config "env"}}' "$LIFECYCLE")" = "json-file|65536|3|suite|FROM_FILE" ] ||
  fail "inspect omitted json-file retention policy"
[ "$(ttos inspect -f '{{index .HostConfig.GroupAdd 0}}|{{index .HostConfig.CapAdd 0}}|{{index .HostConfig.CapDrop 0}}' "$LIFECYCLE")" = "4242|SYS_PTRACE|NET_RAW" ] ||
  fail "inspect omitted supplementary groups or capability changes"
[ "$(ttos exec "$LIFECYCLE" sh -c 'df -k /dev/shm | tail -n 1 | xargs | cut -d" " -f2')" = 32768 ] ||
  fail "restarted container did not retain /dev/shm size"
[ "$(ttos inspect -f "{{.HostConfig.ReadonlyRootfs}}" "$LIFECYCLE")" = true ] || fail "inspect omitted read-only rootfs policy"
[ "$(ttos inspect -f "{{index .HostConfig.ExtraHosts 0}}|{{index .HostConfig.ExtraHosts 1}}|{{index .HostConfig.Dns 0}}|{{index .HostConfig.DnsSearch 0}}|{{index .HostConfig.DnsOptions 0}}" "$LIFECYCLE")" = "ttos.local:192.0.2.10|v6.local:2001:db8::10|203.0.113.53|example.test|ndots:2" ] ||
  fail "inspect omitted Docker network configuration"

if ttos exec "$LIFECYCLE" sh -c "echo forbidden >/root-forbidden" >/dev/null 2>&1; then
  fail "read-only root filesystem accepted a parent write"
fi
[ "$(ttos exec "$LIFECYCLE" cat /state/value)" = "parent" ] || fail "parent managed volume was not initialized"
[ "$(ttos exec "$LIFECYCLE" cat /state-view/value)" = "parent" ] || fail "read-only managed-volume alias did not share parent contents"
if ttos exec "$LIFECYCLE" sh -c "echo forbidden >/state-view/value" >/dev/null 2>&1; then
  fail "read-only managed-volume alias accepted a parent write"
fi
[ "$(ttos exec "$LIFECYCLE" cat /scratch/value)" = "scratch-parent" ] || fail "parent tmpfs was not initialized"
[ "$(ttos exec "$LIFECYCLE" cat /cow-host/value)" = "parent" ] || fail "managed overlay did not retain the parent copy-up"
[ "$(cat "$HOST_COW_FIXTURE/value")" = "host-cow-lower" ] || fail "managed overlay modified its immutable host lower"
[ "$(ttos exec "$LIFECYCLE" cat /host-fixture-two/value)" = "host-readonly-two" ] || fail "second read-only host bind was not readable"
[ "$(ttos exec "$LIFECYCLE" stat -c %a /scratch)" = "700" ] || fail "tmpfs mode was not applied"
ttos exec "$LIFECYCLE" sh -c "test -r /alt-proc/self/status && test -r /alt-sys/kernel" ||
  fail "additional procfs/sysfs mounts were not readable"
for path in /repeat-proc/1/status /nested/views/proc/self/status /repeat-sys/kernel /nested/views/sys/kernel; do
  ttos exec "$LIFECYCLE" test -r "$path" || fail "nested or repeated pseudo-filesystem path $path was not readable"
done
ttos exec "$LIFECYCLE" grep -Eq " /state-view ro(,| )" /proc/self/mountinfo || fail "read-only managed-volume alias lost its bind-style remount flag"
ttos exec "$LIFECYCLE" grep -Eq " /host-fixture ro(,| )" /proc/self/mountinfo || fail "read-only host bind lost its remount flag"
ttos exec "$LIFECYCLE" awk '$5 == "/nested/views/proc" { split($6, options, ","); for (i in options) seen[options[i]] = 1; exit !(seen["ro"] && seen["nosuid"] && seen["nodev"] && seen["noexec"]) } END { if (!seen["ro"]) exit 1 }' /proc/self/mountinfo || fail "nested procfs mount flags were not retained"
[ "$(ttos exec "$LIFECYCLE" cat /host-fixture/value)" = "host-readonly" ] || fail "read-only host bind was not readable"
if ttos exec "$LIFECYCLE" sh -c 'echo forbidden >/host-fixture/value' >/dev/null 2>&1; then
  fail "read-only host bind accepted a write"
fi
ttos fork --check "$LIFECYCLE_PREFIX"
FORK_CHECK_JSON=$(ttos fork --check --json "$LIFECYCLE_PREFIX")
echo "$FORK_CHECK_JSON" | grep -q '"warnings"' || fail "fork check omitted shared-view warnings"
echo "$FORK_CHECK_JSON" | grep -q "$HOST_FIXTURE" || fail "fork check warning omitted first host bind"
echo "$FORK_CHECK_JSON" | grep -q "$HOST_FIXTURE_TWO" || fail "fork check warning omitted second host bind"
echo "$FORK_CHECK_JSON" | grep -q "$HOST_COW_FIXTURE" || fail "fork check warning omitted managed CoW lower"
echo "$FORK_CHECK_JSON" | grep -q "branch-local memory CoW upper" || fail "fork check omitted managed-overlay semantics"
echo "$FORK_CHECK_JSON" | grep -q "cannot enforce" || fail "immutable-source warning overstated enforcement"

echo "== attached exec eligibility recovery =="
ttos exec "$LIFECYCLE" rm -f /state/attached-exec-ready /state/attached-exec-release
ttos exec "$LIFECYCLE" sh -c \
  'echo ready >/state/attached-exec-ready; while [ ! -e /state/attached-exec-release ]; do sleep 0.1; done' \
  >"$TTOS_E2E_ROOT/attached-exec.out" 2>"$TTOS_E2E_ROOT/attached-exec.err" &
ATTACHED_EXEC_HOST_PID=$!
deadline=$(( $(date +%s) + 15 ))
while ! ttos exec "$LIFECYCLE" test -e /state/attached-exec-ready >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "attached exec did not reach its ready point"
  sleep 1
done
if ttos fork --check "$LIFECYCLE" >"$TTOS_E2E_ROOT/attached-check.out" 2>"$TTOS_E2E_ROOT/attached-check.err"; then
  fail "live fork accepted an attached exec with imported host stdio"
fi
grep -Eq '\*host.fileDescription' "$TTOS_E2E_ROOT/attached-check.out" "$TTOS_E2E_ROOT/attached-check.err" ||
  fail "attached-exec rejection did not identify its imported host resource"
ttos exec "$LIFECYCLE" touch /state/attached-exec-release
wait "$ATTACHED_EXEC_HOST_PID" || fail "attached exec did not exit successfully"
ttos fork --check "$LIFECYCLE" >/dev/null || fail "fork eligibility did not recover after attached exec exit"

ttos fork "$LIFECYCLE_PREFIX" "$LIFECYCLE_PREFIX@child"
echo host-mutated >"$HOST_FIXTURE/value"
echo host-mutated-two >"$HOST_FIXTURE_TWO/value"
[ "$(ttos exec "$LIFECYCLE" cat /host-fixture/value)" = "host-mutated" ] || fail "parent did not observe host-side mutation"
[ "$(ttos exec "$LIFECYCLE@child" cat /host-fixture/value)" = "host-mutated" ] || fail "child did not observe shared host-side mutation"
[ "$(ttos exec "$LIFECYCLE" cat /host-fixture-two/value)" = "host-mutated-two" ] || fail "parent did not observe second host mutation"
[ "$(ttos exec "$LIFECYCLE@child" cat /host-fixture-two/value)" = "host-mutated-two" ] || fail "child did not observe second shared host mutation"
[ "$(ttos exec "$LIFECYCLE" sh -c "ulimit -n")" = 256 ] || fail "parent exec did not inherit RLIMIT_NOFILE"
[ "$(ttos exec "$LIFECYCLE" sh -c 'df -k /dev/shm | tail -n 1 | xargs | cut -d" " -f2')" = 32768 ] || fail "parent branch lost /dev/shm size"
ttos exec "$LIFECYCLE" sh -c 'id -G | grep -qw 4242; set -- $(grep ^CapEff: /proc/self/status); [ $((0x$2 & 0x80000)) -ne 0 ]; [ $((0x$2 & 0x2000)) -eq 0 ]' || fail "parent exec lost configured groups or capabilities"
[ "$(ttos exec "$LIFECYCLE@child" sh -c "ulimit -n")" = 256 ] || fail "live-forked child exec did not inherit RLIMIT_NOFILE"
[ "$(ttos exec "$LIFECYCLE@child" sh -c 'df -k /dev/shm | tail -n 1 | xargs | cut -d" " -f2')" = 32768 ] || fail "live-forked child lost /dev/shm size"
ttos exec "$LIFECYCLE@child" sh -c 'id -G | grep -qw 4242; set -- $(grep ^CapEff: /proc/self/status); [ $((0x$2 & 0x80000)) -ne 0 ]; [ $((0x$2 & 0x2000)) -eq 0 ]' || fail "child exec lost configured groups or capabilities"
for path in /repeat-proc/1/status /nested/views/proc/self/status /repeat-sys/kernel /nested/views/sys/kernel; do
  ttos exec "$LIFECYCLE@child" test -r "$path" || fail "live-forked child lost pseudo-filesystem path $path"
done
ttos exec "$LIFECYCLE@child" grep -Eq " /state-view ro(,| )" /proc/self/mountinfo || fail "live-forked child lost read-only volume alias flags"

ttos exec "$LIFECYCLE" sh -c "grep -Eq \"^192\.0\.2\.10[[:space:]]+ttos\.local$\" /etc/hosts; grep -qx \"nameserver 203.0.113.53\" /etc/resolv.conf" || fail "parent branch lost generated network files"
ttos exec "$LIFECYCLE@child" sh -c "grep -Eq \"^2001:db8::10[[:space:]]+v6\.local$\" /etc/hosts; grep -qx \"search example.test\" /etc/resolv.conf; grep -qx \"options ndots:2\" /etc/resolv.conf" || fail "live-forked child lost generated network files"

RUNNING_INSPECT=$(ttos inspect -f '{{.State.Status}}|{{.Config.Image}}|{{len .TTOS.Branches}}' "$LIFECYCLE")
[ "$RUNNING_INSPECT" = "running|docker.io/library/alpine:3.20|2" ] ||
  fail "running forked inspect returned $RUNNING_INSPECT"
[ "$(ttos inspect -f '{{index .Config.Labels "suite"}}|{{index .Config.Labels "file-only"}}|{{index .Config.Labels "empty"}}' "$LIFECYCLE")" = \
  "lifecycle|value|" ] || fail "forked-container inspect did not retain merged labels"
LIFECYCLE_ENV=$(ttos inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$LIFECYCLE")
echo "$LIFECYCLE_ENV" | grep -qx 'FROM_FILE=lifecycle' || fail "forked-container inspect omitted direct environment override"
echo "$LIFECYCLE_ENV" | grep -qx 'TTOS_E2E_HOST_IMPORT=host-import' || fail "forked-container inspect omitted imported host environment"
INSPECT_JSON="$TTOS_E2E_ROOT/lifecycle-inspect.json"
ttos inspect --format json "$LIFECYCLE" >"$INSPECT_JSON"
grep -q "\"Name\":\"$VOLUME\"" "$INSPECT_JSON" || fail "inspect omitted the managed volume"
grep -q "\"Source\":\"$HOST_FIXTURE\"" "$INSPECT_JSON" || fail "inspect omitted the read-only host bind"
grep -q "\"Source\":\"$HOST_FIXTURE_TWO\"" "$INSPECT_JSON" || fail "inspect omitted the second read-only host bind"
grep -q "\"Source\":\"$HOST_COW_FIXTURE\"" "$INSPECT_JSON" || fail "inspect omitted the managed CoW host lower"
grep -q '"managed_cow":true' "$INSPECT_JSON" || fail "TTOS inspect omitted managed CoW metadata"
grep -q '"semantics":"branch-local-cow-over-user-asserted-immutable-host-lower"' "$INSPECT_JSON" || fail "TTOS inspect omitted managed-overlay semantics"
grep -q '"semantics":"shared-external-view"' "$INSPECT_JSON" || fail "TTOS inspect omitted shared external-view semantics"
grep -q '"semantics":"user-asserted-immutable-external-view"' "$INSPECT_JSON" || fail "TTOS inspect omitted immutable-source attestation"
grep -q '"immutable_source_asserted":true' "$INSPECT_JSON" || fail "TTOS inspect omitted immutable-source assertion flag"
grep -q '"processes":\[' "$INSPECT_JSON" || fail "inspect omitted live branch processes"
grep -q "\"/scratch\":\"mode=0700,size=1m\"" "$INSPECT_JSON" || fail "inspect omitted tmpfs options"
grep -q "\"Type\":\"proc\"" "$INSPECT_JSON" || fail "inspect omitted the additional procfs mount"
grep -q "\"Type\":\"sysfs\"" "$INSPECT_JSON" || fail "inspect omitted the additional sysfs mount"
[ "$(ttos inspect -f "{{len .Mounts}}|{{len .HostConfig.Binds}}" "$LIFECYCLE")" = "11|3" ] || fail "internal network files leaked into Docker mount inspection"

FORK_INSPECT=$(ttos inspect --fork-check -f '{{range .TTOS.Branches}}{{.Metadata.ID}}={{if .ForkEligibility}}{{.ForkEligibility.Eligible}}{{else}}error{{end}} {{end}}' "$LIFECYCLE")
echo "$FORK_INSPECT" | grep -q 'main=true' || fail "inspect fork check omitted eligible main branch: $FORK_INSPECT"
echo "$FORK_INSPECT" | grep -q 'child=true' || fail "inspect fork check omitted eligible child branch: $FORK_INSPECT"

PS_CHILD=$(ttos ps --filter branch=child --filter status=running --filter ancestor="$ALPINE_IMAGE" --format '{{.ID}}')
[ "$PS_CHILD" = "$LIFECYCLE_SHORT@child" ] || fail "filtered ps selected $PS_CHILD instead of the child branch"
[ "$(ttos ps --latest --quiet)" = "$LIFECYCLE_SHORT@child" ] || fail "ps --latest did not select the forked child"
ttos ps --filter branch=child --format json |
  grep -q '"created_at":"' ||
  fail "ps JSON omitted the child creation timestamp"
PS_SIZE=$(ttos ps --filter branch=child --size --format '{{.SizeRW}}|{{.SizeRootFS}}')
PS_SIZE_RW=${PS_SIZE%%|*}
PS_SIZE_ROOT=${PS_SIZE#*|}
case "$PS_SIZE_RW:$PS_SIZE_ROOT" in
  *[!0-9:]*|:*) fail "ps --size returned nonnumeric accounting: $PS_SIZE" ;;
esac
[ "$PS_SIZE_ROOT" -gt "$PS_SIZE_RW" ] || fail "ps --size virtual size did not include the image: $PS_SIZE"

ttos exec "$LIFECYCLE@child" sh -c 'echo child >/state/value'
ttos exec "$LIFECYCLE@child" sh -c "echo child >/cow-host/value; echo child-created >/cow-host/child-only"
[ "$(ttos exec "$LIFECYCLE" cat /cow-host/value)" = "parent" ] || fail "child managed-overlay write leaked into parent"
[ "$(ttos exec "$LIFECYCLE@child" cat /cow-host/value)" = "child" ] || fail "child managed-overlay copy-up was not retained"
[ "$(cat "$HOST_COW_FIXTURE/value")" = "host-cow-lower" ] || fail "child managed-overlay write reached the host lower"
[ "$(ttos exec "$LIFECYCLE" cat /state/value)" = "parent" ] || fail "child write leaked into parent managed volume"
[ "$(ttos exec "$LIFECYCLE@child" cat /state/value)" = "child" ] || fail "child managed-volume write was not retained"
[ "$(ttos exec "$LIFECYCLE" cat /state-view/value)" = "parent" ] || fail "child write leaked through parent read-only volume alias"
[ "$(ttos exec "$LIFECYCLE@child" cat /state-view/value)" = "child" ] || fail "child read-only volume alias did not follow branch-local contents"
if ttos exec "$LIFECYCLE@child" sh -c "echo forbidden >/state-view/value" >/dev/null 2>&1; then
  fail "read-only managed-volume alias accepted a child write"
fi

if ttos exec "$LIFECYCLE@child" sh -c "echo forbidden >/child-root-forbidden" >/dev/null 2>&1; then
  fail "read-only root filesystem accepted a child write"
fi
ttos exec "$LIFECYCLE@child" sh -c "echo scratch-child >/scratch/value"
[ "$(ttos exec "$LIFECYCLE" cat /scratch/value)" = "scratch-parent" ] || fail "child write leaked into parent tmpfs"
[ "$(ttos exec "$LIFECYCLE@child" cat /scratch/value)" = "scratch-child" ] || fail "child tmpfs write was not retained"

ttos pause "$LIFECYCLE_PREFIX" "$LIFECYCLE_ID@child"
PAUSED_INSPECT=$(ttos inspect -f '{{range .TTOS.Branches}}{{.Metadata.ID}}={{.Metadata.State}} {{end}}' "$LIFECYCLE")
echo "$PAUSED_INSPECT" | grep -q 'main=paused' || fail "inspect did not report paused main branch: $PAUSED_INSPECT"
echo "$PAUSED_INSPECT" | grep -q 'child=paused' || fail "inspect did not report paused child branch: $PAUSED_INSPECT"
ttos resume "$LIFECYCLE" "$LIFECYCLE@child"
ttos ps --all
ttos top "$LIFECYCLE"
ttos ls --tree
ttos logs --tail 2 "$LIFECYCLE"
ttos logs --tail 2 "$LIFECYCLE@child"
TIMESTAMPED_LOGS=$(ttos logs --timestamps --details --since 24h --until 0s "$LIFECYCLE@child")
echo "$TIMESTAMPED_LOGS" | grep -Eq "^[0-9]{4}-[0-9]{2}-[0-9]{2}T[^ ]+ FROM_FILE=lifecycle,suite=lifecycle tick:" || fail "timestamped detailed logs omitted attributes: $TIMESTAMPED_LOGS"
[ -z "$(ttos logs --since 4102444800 "$LIFECYCLE@child")" ] || fail "logs --since included records older than its boundary"
[ -z "$(ttos logs --until 24h "$LIFECYCLE@child")" ] || fail "logs --until included records newer than its boundary"

# Persist immutable lineage when an intermediate source branch is deleted.
ttos fork "$LIFECYCLE@child" "$LIFECYCLE@grandchild"
[ "$(ttos exec "$LIFECYCLE@grandchild" cat /cow-host/value)" = "child" ] || fail "grandchild did not inherit managed-overlay state"
ttos exec "$LIFECYCLE@grandchild" sh -c "echo grandchild >/cow-host/value; echo grandchild-created >/cow-host/grandchild-only"
[ "$(ttos exec "$LIFECYCLE@child" cat /cow-host/value)" = "child" ] || fail "grandchild managed-overlay write leaked into child"
[ "$(ttos exec "$LIFECYCLE@grandchild" cat /cow-host/value)" = "grandchild" ] || fail "grandchild managed-overlay write was not retained"
ttos branch rm "$LIFECYCLE@child"
[ "$(ttos exec "$LIFECYCLE@grandchild" cat /cow-host/value)" = "grandchild" ] || fail "grandchild managed overlay depended on deleted source branch"
LINEAGE_TREE=$(ttos ls --tree)
echo "$LINEAGE_TREE" | grep -q "child deleted" || fail "tree omitted the deleted intermediate branch"
echo "$LINEAGE_TREE" | grep -q "grandchild running" || fail "tree omitted the live grandchild below its tombstone"
if ttos fork "$LIFECYCLE@main" "$LIFECYCLE@child" >"$TTOS_E2E_ROOT/lineage-reuse.out" 2>"$TTOS_E2E_ROOT/lineage-reuse.err"; then
  fail "fork reused a branch ID retained by immutable lineage"
fi
grep -q "retained by immutable lineage" "$TTOS_E2E_ROOT/lineage-reuse.err" ||
  fail "branch ID reuse rejection lacked a lineage diagnostic"

echo "== branch-native detached exec bookkeeping =="
EXEC_SELECTOR=$(ttos exec --detach "$LIFECYCLE" sh -c 'exec sleep 300')
EXEC_ID=${EXEC_SELECTOR##*:}
[ -n "$EXEC_ID" ] || fail "detached exec did not return a session ID"
ttos fork "$LIFECYCLE" "$LIFECYCLE@process-child" >/dev/null
ttos fork "$LIFECYCLE@process-child" "$LIFECYCLE@process-grandchild" >/dev/null
for branch in main process-child process-grandchild; do
  SESSION_JSON=$(ttos exec ls --json "$LIFECYCLE@$branch")
  echo "$SESSION_JSON" | grep -q "\"id\": \"$EXEC_ID\"" || fail "branch $branch lost cloned exec session $EXEC_ID"
  echo "$SESSION_JSON" | grep -q '"state": "running"' || fail "branch $branch did not retain a running exec session"
done
ttos exec kill "$LIFECYCLE@process-grandchild:$EXEC_ID" >/dev/null
[ "$(ttos exec wait "$LIFECYCLE@process-grandchild:$EXEC_ID")" = 137 ] || fail "grandchild exec wait status crossed branches"
ttos exec ls --json "$LIFECYCLE@process-child" | grep -q '"state": "running"' || fail "grandchild signal reached child exec"
ttos exec ls --json "$LIFECYCLE@main" | grep -q '"state": "running"' || fail "grandchild signal reached parent exec"
ttos exec kill "$LIFECYCLE@process-child:$EXEC_ID" >/dev/null
[ "$(ttos exec wait "$LIFECYCLE@process-child:$EXEC_ID")" = 137 ] || fail "child exec wait status crossed branches"
ttos exec ls --json "$LIFECYCLE@main" | grep -q '"state": "running"' || fail "child signal reached parent exec"
ttos exec kill "$LIFECYCLE@main:$EXEC_ID" >/dev/null
[ "$(ttos exec wait "$LIFECYCLE@main:$EXEC_ID")" = 137 ] || fail "parent exec wait status was not retained"
ttos branch rm "$LIFECYCLE@process-grandchild"
ttos branch rm "$LIFECYCLE@process-child"

echo "== durable daemon events =="
EVENTS_JSON="$TTOS_E2E_ROOT/events.jsonl"
ttos events --json >"$EVENTS_JSON"
for event_type in container branch exec attach eligibility image; do
  grep -q "\"type\":\"$event_type\"" "$EVENTS_JSON" || fail "durable events omitted type $event_type"
done
grep -q '"action":"fork"' "$EVENTS_JSON" || fail "durable events omitted branch fork"
grep -q '"action":"check"' "$EVENTS_JSON" || fail "durable events omitted eligibility check"
EVENT_FOLLOW="$TTOS_E2E_ROOT/events-follow.jsonl"
EVENT_UNTIL=$(( $(date +%s) + 3 ))
ttos events --follow --json --since 0s --until "$EVENT_UNTIL" --filter action=pause >"$EVENT_FOLLOW" &
EVENT_FOLLOW_PID=$!
sleep 1
ttos pause "$LIFECYCLE"
ttos resume "$LIFECYCLE"
wait "$EVENT_FOLLOW_PID" || fail "events follower failed"
grep -q '"type":"branch".*"action":"pause"' "$EVENT_FOLLOW" || fail "events follower missed a new pause event"

ttos stop --time 2 "$LIFECYCLE@grandchild" "$LIFECYCLE"
[ "$(ttos inspect -f '{{.State.Status}}|{{.State.Running}}' "$LIFECYCLE")" = "exited|false" ] ||
  fail "inspect did not report the stopped family as exited"
ttos branch rm "$LIFECYCLE@grandchild"
ttos rm "$LIFECYCLE_ID"
ttos volume rm "$VOLUME"

trap - EXIT HUP INT TERM
if $owned_root; then
  rm -rf "$TTOS_E2E_ROOT"
fi
echo "TTOS end-to-end image matrix passed"
