#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Manual end-to-end coverage for Android in TTC: redroid (an Android image
# for containers) boots, as its container's init, with the virtual GPU
# (--gfx); SurfaceFlinger composes with ANGLE on gfxstream's Vulkan driver;
# Android's network comes up and validates; apps render on the GPU; the
# screen records to video, as screen mirroring such as scrcpy's needs; given
# one, an APK installs and runs; and a live fork of Android, whose GPU state
# the child restores, runs on in both branches.
#
# It needs what runsc/ttc/test/e2e_gfx.sh needs (TTC, TTCD,
# TTC_GFX_RENDERER, TTC_GFXSTREAM_LIBRARY, TTC_GFX_GUEST_DRIVER), and:
#   ANDROID_GFX_DRIVER, gfxstream's Vulkan driver for Android
#     (runsc/ttc/gfxrenderer/guest/build_mesa_gfxstream_android.sh);
#   REDROID_IMAGE, an arm64 redroid image (default
#     redroid/redroid:14.0.0_64only-latest), which the daemon pulls;
#   optionally APK, an app to install and launch.
#
# The daemon runs with host isolation unless TTC_UNSAFE_NO_HOST_ISOLATION=1.
set -eu

TTC=${TTC:-}
TTCD=${TTCD:-}
TTC_GFX_RENDERER=${TTC_GFX_RENDERER:-}
TTC_GFXSTREAM_LIBRARY=${TTC_GFXSTREAM_LIBRARY:-}
TTC_GFX_GUEST_DRIVER=${TTC_GFX_GUEST_DRIVER:-}
ANDROID_GFX_DRIVER=${ANDROID_GFX_DRIVER:-}
REDROID_IMAGE=${REDROID_IMAGE:-redroid/redroid:14.0.0_64only-latest}
APK=${APK:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
# How long Android may take to boot, in seconds.
BOOT_TIMEOUT=${BOOT_TIMEOUT:-300}

fail() {
  echo "ttc redroid e2e: $*" >&2
  exit 1
}

[ -x "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$TTCD" ] || fail "set TTCD to the matching ttcd binary"
[ -x "$TTC_GFX_RENDERER" ] || fail "set TTC_GFX_RENDERER to ttc-gfx-renderer"
[ -f "$TTC_GFXSTREAM_LIBRARY" ] || fail "set TTC_GFXSTREAM_LIBRARY to gfxstream's host library"
[ -f "$TTC_GFX_GUEST_DRIVER/libvulkan_gfxstream.so" ] ||
  fail "set TTC_GFX_GUEST_DRIVER to a directory with Mesa's libvulkan_gfxstream.so"
[ -f "$ANDROID_GFX_DRIVER" ] ||
  fail "set ANDROID_GFX_DRIVER to gfxstream's Vulkan driver for Android"
[ -z "$APK" ] || [ -f "$APK" ] || fail "APK $APK does not exist"
owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  tmp=${TMPDIR:-/tmp}
  TTC_E2E_ROOT=$(mktemp -d "${tmp%/}/ttc-redroid.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTC_E2E_ROOT"
fi
TEST_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
GPU_CONFIG="$TEST_DIR/../gfxrenderer/guest/redroid/gpu_config.sh"
BIN_DIR="$TTC_E2E_ROOT/bin"
DRIVER_DIR="$TTC_E2E_ROOT/driver"
OUT_DIR="$TTC_E2E_ROOT/out"
mkdir -p "$BIN_DIR" "$DRIVER_DIR" "$OUT_DIR"
install -m 0755 "$(readlink -f "$TTC")" "$BIN_DIR/ttc"
install -m 0755 "$(readlink -f "$TTCD")" "$BIN_DIR/ttcd"
install -m 0755 "$(readlink -f "$TTC_GFX_RENDERER")" "$BIN_DIR/ttc-gfx-renderer"
unset TTC_GFX_RENDERER
if [ "$(uname -s)" = Darwin ]; then
  # A copy loses its ad-hoc signature; Hypervisor.framework requires the
  # entitlement on the executable at its final path.
  /usr/bin/codesign --force --sign - \
    --entitlements "$TEST_DIR/../../hvf.entitlements" "$BIN_DIR/ttcd" >/dev/null
fi
export TTC_GFXSTREAM_LIBRARY
install -m 0644 "$TTC_GFX_GUEST_DRIVER/libvulkan_gfxstream.so" "$DRIVER_DIR/"
export TTC_GFX_GUEST_DRIVER="$DRIVER_DIR"

ttc() {
  "$BIN_DIR/ttc" "$@"
}

unset TTC_LOCAL_ROOT
export TTC_ROOT="$TTC_E2E_ROOT/data"
NAME="redroid-$$"

cleanup() {
  ttc daemon stop --stop-all >/dev/null 2>&1 || true
  if [ "$owned_root" = true ]; then
    rm -rf "$TTC_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

set -- daemon start
if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
  set -- "$@" --unsafe-no-host-isolation
fi
ttc "$@"

# adb runs a command in Android, as its shell would; adb_in, in a branch.
adb() {
  ttc exec "$NAME@main" "$@"
}
adb_in() {
  branch=$1
  shift
  ttc exec "$NAME@$branch" "$@"
}

# Android's init is the container's. Before it starts, the container gets
# the Android driver, and redroid's graphics configuration for the GPU.
ttc create --name "$NAME" --gfx --gfx-fork snapshot --cap-add ALL --network ttc --nat \
  --entrypoint /init "$REDROID_IMAGE" \
  qemu=1 androidboot.hardware=redroid androidboot.use_memfd=true >/dev/null
ttc cp "$ANDROID_GFX_DRIVER" "$NAME@main:/vendor/lib64/hw/vulkan.gfxstream.so" >/dev/null
ttc cp "$GPU_CONFIG" "$NAME@main:/vendor/bin/gpu_config.sh" >/dev/null
ttc start "$NAME" >/dev/null

deadline=$(($(date +%s) + BOOT_TIMEOUT))
until [ "$(adb /system/bin/getprop sys.boot_completed 2>/dev/null)" = 1 ]; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "Android did not boot in ${BOOT_TIMEOUT}s"
  sleep 5
done
echo "ttc redroid e2e: booted"

# SurfaceFlinger composes with ANGLE, on the virtual GPU.
gles=$(adb /system/bin/dumpsys SurfaceFlinger 2>&1 | grep '^GLES:') ||
  fail "SurfaceFlinger reports no GLES renderer"
case "$gles" in
  *ANGLE*"Virtio-GPU GFXStream"*) ;;
  *) fail "SurfaceFlinger composes without the virtual GPU: $gles" ;;
esac
echo "ttc redroid e2e: $gles"

# Android brings its Ethernet network up on eth0, and validates it on the
# internet.
deadline=$(($(date +%s) + 120))
until adb /system/bin/dumpsys connectivity 2>/dev/null | grep -q '&VALIDATED&'; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "Android's network is not validated"
  sleep 5
done
echo "ttc redroid e2e: the network is validated"

# launch PACKAGE [BRANCH] starts the package's launcher activity, and checks
# that it renders with ANGLE on gfxstream's driver.
launch() {
  branch=${2:-main}
  component=$(adb_in "$branch" /system/bin/cmd package resolve-activity --brief \
    -c android.intent.category.LAUNCHER "$1" | tail -1)
  case "$component" in
    */*) ;;
    *) fail "$1 has no launcher activity: $component" ;;
  esac
  adb_in "$branch" /system/bin/am start -W -n "$component" >/dev/null || fail "starting $component"
  sleep 5
  pid=$(adb_in "$branch" /system/bin/pidof -s "$1") || fail "$1 is not running"
  maps=$(adb_in "$branch" /system/bin/cat "/proc/$pid/maps")
  case "$maps" in
    *libGLESv2_angle.so*) ;;
    *) fail "$1 does not render with ANGLE" ;;
  esac
  case "$maps" in
    *vulkan.gfxstream.so*) ;;
    *) fail "$1 does not use gfxstream's Vulkan driver" ;;
  esac
  adb_in "$branch" /system/bin/screencap -p "/data/local/tmp/$1.png" || fail "screencap"
  ttc cp "$NAME@$branch:/data/local/tmp/$1.png" "$OUT_DIR/$branch-$1.png" >/dev/null
  # A blank screen compresses to a few kilobytes.
  size=$(wc -c <"$OUT_DIR/$branch-$1.png")
  [ "$size" -gt 20000 ] || fail "$1's screen is blank ($size bytes)"
  echo "ttc redroid e2e: $1 renders on the GPU in $branch"
}

launch com.android.settings

# The screen records to video: Codec2's software encoder, whose buffers come
# from the Sentry's DMA-BUF heap, takes frames from an input surface, as
# screen mirroring such as scrcpy's does.
adb /system/bin/screenrecord --time-limit 3 /data/local/tmp/screen.mp4 >/dev/null 2>&1 ||
  fail "screenrecord failed"
ttc cp "$NAME@main:/data/local/tmp/screen.mp4" "$OUT_DIR/screen.mp4" >/dev/null
size=$(($(wc -c <"$OUT_DIR/screen.mp4")))
[ "$size" -gt 10000 ] || fail "the screen recording is empty ($size bytes)"
echo "ttc redroid e2e: the screen records to video ($size bytes)"

if [ -n "$APK" ]; then
  ttc cp "$APK" "$NAME@main:/data/local/tmp/app.apk" >/dev/null
  result=$(adb /system/bin/pm install -r /data/local/tmp/app.apk 2>&1)
  case "$result" in
    *Success*) ;;
    *) fail "installing $APK: $result" ;;
  esac
  package=$(adb /system/bin/pm list packages -3 | sed -n 's/^package://p' | head -1)
  [ -n "$package" ] || fail "no installed package"
  launch "$package"
fi

# A live fork: the child restores the GPU's state, and both branches run on,
# without restarting Android's framework.
system_server=$(adb /system/bin/pidof system_server) || fail "system_server is not running"
ttc fork "$NAME@main" child >/dev/null || fail "forking Android"
# Every thread of both branches restarts its interrupted system call at once,
# which takes the branches a few seconds.
deadline=$(($(date +%s) + 180))
until adb_in child /system/bin/true >/dev/null 2>&1 && adb /system/bin/true >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || fail "the branches do not answer after the fork"
  sleep 5
done
for branch in main child; do
  [ "$(adb_in "$branch" /system/bin/pidof system_server)" = "$system_server" ] ||
    fail "Android's framework restarted in $branch after the fork"
  adb_in "$branch" /system/bin/dumpsys SurfaceFlinger 2>&1 | grep -q '^GLES:.*ANGLE' ||
    fail "SurfaceFlinger composes without the GPU in $branch after the fork"
done
launch com.android.settings child
echo "ttc redroid e2e: Android runs on in both branches of a live fork"

echo "ttc redroid e2e: ok"
