#!/bin/sh
# Test-only launcher for inherited tests. Production TTC uses ttcd directly.
set -eu

workspace=${TEST_WORKSPACE:-_main}
if [ -n "${RUNFILES_DIR:-}" ]; then
	ttcd="$RUNFILES_DIR/$workspace/runsc/ttcd/ttcd_/ttcd"
elif [ -d "$0.runfiles" ]; then
	ttcd="$0.runfiles/$workspace/runsc/ttcd/ttcd_/ttcd"
else
	echo "runsc compatibility launcher: Bazel runfiles were not found" >&2
	exit 1
fi
exec "$ttcd" runsc "$@"
