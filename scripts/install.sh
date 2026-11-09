#!/bin/sh
# Installs the smrit command line from the latest GitHub release.
#
#   curl -fsSL https://smrit.ai/install.sh | sh
#
# SMRIT_INSTALL_BASE_URL changes where this installer downloads the release
# from. It does not change where the CLI connects: that address is compiled
# into the binary. It exists so the installer can be tested against a local
# server.
set -eu

BASE_URL="${SMRIT_INSTALL_BASE_URL:-https://github.com/smrit-ai/cli/releases/latest/download}"

fail() {
	printf '%s\n' "$1" >&2
	exit 1
}

case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "smrit supports macOS and Linux only." ;;
esac

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "smrit supports 64 bit Intel and ARM processors only." ;;
esac

asset="smrit_${os}_${arch}.tar.gz"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		fail "Neither curl nor wget is installed, so smrit cannot be downloaded."
	fi
}

fetch "$BASE_URL/$asset" "$tmp/$asset" || fail "Could not download $asset from $BASE_URL."
fetch "$BASE_URL/SHA256SUMS" "$tmp/SHA256SUMS" || fail "Could not download SHA256SUMS from $BASE_URL."

# Check only the line for this asset.
line="$(grep "  $asset\$" "$tmp/SHA256SUMS" || true)"
[ -n "$line" ] || fail "SHA256SUMS has no entry for $asset, so the download was not installed."
printf '%s\n' "$line" >"$tmp/check.txt"
if command -v sha256sum >/dev/null 2>&1; then
	verify="sha256sum -c"
elif command -v shasum >/dev/null 2>&1; then
	verify="shasum -a 256 -c"
else
	fail "Neither sha256sum nor shasum is installed, so the download cannot be verified."
fi
(cd "$tmp" && $verify check.txt >/dev/null 2>&1) || fail "The checksum of $asset does not match SHA256SUMS, so nothing was installed."

tar -xzf "$tmp/$asset" -C "$tmp" smrit || fail "Could not unpack $asset."

if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
	dest=/usr/local/bin
else
	dest="$HOME/.local/bin"
	mkdir -p "$dest"
fi

mv "$tmp/smrit" "$dest/smrit"
chmod 755 "$dest/smrit"
printf 'Installed smrit to %s/smrit\n' "$dest"

case ":$PATH:" in
	*":$dest:"*) ;;
	*)
		printf '%s is not on your PATH. Add it with:\n' "$dest"
		printf '  export PATH="%s:$PATH"\n' "$dest"
		;;
esac

"$dest/smrit" --version
