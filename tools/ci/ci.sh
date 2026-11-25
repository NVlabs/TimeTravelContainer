#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The steps of TTC's CI pipelines, on GitLab (.gitlab-ci.yml) and GitHub
# (.github/workflows/ttc-*.yml), which also run by hand from the repository's
# root:
#
#   tools/ci/ci.sh setup          installs what a Linux build needs, and Bazel,
#                                 on Ubuntu, as root
#   tools/ci/ci.sh check          builds ttc and ttcd, and runs the unit tests
#   tools/ci/ci.sh release-plan   decides whether the default branch has
#                                 changed since the last release, into
#                                 release.env
#   tools/ci/ci.sh release-build  builds this platform's release archive,
#                                 ci-out/release/ttc-OS-ARCH.zip
#
# Each runs the Bazel release that .bazelversion names, which it downloads
# once. On Linux, Bazel keeps it, its downloads and its outputs in
# CI_CACHE_DIR (default .ci-cache), which the pipeline caches between jobs. On
# macOS the runner's Bazel output base persists between jobs instead, and
# Bazel's release is kept in CI_TOOLS_DIR (default ~/.cache/ttc-ci); where
# CI_CACHE_DIR is set, as on GitHub's runners, which keep nothing, Bazel keeps
# its downloads and outputs there too.
set -eu

cd "$(dirname "$0")/../.."
root=$(pwd)
os=$(uname -s)
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) arch=$(uname -m) ;;
esac
platform=$(echo "$os" | tr '[:upper:]' '[:lower:]')-$arch
cache=${CI_CACHE_DIR:-$root/.ci-cache}
out=$root/ci-out

# The binaries of a release, and the unit tests that each check runs: those
# of TTC's own packages, which run without privileges or a GPU (the renderer's
# tests on a GPU skip without one), and on macOS the HVF platform's.
binaries="//runsc/ttc:ttc //runsc/ttcd:ttcd"
tests="
//pkg/gfxproto/...
//pkg/sentry/devices/virtgpu/...
//pkg/sentry/ttcchunk/...
//pkg/state/...
//pkg/ttcpack/...
//pkg/ttcremote/...
//pkg/ttcsnapshot/...
//pkg/ttcvolume/...
//runsc/ttc/...
//runsc/ttcd/...
"
if [ "$os" = Darwin ]; then
	# The HVF platform, and its registration and description (runsc finds
	# its platform by both).
	tests="$tests //pkg/sentry/platform/hvf/... //pkg/sentry/platform/platforms/..."
else
	# The helpers that ttcd carries on Linux, which verify_binaries checks:
	# of what its cache holds, Bazel fetches only what a build names.
	helpers="//runsc/ttc/hostbin:gfx_renderer_gz //runsc/ttc/hostbin:cuda_checkpoint_gz"
fi

log() {
	echo "ci: $*" >&2
}

fail() {
	log "$*"
	exit 1
}

bazel_startup() {
	if [ "$os" = Darwin ]; then
		# The runner is a workstation: a server that a job leaves
		# behind ends once idle, rather than holding its memory for
		# three hours.
		echo "--max_idle_secs=900"
	fi
}

bazel_flags() {
	echo "--color=no --curses=no --show_progress_rate_limit=30"
	if [ "$os" = Linux ] || [ -n "${CI_CACHE_DIR:-}" ]; then
		# The caches are in the checkout, where the pipeline's cache
		# takes them from, and Bazel refuses its repository contents
		# cache there.
		echo "--disk_cache=$cache/disk --repository_cache=$cache/repository --repo_contents_cache="
		echo "--experimental_disk_cache_gc_max_size=${CI_BAZEL_DISK_CACHE_SIZE:-6G}"
	fi
}

# ensure_bazel sets BAZEL to the Bazel release that .bazelversion names,
# which it downloads, checked, unless it has it.
ensure_bazel() {
	version=$(cat .bazelversion)
	if [ "$os" = Linux ]; then
		dir=$cache/bin
		name=bazel-$version-linux
		sum="sha256sum"
	else
		dir=${CI_TOOLS_DIR:-$HOME/.cache/ttc-ci}
		name=bazel-$version-darwin
		sum="shasum -a 256"
	fi
	if [ "$arch" = amd64 ]; then
		name=$name-x86_64
	else
		name=$name-arm64
	fi
	BAZEL=$dir/$name
	[ -x "$BAZEL" ] && return 0
	mkdir -p "$dir"
	url=https://releases.bazel.build/$version/release/$name
	curl -fsSL --retry 3 -o "$BAZEL.tmp" "$url"
	want=$(curl -fsSL --retry 3 "$url.sha256" | awk '{print $1}')
	got=$($sum "$BAZEL.tmp" | awk '{print $1}')
	[ -n "$want" ] && [ "$want" = "$got" ] || fail "$url: sha256 $got, want $want"
	chmod +x "$BAZEL.tmp"
	mv "$BAZEL.tmp" "$BAZEL"
}

# shellcheck disable=SC2046 # The flags are words.
run_bazel() {
	command=$1
	shift
	"$BAZEL" $(bazel_startup) "$command" $(bazel_flags) "$@"
}

# output_path prints the path, from the checkout, of the executable that
# target built. A binary of a configuration of its own is not in bazel-bin:
# on macOS, ttcd builds with cgo, which the build's options turn off.
output_path() {
	run_bazel cquery --output=files "$1" 2>/dev/null
}

setup() {
	[ "$os" = Linux ] || fail "setup is for Linux jobs"
	if [ "${RELEASE:-1}" != 1 ]; then
		log "no release; nothing to set up"
		return 0
	fi
	export DEBIAN_FRONTEND=noninteractive
	cross=crossbuild-essential-arm64
	extra=libc6-dev-i386
	if [ "$arch" = arm64 ]; then
		cross=crossbuild-essential-amd64
		extra=
	fi
	# What the builder image (images/default) has for building runsc. The
	# packages are kept in the cache.
	mkdir -p "$cache/apt/partial"
	apt-get update -qq
	# shellcheck disable=SC2086 # extra may be empty.
	apt-get install -y -qq --no-install-recommends \
		-o Dir::Cache::Archives="$cache/apt" -o APT::Keep-Downloaded-Packages=true \
		build-essential "$cross" $extra clang libbpf-dev \
		ca-certificates curl git file python-is-python3 python3 \
		zip unzip patch diffutils pkg-config libffi-dev libssl-dev >/dev/null
	ensure_bazel
	# The checkout belongs to another user than the job's, and the build's
	# version stamp asks git about it.
	git config --global --add safe.directory "$root"
	"$BAZEL" --version
}

# collect_test_results copies the JUnit reports of the tests that ran since
# the marker, and the logs of those that failed, into ci-out, which the
# pipeline keeps: Bazel's test logs lie outside the checkout, and on a runner
# whose output base persists hold earlier jobs' results too. Each
# configuration that tests build in has its own test logs: those of a test in
# a configuration of its own, as a test with cgo is on macOS, are not in
# bazel-testlogs.
collect_test_results() {
	marker=$1
	outputs=$("$BAZEL" $(bazel_startup) info $(bazel_flags) output_path 2>/dev/null) || return 0
	[ -d "$outputs" ] || return 0
	# Only this check's results stay. An earlier check by hand leaves its
	# reports read-only, as Bazel makes them, and cp cannot replace them.
	rm -rf "$out/junit" "$out/failed"
	mkdir -p "$out/junit" "$out/failed"
	for testlogs in "$outputs"/*/testlogs; do
		[ -d "$testlogs" ] || continue
		(
			cd "$testlogs"
			find . -name test.xml -newer "$marker" | while read -r report; do
				dir=$(dirname "$report")
				mkdir -p "$out/junit/$dir"
				cp "$report" "$out/junit/$dir/"
				if grep -q '<failure' "$report" || grep -q '<error' "$report"; then
					mkdir -p "$out/failed/$dir"
					cp "$dir/test.log" "$out/failed/$dir/" 2>/dev/null || true
				fi
			done
		)
	done
}

check() {
	ensure_bazel
	mkdir -p "$out"
	# shellcheck disable=SC2086 # The targets are words.
	run_bazel build $binaries ${helpers:-}
	verify_binaries
	marker="$out/.tests-started"
	touch "$marker"
	status=0
	# shellcheck disable=SC2086 # The targets are words.
	run_bazel test --test_tag_filters=-nogo --test_output=errors --keep_going $tests || status=$?
	collect_test_results "$marker"
	return $status
}

# verify_binaries checks what a release needs of the binaries just built: on
# Linux, static executables, whose ttcd carries the GPU renderer and
# ttc-cuda-checkpoint; on macOS, a ttcd that is its renderer, and that loads
# no library but Apple's.
verify_binaries() {
	ttc=$(output_path //runsc/ttc:ttc)
	ttcd=$(output_path //runsc/ttcd:ttcd)
	if [ "$os" = Linux ]; then
		for binary in "$ttc" "$ttcd"; do
			file -L "$binary" | grep -q 'statically linked' || fail "$binary is not static: $(file -L "$binary")"
		done
		for helper in ttc-gfx-renderer ttc-cuda-checkpoint; do
			gz=bazel-bin/runsc/ttc/hostbin/$helper.gz
			[ -s "$gz" ] || fail "ttcd carries no $helper"
			# ttcd embeds the archive as it is.
			python3 -c 'import sys; sys.exit(open(sys.argv[2], "rb").read() not in open(sys.argv[1], "rb").read())' "$ttcd" "$gz" ||
				fail "ttcd carries another $helper than $gz"
		done
	else
		"$ttcd" _gfx-renderer >/dev/null 2>&1 && status=0 || status=$?
		# The renderer's usage exits 2; a ttcd that is not the renderer
		# would start a daemon, or fail otherwise.
		[ "$status" = 2 ] || fail "ttcd is not the GPU renderer (exit $status)"
		# Signed by a developer's certificate, under the hardened runtime,
		# ttcd may load only Apple's libraries and the developer's: it
		# links the others, MoltenVK among them.
		others=$(otool -L "$ttcd" | tail -n +2 | awk '{print $1}' | grep -vE '^(/usr/lib/|/System/Library/)' || true)
		[ -z "$others" ] || fail "ttcd loads libraries other than Apple's: $others"
	fi
}

# release_plan writes release.env, which the release's jobs read: RELEASE=1
# if the default branch changed since the last release, other than in its
# documentation, and RELEASE_TAG, the new release's tag; and release-notes.md,
# the release's description.
release_plan() {
	[ -n "${CI_API_V4_URL:-}${GITHUB_API_URL:-}" ] || fail "release-plan runs in a pipeline"
	last=$(python3 tools/ci/release.py last)
	tag="daily-$(date -u +%Y%m%d)-$(git rev-parse --short=10 HEAD)"
	release=1
	if [ -n "$last" ]; then
		last_tag=${last% *}
		last_commit=${last#* }
		if ! git cat-file -e "$last_commit^{commit}" 2>/dev/null; then
			git fetch --quiet origin "$last_commit" || true
		fi
		if git cat-file -e "$last_commit^{commit}" 2>/dev/null; then
			changed=$(git diff --name-only "$last_commit" HEAD -- . ':(exclude)g3doc' ':(exclude)website' ':(exclude)*.md' | head -n 20)
			if [ "$last_commit" = "$(git rev-parse HEAD)" ]; then
				log "no change since $last_tag; no release"
				release=0
			elif [ -z "$changed" ]; then
				log "nothing but documentation changed since $last_tag; no release"
				release=0
			else
				log "changed since $last_tag:"
				echo "$changed" | sed 's/^/  /' >&2
			fi
		else
			log "the last release's commit $last_commit is not in the history; releasing"
		fi
	else
		log "no release yet; releasing"
	fi
	{
		echo "RELEASE=$release"
		echo "RELEASE_TAG=$tag"
	} >release.env
	cat release.env >&2
	{
		echo "Daily build of $(git rev-parse HEAD), for Linux x86-64 and arm64 and macOS arm64:"
		echo "each platform's zip holds ttc and ttcd, to install side by side, with their"
		echo "LICENSE and THIRD_PARTY_NOTICES.md."
		echo
		echo "On macOS, ttcd is signed ad hoc with the Hypervisor entitlement, which the zip's"
		echo "hvf.entitlements holds, to sign it again if need be:"
		echo "\`codesign --force --sign - --entitlements hvf.entitlements ttcd\`. A zip that a"
		echo "browser downloads is quarantined: \`xattr -d com.apple.quarantine\` it before"
		echo "unpacking it."
		if [ -n "$last" ] && git cat-file -e "$last_commit^{commit}" 2>/dev/null; then
			echo
			echo "Changes since $last_tag:"
			echo
			git log --no-merges --format='- %s (%h)' "$last_commit..HEAD" | head -n 200
		fi
	} >release-notes.md
}

release_build() {
	if [ "${RELEASE:-1}" != 1 ]; then
		log "no release"
		return 0
	fi
	mkdir -p "$out/release"
	if [ -n "${RELEASE_TAG:-}" ]; then
		# The binaries' version, which git describes, is the release's
		# tag, which the release makes once they are built. The tag here
		# goes once they are: a runner's checkout may persist.
		git tag -f "$RELEASE_TAG" HEAD >/dev/null
		trap 'git tag -d "$RELEASE_TAG" >/dev/null 2>&1 || true' EXIT
	fi
	check
	# The platform's archive holds ttc and ttcd as they install, their
	# license and third-party notices, and on macOS the entitlements that
	# ttcd's signature carries, to sign it again.
	stage=$out/stage
	rm -rf "$stage"
	mkdir -p "$stage"
	cp "$(output_path //runsc/ttc:ttc)" "$(output_path //runsc/ttcd:ttcd)" "$stage/"
	chmod 0755 "$stage/ttc" "$stage/ttcd"
	cp LICENSE THIRD_PARTY_NOTICES.md "$stage/"
	chmod 0644 "$stage/LICENSE" "$stage/THIRD_PARTY_NOTICES.md"
	files="ttc ttcd LICENSE THIRD_PARTY_NOTICES.md"
	if [ "$os" = Darwin ]; then
		# Hypervisor.framework requires its entitlement of ttcd, which an
		# ad-hoc signature carries.
		codesign --force --sign - --entitlements runsc/hvf.entitlements "$stage/ttcd"
		cp runsc/hvf.entitlements "$stage/"
		chmod 0644 "$stage/hvf.entitlements"
		files="$files hvf.entitlements"
	fi
	archive=$out/release/ttc-$platform.zip
	rm -f "$archive"
	# shellcheck disable=SC2086 # The files are words.
	(cd "$stage" && zip -q -X "$archive" $files)
	rm -rf "$stage"
	verify_archive "$archive" $files
}

# verify_archive checks that archive holds exactly the files that follow it,
# that its ttc and ttcd run from where it unpacks, and on macOS that ttcd
# keeps its signature and entitlement there.
verify_archive() {
	archive=$1
	shift
	unpacked=$out/unpacked
	rm -rf "$unpacked"
	mkdir -p "$unpacked"
	(cd "$unpacked" && unzip -q "$archive")
	want=$(printf '%s\n' "$@" | sort)
	got=$(cd "$unpacked" && find . -type f | sed 's#^\./##' | sort)
	[ "$got" = "$want" ] || fail "$archive holds $(echo "$got" | tr '\n' ' '), want $(echo "$want" | tr '\n' ' ')"
	for binary in ttc ttcd; do
		[ -x "$unpacked/$binary" ] || fail "$archive's $binary is not executable"
		"$unpacked/$binary" --version
	done
	if [ "$os" = Darwin ]; then
		codesign --verify --strict "$unpacked/ttcd"
		codesign -d --entitlements - "$unpacked/ttcd" 2>/dev/null | grep -q com.apple.security.hypervisor ||
			fail "$archive's ttcd lacks the Hypervisor entitlement"
	fi
	rm -rf "$unpacked"
}

case "${1:-}" in
setup) setup ;;
check) check ;;
release-plan) release_plan ;;
release-build) release_build ;;
*)
	echo "usage: $0 setup|check|release-plan|release-build" >&2
	exit 2
	;;
esac
