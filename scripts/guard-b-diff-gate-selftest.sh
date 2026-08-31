#!/usr/bin/env bash
# guard-b-diff-gate-selftest.sh — prove guard-b-diff-gate.sh actually fires.
#
# This repo, not vendored — see scripts/guard-b-diff-gate.sh's own header.
#
# Mirrors scripts/guard-b-lint-selftest.sh's own reasoning: a gate nobody has
# watched fail is not evidence of anything. This builds a throwaway git repo,
# proves the gate goes RED when a rev-range adds a file containing a developer
# absolute path, proves it goes GREEN once that path is removed, and proves a
# PRE-EXISTING violation in a file the range does NOT touch is correctly left
# alone — the whole point of scoping to the diff instead of the tracked tree.
#
# Usage: scripts/guard-b-diff-gate-selftest.sh
# Exit 0 = every case behaved as specified; exit 1 otherwise.

set -eo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATE="$REPO_ROOT/scripts/guard-b-diff-gate.sh"

# Isolate every git command this self-test runs from the invoking user's real
# global/system git config (signing, tag annotation, hooks, etc.) — the
# throwaway repos below must behave identically in CI and on a developer
# machine with an opinionated ~/.gitconfig.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0

fail() {
  echo "diff-gate self-test FAIL: $1" >&2
  FAIL=$((FAIL + 1))
}

# The literal string "/Users/<name>/" must not appear in this file's own
# source, or guard-b would flag this selftest the same way it flags the
# fixtures it builds. Assembled at runtime, same convention
# guard-b-lint-selftest.sh uses for its own banned-literal fragments.
UD='U'
DEVPATH="/${UD}sers/someone/Developer/org/repo/notes.txt"

# init_repo — a fresh throwaway git repo the gate can run rev-ranges against.
init_repo() {
  local d="$1"
  mkdir -p "$d"
  (cd "$d" && git init -q .) >/dev/null 2>&1
}

commit_all() {
  local d="$1" msg="$2"
  (cd "$d" && git add -A && git commit -q -m "$msg") >/dev/null 2>&1
}

# ---- Case 1: a rev-range that ADDS a dev-abs-path file must FAIL -----------
check_red() {
  local d="$TMP/red-repo" out rc
  init_repo "$d"
  echo "clean base content" > "$d/base.txt"
  commit_all "$d" "base"
  (cd "$d" && git tag base)

  printf 'cloned from %s\n' "$DEVPATH" > "$d/leaked-cache-blob.txt"
  commit_all "$d" "add cache blob"

  set +e
  out="$(cd "$d" && "$GATE" DEV_ABS_PATH base..HEAD 2>&1)"
  rc=$?
  set -e

  echo "--- RED case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"

  if [[ $rc -ne 1 ]]; then
    fail "adding a dev-abs-path file did not fail the gate (rc=$rc)"
    return
  fi
  if ! printf '%s\n' "$out" | grep -q '\[rule: DEV_ABS_PATH '; then
    fail "gate failed, but not on DEV_ABS_PATH"
    return
  fi
  PASS=$((PASS + 1))
}
check_red

# ---- Case 2: removing the dev-abs-path content must PASS -------------------
check_green() {
  local d="$TMP/green-repo" out rc
  init_repo "$d"
  echo "clean base content" > "$d/base.txt"
  commit_all "$d" "base"
  (cd "$d" && git tag base)

  printf 'cloned from %s\n' "$DEVPATH" > "$d/leaked-cache-blob.txt"
  commit_all "$d" "add cache blob"

  # Remove the leaked path from the very file the previous commit added.
  echo "no developer paths here" > "$d/leaked-cache-blob.txt"
  commit_all "$d" "scrub the leaked path"

  set +e
  out="$(cd "$d" && "$GATE" DEV_ABS_PATH base..HEAD 2>&1)"
  rc=$?
  set -e

  echo "--- GREEN case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"

  if [[ $rc -ne 0 ]]; then
    fail "scrubbing the dev-abs-path content did not pass the gate (rc=$rc)"
    return
  fi
  PASS=$((PASS + 1))
}
check_green

# ---- Case 3: a pre-existing violation OUTSIDE the diff must not fire -------
# This is the whole point of diff-scoping rather than flipping --all: a path
# nobody touches in this range cannot newly regress the gate.
check_preexisting_untouched_is_clean() {
  local d="$TMP/preexisting-repo" out rc
  init_repo "$d"
  printf 'legacy fixture: %s\n' "$DEVPATH" > "$d/legacy-fixture.txt"
  echo "clean base content" > "$d/base.txt"
  commit_all "$d" "base (already carries a pre-existing dev-abs-path fixture)"
  (cd "$d" && git tag base)

  # This range only touches base.txt — legacy-fixture.txt is untouched.
  echo "an unrelated, harmless change" >> "$d/base.txt"
  commit_all "$d" "unrelated change"

  set +e
  out="$(cd "$d" && "$GATE" DEV_ABS_PATH base..HEAD 2>&1)"
  rc=$?
  set -e

  echo "--- pre-existing/untouched case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"

  if [[ $rc -ne 0 ]]; then
    fail "a pre-existing violation in an UNTOUCHED file failed the gate (rc=$rc)"
    return
  fi
  PASS=$((PASS + 1))
}
check_preexisting_untouched_is_clean

# ---- Case 4: touching the SAME pre-existing violation must fire ------------
# Modifying a file that already carries the violation brings it into scope,
# same as guard-b-lint.sh --staged scanning whole staged files today.
check_touching_preexisting_fires() {
  local d="$TMP/touch-repo" out rc
  init_repo "$d"
  printf 'legacy fixture: %s\nsecond line\n' "$DEVPATH" > "$d/legacy-fixture.txt"
  commit_all "$d" "base (already carries a pre-existing dev-abs-path fixture)"
  (cd "$d" && git tag base)

  printf 'legacy fixture: %s\nsecond line, edited\n' "$DEVPATH" > "$d/legacy-fixture.txt"
  commit_all "$d" "touch the file that already carries the violation"

  set +e
  out="$(cd "$d" && "$GATE" DEV_ABS_PATH base..HEAD 2>&1)"
  rc=$?
  set -e

  echo "--- touched-pre-existing case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"

  if [[ $rc -ne 1 ]]; then
    fail "modifying a file that already carries the violation did not fail the gate (rc=$rc)"
    return
  fi
  PASS=$((PASS + 1))
}
check_touching_preexisting_fires

# ---- Report -----------------------------------------------------------------
echo ""
if [[ $FAIL -eq 0 ]]; then
  echo "guard-b-diff-gate self-test: OK — $PASS/4 checks behaved as specified."
  exit 0
fi
echo "guard-b-diff-gate self-test: FAILED — $FAIL failing, $PASS passing (of 4)." >&2
exit 1
