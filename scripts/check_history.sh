#!/bin/sh
# Checks the whole git history before the repository is trusted as public:
#   1. no commit message carries a trailer or a generated-with line;
#   2. no file version ever committed holds anything shaped like a credential,
#      other than the fake values listed in scripts/history_allowlist.txt.
# A report names commit, path and pattern, never the matching text.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
# CHECK_HISTORY_TARGET points the check at another repository; the tests use it.
target="${CHECK_HISTORY_TARGET:-$root}"
allowlist="${CHECK_HISTORY_ALLOWLIST:-$root/scripts/history_allowlist.txt}"
cd "$root"

if [ "$(git -C "$target" rev-parse --is-shallow-repository)" = true ]; then
	echo "check_history: the checkout is shallow; fetch the full history first." >&2
	exit 2
fi

status=0

bad=$(git -C "$target" log --all --format='%B' | grep -iE '^(co-authored-by|signed-off-by|claude-session|generated-with)|generated with|claude\.ai/code' || true)
if [ -n "$bad" ]; then
	echo "check_history: commit messages carry a trailer or a generated-with line:" >&2
	for sha in $(git -C "$target" log --all --format='%H' --grep='co-authored-by' -i --grep='signed-off-by' -i --grep='claude-session' -i --grep='generated-with' -i --grep='generated with' -i --grep='claude.ai/code' -i); do
		echo "  commit $sha" >&2
	done
	status=1
else
	echo "commit messages: no trailers"
fi

go run ./cmd/historyscan "$target" "$allowlist" || status=1

exit $status
