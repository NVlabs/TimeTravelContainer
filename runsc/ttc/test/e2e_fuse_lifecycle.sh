#!/bin/sh
# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -eu

TTC=${TTC:-ttc}
IMAGE=${TTC_FUSE_TEST_IMAGE:-ubuntu:24.04}
NAME=${TTC_FUSE_TEST_NAME:-ttc-fuse-lifecycle-$$}

cleanup() {
	"$TTC" rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

fail() {
	echo "TTC FUSE lifecycle: $*" >&2
	exit 1
}

"$TTC" run -d --cap-add SYS_ADMIN --network=ttc --nat --name "$NAME" "$IMAGE" sleep infinity >/dev/null
"$TTC" exec "$NAME@main" sh -lc \
	'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fuse-overlayfs'
"$TTC" exec "$NAME@main" sh -lc '
	mkdir -p /tmp/fuse/lower /tmp/fuse/upper /tmp/fuse/work /tmp/fuse/merged
	echo base >/tmp/fuse/lower/base
	fuse-overlayfs -f -o lowerdir=/tmp/fuse/lower,upperdir=/tmp/fuse/upper,workdir=/tmp/fuse/work /tmp/fuse/merged >/tmp/fuse/server.log 2>&1 &
	for delay in 1 1 1 1 1; do
		test -f /tmp/fuse/merged/base && break
		sleep "$delay"
	done
	test "$(cat /tmp/fuse/merged/base)" = base
	echo parent >/tmp/fuse/merged/branch
'

"$TTC" fork "$NAME@main" child >/dev/null
child_before=$("$TTC" exec "$NAME@child" cat /tmp/fuse/merged/branch)
test "$child_before" = parent || fail "child initially read $child_before"
"$TTC" exec "$NAME@child" sh -lc 'echo child >/tmp/fuse/merged/branch'
parent_after=$("$TTC" exec "$NAME@main" cat /tmp/fuse/merged/branch)
test "$parent_after" = parent || fail "child FUSE write leaked into parent: $parent_after"

"$TTC" dehydrate "$NAME@child" >/dev/null
"$TTC" rehydrate "$NAME@child" >/dev/null
child_after=$("$TTC" exec "$NAME@child" cat /tmp/fuse/merged/branch)
test "$child_after" = child || fail "rehydrated child read $child_after"
"$TTC" exec "$NAME@child" sh -lc 'echo rehydrated >/tmp/fuse/merged/branch'
parent_final=$("$TTC" exec "$NAME@main" cat /tmp/fuse/merged/branch)
test "$parent_final" = parent || fail "rehydrated FUSE write leaked into parent: $parent_final"

echo "TTC in-sandbox FUSE fork/dehydrate/rehydrate lifecycle passed"
