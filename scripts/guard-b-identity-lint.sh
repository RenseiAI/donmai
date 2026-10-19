#!/usr/bin/env bash
# guard-b-identity-lint.sh — feed commit identities into the vendored guard-b engine.
#
# This repo, not vendored from donmai-architecture (see scripts/guard-b-lint.sh's
# own header for what IS vendored) — free to evolve independently of the
# upstream engine.
#
# ── Why this wrapper exists ─────────────────────────────────────────────────
# guard-b-lint.sh scans file contents, commit messages (--commits), and the
# composed squash message (--stdin). It never sees commit AUTHOR or COMMITTER
# names, nor the `Co-authored-by` trailers a squash merge composes into the
# published commit. A commit authored as `Donmai Agent (<tracker id>)` passed
# every check, and a default squash merge would have published the identifier
# on main. Nothing reached main; the author default is fixed elsewhere. This
# wrapper closes the hole by feeding exactly those identity strings through
# the same engine and the same rule table, so a leak in an author name fails
# the same way a leak in a file would.
#
# The engine itself is untouched: the wrapper collects identity text and pipes
# it to `guard-b-lint.sh --stdin` under a label naming the commit (or the
# staged state), one invocation per field so a failure names the commit and field,
# without publishing the identity or the engine's matching text.
#
# Usage: guard-b-identity-lint.sh (--staged | --commits <rev-range>)
#   --staged            scan what the NEXT commit would record: `git var
#                       GIT_AUTHOR_IDENT` and `git var GIT_COMMITTER_IDENT`
#                       (which honour the same GIT_AUTHOR_* / GIT_COMMITTER_*
#                       env overrides git commit reads). Local `make guard`
#                       runs this.
#   --commits <range>   scan every commit in a rev-range: author name, author
#                       email, committer name, committer email, plus the
#                       `Co-authored-by` trailer lines from the body — the
#                       trailers a squash would compose. CI runs this on the
#                       PR range and on what just landed on main.
#
# Exit codes: 0 clean (or nothing to scan), 1 an identity matched a rule,
# 2 usage error OR the rev-range could not be evaluated at all (fails closed:
# an unreadable range must never look like a clean scan).
#
# Run from the repo root being scanned. Only the path to guard-b-lint.sh is
# resolved relative to this script's own location, since that engine is always
# its sibling.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="$SCRIPT_DIR/guard-b-lint.sh"

if [[ $# -lt 1 ]]; then
  echo "usage: $0 (--staged | --commits <rev-range>)" >&2
  exit 2
fi

MODE="$1"
RANGE=""

case "$MODE" in
  --staged)
    [[ $# -eq 1 ]] || { echo "usage: $0 (--staged | --commits <rev-range>)" >&2; exit 2; }
    ;;
  --commits)
    [[ $# -eq 2 ]] || { echo "usage: $0 --commits <rev-range>" >&2; exit 2; }
    RANGE="$2"
    ;;
  *)
    echo "guard-b-identity-lint: unknown mode: $MODE (want --staged or --commits)" >&2
    exit 2
    ;;
esac

# Never forward engine diagnostics: they contain the identity and matched text.
# Keep the vendored rule table in scripts/guard-b-lint.sh as the only policy.
check_stream() {
  local label="$1" rc
  if "$GUARD" --stdin "$label" >/dev/null 2>&1; then
    return 0
  else
    rc=$?
  fi
  if [[ $rc -eq 1 ]]; then
    echo "guard-b-identity-lint: BLOCKING — $label matches a guard-b rule." >&2
    FAIL=1
    return 0
  fi
  echo "guard-b-identity-lint: $label: engine error (exit $rc)." >&2
  exit 2
}

FAIL=0

if [[ "$MODE" == "--staged" ]]; then
  for role in AUTHOR COMMITTER; do
    ident="$(git var "GIT_${role}_IDENT")"
    # git var appends timestamp and timezone after the closing email bracket.
    ident="${ident%>*}"
    name="${ident% <*}"
    email="${ident##* <}"
    # Use redirection rather than a pipe so FAIL persists in this shell.
    check_stream "staged ${role}.name" <<< "$name"
    check_stream "staged ${role}.email" <<< "$email"
  done
  if [[ $FAIL -ne 0 ]]; then
    echo "Fix git user.name/user.email or GIT_AUTHOR_*/GIT_COMMITTER_* before committing." >&2
    exit 1
  fi
  echo "guard-b-identity-lint (staged): OK — staged author/committer identity is clean."
  exit 0
fi

SHAS_TMP="$(mktemp)"
trap 'rm -f "$SHAS_TMP"' EXIT
if ! git rev-list "$RANGE" > "$SHAS_TMP" 2>/dev/null; then
  echo "guard-b-identity-lint: FAILED — could not evaluate rev-range; refusing a clean scan." >&2
  exit 2
fi

COUNT=0
while IFS= read -r sha; do
  [[ -n "$sha" ]] || continue
  COUNT=$((COUNT + 1))
  # Lowercase placeholders read the recorded fields, ignoring mailmap rewrites.
  for spec in 'author.name:%an' 'author.email:%ae' 'committer.name:%cn' 'committer.email:%ce'; do
    field="${spec%%:*}"
    value="$(git log -1 --format="${spec#*:}" "$sha")"
    check_stream "commit $sha $field" <<< "$value"
  done
  body="$(git log -1 --format='%B' "$sha")"
  trailers="$(printf '%s\n' "$body" | grep -i '^co-authored-by:' || true)"
  check_stream "commit $sha Co-authored-by" <<< "$trailers"
done < "$SHAS_TMP"

if [[ $FAIL -ne 0 ]]; then
  exit 1
fi

echo "guard-b-identity-lint: OK — $COUNT commit(s), identities clean."
