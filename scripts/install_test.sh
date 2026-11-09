#!/bin/sh
# Runs install.sh against a local file server holding a fake release: once
# good, once with a bad checksum.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
server_pid=""
cleanup() {
	[ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

case "$(uname -s)" in Darwin) os=darwin ;; *) os=linux ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; *) arch=arm64 ;; esac
asset="smrit_${os}_${arch}.tar.gz"

if command -v sha256sum >/dev/null 2>&1; then sum="sha256sum"; else sum="shasum -a 256"; fi

mkdir -p "$work/stage" "$work/good" "$work/bad"
printf '#!/bin/sh\necho "0.0.0-fake"\n' >"$work/stage/smrit"
chmod 755 "$work/stage/smrit"
tar -czf "$work/good/$asset" -C "$work/stage" smrit
(cd "$work/good" && $sum "$asset" >SHA256SUMS)
cp "$work/good/$asset" "$work/bad/$asset"
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" "$asset" >"$work/bad/SHA256SUMS"

port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
(cd "$work" && python3 -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1) &
server_pid=$!
# Wait for the server to accept connections; this is a readiness check, not a timing assertion.
i=0
until python3 -c "import socket,sys; socket.create_connection(('127.0.0.1',$port),1).close()" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -lt 200 ] || { echo "FAIL: file server did not start"; exit 1; }
	sleep 0.1
done

run_install() {
	# $1 = release dir, $2 = a fresh directory used as HOME and, under it, the install dir.
	# SMRIT_INSTALL_DIR keeps the test away from any system directory.
	HOME="$2" SMRIT_INSTALL_DIR="$2/bin" SMRIT_INSTALL_BASE_URL="http://127.0.0.1:$port/$1" sh "$here/install.sh"
}

fail() { echo "FAIL: $1"; exit 1; }

no_dashes() {
	# Printed messages use no dash punctuation: no spaced hyphen, em dash or en dash.
	case "$1" in
		*" - "* | *" -- "* | *"—"* | *"–"*) fail "dash punctuation in output: $1" ;;
	esac
}

expect_refused() {
	# $1 = release dir, $2 = home, $3 = word the message must contain
	if out="$(run_install "$1" "$2" 2>&1)"; then fail "$1 was accepted: $out"; fi
	case "$out" in *"$3"*) ;; *) fail "$1: expected message containing $3, got: $out" ;; esac
	[ ! -e "$2/bin/smrit" ] || fail "$1: binary installed anyway"
	[ -z "$(ls -A "$2/bin" 2>/dev/null)" ] || fail "$1: files left in the install dir"
	no_dashes "$out"
}

# A good release installs, replaces cleanly, and runs.
mkdir -p "$work/home1"
out="$(run_install good "$work/home1" 2>&1)" || fail "good release did not install: $out"
case "$out" in *"0.0.0-fake"*) ;; *) fail "smrit --version did not run: $out" ;; esac
[ -x "$work/home1/bin/smrit" ] || fail "no binary in the install dir"
no_dashes "$out"
out="$(run_install good "$work/home1" 2>&1)" || fail "reinstall over an existing smrit failed: $out"
[ -z "$(ls -A "$work/home1/bin" | grep -v '^smrit$' || true)" ] || fail "temp files left behind"

# A bad checksum stops and installs nothing.
mkdir -p "$work/home2"; expect_refused bad "$work/home2" "checksum"

# A SHA256SUMS without a line for this asset, and one that only partly matches the name.
mkdir -p "$work/missing" "$work/home3"
cp "$work/good/$asset" "$work/missing/$asset"
printf '%s  other_%s\n' "$(cut -d' ' -f1 "$work/good/SHA256SUMS")" "$asset" >"$work/missing/SHA256SUMS"
expect_refused missing "$work/home3" "SHA256SUMS"

# Two lines for one asset is ambiguous.
mkdir -p "$work/dup" "$work/home4"
cp "$work/good/$asset" "$work/dup/$asset"
cat "$work/good/SHA256SUMS" "$work/good/SHA256SUMS" >"$work/dup/SHA256SUMS"
expect_refused dup "$work/home4" "SHA256SUMS"

# A failed download (404).
mkdir -p "$work/home5"; expect_refused nonexistent "$work/home5" "download"

# The wget fallback. PATH holds the tools the installer needs and a fake wget
# but no curl. The fake records its arguments and copies the file from the
# good release, so nothing reaches the network; FAKE_WGET_HELP is what its
# --help prints (BusyBox wget has no --https-only).
mkdir -p "$work/nocurl"
for tool in uname mktemp rm awk wc tr sha256sum shasum tar mkdir cp chmod mv grep cat basename; do
	p="$(command -v "$tool" 2>/dev/null)" || continue
	ln -s "$p" "$work/nocurl/$tool"
done
cat >"$work/nocurl/wget" <<'FAKE'
#!/bin/sh
case "$1" in --help) printf '%s\n' "$FAKE_WGET_HELP" >&2; exit 1 ;; esac
printf '%s\n' "$*" >>"$FAKE_WGET_LOG"
out=""; url=""
while [ $# -gt 0 ]; do
	case "$1" in -O) out="$2"; shift ;; -*) ;; *) url="$1" ;; esac
	shift
done
cp "$FAKE_WGET_FROM/$(basename "$url")" "$out"
FAKE
chmod 755 "$work/nocurl/wget"

wget_install() {
	# $1 = home, $2 = what wget --help prints, $3 = SMRIT_INSTALL_BASE_URL or empty
	: >"$1.log"
	if [ -n "$3" ]; then
		PATH="$work/nocurl" HOME="$1" SMRIT_INSTALL_DIR="$1/bin" SMRIT_INSTALL_BASE_URL="$3" \
			FAKE_WGET_HELP="$2" FAKE_WGET_LOG="$1.log" FAKE_WGET_FROM="$work/good" /bin/sh "$here/install.sh"
	else
		PATH="$work/nocurl" HOME="$1" SMRIT_INSTALL_DIR="$1/bin" \
			FAKE_WGET_HELP="$2" FAKE_WGET_LOG="$1.log" FAKE_WGET_FROM="$work/good" /bin/sh "$here/install.sh"
	fi
}

gnu_help="  --https-only                     only follow secure HTTPS links"
busybox_help="Usage: wget [-cqS] [-O FILE] URL"

# A wget that knows --https-only gets it on both downloads.
mkdir -p "$work/home6"
out="$(wget_install "$work/home6" "$gnu_help" "" 2>&1)" || fail "wget install failed: $out"
[ -x "$work/home6/bin/smrit" ] || fail "wget install left no binary"
[ "$(grep -c -- '--https-only' "$work/home6.log")" = 2 ] || fail "wget was not given --https-only: $(cat "$work/home6.log")"

# BusyBox wget does not know it, so it is not passed.
mkdir -p "$work/home7"
out="$(wget_install "$work/home7" "$busybox_help" "" 2>&1)" || fail "BusyBox wget install failed: $out"
[ -x "$work/home7/bin/smrit" ] || fail "BusyBox wget install left no binary"
[ "$(wc -l <"$work/home7.log" | tr -d ' ')" = 2 ] || fail "wget did not download twice: $(cat "$work/home7.log")"
! grep -q -- '--https-only' "$work/home7.log" || fail "BusyBox wget was given --https-only"

# With the base URL overridden for tests, the https only rule is off for wget too.
mkdir -p "$work/home8"
out="$(wget_install "$work/home8" "$gnu_help" "http://127.0.0.1:$port/good" 2>&1)" || fail "wget install with a base URL failed: $out"
[ "$(wc -l <"$work/home8.log" | tr -d ' ')" = 2 ] || fail "wget did not download twice: $(cat "$work/home8.log")"
! grep -q -- '--https-only' "$work/home8.log" || fail "wget was given --https-only with the base URL overridden"

# A hostile VERSION is refused before anything runs.
marker="$work/pwned"
if (cd "$here/.." && make release VERSION="1.0.0\"\$(touch $marker)\"" >/dev/null 2>&1); then fail "hostile VERSION accepted"; fi
if (cd "$here/.." && make release VERSION='1.0.0";touch '"$marker"';"' >/dev/null 2>&1); then fail "hostile VERSION accepted"; fi
[ ! -e "$marker" ] || fail "hostile VERSION executed"
if (cd "$here/.." && make release VERSION= >/dev/null 2>&1); then fail "empty VERSION accepted"; fi

echo "install tests passed"
