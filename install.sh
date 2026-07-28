#!/bin/sh
#
# Install the latest TimeTravelContainer release for the current user.

set -eu

repository=${TTC_GITHUB_REPOSITORY:-gzz2000/TimeTravelOS}
install_dir=${TTC_INSTALL_DIR:-"$HOME/.local/bin"}

case "$(uname -s)" in
Linux) ;;
*)
	echo "ttc installer: Linux is required" >&2
	exit 1
	;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*)
	echo "ttc installer: unsupported architecture: $(uname -m)" >&2
	exit 1
	;;
esac

download() {
	url=$1
	output=$2
	if command -v curl >/dev/null 2>&1; then
		curl -fL --retry 3 --retry-delay 1 -o "$output" "$url"
	elif command -v wget >/dev/null 2>&1; then
		wget -O "$output" "$url"
	else
		echo "ttc installer: install curl or wget first" >&2
		exit 1
	fi
}

tmp=${TMPDIR:-/tmp}/ttc-install.$$
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
mkdir -m 0700 "$tmp"

release_url="https://github.com/$repository/releases/latest/download"
echo "Downloading TimeTravelContainer for linux/$arch"
download "$release_url/ttc-linux-$arch" "$tmp/ttc-linux-$arch"
download "$release_url/ttcd-linux-$arch" "$tmp/ttcd-linux-$arch"
download "$release_url/SHA256SUMS" "$tmp/SHA256SUMS"

(
	cd "$tmp"
	grep -E "  (ttc|ttcd)-linux-$arch\$" SHA256SUMS >SELECTED_SHA256SUMS
	if [ "$(wc -l <SELECTED_SHA256SUMS)" -ne 2 ]; then
		echo "ttc installer: release checksum list is incomplete" >&2
		exit 1
	fi
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum -c SELECTED_SHA256SUMS
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 -c SELECTED_SHA256SUMS
	else
		echo "ttc installer: sha256sum or shasum is required" >&2
		exit 1
	fi
)

mkdir -p "$install_dir"
install -m 0755 "$tmp/ttc-linux-$arch" "$install_dir/ttc.new"
install -m 0755 "$tmp/ttcd-linux-$arch" "$install_dir/ttcd.new"
mv -f "$install_dir/ttc.new" "$install_dir/ttc"
mv -f "$install_dir/ttcd.new" "$install_dir/ttcd"

data_dir=${TTC_DATA_ROOT:-${XDG_DATA_HOME:-"$HOME/.local/share"}/ttc}
if [ -n "${TTC_RUNTIME_DIR:-}" ]; then
	runtime_dir=$TTC_RUNTIME_DIR
elif [ -n "${XDG_RUNTIME_DIR:-}" ]; then
	runtime_dir=$XDG_RUNTIME_DIR/ttc
else
	runtime_dir=${TMPDIR:-/tmp}/ttc-$(id -u)
fi
mkdir -p "$data_dir" "$runtime_dir"
chmod 0700 "$data_dir" "$runtime_dir"

profile=$HOME/.profile
if [ "$install_dir" = "$HOME/.local/bin" ] &&
	! grep -Fq '# TimeTravelContainer PATH' "$profile" 2>/dev/null; then
	{
		echo
		echo '# TimeTravelContainer PATH'
		echo 'case ":$PATH:" in'
		echo '  *":$HOME/.local/bin:"*) ;;'
		echo '  *) export PATH="$HOME/.local/bin:$PATH" ;;'
		echo 'esac'
	} >>"$profile"
	profile_note="Restart your shell or run: export PATH=\"\$HOME/.local/bin:\$PATH\""
else
	profile_note=""
fi

echo "Installed ttc and ttcd in $install_dir."
if [ -n "$profile_note" ]; then
	echo "$profile_note"
elif ! command -v ttc >/dev/null 2>&1; then
	echo "Add $install_dir to PATH before running ttc."
fi
