#!/bin/sh
# Installs the smrit command line from the latest GitHub release.
#
#   curl -fsSL https://smrit.ai/install.sh | sh
#
# The checksum guards against a corrupted or truncated download. It does not
# protect against a compromised release, because SHA256SUMS comes from the
# same place as the binary.
#
# SMRIT_INSTALL_BASE_URL changes where this installer downloads the release
# from. It does not change where the CLI connects: that address is compiled
# into the binary. It exists so the installer can be tested against a local
# server, and it turns off the https only rule for downloads.
#
# SMRIT_INSTALL_DIR is a directory to install into instead of the default
# (/usr/local/bin when writable without sudo, else $HOME/.local/bin).
#
# Everything is inside main, called on the last line, so a download cut short
# runs nothing.
set -eu

main() {
	BASE_URL="${SMRIT_INSTALL_BASE_URL:-https://github.com/smrit-ai/cli/releases/latest/download}"

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
	staged=""
	trap 'rm -rf "$tmp"; [ -z "$staged" ] || rm -f "$staged"' EXIT INT TERM

	fetch "$BASE_URL/$asset" "$tmp/$asset" || fail "Could not download $asset from $BASE_URL."
	fetch "$BASE_URL/SHA256SUMS" "$tmp/SHA256SUMS" || fail "Could not download SHA256SUMS from $BASE_URL."

	# Exactly one line whose second field is this asset, and only that line is checked.
	awk -v a="$asset" '$2 == a' "$tmp/SHA256SUMS" >"$tmp/check.txt"
	lines="$(wc -l <"$tmp/check.txt" | tr -d ' ')"
	[ "$lines" = 1 ] || fail "SHA256SUMS must have exactly one entry for $asset, so nothing was installed."
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$tmp" && sha256sum -c check.txt >/dev/null 2>&1) || bad_checksum
	elif command -v shasum >/dev/null 2>&1; then
		(cd "$tmp" && shasum -a 256 -c check.txt >/dev/null 2>&1) || bad_checksum
	else
		fail "Neither sha256sum nor shasum is installed, so the download cannot be verified."
	fi

	tar -xzf "$tmp/$asset" -C "$tmp" smrit || fail "Could not unpack $asset."

	if [ -n "${SMRIT_INSTALL_DIR:-}" ]; then
		dest="$SMRIT_INSTALL_DIR"
	elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		dest=/usr/local/bin
	else
		[ -n "${HOME:-}" ] || fail "HOME is not set, so there is nowhere to install smrit."
		dest="$HOME/.local/bin"
	fi
	mkdir -p "$dest" || fail "Could not create $dest."

	# Stage beside the target, then rename: same filesystem, so the swap is atomic.
	staged="$dest/.smrit.new.$$"
	cp "$tmp/smrit" "$staged" || fail "Could not write to $dest."
	chmod 755 "$staged" || fail "Could not make smrit executable."
	mv -f "$staged" "$dest/smrit" || fail "Could not install smrit into $dest."
	staged=""
	printf 'Installed smrit to %s/smrit\n' "$dest"

	case ":$PATH:" in
		*":$dest:"*) ;;
		*)
			printf '%s is not on your PATH. Add it with:\n' "$dest"
			printf '  export PATH="%s:$PATH"\n' "$dest"
			;;
	esac

	"$dest/smrit" --version
}

fail() {
	printf '%s\n' "$1" >&2
	exit 1
}

bad_checksum() {
	fail "The checksum of $asset does not match SHA256SUMS, so nothing was installed."
}

# fetch URL FILE: https only with TLS 1.2 or newer, unless the base URL is overridden for tests.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		if [ -n "${SMRIT_INSTALL_BASE_URL:-}" ]; then
			curl -fsSL -o "$2" "$1"
		else
			curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"
		fi
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		fail "Neither curl nor wget is installed, so smrit cannot be downloaded."
	fi
}

main "$@"
