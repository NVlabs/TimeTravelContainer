#!/bin/sh
# Test-only launcher for inherited tests. Production TTC uses ttcd directly.
set -eu

workspace=${TEST_WORKSPACE:-_main}
if [ -n "${RUNFILES_DIR:-}" ] && [ -x "$RUNFILES_DIR/$workspace/runsc/ttcd/ttcd_/ttcd" ]; then
	ttcd="$RUNFILES_DIR/$workspace/runsc/ttcd/ttcd_/ttcd"
elif [ -x "$0.runfiles/$workspace/runsc/ttcd/ttcd_/ttcd" ]; then
	ttcd="$0.runfiles/$workspace/runsc/ttcd/ttcd_/ttcd"
elif [ -x "$(dirname "$0")/ttcd/ttcd_/ttcd" ]; then
	# A launcher reached through another test's runfiles has its data next to
	# the launcher symlink, while RUNFILES_DIR is intentionally cleared before
	# the Sentry re-executes.
	ttcd="$(dirname "$0")/ttcd/ttcd_/ttcd"
elif [ -x "$(dirname "$0")/ttcd" ]; then
	# The upstream syscall runner assembles an installed release layout, where
	# the TTC compatibility launcher and ttcd are adjacent binaries.
	ttcd="$(dirname "$0")/ttcd"
else
	echo "runsc compatibility launcher: Bazel runfiles were not found" >&2
	exit 1
fi
exec "$ttcd" runsc "$@"
