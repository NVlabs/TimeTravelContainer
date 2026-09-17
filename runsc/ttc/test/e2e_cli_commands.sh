#!/bin/sh
# Copyright 2026 The gVisor Authors.
# Licensed under the Apache License, Version 2.0 (the "License");

# Covers the CLI paths that are intentionally not repeated in the larger image,
# storage, terminal, and distributed workload matrices. Keep this focused on
# command composition and cross-command state transitions rather than image
# compatibility (e2e_image_matrix.sh owns that coverage).
set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_CLI_TEST_IMAGE:-busybox:1.36}
ROOT=${TTC_CLI_TEST_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-cli-commands.XXXXXX")}
KEEP_ROOT=${TTC_CLI_TEST_KEEP_ROOT:-}
NAME=${TTC_CLI_TEST_NAME:-ttc-cli-commands-$$}
RESTORED=$NAME-restored
VOLUME=$NAME-volume
SOCKET=$ROOT/ttcd.sock
CHECKPOINT=$ROOT/checkpoint.bin
HOST_INPUT=$ROOT/host-input
HOST_OUTPUT=$ROOT/host-output

fail() {
	echo "TTC CLI command matrix: $*" >&2
	exit 1
}

test -x "$TTC" || fail "TTC is not executable: $TTC"
test -x "$TTCD" || fail "TTCD is not executable: $TTCD"
mkdir -p "$ROOT/bin"
chmod 700 "$ROOT"
install -m 0755 "$(readlink -f "$TTC")" "$ROOT/bin/ttc"
install -m 0755 "$(readlink -f "$TTCD")" "$ROOT/bin/ttcd"
TTC=$ROOT/bin/ttc

ttc() { TTC_SOCKET=$SOCKET "$TTC" "$@"; }

cleanup() {
	set +e
	if test -S "$SOCKET"; then
		ttc rm --force "$RESTORED" "$NAME" >/dev/null 2>&1
		ttc volume rm --force "$VOLUME" >/dev/null 2>&1
		ttc daemon stop --stop-all >/dev/null 2>&1
	fi
	if test -z "$KEEP_ROOT" && test -z "${TTC_CLI_TEST_ROOT:-}"; then
		find "$ROOT" -depth -mindepth 1 -delete 2>/dev/null || true
		rmdir "$ROOT" 2>/dev/null || true
	else
		echo "TTC CLI command root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

# Completion, version, and help are client-owned and require no daemon.
for shell in bash zsh fish; do
	"$TTC" completion "$shell" | grep -q ttc || fail "$shell completion is empty"
done
"$TTC" --version | grep -q 'ttc version'
"$TTC" daemon --help | grep -q 'requests'

TTC_ROOT=$ROOT/data TTC_SOCKET=$SOCKET "$TTC" daemon start --unsafe-no-host-isolation >/dev/null
ttc daemon status | jq -e '.pid > 0 and .version > 0' >/dev/null

ttc pull "$IMAGE" >/dev/null
ttc images --json | grep -q 'busybox'
ttc image ls --json | grep -q 'busybox'
ttc image inspect "$IMAGE" | grep -q 'busybox'

ttc volume create "$VOLUME" >/dev/null
ttc volume ls | grep -q "$VOLUME"
ttc volume inspect "$VOLUME" | grep -q "$VOLUME"

ttc create --name "$NAME" -v "$VOLUME:/data" "$IMAGE" sh -c \
	'trap "exit 0" TERM; while :; do sleep 1; done' >/dev/null
ttc inspect "$NAME" | grep -q "$NAME"
ttc start "$NAME@main" >/dev/null
ttc exec "$NAME@main" sh -c 'echo persistent >/data/value; echo rootfs >/rootfs-value'

# Named sessions remain controllable independently of the client that created
# them, and terminal observation must be available without attaching.
ttc exec -itd --session repl "$NAME@main" sh >/dev/null
ttc exec ls --json "$NAME@main" | grep -q 'repl'
ttc tty stat "$NAME@main" --session repl --json | grep -q 'session_epoch'
watch_status=0
timeout 1s "$TTC" --daemon-socket "$SOCKET" tty watch "$NAME@main" --session repl --json >/dev/null 2>&1 || watch_status=$?
test "$watch_status" -eq 124 || fail "tty watch exited unexpectedly with $watch_status"
ttc exec kill --signal KILL "$NAME@main:repl" >/dev/null
ttc exec wait "$NAME@main:repl" >/dev/null || true

printf 'copied-through-cli\n' >"$HOST_INPUT"
ttc cp "$HOST_INPUT" "$NAME@main:/copied"
ttc cp "$NAME@main:/copied" "$HOST_OUTPUT"
cmp "$HOST_INPUT" "$HOST_OUTPUT"
ttc diff --json "$NAME@main" | grep -q 'copied'

# Rename updates the friendly lineage view without changing branch identity or
# leaving the old name reserved.
ttc fork "$NAME@main" child >/dev/null
ttc fork "$NAME@child" grandchild >/dev/null
ttc rename "$NAME@child" renamed >/dev/null
ttc ls --json | jq -e --arg name "$NAME" \
	'any(.[] | select(.name == $name).branches[]; .id == "grandchild" and .parent == "renamed")' >/dev/null
if ttc exec "$NAME@child" true >/dev/null 2>&1; then
	fail "old branch name remained usable after rename"
fi
ttc fork "$NAME@main" child >/dev/null

ttc quarantine "$NAME@renamed" >/dev/null
ttc ls --json | jq -e --arg name "$NAME" \
	'any(.[] | select(.name == $name).branches[]; .id == "renamed" and .quarantined == true)' >/dev/null
ttc unquarantine "$NAME@renamed" >/dev/null
ttc pause "$NAME@renamed" >/dev/null
ttc resume "$NAME@renamed" >/dev/null

# Checkpoint/restore remains orthogonal to live branch snapshots and creates a
# new family. The restored rootfs and managed volume contents are both checked.
ttc checkpoint "$NAME@main" "$CHECKPOINT"
ttc restore --name "$RESTORED" "$CHECKPOINT" >/dev/null
test "$(ttc exec "$RESTORED@main" cat /rootfs-value)" = rootfs
test "$(ttc exec "$RESTORED@main" cat /data/value)" = persistent

# Cold branches are still copyable and forkable, then can return to execution.
ttc dehydrate "$NAME@renamed" >/dev/null
ttc wait --condition durable "$NAME@renamed" >/dev/null
ttc cp "$NAME@renamed:/rootfs-value" "$ROOT/dehydrated-copy"
ttc fork "$NAME@renamed" coldchild >/dev/null
ttc rehydrate "$NAME@renamed" >/dev/null
test "$(ttc exec "$NAME@coldchild" cat /rootfs-value)" = rootfs

# stop on a paused branch discards execution directly; start and restart must
# reconstruct it without changing its writable filesystem.
ttc pause "$NAME@grandchild" >/dev/null
ttc stop "$NAME@grandchild" >/dev/null
ttc start "$NAME@grandchild" >/dev/null
ttc restart "$NAME@main" >/dev/null
test "$(ttc exec "$NAME@main" cat /rootfs-value)" = rootfs

ttc top --json "$NAME@main" | jq -e 'length > 0' >/dev/null
ttc stats --json "$NAME@main" | jq -e '.branches | length >= 1' >/dev/null
ttc logs --tail 20 "$NAME@main" >/dev/null
ttc ps -a --format '{{.Container}}@{{.Branch}}' | grep -q "$NAME@main"

# Durable request history is an operator-facing fault-recovery interface. A
# normal completed mutation supplies a stable record for list and inspect.
request_id=$(ttc daemon requests --state completed --json | jq -r '.[0].request_id')
test -n "$request_id" && test "$request_id" != null
ttc daemon request inspect --json "$request_id" | jq -e '.state == "completed"' >/dev/null
if ttc daemon request resolve --outcome abandoned "$request_id" >/dev/null 2>&1; then
	fail "completed request accepted an operator resolution"
fi
ttc events --json --filter container="$NAME" | grep -q "$NAME"

# kill and wait exercise the terminated state independently of graceful stop.
ttc kill --signal KILL "$RESTORED@main" >/dev/null
ttc wait "$RESTORED@main" >/dev/null
ttc rm --force "$RESTORED"
ttc rm --force "$NAME"
ttc volume rm "$VOLUME"
# Exercise the Docker-compatible top-level alias independently of `image rm`.
# The dedicated test root has no remaining family references at this point.
ttc rmi "$IMAGE" >/dev/null
if ttc images --json | grep -q 'busybox'; then
	fail "rmi left the removed image in inventory"
fi
ttc daemon stop >/dev/null

echo "TTC client-local and standalone CLI command matrix passed"
