#!/bin/sh
# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0

set -eu

TTC=${TTC:-ttc}
IMAGE=${TTC_FANOUT_TEST_IMAGE:-busybox:1.36}
COUNT=${TTC_FANOUT_TEST_BRANCHES:-32}
NAME=${TTC_FANOUT_TEST_NAME:-ttc-fanout-lifecycle-$$}

cleanup() {
	"$TTC" rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

branches=
index=1
while test "$index" -le "$COUNT"; do
	branches="$branches b$index"
	index=$((index + 1))
done

"$TTC" run -d --name "$NAME" --shm-size 64m "$IMAGE" sh -c \
	'dd if=/dev/zero of=/dev/shm/state bs=1M count=32 2>/dev/null; exec sleep 3600' >/dev/null
# shellcheck disable=SC2086 # Each generated word is one validated branch name.
"$TTC" fork "$NAME@main" $branches >/dev/null

index=1
while test "$index" -le "$COUNT"; do
	offset=$((index * 4096))
	"$TTC" exec "$NAME@b$index" sh -c \
		"printf '%s' '$index' | dd of=/dev/shm/state bs=1 seek=$offset conv=notrunc 2>/dev/null"
	index=$((index + 1))
done

index=1
while test "$index" -le "$COUNT"; do
	"$TTC" dehydrate "$NAME@b$index" >/dev/null
	index=$((index + 2))
done

index=1
while test "$index" -le "$COUNT"; do
	offset=$((index * 4096))
	"$TTC" rehydrate "$NAME@b$index" >/dev/null
	got=$("$TTC" exec "$NAME@b$index" sh -c \
		"dd if=/dev/shm/state bs=1 skip=$offset count=${#index} 2>/dev/null")
	test "$got" = "$index" || {
		echo "TTC fanout lifecycle: branch b$index read '$got', want '$index'" >&2
		exit 1
	}
	index=$((index + 2))
done

echo "TTC $COUNT-branch batch-fork and hydration lifecycle passed"
