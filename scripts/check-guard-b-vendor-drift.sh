#!/usr/bin/env bash
# check-guard-b-vendor-drift.sh — proves the vendored guard-b-lint.sh and
# guard-b-lint-selftest.sh still match their pinned donmai-architecture
# commit, byte for byte, past the provenance header this repo prepends.
#
# "Vendor a script into three repos, with a note saying it's kept in sync"
# only avoids drift if something actually checks that claim — a comment is a
# declaration, not an enforcement. This fetches the pinned commit's copy of
# each file straight from GitHub and diffs it against the local copy (header
# stripped). A mismatch means one of two things, and either is worth knowing:
# a hand-edit slipped into the vendored copy, or upstream has moved on and
# this repo's pin has gone stale.
#
# Requires network access (raw.githubusercontent.com) — run this in CI, not
# as a local pre-commit hook.
#
# Usage: scripts/check-guard-b-vendor-drift.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UPSTREAM_REPO="RenseiAI/donmai-architecture"
HEADER_MARKER='# ============================================================================'

# <local-path>|<pinned-commit-sha>|<upstream-path>
#
# Keep this pin in lockstep with the "Pinned commit:" line in each vendored
# file's own header — bumping one without the other is exactly the silent
# drift this script exists to catch, so it deliberately re-derives nothing
# from the vendored file itself and instead re-fetches from the pin below.
PINS=(
  "scripts/guard-b-lint.sh|0e601b3c6b5ff0dcecca6a7512786548726b6d6d|scripts/guard-b-lint.sh"
  "scripts/guard-b-lint-selftest.sh|0e601b3c6b5ff0dcecca6a7512786548726b6d6d|scripts/guard-b-lint-selftest.sh"
)

# strip_header <file> — everything from the first line AFTER the header's
# closing "====" marker (the second occurrence of that exact line) onward,
# with the shebang re-attached so the result compares equal to the upstream
# file (which carries no provenance header and starts with its own shebang).
strip_header() {
  { printf '#!/usr/bin/env bash\n'
    awk -v m="$HEADER_MARKER" 'BEGIN{n=0} $0==m{n++; next} n>=2{print}' "$1"
  }
}

fail=0
for pin in "${PINS[@]}"; do
  IFS='|' read -r local_path sha upstream_path <<< "$pin"
  local_file="$REPO_ROOT/$local_path"

  if [[ ! -f "$local_file" ]]; then
    echo "check-guard-b-vendor-drift: MISSING local file: $local_path" >&2
    fail=1
    continue
  fi

  upstream_url="https://raw.githubusercontent.com/${UPSTREAM_REPO}/${sha}/${upstream_path}"
  upstream_tmp="$(mktemp)"
  local_tmp="$(mktemp)"

  if ! curl -fsSL --max-time 20 "$upstream_url" -o "$upstream_tmp"; then
    echo "check-guard-b-vendor-drift: could not fetch $upstream_url" >&2
    fail=1
    rm -f "$upstream_tmp" "$local_tmp"
    continue
  fi

  strip_header "$local_file" > "$local_tmp"

  if ! diff -q "$local_tmp" "$upstream_tmp" > /dev/null; then
    echo "check-guard-b-vendor-drift: DRIFT in $local_path vs ${UPSTREAM_REPO}@${sha:0:12}" >&2
    echo "  Either this vendored copy was hand-edited (revert it and re-vendor" >&2
    echo "  from upstream), or upstream has moved on since this pin (copy the" >&2
    echo "  new file, then bump the pinned commit in BOTH $local_path's own" >&2
    echo "  header AND the PINS entry in scripts/check-guard-b-vendor-drift.sh)." >&2
    diff -u "$upstream_tmp" "$local_tmp" | head -60 >&2 || true
    fail=1
  fi
  rm -f "$upstream_tmp" "$local_tmp"
done

if [[ $fail -eq 0 ]]; then
  echo "check-guard-b-vendor-drift: OK — vendored guard-b matches its pinned upstream commit."
fi
exit $fail
