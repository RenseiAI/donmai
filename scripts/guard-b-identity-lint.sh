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
# staged state), one invocation per commit so a failure points at the commit
# that carries it.
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

# check_stream <label> — scan stdin under <label>; 0 clean, 1 violation.
check_stream() {
  local label="$1" out rc
  set +e
  out="$(printf '%s' "$(cat)" | "$GUARD" --stdin "$label" 2>&1)"
  rc=$?
  set -e
  if [[ $rc -eq 1 ]]; then
    printf '%s\n' "$out"
    return 1
  fi
  if [[ $rc -ne 0 ]]; then
    printf '%s\n' "$out" >&2
    echo "guard-b-identity-lint ($label): engine error (exit $rc)." >&2
    return 2
  fi
  return 0
}

FAIL=0

if [[ "$MODE" == "--staged" ]]; then
  STAGED_TMP="$(mktemp)"
  trap 'rm -f "$STAGED_TMP"' EXIT
  {
    git var GIT_AUTHOR_IDENT
    git var GIT_COMMITTER_IDENT
  } > "$STAGED_TMP"
  set +e
  check_stream "staged-identity" < "$STAGED_TMP"
  rc=$?
  set -e
  if [[ $rc -eq 1 ]]; then
    echo "guard-b-identity-lint: BLOCKING — the staged author/committer identity matches a guard-b rule." >&2
    echo "Fix the identity (git config user.name/user.email, or the GIT_AUTHOR_*/GIT_COMMITTER_* env) before committing." >&2
    exit 1
  fi
  [[ $rc -eq 0 ]] || exit 2
  echo "guard-b-identity-lint (staged): OK — staged author/committer identity is clean."
  exit 0
fi

# ---- --commits: one --stdin scan per commit, labelled by short sha ---------
SHAS_TMP="$(mktemp)"
trap 'rm -f "$SHAS_TMP"' EXIT

set +e
git rev-list "$RANGE" > "$SHAS_TMP" 2>/tmp/guard-b-identity-revlist.err
REV_RC=$?
set -e

if [[ $REV_RC -ne 0 ]]; then
  cat /tmp/guard-b-identity-revlist.err >&2 || true
  echo "guard-b-identity-lint: FAILED — could not evaluate rev-range '$RANGE' (git rev-list exited $REV_RC)." >&2
  echo "guard-b-identity-lint: refusing to report a clean scan for a range that could not be read." >&2
  exit 2
fi

SHAS=()
while IFS= read -r sha; do
  [[ -n "$sha" ]] || continue
  SHAS+=("$sha")
done < "$SHAS_TMP"

if [[ ${#SHAS[@]} -eq 0 ]]; then
  echo "guard-b-identity-lint: no commits in $RANGE — nothing to scan."
  exit 0
fi

for sha in "${SHAS[@]}"; do
  short="${sha:0:12}"
  IDENT_TMP="$(mktemp)"
  {
    git log -1 --format='%an%n%ae%n%cn%n%ce' "$sha"
    git log -1 --format='%B' "$sha" | grep -i '^co-authored-by:' || true
  } > "$IDENT_TMP"
  set +e
  check_stream "commit-identity:${short}" < "$IDENT_TMP"
  rc=$?
  set -e
  rm -f "$IDENT_TMP"
  if [[ $rc -eq 1 ]]; then
    echo "guard-b-identity-lint: BLOCKING — commit $short carries an author, committer, or Co-authored-by identity matching a guard-b rule." >&2
    FAIL=1
  elif [[ $rc -ne 0 ]]; then
    exit 2
  fi
done

if [[ $FAIL -ne 0 ]]; then
  echo "guard-b-identity-lint: BLOCKING — identity violations found in $RANGE." >&2
  exit 1
fi

echo "guard-b-identity-lint: OK — ${#SHAS[@]} commit(s) in $RANGE, identities clean."
exit 0
