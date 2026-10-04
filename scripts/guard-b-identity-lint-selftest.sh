#!/usr/bin/env bash
# guard-b-identity-lint-selftest.sh — prove guard-b-identity-lint.sh fires.
#
# This repo, not vendored — see scripts/guard-b-identity-lint.sh's own header.
#
# Mirrors scripts/guard-b-lint-selftest.sh's own reasoning: a gate nobody has
# watched fail is not evidence of anything. This builds throwaway git repos
# and proves the identity wrapper goes RED when a commit author, committer,
# or Co-authored-by trailer carries a tracker identifier, and GREEN once the
# identity is clean. It also proves --staged reads the identity the next
# commit would record, and that an unreadable rev-range fails closed.
#
# The banned literals are assembled at runtime from fragments, the same
# convention guard-b-lint-selftest.sh uses: this file is itself scanned, so a
# literal tracker identifier in its source would be a real violation.
#
# Usage: scripts/guard-b-identity-lint-selftest.sh
# Exit 0 = every case behaved as specified; exit 1 otherwise.

set -eo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WRAPPER="$REPO_ROOT/scripts/guard-b-identity-lint.sh"

# Isolate every git command from the invoking user's real git config,
# and from ambient identity: CI runners have no user.name/user.email and
# no GIT_AUTHOR_*/GIT_COMMITTER_* env, so every throwaway `git commit`
# below must not depend on ambient config (else git exits 128 and `set -e`
# aborts the step with no output). Mirrors guard-b-lint-selftest.sh.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t
export GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t
export GIT_COMMITTER_EMAIL=t@example.invalid

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0

fail() {
  echo "identity-lint self-test FAIL: $1" >&2
  FAIL=$((FAIL + 1))
}

# Fragments: joined only at runtime, never a banned literal in source.
U='R'
L='r'

init_repo() {
  local d="$1"
  mkdir -p "$d"
  (cd "$d" && git -c init.defaultBranch=main init -q .) >/dev/null 2>&1
}

commit_with_ident() {
  local d="$1" aname="$2" aemail="$3" cname="$4" cemail="$5" msg="$6" body="$7"
  (cd "$d" \
    && GIT_AUTHOR_NAME="$aname" GIT_AUTHOR_EMAIL="$aemail" \
       GIT_COMMITTER_NAME="$cname" GIT_COMMITTER_EMAIL="$cemail" \
       git commit -q --allow-empty -m "$msg" -m "$body") >/dev/null 2>&1
}

# ---- Case 1: a tracker ID in the author name must FAIL ----------------------
check_author_red() {
  local d="$TMP/author-repo" out rc
  local bad="Agent (${U}EN-9990)"
  init_repo "$d"
  (cd "$d" && git commit -q --allow-empty -m base) >/dev/null 2>&1
  (cd "$d" && git tag base) >/dev/null 2>&1
  commit_with_ident "$d" "$bad" "agent@example.invalid" \
    "Clean Name" "clean@example.invalid" "work" ""
  set +e
  out="$(cd "$d" && "$WRAPPER" --commits base..HEAD 2>&1)"
  rc=$?
  set -e
  echo "--- author RED case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"
  if [[ $rc -ne 1 ]]; then
    fail "tracker ID in author name did not fail the wrapper (rc=$rc)"
    return
  fi
  if ! printf '%s\n' "$out" | grep -q "rule: TRACKER_ID "; then
    fail "wrapper failed, but not on TRACKER_ID for the author name"
    return
  fi
  PASS=$((PASS + 1))
}
check_author_red

# ---- Case 2: a tracker ID in a Co-authored-by trailer must FAIL -------------
check_coauthor_red() {
  local d="$TMP/coauthor-repo" out rc
  local trailer="Co-authored-by: Agent (${U}EN-9990) <agent@example.invalid>"
  init_repo "$d"
  (cd "$d" && git commit -q --allow-empty -m base) >/dev/null 2>&1
  (cd "$d" && git tag base) >/dev/null 2>&1
  commit_with_ident "$d" "Clean Name" "clean@example.invalid" \
    "Clean Name" "clean@example.invalid" "work" "$trailer"
  set +e
  out="$(cd "$d" && "$WRAPPER" --commits base..HEAD 2>&1)"
  rc=$?
  set -e
  echo "--- co-author RED case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"
  if [[ $rc -ne 1 ]]; then
    fail "tracker ID in Co-authored-by trailer did not fail the wrapper (rc=$rc)"
    return
  fi
  if ! printf '%s\n' "$out" | grep -q "rule: TRACKER_ID "; then
    fail "wrapper failed, but not on TRACKER_ID for the trailer"
    return
  fi
  PASS=$((PASS + 1))
}
check_coauthor_red

# ---- Case 3: a tracker slug in the committer email must FAIL ----------------
check_committer_red() {
  local d="$TMP/committer-repo" out rc
  local slug="agent/${L}en-9990-wire"
  init_repo "$d"
  (cd "$d" && git commit -q --allow-empty -m base) >/dev/null 2>&1
  (cd "$d" && git tag base) >/dev/null 2>&1
  commit_with_ident "$d" "Clean Name" "clean@example.invalid" \
    "Clean Name" "${slug}@example.invalid" "work" ""
  set +e
  out="$(cd "$d" && "$WRAPPER" --commits base..HEAD 2>&1)"
  rc=$?
  set -e
  echo "--- committer RED case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"
  if [[ $rc -ne 1 ]]; then
    fail "tracker slug in committer email did not fail the wrapper (rc=$rc)"
    return
  fi
  if ! printf '%s\n' "$out" | grep -q "rule: TRACKER_ID_SLUG "; then
    fail "wrapper failed, but not on TRACKER_ID_SLUG for the committer email"
    return
  fi
  PASS=$((PASS + 1))
}
check_committer_red

# ---- Case 4: clean identities must PASS -------------------------------------
check_green() {
  local d="$TMP/green-repo" out rc
  init_repo "$d"
  (cd "$d" && git commit -q --allow-empty -m base) >/dev/null 2>&1
  (cd "$d" && git tag base) >/dev/null 2>&1
  commit_with_ident "$d" "Clean Name" "clean@example.invalid" \
    "Clean Name" "clean@example.invalid" "work" \
    "Co-authored-by: Clean Name <clean@example.invalid>"
  set +e
  out="$(cd "$d" && "$WRAPPER" --commits base..HEAD 2>&1)"
  rc=$?
  set -e
  echo "--- GREEN case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"
  if [[ $rc -ne 0 ]]; then
    fail "clean identities did not pass the wrapper (rc=$rc)"
    return
  fi
  PASS=$((PASS + 1))
}
check_green

# ---- Case 5: removing the wrapper must turn the RED case GREEN --------------
# The behavioural proof the seat requires: the same bad author that fails
# above passes through the bare engine untouched, so the test pins the
# wrapper rather than a rule that would fire anyway.
check_revert_proof() {
  local d="$TMP/revert-repo" out rc
  local bad="Agent (${U}EN-9990)"
  init_repo "$d"
  (cd "$d" && git commit -q --allow-empty -m base) >/dev/null 2>&1
  (cd "$d" && git tag base) >/dev/null 2>&1
  commit_with_ident "$d" "$bad" "agent@example.invalid" \
    "Clean Name" "clean@example.invalid" "work" ""
  set +e
  out="$(cd "$d" && "$REPO_ROOT/scripts/guard-b-lint.sh" --commits base..HEAD 2>&1)"
  rc=$?
  set -e
  echo "--- revert-proof case (bare engine, bad author): literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"
  if [[ $rc -ne 0 ]]; then
    fail "bare engine flagged the bad author without the wrapper (rc=$rc) — proof is void"
    return
  fi
  PASS=$((PASS + 1))
}
check_revert_proof

# ---- Case 6: --staged must read the identity the next commit would record ---
check_staged_red() {
  local d="$TMP/staged-repo" out rc
  local bad="Agent (${U}EN-9990)"
  init_repo "$d"
  (cd "$d" && git commit -q --allow-empty -m base) >/dev/null 2>&1
  set +e
  out="$(cd "$d" && GIT_AUTHOR_NAME="$bad" GIT_AUTHOR_EMAIL="agent@example.invalid" \
    GIT_COMMITTER_NAME="Clean Name" GIT_COMMITTER_EMAIL="clean@example.invalid" \
    "$WRAPPER" --staged 2>&1)"
  rc=$?
  set -e
  echo "--- staged RED case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"
  if [[ $rc -ne 1 ]]; then
    fail "--staged with a leaking author ident did not fail (rc=$rc)"
    return
  fi
  PASS=$((PASS + 1))
}
check_staged_red

# ---- Case 7: an unreadable range must fail closed ---------------------------
check_bad_range() {
  local d="$TMP/badrange-repo" out rc
  init_repo "$d"
  (cd "$d" && git commit -q --allow-empty -m base) >/dev/null 2>&1
  set +e
  out="$(cd "$d" && "$WRAPPER" --commits "no-such-ref..HEAD" 2>&1)"
  rc=$?
  set -e
  echo "--- bad-range case: literal output ---"
  printf '%s\n' "$out"
  echo "--- exit code: $rc ---"
  if [[ $rc -ne 2 ]]; then
    fail "unreadable rev-range did not fail closed with exit 2 (rc=$rc)"
    return
  fi
  PASS=$((PASS + 1))
}
check_bad_range

# ---- Report -----------------------------------------------------------------
N=7
echo ""
if [[ $FAIL -eq 0 ]]; then
  echo "guard-b identity-lint self-test: OK — $PASS/$N checks behaved as specified."
  exit 0
fi
echo "guard-b identity-lint self-test: FAILED — $FAIL failing, $PASS passing (of $N)." >&2
exit 1
