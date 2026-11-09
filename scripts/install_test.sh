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

# install.sh uses /usr/local/bin when it is writable; never clobber a real install there.
if [ -w /usr/local/bin ] && [ -e /usr/local/bin/smrit ]; then
	echo "Refusing to run: /usr/local/bin is writable and already holds a smrit."
	exit 1
fi

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
	# $1 = release dir, $2 = home dir. PATH excludes /usr/local/bin writability concerns only
	# when it is not writable; the test accepts either destination.
	HOME="$2" SMRIT_INSTALL_BASE_URL="http://127.0.0.1:$port/$1" sh "$here/install.sh"
}

fail() { echo "FAIL: $1"; exit 1; }

# A good release installs and runs.
mkdir -p "$work/home1"
out="$(run_install good "$work/home1" 2>&1)" || fail "good release did not install: $out"
case "$out" in *"0.0.0-fake"*) ;; *) fail "smrit --version did not run: $out" ;; esac
if [ -x "$work/home1/.local/bin/smrit" ]; then
	:
elif [ -x /usr/local/bin/smrit ] && [ "$(/usr/local/bin/smrit --version 2>/dev/null)" = "0.0.0-fake" ]; then
	echo "NOTE: /usr/local/bin was writable; removing the fake from it"
	rm -f /usr/local/bin/smrit
else
	fail "no binary was installed"
fi
case "$out" in *" - "* | *"—"* | *"–"*) fail "installer output has dash punctuation: $out" ;; esac

# A bad checksum stops and installs nothing.
mkdir -p "$work/home2"
if out="$(run_install bad "$work/home2" 2>&1)"; then fail "bad checksum was accepted: $out"; fi
case "$out" in *"checksum"*) ;; *) fail "no checksum message: $out" ;; esac
[ ! -e "$work/home2/.local/bin/smrit" ] || fail "binary installed despite bad checksum"
if [ -w /usr/local/bin ] 2>/dev/null; then
	[ "$(/usr/local/bin/smrit --version 2>/dev/null || true)" != "0.0.0-fake" ] || fail "binary installed to /usr/local/bin despite bad checksum"
fi

echo "install tests passed"
