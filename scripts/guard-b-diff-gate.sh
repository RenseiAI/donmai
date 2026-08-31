#!/usr/bin/env bash
# guard-b-diff-gate.sh — BLOCKING gate for one guard-b rule, scoped to the
# files a rev-range adds, copies, modifies, or renames.
#
# This repo, not vendored from donmai-architecture (see scripts/guard-b-lint.sh's
# own header for what IS vendored) — free to evolve independently of the
# upstream engine.
#
# ── Why a diff-scoped gate instead of flipping --all to blocking ────────────
# guard-b-lint.sh --all (the tracked-tree scan, .github/workflows/guard-b.yml's
# "guard-b-tree-residue" job) already flags real violations correctly — it is
# report-only only because most guard-b rules carry a large pre-existing
# residue this repo was never curated against, and flipping the whole scan to
# blocking would redden every future PR for content nobody touched (see that
# job's header, and .guard-allowlist's).
#
# A rule with NO such residue doesn't need to wait for that curation project:
# scoping the blocking check to files this change actually ADDS or MODIFIES
# means a path nobody touches cannot newly regress it, while a path that IS
# touched gets scanned in full (whole file content, not just the diff hunk —
# same semantics as guard-b-lint.sh --staged) and any pre-existing hit in that
# file must already be curated in .guard-allowlist or the gate fails, exactly
# like --staged already behaves for a local `make guard` run. This is the
# per-rule version of the remediation path guard-b.yml's header describes:
# curate a rule's residue, then it can go blocking without a repo-wide flip.
#
# This directly catches the shape of incident that motivated it: a directory
# of files added wholesale in one PR/push that nobody reads file-by-file (see
# the PR that adds this gate for the concrete case — a linter cache directory
# containing developer absolute paths, committed because a .gitignore pattern
# had the wrong directory name).
#
# Usage: guard-b-diff-gate.sh <RULE_ID> <rev-range>
#   <RULE_ID>    a rule ID guard-b-lint.sh defines (e.g. DEV_ABS_PATH). The
#                gate fails only on THIS rule's findings — other rules' hits
#                in the same files are left to the existing checks (self-test,
#                --commits, --stdin) and the report-only --all scan.
#   <rev-range>  anything `git diff --name-only <rev-range>` accepts: a
#                two-dot range for a linear push, or a three-dot range
#                (`origin/<base>...HEAD`) to scope a pull request to only the
#                commits it actually adds.
#
# Exit codes: 0 clean (or nothing to scan), 1 the named rule fired on a
# changed file, 2 usage error OR the rev-range could not be evaluated at all
# (fails closed — see below, never reported as a clean scan).
#
# Run from the repo root being scanned (same expectation guard-b-lint.sh's
# own --staged/--all modes have) — every git and path operation below is
# relative to the CALLER's current directory, not to where this script file
# happens to live, so a self-test can point it at a throwaway repo elsewhere
# on disk (scripts/guard-b-diff-gate-selftest.sh does exactly that). Only the
# path to the guard-b-lint.sh engine itself is resolved relative to this
# script's own location, since that engine is always its sibling.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="$SCRIPT_DIR/guard-b-lint.sh"

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <RULE_ID> <rev-range>" >&2
  exit 2
fi
RULE_ID="$1"
RANGE="$2"

# ---- Collect files this range adds, copies, modifies, or renames -----------
# --diff-filter=ACMR excludes Deleted files (nothing to scan there) — same
# filter guard-b-lint.sh's own --staged mode applies to the index.
#
# Fail CLOSED on a git-diff error. An unresolvable range (an unfetched base
# ref, a typo, a repo git can't read) must never look identical to a range
# that legitimately touches nothing — that is precisely the silent-green
# failure this gate exists to prevent (see the header above and the workflow
# comment this script's introduction quotes: "a filtered trigger reports
# 'green' by not running"). git's own stderr is left unredirected so an
# operator sees the real reason inline, right above this gate's own message.
DIFF_OUT="$(mktemp)"
trap 'rm -f "$DIFF_OUT"' EXIT

set +e
git diff --name-only --diff-filter=ACMR "$RANGE" -- . > "$DIFF_OUT"
DIFF_RC=$?
set -e

if [[ $DIFF_RC -ne 0 ]]; then
  echo "guard-b-diff-gate ($RULE_ID): FAILED — could not evaluate rev-range '$RANGE' (git diff exited $DIFF_RC; see git's error above)." >&2
  echo "guard-b-diff-gate: refusing to report a clean scan for a range that could not be read. Common" >&2
  echo "cause: the base ref was never fetched locally (run 'git fetch origin <base-branch>' first) or" >&2
  echo "the range syntax is invalid." >&2
  exit 2
fi

FILES=()
while IFS= read -r f; do
  [[ -n "$f" ]] || continue
  FILES+=("$f")
done < "$DIFF_OUT"

if [[ ${#FILES[@]} -eq 0 ]]; then
  echo "guard-b-diff-gate ($RULE_ID): no added/copied/modified/renamed files in $RANGE — nothing to scan."
  exit 0
fi

# A file the diff names may no longer exist in the working tree (rare in CI,
# where the tree matches HEAD, but cheap to guard against).
EXISTING=()
for f in "${FILES[@]}"; do
  [[ -f "$f" ]] && EXISTING+=("$f")
done

if [[ ${#EXISTING[@]} -eq 0 ]]; then
  echo "guard-b-diff-gate ($RULE_ID): no surviving added/copied/modified/renamed files in $RANGE — nothing to scan."
  exit 0
fi

echo "guard-b-diff-gate ($RULE_ID): scanning ${#EXISTING[@]} changed file(s)."

set +e
OUT="$("$GUARD" "${EXISTING[@]}" 2>&1)"
set -e

# guard-b-lint.sh reports one line per violation, each carrying
# "[rule: <ID> — <description>]" — the same shape its own self-test asserts
# on (grep -q "rule: $want_rule "). Only THIS rule's lines fail this gate;
# any other rule's hit in the same files is not this gate's concern.
HITS="$(printf '%s\n' "$OUT" | grep -E -- "\[rule: ${RULE_ID} " || true)"

if [[ -n "$HITS" ]]; then
  echo ""
  echo "guard-b-diff-gate: BLOCKING — $RULE_ID found in a changed file:"
  echo "------------------------------------------------------------"
  printf '%s\n' "$HITS"
  echo "------------------------------------------------------------"
  echo ""
  echo "Remove the flagged content, or if it is a legitimate, reviewed"
  echo "exception, add a narrow entry to .guard-allowlist (see that file's"
  echo "header for the grammar: <RULE_ID> :: <location-regex> :: <match-regex>)."
  exit 1
fi

echo "guard-b-diff-gate ($RULE_ID): OK — no violations of this rule in changed files."
exit 0
