#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# This is a manual, networked end-to-end matrix. It intentionally uses public,
# unmodified Docker images and TTC's native OCI content store.
set -eu

TTC=${TTC:-}
TTCD=${TTCD:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
TTC_E2E_DEADLINE_SECONDS=${TTC_E2E_DEADLINE_SECONDS:-}
TTC_E2E_GUEST_CURL_TIMEOUT=${TTC_E2E_GUEST_CURL_TIMEOUT:-}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-busybox:1.36}
ALPINE_IMAGE=${ALPINE_IMAGE:-alpine:3.20}
PYTHON_IMAGE=${PYTHON_IMAGE:-python:3.12-alpine}
NGINX_IMAGE=${NGINX_IMAGE:-nginx:1.27-alpine}
REDIS_IMAGE=${REDIS_IMAGE:-redis:7-alpine}

fail() {
  echo "ttc e2e: $*" >&2
  exit 1
}

container_id_valid() {
  [ "${#1}" -eq 64 ] || return 1
  case "$1" in
    *[!0-9a-f]*) return 1 ;;
  esac
}

[ -n "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$TTC" ] || fail "TTC is not executable: $TTC"
[ -n "$TTCD" ] || fail "set TTCD to the matching ttcd binary"
[ -x "$TTCD" ] || fail "TTCD is not executable: $TTCD"
# Rootless Linux sandboxes need user namespaces; macOS confines ttcd with
# Seatbelt instead.
if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" != 1 ] && [ "$(uname -s)" = Linux ]; then
  command -v unshare >/dev/null 2>&1 || fail "unshare is required for the rootless prerequisite probe"
  if ! unshare -Ur /bin/true >/dev/null 2>&1; then
    fail "rootless UID/GID mapping is unavailable; enable unprivileged user namespaces or set TTC_UNSAFE_NO_HOST_ISOLATION=1 for trusted workloads"
  fi
fi
if [ -z "$TTC_E2E_DEADLINE_SECONDS" ]; then
  TTC_E2E_DEADLINE_SECONDS=15
fi
if [ -z "$TTC_E2E_GUEST_CURL_TIMEOUT" ]; then
  TTC_E2E_GUEST_CURL_TIMEOUT=2
fi

owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  # macOS gives TMPDIR a trailing slash; a doubled one would differ from
  # the cleaned paths TTC reports.
  tmp=${TMPDIR:-/tmp}
  TTC_E2E_ROOT=$(mktemp -d "${tmp%/}/ttc-e2e.XXXXXX")
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
HOST_FIXTURE="$TTC_E2E_ROOT/host-fixture"
HOST_FIXTURE_TWO="$TTC_E2E_ROOT/host-fixture-two"
HOST_COW_FIXTURE="$TTC_E2E_ROOT/host-cow-fixture"
mkdir -p "$HOST_FIXTURE" "$HOST_FIXTURE_TWO" "$HOST_COW_FIXTURE"
echo host-readonly >"$HOST_FIXTURE/value"
echo host-readonly-two >"$HOST_FIXTURE_TWO/value"
echo host-cow-lower >"$HOST_COW_FIXTURE/value"
ENV_FILE="$TTC_E2E_ROOT/container.env"
LABEL_FILE="$TTC_E2E_ROOT/container.labels"
TTC_E2E_HOST_IMPORT=host-import
export TTC_E2E_HOST_IMPORT
printf "%s\n" "# environment file" "FROM_FILE=file" "TTC_E2E_HOST_IMPORT" >"$ENV_FILE"
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
VNET="$PREFIX-vnet"
NETNS="$PREFIX-netns"
DAEMON_STOP="$PREFIX-daemon-stop"
STOP_POLICY="$PREFIX-stop-policy"
PIDS_LIMIT="$PREFIX-pids-limit"
HOSTNET_PORT=$((20000 + ($$ % 30000)))
NAT_FIXTURE_PORT=$((20000 + (($$ + 1000) % 30000)))
PUBLISHED_PORT=$((20000 + (($$ + 2000) % 30000)))
CHILD_PUBLISHED_PORT=$((20000 + (($$ + 3000) % 30000)))
NAT_FIXTURE_PID=
ttc() {
  "$TTC" "$@"
}

ttc_daemon_start() {
  if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
    ttc daemon start --unsafe-no-host-isolation
    return
  fi
  ttc daemon start
}

ttc_daemon_crash() {
  daemon_status=$(ttc daemon status)
  daemon_pid=$(printf '%s\n' "$daemon_status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
  [ -n "$daemon_pid" ] || fail "could not parse ttcd PID from status: $daemon_status"
  kill -KILL "$daemon_pid"
  deadline=$(( $(date +%s) + 5 ))
  while ttc daemon status >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "killed ttcd remained reachable"
    sleep 1
  done
}

stop_nat_fixture() {
  if [ -n "$NAT_FIXTURE_PID" ]; then
    kill "$NAT_FIXTURE_PID" >/dev/null 2>&1 || true
    wait "$NAT_FIXTURE_PID" >/dev/null 2>&1 || true
    NAT_FIXTURE_PID=
  fi
}

cleanup() {
  for container in "$PREFIX-busybox" "$PREFIX-alpine" "$PREFIX-python" "$PREFIX-nginx" "$PREFIX-redis" "$CONSOLE_STRESS" "$RETENTION" "$NOLOG" "$MULTI_A" "$MULTI_B" "$HOSTNET" "$VNET" "$NETNS" "$STOP_POLICY" "$PIDS_LIMIT" "$LIFECYCLE" "$ATTACH_INPUT" "$AUTO_FOREGROUND" "$AUTO_DETACHED" "$AUTO_CREATED" "$DAEMON_STOP"; do
    ttc rm --force "$container" >/dev/null 2>&1 || true
  done
  ttc daemon stop >/dev/null 2>&1 || true
  stop_nat_fixture
  if $owned_root; then
    rm -rf "$TTC_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

ttc_daemon_start

echo "== pulling public image matrix =="
for image in "$BUSYBOX_IMAGE" "$ALPINE_IMAGE" "$PYTHON_IMAGE" "$NGINX_IMAGE" "$REDIS_IMAGE"; do
  ttc pull "$image"
done
PULL_REUSE="$TTC_E2E_ROOT/pull-reuse.out"
ttc pull "$BUSYBOX_IMAGE" >"$PULL_REUSE"
grep -Eq '^Content: 0 bytes downloaded, [1-9][0-9]* bytes reused$' "$PULL_REUSE" || fail "repeat pull did not report content reuse"
PULL_QUIET="$TTC_E2E_ROOT/pull-quiet.out"
ttc pull -q "$BUSYBOX_IMAGE" >"$PULL_QUIET"
[ "$(wc -l <"$PULL_QUIET")" -eq 1 ] || fail "quiet pull emitted multiple lines"
grep -Eq '^sha256:[0-9a-f]{64}$' "$PULL_QUIET" || fail "quiet pull emitted more than the digest"

echo "== image inventory =="
IMAGE_LIST="$TTC_E2E_ROOT/images.json"
ttc images --json >"$IMAGE_LIST"
for reference in docker.io/library/busybox:1.36 docker.io/library/alpine:3.20 docker.io/library/python:3.12-alpine docker.io/library/nginx:1.27-alpine docker.io/library/redis:7-alpine; do
  grep -q "\"reference\": \"$reference\"" "$IMAGE_LIST" || fail "image inventory omitted $reference"
done
[ "$(ttc images -q | wc -l)" -ge 5 ] || fail "quiet image inventory omitted an image ID"

echo "== automatic container removal =="
ttc run --rm --pull always --name "$AUTO_FOREGROUND" "$BUSYBOX_IMAGE" true
if ttc inspect "$AUTO_FOREGROUND" >/dev/null 2>&1; then
  fail "foreground --rm retained its container"
fi
AUTO_DETACHED_ID=$(ttc run --rm --detach --name "$AUTO_DETACHED" "$BUSYBOX_IMAGE" sh -c "sleep 2")
container_id_valid "$AUTO_DETACHED_ID" || fail "detached --rm returned invalid ID: $AUTO_DETACHED_ID"
[ "$(ttc inspect -f "{{.HostConfig.AutoRemove}}" "$AUTO_DETACHED")" = true ] ||
  fail "inspect omitted detached auto-remove policy"
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
while ttc inspect "$AUTO_DETACHED" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "detached --rm did not remove its container"
  sleep 1
done
AUTO_CREATED_ID=$(ttc create --rm --name "$AUTO_CREATED" "$BUSYBOX_IMAGE" sh -c "sleep 1")
[ "$(ttc inspect -f "{{.HostConfig.AutoRemove}}" "$AUTO_CREATED")" = true ] ||
  fail "create --rm did not persist its policy"
ttc start "$AUTO_CREATED_ID@main" >/dev/null
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
while ttc inspect "$AUTO_CREATED" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "started create --rm container was retained"
  sleep 1
done

echo "== one-shot compatibility probes =="
if ttc create --pull never --name "$PREFIX-never-missing" "registry.invalid/ttc/missing:latest" true >/dev/null 2>&1; then
  fail "--pull never unexpectedly created a missing image"
fi

BUSYBOX_ID=$(ttc create --pull never --name "$PREFIX-busybox" \
  --env-file "$ENV_FILE" --env FROM_FILE=direct \
  --label-file "$LABEL_FILE" --label suite=create \
  "$BUSYBOX_IMAGE" sh -c \
  'test "$FROM_FILE" = direct && test "$TTC_E2E_HOST_IMPORT" = host-import && test -r /proc/self/status && test -r /sys/kernel && echo busybox-ok && echo busybox-err >&2')
container_id_valid "$BUSYBOX_ID" || fail "create returned invalid container ID: $BUSYBOX_ID"
BUSYBOX_PREFIX=$(printf %.16s "$BUSYBOX_ID")
[ "$(ttc inspect -f "{{.Id}}|{{.Name}}" "$PREFIX-busybox")" = "$BUSYBOX_ID|/$PREFIX-busybox" ] ||
  fail "inspect by name did not expose the stable ID and name"
[ "$(ttc inspect -f "{{.Id}}|{{.Name}}" "$BUSYBOX_PREFIX")" = "$BUSYBOX_ID|/$PREFIX-busybox" ] ||
  fail "inspect did not resolve an unambiguous ID prefix"
[ "$(ttc inspect -f '{{.State.Status}}|{{.Config.Image}}|{{.Path}}|{{len .TTC.Branches}}' "$PREFIX-busybox")" = \
  "created|docker.io/library/busybox:1.36|sh|1" ] ||
  fail "created-container inspect did not expose Docker config and TTC branch state"
[ "$(ttc inspect -f '{{index .Config.Labels "suite"}}|{{index .Config.Labels "file-only"}}|{{index .Config.Labels "empty"}}' "$PREFIX-busybox")" = \
  "create|value|" ] || fail "created-container inspect did not retain merged labels"
BUSYBOX_ENV=$(ttc inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$PREFIX-busybox")
echo "$BUSYBOX_ENV" | grep -qx 'FROM_FILE=direct' || fail "created-container inspect omitted direct environment override"
echo "$BUSYBOX_ENV" | grep -qx 'TTC_E2E_HOST_IMPORT=host-import' || fail "created-container inspect omitted imported host environment"
ttc start --attach "$BUSYBOX_PREFIX@main"
[ "$(ttc inspect -f '{{.State.Status}}|{{.State.ExitCode}}|{{.State.Running}}' "$PREFIX-busybox")" = \
  "exited|0|false" ] ||
  fail "exited-container inspect did not expose exit state"
ATTACH_STDOUT="$TTC_E2E_ROOT/attach.stdout"
ATTACH_STDERR="$TTC_E2E_ROOT/attach.stderr"
ttc attach --no-stderr "$PREFIX-busybox@main" >"$ATTACH_STDOUT" 2>"$ATTACH_STDERR"
grep -qx 'busybox-ok' "$ATTACH_STDOUT" || fail "stdout-only attach omitted stdout"
[ -z "$(sed '/^ttc: warning:/d' "$ATTACH_STDERR")" ] || fail "stdout-only attach leaked stderr"
ttc attach --no-stdout "$PREFIX-busybox@main" >"$ATTACH_STDOUT" 2>"$ATTACH_STDERR"
[ ! -s "$ATTACH_STDOUT" ] || fail "stderr-only attach leaked stdout"
[ "$(sed '/^ttc: warning:/d' "$ATTACH_STDERR")" = busybox-err ] || fail "stderr-only attach omitted stderr"

ttc run --name "$ATTACH_INPUT" --detach --interactive "$BUSYBOX_IMAGE" sh -c \
  'IFS= read -r value; test "$value" = stdin-ok; echo hidden-stdout; echo hidden-stderr >&2' >/dev/null
printf 'stdin-ok\n' | ttc attach --interactive --no-stdout --no-stderr "$ATTACH_INPUT@main" >"$ATTACH_STDOUT" 2>"$ATTACH_STDERR"
[ ! -s "$ATTACH_STDOUT" ] || fail "stdin-only attach leaked stdout"
[ -z "$(sed '/^ttc: warning:/d' "$ATTACH_STDERR")" ] || fail "stdin-only attach leaked stderr"
[ "$(ttc inspect -f '{{.State.ExitCode}}' "$ATTACH_INPUT")" = 0 ] || fail "stdin-only attach did not deliver input"
ttc rm "$ATTACH_INPUT"
ttc container restart --time 1 "$PREFIX-busybox@main"
[ "$(ttc container wait "$PREFIX-busybox@main")" = "0" ] ||
  fail "restart/wait did not preserve the BusyBox command exit status"
ttc rm "$BUSYBOX_ID"
ttc image inspect "$BUSYBOX_IMAGE" |
  grep -q '"Materialized": true' ||
  fail "image inspection did not report the materialized BusyBox root"

echo "== disk-backed managed volume =="
MATRIX_VOLUME="$PREFIX-volume"
ttc volume create "$MATRIX_VOLUME" >/dev/null
ttc run --name "$PREFIX-volume-write" -v "$MATRIX_VOLUME:/data" "$BUSYBOX_IMAGE" \
  sh -c 'echo persistent >/data/value'
ttc rm "$PREFIX-volume-write"
[ "$(ttc run --rm -v "$MATRIX_VOLUME:/data:ro" "$BUSYBOX_IMAGE" cat /data/value)" = persistent ] ||
  fail "named volume did not persist direct disk data across containers"
ttc volume rm "$MATRIX_VOLUME"

ttc container create --name "$MULTI_A" "$BUSYBOX_IMAGE" sh -c 'exit 3'
ttc container create --name "$MULTI_B" "$BUSYBOX_IMAGE" sh -c 'exit 4'
MULTI_STARTED=$(ttc container start "$MULTI_A@main" "$MULTI_B@main")
echo "$MULTI_STARTED" | grep -qx "$MULTI_A@main" || fail "multi-start omitted $MULTI_A@main"
echo "$MULTI_STARTED" | grep -qx "$MULTI_B@main" || fail "multi-start omitted $MULTI_B@main"
MULTI_WAIT=$(ttc wait "$MULTI_A@main" "$MULTI_B@main")
[ "$MULTI_WAIT" = "$(printf '3\n4')" ] || fail "multi-wait returned: $MULTI_WAIT"
ttc container rm "$MULTI_A" "$MULTI_B"

echo "== persisted host networking and fork rejection =="
command -v curl >/dev/null 2>&1 || fail "curl is required for the host-network service probe"
if [ "$(uname -s)" = Darwin ]; then
  ttc container create --network host --waive-live-fork --name "$HOSTNET" "$BUSYBOX_IMAGE" true \
    >"$TTC_E2E_ROOT/hostnet-create.out"
  if ttc container start "$HOSTNET" >"$TTC_E2E_ROOT/hostnet-start.out" \
      2>"$TTC_E2E_ROOT/hostnet-start.err"; then
    fail "native macOS unexpectedly started host networking"
  fi
  grep -q 'native macOS supports only --network=ttc or --network=none' \
    "$TTC_E2E_ROOT/hostnet-start.err" || fail "native macOS host-network rejection was not explicit"
  ttc rm "$HOSTNET" >/dev/null
else
  ttc container create --network host --waive-live-fork --name "$HOSTNET" "$BUSYBOX_IMAGE" sh -c \
    "mkdir -p /tmp/www; echo hostnet-ok >/tmp/www/index.html; exec httpd -f -p $HOSTNET_PORT -h /tmp/www"
  ttc container start "$HOSTNET"
  deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
  HOSTNET_RESPONSE=
  while [ "$HOSTNET_RESPONSE" != hostnet-ok ]; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "host-network BusyBox service did not become reachable"
    HOSTNET_RESPONSE=$(curl -fsS --max-time 1 "http://127.0.0.1:$HOSTNET_PORT/" 2>/dev/null || true)
    [ "$HOSTNET_RESPONSE" = hostnet-ok ] || sleep 1
  done
  [ "$(ttc inspect -f '{{.HostConfig.NetworkMode}}' "$HOSTNET")" = host ] ||
    fail "inspect did not retain host network mode"
  if ttc fork "$HOSTNET@main" rejected >"$TTC_E2E_ROOT/hostnet-fork.out" 2>"$TTC_E2E_ROOT/hostnet-fork.err"; then
    fail "host-network container was incorrectly forked"
  fi
  ttc exec "$HOSTNET@main" true || fail "host-network source did not resume after rejected fork"
  ttc stop --time 1 "$HOSTNET@main"
  ttc rm "$HOSTNET"
fi

echo "== rootless virtual NIC, NAT, publication, and live fork =="
command -v python3 >/dev/null 2>&1 || fail "python3 is required for the TTC NAT fixture"
mkdir -p "$TTC_E2E_ROOT/nat-fixture"
echo nat-ok >"$TTC_E2E_ROOT/nat-fixture/index.html"
python3 -m http.server "$NAT_FIXTURE_PORT" --bind 127.0.0.1 \
  --directory "$TTC_E2E_ROOT/nat-fixture" \
  >"$TTC_E2E_ROOT/nat-fixture.log" 2>&1 &
NAT_FIXTURE_PID=$!
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
until curl -fsS --max-time 1 "http://127.0.0.1:$NAT_FIXTURE_PORT/" >/dev/null 2>&1; do
  kill -0 "$NAT_FIXTURE_PID" >/dev/null 2>&1 || fail "TTC NAT fixture exited during startup"
  [ "$(date +%s)" -lt "$deadline" ] || fail "TTC NAT fixture did not become ready"
  sleep 1
done

if ttc run --rm --network ttc "$BUSYBOX_IMAGE" \
    wget -T 2 -qO- "http://10.0.2.2:$NAT_FIXTURE_PORT/" >/dev/null 2>&1; then
  fail "NAT-disabled TTC virtual NIC reached a host service"
fi
[ "$(ttc run --rm --network ttc --nat "$BUSYBOX_IMAGE" \
    wget -T "$TTC_E2E_GUEST_CURL_TIMEOUT" -qO- "http://10.0.2.2:$NAT_FIXTURE_PORT/")" = nat-ok ] ||
  fail "TTC user-mode NAT did not reach the host fixture"

ttc run --name "$VNET" --detach --network ttc \
  --publish "127.0.0.1:$PUBLISHED_PORT:8080" \
  "$BUSYBOX_IMAGE" sh -c \
  'mkdir -p /www; echo parent >/www/index.html; exec httpd -f -p 8080 -h /www' >/dev/null
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
VNET_RESPONSE=
while [ "$VNET_RESPONSE" != parent ]; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "published TTC TCP port did not become reachable"
  VNET_RESPONSE=$(curl -fsS --max-time "$TTC_E2E_GUEST_CURL_TIMEOUT" "http://127.0.0.1:$PUBLISHED_PORT/" 2>/dev/null || true)
  [ "$VNET_RESPONSE" = parent ] || sleep 1
done
[ "$(ttc inspect -f '{{(index (index .HostConfig.PortBindings "8080/tcp") 0).HostIP}}:{{(index (index .HostConfig.PortBindings "8080/tcp") 0).HostPort}}' "$VNET")" = "127.0.0.1:$PUBLISHED_PORT" ] ||
  fail "inspect omitted the persisted TTC port binding"
ttc fork "$VNET@main" child >/dev/null
ttc exec "$VNET@child" sh -c 'echo child >/www/index.html'
[ "$(curl -fsS --max-time "$TTC_E2E_GUEST_CURL_TIMEOUT" "http://127.0.0.1:$PUBLISHED_PORT/")" = parent ] ||
  fail "published host port switched away from the source branch after fork"
[ "$(ttc exec "$VNET@child" cat /www/index.html)" = child ] ||
  fail "forked TTC branch did not retain its branch-local network service state"
ttc port publish "$VNET@child" "127.0.0.1:$CHILD_PUBLISHED_PORT:8080" >/dev/null
[ "$(curl -fsS --max-time "$TTC_E2E_GUEST_CURL_TIMEOUT" "http://127.0.0.1:$CHILD_PUBLISHED_PORT/")" = child ] ||
  fail "dynamically published child port did not reach the forked branch"
ttc port ls "$VNET@child" | grep -q "127.0.0.1:$CHILD_PUBLISHED_PORT" ||
  fail "branch port listing omitted the dynamically published child port"
ttc fork "$VNET@main" second >/dev/null
ttc fork "$VNET@child" grandchild >/dev/null
ttc_daemon_crash
ttc_daemon_start
[ "$(curl -fsS --max-time "$TTC_E2E_GUEST_CURL_TIMEOUT" "http://127.0.0.1:$PUBLISHED_PORT/")" = parent ] ||
  fail "published TTC port did not survive daemon restart reconciliation"
[ "$(curl -fsS --max-time "$TTC_E2E_GUEST_CURL_TIMEOUT" "http://127.0.0.1:$CHILD_PUBLISHED_PORT/")" = child ] ||
  fail "dynamically published child port did not survive daemon restart reconciliation"
ttc port unpublish "$VNET@child" "127.0.0.1:$CHILD_PUBLISHED_PORT:8080" >/dev/null
if curl -fsS --max-time "$TTC_E2E_GUEST_CURL_TIMEOUT" "http://127.0.0.1:$CHILD_PUBLISHED_PORT/" >/dev/null 2>&1; then
  fail "unpublished child port remained reachable"
fi
ttc port publish "$VNET@child" "127.0.0.1:$CHILD_PUBLISHED_PORT:8080" >/dev/null
ttc rm -f "$VNET@child"
if curl -fsS --max-time "$TTC_E2E_GUEST_CURL_TIMEOUT" "http://127.0.0.1:$CHILD_PUBLISHED_PORT/" >/dev/null 2>&1; then
  fail "deleted child branch left its published port reachable"
fi
if ttc port ls --json "$VNET@main" | grep -q '"branch_id": "child"'; then
  fail "deleted child branch retained published-port metadata"
fi
ttc rm --force "$VNET"

echo "== Sentry-managed secondary network namespace =="
ttc run --name "$NETNS" --detach "$BUSYBOX_IMAGE" sleep 300 >/dev/null
ttc exec --detach --cap CAP_SYS_ADMIN --cap CAP_NET_ADMIN "$NETNS@main" \
  unshare -n sh -c \
  'ip link set lo up; mkdir -p /tmp/ns-www; echo namespace-ok >/tmp/ns-www/index.html; httpd -f -p 8080 -h /tmp/ns-www & while :; do wget -qO /tmp/ns-result http://127.0.0.1:8080 || true; sleep 1; done' \
  >/dev/null
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
until [ "$(ttc exec "$NETNS@main" cat /tmp/ns-result 2>/dev/null || true)" = namespace-ok ]; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "secondary network namespace service did not become ready"
  sleep 1
done
ttc fork "$NETNS@main" child >/dev/null
sleep 2
[ "$(ttc exec "$NETNS@main" cat /tmp/ns-result)" = namespace-ok ] ||
  fail "source secondary network namespace stopped after fork"
[ "$(ttc exec "$NETNS@child" cat /tmp/ns-result)" = namespace-ok ] ||
  fail "forked secondary network namespace did not retain internal TCP"
ttc rm --force "$NETNS"

ttc run --name "$PREFIX-alpine" "$ALPINE_IMAGE" sh -c \
  'grep -q Alpine /etc/os-release && apk --version >/dev/null && echo alpine-ok'
ttc rm "$PREFIX-alpine"
ttc run --name "$PREFIX-alpine" --user 123:456 "$ALPINE_IMAGE" sh -c \
  'test "$(stat -c %a /)" = 755 && test -r /etc/os-release && \
   touch /dev/shm/nonroot-ok && test "$(stat -c %u:%g /dev/shm/nonroot-ok)" = 123:456 && \
   echo alpine-nonroot-ok'
ttc rm "$PREFIX-alpine"

ttc run --name "$PREFIX-python" --ulimit nofile=64:128 --ulimit core=0 "$PYTHON_IMAGE" python3 -c \
  'import json, resource, sqlite3, threading; assert resource.getrlimit(resource.RLIMIT_NOFILE) == (64, 128); assert resource.getrlimit(resource.RLIMIT_CORE) == (0, 0); db=sqlite3.connect(":memory:"); db.execute("create table t(v)"); db.execute("insert into t values (7)"); assert db.execute("select v from t").fetchone()[0] == 7; t=threading.Thread(target=lambda: json.dumps({"ok": True})); t.start(); t.join(); print("python-ok")'
[ "$(ttc inspect -f '{{len .HostConfig.Ulimits}}|{{(index .HostConfig.Ulimits 0).Name}}|{{(index .HostConfig.Ulimits 0).Soft}}|{{(index .HostConfig.Ulimits 0).Hard}}|{{(index .HostConfig.Ulimits 1).Name}}|{{(index .HostConfig.Ulimits 1).Soft}}|{{(index .HostConfig.Ulimits 1).Hard}}' "$PREFIX-python")" = "2|core|0|0|nofile|64|128" ] ||
  fail "inspect did not retain sorted Docker ulimits"
ttc rm "$PREFIX-python"

echo "== rootless Sentry PID limit and branch-local inheritance =="
ttc run --name "$PIDS_LIMIT" --detach --pids-limit 3 "$PYTHON_IMAGE" python3 -c "
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
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
until ttc logs "$PIDS_LIMIT@main" | grep -q pids-ready; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "PID-limited container did not reach its cap"
  sleep 1
done
[ "$(ttc inspect -f "{{.HostConfig.PidsLimit}}" "$PIDS_LIMIT")" = 3 ] ||
  fail "inspect did not retain --pids-limit"
if ttc exec "$PIDS_LIMIT@main" true >/dev/null 2>&1; then
  fail "exec bypassed a full PID limit"
fi
ttc fork "$PIDS_LIMIT@main" child
if ttc exec "$PIDS_LIMIT@child" true >/dev/null 2>&1; then
  fail "live-forked child lost its full PID limit"
fi
ttc kill --signal SIGUSR1 "$PIDS_LIMIT@main" >/dev/null
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
until ttc logs "$PIDS_LIMIT@main" | grep -q pids-released; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "parent branch did not release a PID slot"
  sleep 1
done
ttc exec "$PIDS_LIMIT@main" true
if ttc exec "$PIDS_LIMIT@child" true >/dev/null 2>&1; then
  fail "parent PID release leaked into the child branch"
fi
ttc stop --time 1 "$PIDS_LIMIT@child" "$PIDS_LIMIT@main"
ttc rm -f "$PIDS_LIMIT@child"
ttc rm "$PIDS_LIMIT"

ttc create --name "$PREFIX-nginx" "$NGINX_IMAGE" nginx -t
[ "$(ttc inspect -f '{{.Config.StopSignal}}' "$PREFIX-nginx")" = SIGQUIT ] ||
  fail "nginx image STOPSIGNAL was not inherited"
ttc start --attach "$PREFIX-nginx@main"
ttc rm "$PREFIX-nginx"

[ "$(ttc run --name "$PREFIX-redis" "$REDIS_IMAGE" sh -c \
  'test -d /data && echo redis-volume-ok >/data/ttc-volume && cat /data/ttc-volume')" = redis-volume-ok ] ||
  fail "Redis image VOLUME was not backed by a TTC managed volume"
ttc rm "$PREFIX-redis"

echo "== persisted stop signal and timeout policy =="
ttc run --name "$STOP_POLICY" --detach --stop-signal SIGUSR1 --stop-timeout 3 \
  "$ALPINE_IMAGE" sh -c "trap \"exit 42\" USR1; echo stop-policy-ready; while :; do sleep 0.05; done"
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
until ttc logs "$STOP_POLICY@main" | grep -q stop-policy-ready; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "custom-stop container did not become ready"
  sleep 1
done
[ "$(ttc inspect -f '{{.Config.StopSignal}}|{{.HostConfig.StopTimeout}}' "$STOP_POLICY")" = "SIGUSR1|3" ] ||
  fail "inspect did not retain custom stop policy"
ttc stop "$STOP_POLICY@main"
[ "$(ttc inspect -f '{{.State.ExitCode}}' "$STOP_POLICY")" = 42 ] ||
  fail "ttc stop did not deliver the configured SIGUSR1"
ttc rm "$STOP_POLICY"

echo "== quick-exit console integrity =="
STRESS_BYTES=262144
STRESS_RUN_STDOUT="$TTC_E2E_ROOT/console-stress-run.stdout"
STRESS_RUN_STDERR="$TTC_E2E_ROOT/console-stress-run.stderr"
STRESS_LOG_STDOUT="$TTC_E2E_ROOT/console-stress-log.stdout"
STRESS_LOG_STDERR="$TTC_E2E_ROOT/console-stress-log.stderr"
ttc run --name "$CONSOLE_STRESS" "$BUSYBOX_IMAGE" sh -c \
  'dd if=/dev/zero bs=65536 count=4 status=none; dd if=/dev/zero of=/dev/stderr bs=65536 count=4 status=none' \
  >"$STRESS_RUN_STDOUT" 2>"$STRESS_RUN_STDERR"
ttc logs "$CONSOLE_STRESS@main" >"$STRESS_LOG_STDOUT" 2>"$STRESS_LOG_STDERR"

EXPECTED_ZERO=$(head -c "$STRESS_BYTES" /dev/zero | sha256sum | awk '{print $1}')
[ "$(wc -c <"$STRESS_RUN_STDOUT")" -eq "$STRESS_BYTES" ] || fail "foreground stdout was truncated"
[ "$(sed '/^ttc: warning:/d' "$STRESS_RUN_STDERR" | wc -c)" -eq "$STRESS_BYTES" ] || fail "foreground stderr was truncated"
[ "$(wc -c <"$STRESS_LOG_STDOUT")" -eq "$STRESS_BYTES" ] || fail "persisted stdout was truncated"
[ "$(sed '/^ttc: warning:/d' "$STRESS_LOG_STDERR" | wc -c)" -eq "$STRESS_BYTES" ] || fail "persisted stderr was truncated"
[ "$(sha256sum "$STRESS_RUN_STDOUT" | awk '{print $1}')" = "$EXPECTED_ZERO" ] || fail "foreground stdout content changed"
[ "$(sed '/^ttc: warning:/d' "$STRESS_RUN_STDERR" | sha256sum | awk '{print $1}')" = "$EXPECTED_ZERO" ] || fail "foreground stderr content changed"
[ "$(sha256sum "$STRESS_LOG_STDOUT" | awk '{print $1}')" = "$EXPECTED_ZERO" ] || fail "persisted stdout content changed"
[ "$(sed '/^ttc: warning:/d' "$STRESS_LOG_STDERR" | sha256sum | awk '{print $1}')" = "$EXPECTED_ZERO" ] || fail "persisted stderr content changed"
ttc rm "$CONSOLE_STRESS"

echo "== bounded log retention accounting =="
ttc run --name "$RETENTION" --detach --log-opt max-size=128 --log-opt max-file=1 "$PYTHON_IMAGE" python3 -c 'import sys,time; [(sys.stdout.write("X"*96+"\n"),sys.stdout.flush(),time.sleep(0.02)) for _ in range(8)]' >/dev/null
ttc wait "$RETENTION@main" >/dev/null
RETENTION_INSPECT="$TTC_E2E_ROOT/retention-inspect.json"
ttc inspect "$RETENTION" >"$RETENTION_INSPECT"
grep -Eq '"evicted_bytes": [1-9][0-9]*' "$RETENTION_INSPECT" || fail "inspect omitted evicted output bytes"
grep -Eq '"evicted_generations": [1-9][0-9]*' "$RETENTION_INSPECT" || fail "inspect omitted evicted output generations"
ttc logs "$RETENTION@main" >/dev/null
RETENTION_EVENTS="$TTC_E2E_ROOT/retention-events.jsonl"
ttc events --json --filter action=logs >"$RETENTION_EVENTS"
grep -q "\"subject\":\"$RETENTION@main\"" "$RETENTION_EVENTS" || fail "events omitted retention log subject"
grep -q '"output_evicted_bytes":"' "$RETENTION_EVENTS" || fail "events omitted output eviction snapshot"
ttc rm "$RETENTION"

echo "== explicit logging drivers =="
ttc create --name "$NOLOG" --log-driver none "$BUSYBOX_IMAGE" sh -c "echo hidden" >/dev/null
[ "$(ttc inspect -f '{{.HostConfig.LogConfig.Type}}' "$NOLOG")" = "none" ] ||
  fail "inspect omitted the none log driver"
ttc start "$NOLOG@main" >/dev/null
if ttc logs "$NOLOG@main" >"$TTC_E2E_ROOT/nolog.out" 2>"$TTC_E2E_ROOT/nolog.err"; then
  fail "logs accepted a container configured with the none log driver"
fi
grep -q "does not support reading" "$TTC_E2E_ROOT/nolog.err" ||
  fail "none log-driver rejection lacked a compatibility diagnostic"
ttc rm --force "$NOLOG"

echo "== detached lifecycle, tmpfs CoW, and live fork =="
LIFECYCLE_ID=$(ttc run --name "$LIFECYCLE" --detach --read-only \
  --ulimit nofile=256:256 \
  --shm-size 32m \
  --group-add 4242 --cap-add sys_ptrace --cap-drop net_raw \
  --add-host ttc.local:192.0.2.10 --add-host v6.local=2001:db8::10 \
  --dns 203.0.113.53 --dns-search example.test --dns-option ndots:2 \
  --env-file "$ENV_FILE" --env FROM_FILE=lifecycle \
  --label-file "$LABEL_FILE" --label suite=lifecycle \
  --log-driver json-file --log-opt max-size=64k --log-opt max-file=3 --log-opt labels=suite --log-opt env=FROM_FILE \
  --tmpfs /state:mode=0755 \
  --volume "$HOST_FIXTURE:/host-fixture:ro" \
  --mount "type=bind,source=$HOST_FIXTURE_TWO,target=/host-fixture-two,readonly,immutable-source" \
  --mount "type=bind,source=$HOST_COW_FIXTURE,target=/cow-host,lazy-copy" \
  --tmpfs /scratch:mode=0700,size=1m \
  --mount type=proc,target=/alt-proc,readonly \
  --mount type=proc,target=/repeat-proc,readonly \
  --mount type=proc,target=/nested/views/proc,readonly \
  --mount type=sysfs,target=/alt-sys,readonly \
  --mount type=sysfs,target=/repeat-sys,readonly \
  --mount type=sysfs,target=/nested/views/sys,readonly \
  "$ALPINE_IMAGE" sh -c \
  'grep -Eq "^192\.0\.2\.10[[:space:]]+ttc\.local$" /etc/hosts; grep -Eq "^2001:db8::10[[:space:]]+v6\.local$" /etc/hosts; grep -qx "nameserver 203.0.113.53" /etc/resolv.conf; grep -qx "search example.test" /etc/resolv.conf; grep -qx "options ndots:2" /etc/resolv.conf; echo parent >/state/value; echo scratch-parent >/scratch/value; test "$(cat /cow-host/value)" = host-cow-lower; echo parent >/cow-host/value; echo parent-created >/cow-host/created; id -G | grep -qw 4242; set -- $(grep ^CapEff: /proc/self/status); [ $((0x$2 & 0x80000)) -ne 0 ]; [ $((0x$2 & 0x2000)) -eq 0 ]; test -r /alt-proc/self/status; test -r /repeat-proc/1/status; test -r /nested/views/proc/self/status; test -r /alt-sys/kernel; test -r /repeat-sys/kernel; test -r /nested/views/sys/kernel; i=0; while :; do echo "tick:$i"; i=$((i+1)); sleep 1; done')
container_id_valid "$LIFECYCLE_ID" || fail "detached run returned invalid container ID: $LIFECYCLE_ID"
LIFECYCLE_PREFIX=$(printf %.16s "$LIFECYCLE_ID")
LIFECYCLE_SHORT=$(printf %.12s "$LIFECYCLE_ID")
[ "$(ttc inspect -f "{{.Id}}|{{.Name}}" "$LIFECYCLE_PREFIX")" = "$LIFECYCLE_ID|/$LIFECYCLE" ] ||
  fail "detached container was not addressable by ID prefix"

deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
while ! ttc logs --tail 10 "$LIFECYCLE@main" 2>/dev/null | grep -q 'tick:'; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "detached init did not produce console output"
  sleep 1
done

echo "== daemon restart reconciliation =="
ttc daemon status | grep -Eq '"version":[[:space:]]*[1-9][0-9]*[,}]' ||
  fail "ttcd did not report a valid API version"
ttc_daemon_crash
if ttc daemon status >/dev/null 2>&1; then
  fail "ttcd remained reachable after crash"
fi
ttc_daemon_start
[ "$(ttc inspect -f '{{.State.Status}}' "$LIFECYCLE")" = running ] ||
  fail "restarted ttcd did not reconcile the running container"
ttc daemon status | grep -Eq '"version":[[:space:]]*[1-9][0-9]*[,}]' || fail "ttcd did not restart"
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
while ! ttc logs --tail 10 "$LIFECYCLE@main" 2>/dev/null | grep -q 'tick:'; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "console draining did not survive daemon restart"
  sleep 1
done
echo "== console follower crash recovery =="
# One follower process keeps the logs of a family's consoles. A reader of a
# console whose follower is gone starts another.
family_follower_pid() {
  pgrep -f "$LIFECYCLE_ID console-follow-family" | grep -vx "${1:-}" | head -n 1
}
last_tick() {
  ttc logs --tail 10 "$LIFECYCLE@main" 2>/dev/null | sed -n 's/.*tick:\([0-9][0-9]*\).*/\1/p' | tail -n 1
}
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
while OLD_FOLLOWER_PID=$(family_follower_pid); [ -z "$OLD_FOLLOWER_PID" ]; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "the family's console follower is not running"
  sleep 1
done
kill -KILL "$OLD_FOLLOWER_PID"
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
while :; do
  ttc logs --tail 10 "$LIFECYCLE@main" >/dev/null 2>&1 || true
  NEW_FOLLOWER_PID=$(family_follower_pid "$OLD_FOLLOWER_PID")
  if [ -n "$NEW_FOLLOWER_PID" ] && kill -0 "$NEW_FOLLOWER_PID" 2>/dev/null; then
    break
  fi
  [ "$(date +%s)" -lt "$deadline" ] || fail "console follower was not replaced after SIGKILL"
  sleep 1
done
REPLACED_TICK=$(last_tick)
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
while TICK=$(last_tick); [ -z "$TICK" ] || [ "$TICK" -le "${REPLACED_TICK:--1}" ]; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "console output did not resume after follower replacement"
  sleep 1
done

ttc container restart --time 2 "$LIFECYCLE@main"
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
while ! ttc logs --tail 10 "$LIFECYCLE@main" 2>/dev/null | grep -q 'tick:'; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "restarted detached init did not produce console output"
  sleep 1
done

ttc exec "$LIFECYCLE@main" grep -qx "$LIFECYCLE" /etc/hostname ||
  fail "restarted container did not retain /etc/hostname"
ttc exec "$LIFECYCLE@main" sh -c "grep -Eq \"^192\.0\.2\.10[[:space:]]+ttc\.local$\" /etc/hosts; grep -Eq \"^2001:db8::10[[:space:]]+v6\.local$\" /etc/hosts; grep -qx \"nameserver 203.0.113.53\" /etc/resolv.conf; grep -qx \"search example.test\" /etc/resolv.conf; grep -qx \"options ndots:2\" /etc/resolv.conf" ||
  fail "restarted container did not retain generated network files"

EXEC_GROUPS=$(ttc exec --user 123:456 --additional-gids 4242 --additional-gids 4243 "$LIFECYCLE@main" /bin/busybox id)
echo "$EXEC_GROUPS" | grep -q "4242,4243" || fail "exec supplementary groups missing: $EXEC_GROUPS"
ttc exec --cap CAP_SYS_ADMIN "$LIFECYCLE@main" sh -c \
  'set -- $(grep ^CapEff: /proc/self/status); value=$2; [ $((0x$value & 0x200000)) -ne 0 ]'
PROCESS_JSON="$TTC_E2E_ROOT/process.json"
printf '%s\n' \
  '{"terminal":false,"user":{"uid":123,"gid":456,"additionalGids":[4342,4343]},"args":["sh","-c","id; echo mark:$MARK"],"env":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin","MARK=process-ok"],"cwd":"/"}' \
  >"$PROCESS_JSON"
PROCESS_OUTPUT=$(ttc exec --process "$PROCESS_JSON" "$LIFECYCLE@main")
echo "$PROCESS_OUTPUT" | grep -q 'groups=4342,4343' || fail "process JSON supplementary groups missing: $PROCESS_OUTPUT"
echo "$PROCESS_OUTPUT" | grep -q 'mark:process-ok' || fail "process JSON environment missing: $PROCESS_OUTPUT"

[ "$(ttc inspect -f "{{.HostConfig.ShmSize}}" "$LIFECYCLE")" = 33554432 ] || fail "inspect omitted shared memory size"
[ "$(ttc inspect -f '{{.HostConfig.LogConfig.Type}}|{{index .HostConfig.LogConfig.Config "max-size"}}|{{index .HostConfig.LogConfig.Config "max-file"}}|{{index .HostConfig.LogConfig.Config "labels"}}|{{index .HostConfig.LogConfig.Config "env"}}' "$LIFECYCLE")" = "json-file|65536|3|suite|FROM_FILE" ] ||
  fail "inspect omitted json-file retention policy"
[ "$(ttc inspect -f '{{index .HostConfig.GroupAdd 0}}|{{index .HostConfig.CapAdd 0}}|{{index .HostConfig.CapDrop 0}}' "$LIFECYCLE")" = "4242|SYS_PTRACE|NET_RAW" ] ||
  fail "inspect omitted supplementary groups or capability changes"
[ "$(ttc exec "$LIFECYCLE@main" sh -c 'df -k /dev/shm | tail -n 1 | xargs | cut -d" " -f2')" = 32768 ] ||
  fail "restarted container did not retain /dev/shm size"
[ "$(ttc inspect -f "{{.HostConfig.ReadonlyRootfs}}" "$LIFECYCLE")" = true ] || fail "inspect omitted read-only rootfs policy"
[ "$(ttc inspect -f "{{index .HostConfig.ExtraHosts 0}}|{{index .HostConfig.ExtraHosts 1}}|{{index .HostConfig.Dns 0}}|{{index .HostConfig.DnsSearch 0}}|{{index .HostConfig.DnsOptions 0}}" "$LIFECYCLE")" = "ttc.local:192.0.2.10|v6.local:2001:db8::10|203.0.113.53|example.test|ndots:2" ] ||
  fail "inspect omitted Docker network configuration"

if ttc exec "$LIFECYCLE@main" sh -c "echo forbidden >/root-forbidden" >/dev/null 2>&1; then
  fail "read-only root filesystem accepted a parent write"
fi
[ "$(ttc exec "$LIFECYCLE@main" cat /state/value)" = "parent" ] || fail "parent state tmpfs was not initialized"
[ "$(ttc exec "$LIFECYCLE@main" cat /scratch/value)" = "scratch-parent" ] || fail "parent tmpfs was not initialized"
[ "$(ttc exec "$LIFECYCLE@main" cat /cow-host/value)" = "parent" ] || fail "managed overlay did not retain the parent copy-up"
[ "$(cat "$HOST_COW_FIXTURE/value")" = "host-cow-lower" ] || fail "managed overlay modified its host lower"
[ "$(ttc exec "$LIFECYCLE@main" cat /host-fixture-two/value)" = "host-readonly-two" ] || fail "second read-only host bind was not readable"
[ "$(ttc exec "$LIFECYCLE@main" stat -c %a /scratch)" = "700" ] || fail "tmpfs mode was not applied"
ttc exec "$LIFECYCLE@main" sh -c "test -r /alt-proc/self/status && test -r /alt-sys/kernel" ||
  fail "additional procfs/sysfs mounts were not readable"
for path in /repeat-proc/1/status /nested/views/proc/self/status /repeat-sys/kernel /nested/views/sys/kernel; do
  ttc exec "$LIFECYCLE@main" test -r "$path" || fail "nested or repeated pseudo-filesystem path $path was not readable"
done
ttc exec "$LIFECYCLE@main" grep -Eq " /host-fixture ro(,| )" /proc/self/mountinfo || fail "read-only host bind lost its remount flag"
ttc exec "$LIFECYCLE@main" awk '$5 == "/nested/views/proc" { split($6, options, ","); for (i in options) seen[options[i]] = 1; exit !(seen["ro"] && seen["nosuid"] && seen["nodev"] && seen["noexec"]) } END { if (!seen["ro"]) exit 1 }' /proc/self/mountinfo || fail "nested procfs mount flags were not retained"
[ "$(ttc exec "$LIFECYCLE@main" cat /host-fixture/value)" = "host-readonly" ] || fail "read-only host bind was not readable"
if ttc exec "$LIFECYCLE@main" sh -c 'echo forbidden >/host-fixture/value' >/dev/null 2>&1; then
  fail "read-only host bind accepted a write"
fi
echo "== attached exec live-fork continuity =="
ttc exec "$LIFECYCLE@main" rm -f /state/attached-exec-ready /state/attached-exec-release
ttc exec "$LIFECYCLE@main" sh -c \
  'echo ready >/state/attached-exec-ready; while [ ! -e /state/attached-exec-release ]; do sleep 0.1; done' \
  >"$TTC_E2E_ROOT/attached-exec.out" 2>"$TTC_E2E_ROOT/attached-exec.err" &
ATTACHED_EXEC_HOST_PID=$!
deadline=$(( $(date +%s) + TTC_E2E_DEADLINE_SECONDS ))
while ! ttc exec "$LIFECYCLE@main" test -e /state/attached-exec-ready >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "attached exec did not reach its ready point"
  sleep 1
done
ttc fork "$LIFECYCLE@main" attached-exec >/dev/null ||
  fail "live fork rejected an attached exec whose stdio is daemon-mediated"
ttc exec "$LIFECYCLE@main" touch /state/attached-exec-release
wait "$ATTACHED_EXEC_HOST_PID" || fail "attached exec did not exit successfully"
ttc rm -f "$LIFECYCLE@attached-exec"

ttc fork "$LIFECYCLE_PREFIX@main" child
echo host-mutated >"$HOST_FIXTURE/value"
echo host-mutated-two >"$HOST_FIXTURE_TWO/value"
[ "$(ttc exec "$LIFECYCLE@main" cat /host-fixture/value)" = "host-mutated" ] || fail "parent did not observe host-side mutation"
[ "$(ttc exec "$LIFECYCLE@child" cat /host-fixture/value)" = "host-mutated" ] || fail "child did not observe shared host-side mutation"
[ "$(ttc exec "$LIFECYCLE@main" cat /host-fixture-two/value)" = "host-mutated-two" ] || fail "parent did not observe second host mutation"
[ "$(ttc exec "$LIFECYCLE@child" cat /host-fixture-two/value)" = "host-mutated-two" ] || fail "child did not observe second shared host mutation"
[ "$(ttc exec "$LIFECYCLE@main" sh -c "ulimit -n")" = 256 ] || fail "parent exec did not inherit RLIMIT_NOFILE"
[ "$(ttc exec "$LIFECYCLE@main" sh -c 'df -k /dev/shm | tail -n 1 | xargs | cut -d" " -f2')" = 32768 ] || fail "parent branch lost /dev/shm size"
ttc exec "$LIFECYCLE@main" sh -c 'id -G | grep -qw 4242; set -- $(grep ^CapEff: /proc/self/status); [ $((0x$2 & 0x80000)) -ne 0 ]; [ $((0x$2 & 0x2000)) -eq 0 ]' || fail "parent exec lost configured groups or capabilities"
[ "$(ttc exec "$LIFECYCLE@child" sh -c "ulimit -n")" = 256 ] || fail "live-forked child exec did not inherit RLIMIT_NOFILE"
[ "$(ttc exec "$LIFECYCLE@child" sh -c 'df -k /dev/shm | tail -n 1 | xargs | cut -d" " -f2')" = 32768 ] || fail "live-forked child lost /dev/shm size"
ttc exec "$LIFECYCLE@child" sh -c 'id -G | grep -qw 4242; set -- $(grep ^CapEff: /proc/self/status); [ $((0x$2 & 0x80000)) -ne 0 ]; [ $((0x$2 & 0x2000)) -eq 0 ]' || fail "child exec lost configured groups or capabilities"
for path in /repeat-proc/1/status /nested/views/proc/self/status /repeat-sys/kernel /nested/views/sys/kernel; do
  ttc exec "$LIFECYCLE@child" test -r "$path" || fail "live-forked child lost pseudo-filesystem path $path"
done

ttc exec "$LIFECYCLE@main" sh -c "grep -Eq \"^192\.0\.2\.10[[:space:]]+ttc\.local$\" /etc/hosts; grep -qx \"nameserver 203.0.113.53\" /etc/resolv.conf" || fail "parent branch lost generated network files"
ttc exec "$LIFECYCLE@child" sh -c "grep -Eq \"^2001:db8::10[[:space:]]+v6\.local$\" /etc/hosts; grep -qx \"search example.test\" /etc/resolv.conf; grep -qx \"options ndots:2\" /etc/resolv.conf" || fail "live-forked child lost generated network files"

RUNNING_INSPECT=$(ttc inspect -f '{{.State.Status}}|{{.Config.Image}}|{{len .TTC.Branches}}' "$LIFECYCLE")
[ "$RUNNING_INSPECT" = "running|docker.io/library/alpine:3.20|2" ] ||
  fail "running forked inspect returned $RUNNING_INSPECT"
[ "$(ttc inspect -f '{{index .Config.Labels "suite"}}|{{index .Config.Labels "file-only"}}|{{index .Config.Labels "empty"}}' "$LIFECYCLE")" = \
  "lifecycle|value|" ] || fail "forked-container inspect did not retain merged labels"
LIFECYCLE_ENV=$(ttc inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$LIFECYCLE")
echo "$LIFECYCLE_ENV" | grep -qx 'FROM_FILE=lifecycle' || fail "forked-container inspect omitted direct environment override"
echo "$LIFECYCLE_ENV" | grep -qx 'TTC_E2E_HOST_IMPORT=host-import' || fail "forked-container inspect omitted imported host environment"
INSPECT_JSON="$TTC_E2E_ROOT/lifecycle-inspect.json"
ttc inspect --format json "$LIFECYCLE" >"$INSPECT_JSON"
grep -q "\"Source\":\"$HOST_FIXTURE\"" "$INSPECT_JSON" || fail "inspect omitted the read-only host bind"
grep -q "\"Source\":\"$HOST_FIXTURE_TWO\"" "$INSPECT_JSON" || fail "inspect omitted the second read-only host bind"
grep -q "\"Source\":\"$HOST_COW_FIXTURE\"" "$INSPECT_JSON" || fail "inspect omitted the managed CoW host lower"
grep -q '"managed_cow":true' "$INSPECT_JSON" || fail "TTC inspect omitted managed CoW metadata"
grep -q '"semantics":"branch-local-persistent-cow-over-live-host-lower"' "$INSPECT_JSON" || fail "TTC inspect omitted managed-overlay semantics"
grep -q '"semantics":"shared-external-view"' "$INSPECT_JSON" || fail "TTC inspect omitted shared external-view semantics"
grep -q '"semantics":"user-asserted-immutable-external-view"' "$INSPECT_JSON" || fail "TTC inspect omitted immutable-source attestation"
grep -q '"immutable_source_asserted":true' "$INSPECT_JSON" || fail "TTC inspect omitted immutable-source assertion flag"
grep -q '"processes":\[' "$INSPECT_JSON" || fail "inspect omitted live branch processes"
grep -q "\"/scratch\":\"mode=0700,size=1m\"" "$INSPECT_JSON" || fail "inspect omitted tmpfs options"
grep -q "\"Type\":\"proc\"" "$INSPECT_JSON" || fail "inspect omitted the additional procfs mount"
grep -q "\"Type\":\"sysfs\"" "$INSPECT_JSON" || fail "inspect omitted the additional sysfs mount"
[ "$(ttc inspect -f "{{len .Mounts}}|{{len .HostConfig.Binds}}|{{len .HostConfig.Tmpfs}}" "$LIFECYCLE")" = "9|3|2" ] ||
  fail "Docker mount inspection exposed internal mounts or omitted user mounts"

PS_CHILD=$(ttc ps --filter branch=child --filter status=running --filter ancestor="$ALPINE_IMAGE" --format '{{.ID}}')
[ "$PS_CHILD" = "$LIFECYCLE_SHORT@child" ] || fail "filtered ps selected $PS_CHILD instead of the child branch"
[ "$(ttc ps --latest --quiet)" = "$LIFECYCLE_SHORT@child" ] || fail "ps --latest did not select the forked child"
ttc ps --filter branch=child --format json |
  grep -q '"created_at":"' ||
  fail "ps JSON omitted the child creation timestamp"
PS_SIZE=$(ttc ps --filter branch=child --size --format '{{.SizeRW}}|{{.SizeRootFS}}')
PS_SIZE_RW=${PS_SIZE%%|*}
PS_SIZE_ROOT=${PS_SIZE#*|}
case "$PS_SIZE_RW:$PS_SIZE_ROOT" in
  *[!0-9:]*|:*) fail "ps --size returned nonnumeric accounting: $PS_SIZE" ;;
esac
[ "$PS_SIZE_ROOT" -gt "$PS_SIZE_RW" ] || fail "ps --size virtual size did not include the image: $PS_SIZE"

ttc exec "$LIFECYCLE@child" sh -c 'echo child >/state/value'
ttc exec "$LIFECYCLE@child" sh -c "echo child >/cow-host/value; echo child-created >/cow-host/child-only"
[ "$(ttc exec "$LIFECYCLE@main" cat /cow-host/value)" = "parent" ] || fail "child managed-overlay write leaked into parent"
[ "$(ttc exec "$LIFECYCLE@child" cat /cow-host/value)" = "child" ] || fail "child managed-overlay copy-up was not retained"
[ "$(cat "$HOST_COW_FIXTURE/value")" = "host-cow-lower" ] || fail "child managed-overlay write reached the host lower"
[ "$(ttc exec "$LIFECYCLE@main" cat /state/value)" = "parent" ] || fail "child write leaked into parent state tmpfs"
[ "$(ttc exec "$LIFECYCLE@child" cat /state/value)" = "child" ] || fail "child state-tmpfs write was not retained"

if ttc exec "$LIFECYCLE@child" sh -c "echo forbidden >/child-root-forbidden" >/dev/null 2>&1; then
  fail "read-only root filesystem accepted a child write"
fi
ttc exec "$LIFECYCLE@child" sh -c "echo scratch-child >/scratch/value"
[ "$(ttc exec "$LIFECYCLE@main" cat /scratch/value)" = "scratch-parent" ] || fail "child write leaked into parent tmpfs"
[ "$(ttc exec "$LIFECYCLE@child" cat /scratch/value)" = "scratch-child" ] || fail "child tmpfs write was not retained"

ttc pause "$LIFECYCLE_PREFIX@main" "$LIFECYCLE_ID@child"
PAUSED_INSPECT=$(ttc inspect -f '{{range .TTC.Branches}}{{.Metadata.ID}}={{.Metadata.State}} {{end}}' "$LIFECYCLE")
echo "$PAUSED_INSPECT" | grep -q 'main=paused' || fail "inspect did not report paused main branch: $PAUSED_INSPECT"
echo "$PAUSED_INSPECT" | grep -q 'child=paused' || fail "inspect did not report paused child branch: $PAUSED_INSPECT"
ttc unpause "$LIFECYCLE@main" "$LIFECYCLE@child"
ttc ps --all
ttc top "$LIFECYCLE@main"
ttc ls --tree
ttc logs --tail 2 "$LIFECYCLE@main"
ttc logs --tail 2 "$LIFECYCLE@child"
TIMESTAMPED_LOGS=$(ttc logs --timestamps --details --since 24h --until 0s "$LIFECYCLE@child")
echo "$TIMESTAMPED_LOGS" | grep -Eq "^[0-9]{4}-[0-9]{2}-[0-9]{2}T[^ ]+ FROM_FILE=lifecycle,suite=lifecycle tick:" || fail "timestamped detailed logs omitted attributes: $TIMESTAMPED_LOGS"
[ -z "$(ttc logs --since 4102444800 "$LIFECYCLE@child")" ] || fail "logs --since included records older than its boundary"
[ -z "$(ttc logs --until 24h "$LIFECYCLE@child")" ] || fail "logs --until included records newer than its boundary"

# Splice display lineage when an intermediate source branch is deleted.
ttc fork "$LIFECYCLE@child" grandchild
[ "$(ttc exec "$LIFECYCLE@grandchild" cat /cow-host/value)" = "child" ] || fail "grandchild did not inherit managed-overlay state"
ttc exec "$LIFECYCLE@grandchild" sh -c "echo grandchild >/cow-host/value; echo grandchild-created >/cow-host/grandchild-only"
[ "$(ttc exec "$LIFECYCLE@child" cat /cow-host/value)" = "child" ] || fail "grandchild managed-overlay write leaked into child"
[ "$(ttc exec "$LIFECYCLE@grandchild" cat /cow-host/value)" = "grandchild" ] || fail "grandchild managed-overlay write was not retained"
if ttc rm "$LIFECYCLE@child" >"$TTC_E2E_ROOT/branch-rm-running.out" 2>"$TTC_E2E_ROOT/branch-rm-running.err"; then
  fail "rm removed a running branch without --force"
fi
grep -q "use -f to force removal" "$TTC_E2E_ROOT/branch-rm-running.err" ||
  fail "running branch removal rejection lacked a force diagnostic"
ttc rm -f "$LIFECYCLE@child"
[ "$(ttc exec "$LIFECYCLE@grandchild" cat /cow-host/value)" = "grandchild" ] || fail "grandchild managed overlay depended on deleted source branch"
LINEAGE_TREE=$(ttc ls --tree)
echo "$LINEAGE_TREE" | grep -q "grandchild running" || fail "tree omitted the reparented live grandchild"
if echo "$LINEAGE_TREE" | grep -q "child deleted"; then
  fail "tree retained a deleted intermediate branch"
fi
ttc fork "$LIFECYCLE@main" child >/dev/null || fail "fork did not reuse a deleted branch name"
ttc rm -f "$LIFECYCLE@child"

echo "== branch-native detached exec bookkeeping =="
EXEC_SELECTOR=$(ttc exec --detach "$LIFECYCLE@main" sh -c 'exec sleep 300')
EXEC_ID=${EXEC_SELECTOR##*:}
[ -n "$EXEC_ID" ] || fail "detached exec did not return a session ID"
ttc fork "$LIFECYCLE@main" process-child >/dev/null
ttc fork "$LIFECYCLE@process-child" process-grandchild >/dev/null
for branch in main process-child process-grandchild; do
  SESSION_JSON=$(ttc exec ls --json "$LIFECYCLE@$branch")
  echo "$SESSION_JSON" | grep -q "\"id\": \"$EXEC_ID\"" || fail "branch $branch lost cloned exec session $EXEC_ID"
  echo "$SESSION_JSON" | grep -q '"state": "running"' || fail "branch $branch did not retain a running exec session"
done
ttc exec kill "$LIFECYCLE@process-grandchild:$EXEC_ID" >/dev/null
[ "$(ttc exec wait "$LIFECYCLE@process-grandchild:$EXEC_ID")" = 137 ] || fail "grandchild exec wait status crossed branches"
ttc exec ls --json "$LIFECYCLE@process-child" | grep -q '"state": "running"' || fail "grandchild signal reached child exec"
ttc exec ls --json "$LIFECYCLE@main" | grep -q '"state": "running"' || fail "grandchild signal reached parent exec"
ttc exec kill "$LIFECYCLE@process-child:$EXEC_ID" >/dev/null
[ "$(ttc exec wait "$LIFECYCLE@process-child:$EXEC_ID")" = 137 ] || fail "child exec wait status crossed branches"
ttc exec ls --json "$LIFECYCLE@main" | grep -q '"state": "running"' || fail "child signal reached parent exec"
ttc exec kill "$LIFECYCLE@main:$EXEC_ID" >/dev/null
[ "$(ttc exec wait "$LIFECYCLE@main:$EXEC_ID")" = 137 ] || fail "parent exec wait status was not retained"
ttc rm -f "$LIFECYCLE@process-grandchild"
ttc rm -f "$LIFECYCLE@process-child"

echo "== durable daemon events =="
EVENTS_JSON="$TTC_E2E_ROOT/events.jsonl"
ttc events --json >"$EVENTS_JSON"
for event_type in container branch exec attach image; do
  grep -q "\"type\":\"$event_type\"" "$EVENTS_JSON" || fail "durable events omitted type $event_type"
done
grep -q '"action":"fork"' "$EVENTS_JSON" || fail "durable events omitted branch fork"
EVENT_FOLLOW="$TTC_E2E_ROOT/events-follow.jsonl"
EVENT_UNTIL=$(( $(date +%s) + 3 ))
ttc events --follow --json --since 0s --until "$EVENT_UNTIL" --filter action=pause >"$EVENT_FOLLOW" &
EVENT_FOLLOW_PID=$!
sleep 1
ttc pause "$LIFECYCLE@main"
ttc resume "$LIFECYCLE@main"
wait "$EVENT_FOLLOW_PID" || fail "events follower failed"
grep -q '"type":"branch".*"action":"pause"' "$EVENT_FOLLOW" || fail "events follower missed a new pause event"

ttc stop --time 2 "$LIFECYCLE@grandchild" "$LIFECYCLE@main"
[ "$(ttc inspect -f '{{.State.Status}}|{{.State.Running}}' "$LIFECYCLE")" = "exited|false" ] ||
  fail "inspect did not report the stopped family as exited"
ttc rm -f "$LIFECYCLE@grandchild"
ttc rm "$LIFECYCLE_ID"

echo "== explicit daemon stop policy =="
ttc run --detach --name "$DAEMON_STOP" "$BUSYBOX_IMAGE" sleep 300 >/dev/null
ttc fork "$DAEMON_STOP@main" child >/dev/null
ttc pause "$DAEMON_STOP@child" >/dev/null
if ttc daemon stop >/dev/null 2>&1; then
  fail "daemon stop accepted active branches without an explicit policy"
fi
ttc daemon status >/dev/null || fail "rejected daemon stop made ttcd unreachable"
ttc daemon stop --stop-all
if ttc daemon status >/dev/null 2>&1; then
  fail "ttcd remained reachable after intentional stop"
fi
ttc_daemon_start
DAEMON_STOP_STATES=$(ttc inspect -f '{{range .TTC.Branches}}{{.Metadata.ID}}={{.Metadata.State}} {{end}}' "$DAEMON_STOP")
case "$DAEMON_STOP_STATES" in
  *running*|*paused*) fail "intentional daemon stop left a live branch: $DAEMON_STOP_STATES" ;;
esac
ttc rm --force "$DAEMON_STOP"
ttc daemon stop >/dev/null

trap - EXIT HUP INT TERM
stop_nat_fixture
if $owned_root; then
  rm -rf "$TTC_E2E_ROOT"
fi
echo "TTC end-to-end image matrix passed"
