#!/usr/bin/env bash
# scripts/create-worktree.test.sh — exercises scripts/create-worktree.sh and
# .githooks/pre-commit against a throwaway sandbox repo (never the real repo).
#
# Not wired into `make test`/`make lint`/CI: this repo's gates are Go
# (go test/golangci-lint), and this suite drives real `git`/subshell commits
# against a scratch bare-origin + clone, which doesn't fit that runner. Run
# by hand after touching scripts/create-worktree.sh,
# scripts/install-git-hooks.sh, or .githooks/pre-commit — or via `make hooks-test`:
#
#   bash scripts/create-worktree.test.sh
#
# Covers the fatal flaws the 2026-08-09 hardening pass closed:
#   - exact-match reuse (the "auth" vs "auth-fix" substring-grep bug)
#   - collision refusal (directory git doesn't know about; branch with no
#     worktree) — never force-removed/force-deleted
#   - metadata-only-stale auto-cleanup ONLY when the branch is fully merged;
#     refusal when it isn't
#   - fail-closed stdout discipline: every failure path is empty on stdout
#   - the pre-commit main-commit guard: blocked in the primary checkout on
#     main, allowed with the escape hatch, allowed on a worktree branch
#
# Exits 0 if every case passes, 1 (with a summary of failures) otherwise.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

FAILURES=0
PASS_COUNT=0
note() { printf '%s\n' "$*"; }
pass() { PASS_COUNT=$((PASS_COUNT + 1)); note "  PASS: $*"; }
fail() { FAILURES=$((FAILURES + 1)); note "  FAIL: $*"; }

SANDBOX="$(mktemp -d "${TMPDIR:-/tmp}/create-worktree-test.XXXXXX")"
cleanup() { rm -rf "$SANDBOX"; }
trap cleanup EXIT

note "sandbox: $SANDBOX"

# --- Build a throwaway origin + clone, seeded with the scripts under test --
git init -q --initial-branch=main "$SANDBOX/origin"
git -C "$SANDBOX/origin" config receive.denyCurrentBranch updateInstead
(
  cd "$SANDBOX/origin" || exit 1
  echo hello > file.txt
  git add -A
  git -c user.email=t@t.com -c user.name=t commit -q -m "initial"
)

git clone -q "$SANDBOX/origin" "$SANDBOX/repo"
mkdir -p "$SANDBOX/repo/scripts" "$SANDBOX/repo/.githooks"
cp "$REPO_ROOT/scripts/create-worktree.sh" "$SANDBOX/repo/scripts/create-worktree.sh"
cp "$REPO_ROOT/scripts/install-git-hooks.sh" "$SANDBOX/repo/scripts/install-git-hooks.sh"
cp "$REPO_ROOT/.githooks/pre-commit" "$SANDBOX/repo/.githooks/pre-commit"
chmod +x "$SANDBOX/repo/scripts/create-worktree.sh" "$SANDBOX/repo/scripts/install-git-hooks.sh" "$SANDBOX/repo/.githooks/pre-commit"
(
  cd "$SANDBOX/repo" || exit 1
  git add -A
  git -c user.email=t@t.com -c user.name=t commit -q -m "add worktree scripts"
  git push -q origin main
  git remote set-head origin -a
)

cd "$SANDBOX/repo" || exit 1

# --- 1. Exact-match reuse: "auth" must never reuse "auth-fix" -------------
note "1. exact-match reuse (auth vs auth-fix)"
OUT_FIX="$(./scripts/create-worktree.sh auth-fix 2>/dev/null)"
if [ -n "$OUT_FIX" ] && [ -d "$OUT_FIX" ]; then pass "created auth-fix"; else fail "auth-fix creation ($OUT_FIX)"; fi

OUT_AUTH="$(./scripts/create-worktree.sh auth 2>/dev/null)"
if [ -n "$OUT_AUTH" ] && [ -d "$OUT_AUTH" ] && [ "$OUT_AUTH" != "$OUT_FIX" ]; then
  pass "auth got its own path, distinct from auth-fix"
else
  fail "auth returned [$OUT_AUTH], expected a path distinct from auth-fix ($OUT_FIX)"
fi

OUT_FIX_AGAIN="$(./scripts/create-worktree.sh auth-fix 2>/dev/null)"
if [ "$OUT_FIX_AGAIN" = "$OUT_FIX" ]; then pass "re-requesting auth-fix exactly reuses it"; else fail "auth-fix reuse mismatch"; fi

# --- 2. Collision refusal: untracked directory -----------------------------
note "2. collision refusal — directory git doesn't know about"
mkdir -p "$SANDBOX/repo.wt/collide-dir"
OUT="$(./scripts/create-worktree.sh collide-dir 2>/tmp/cw-err-$$.log)"; RC=$?
if [ -z "$OUT" ] && [ "$RC" -ne 0 ] && grep -q "REFUSED" "/tmp/cw-err-$$.log"; then
  pass "refused loudly, empty stdout, nonzero exit"
else
  fail "collision (dir) not refused correctly: out=[$OUT] rc=$RC"
fi
if [ -e "$SANDBOX/repo.wt/collide-dir/.git" ]; then fail "directory was converted into a worktree"; else pass "untracked directory left untouched"; fi
rm -f "/tmp/cw-err-$$.log"

# --- 3. Collision refusal: orphan branch -----------------------------------
note "3. collision refusal — branch exists with no worktree"
git branch worktree-branchy >/dev/null
OUT="$(./scripts/create-worktree.sh branchy 2>/tmp/cw-err2-$$.log)"; RC=$?
if [ -z "$OUT" ] && [ "$RC" -ne 0 ]; then pass "refused loudly, empty stdout, nonzero exit"; else fail "branch collision not refused: out=[$OUT] rc=$RC"; fi
if git rev-parse --verify --quiet refs/heads/worktree-branchy >/dev/null; then pass "branch NOT force-deleted"; else fail "branch was force-deleted"; fi
rm -f "/tmp/cw-err2-$$.log"

# --- 4. Metadata-only stale + fully-merged branch -> auto-cleaned ----------
note "4. metadata-only stale, branch fully merged -> auto-cleanup + recreate"
./scripts/create-worktree.sh stale-merged >/dev/null 2>&1
rm -rf "$SANDBOX/repo.wt/stale-merged"
OUT="$(./scripts/create-worktree.sh stale-merged 2>/dev/null)"; RC=$?
if [ -n "$OUT" ] && [ "$RC" -eq 0 ] && [ -d "$OUT" ]; then pass "stale metadata cleared, worktree recreated"; else fail "stale-merged recreate failed: out=[$OUT] rc=$RC"; fi

# --- 5. Metadata-only stale + UNMERGED branch -> refused, never destroyed --
note "5. metadata-only stale, branch NOT merged -> refuse, branch preserved"
./scripts/create-worktree.sh stale-unmerged >/dev/null 2>&1
(cd "$SANDBOX/repo.wt/stale-unmerged" && echo wip > newfile.txt && git add -A && git -c user.email=t@t.com -c user.name=t commit -q -m "unmerged work")
rm -rf "$SANDBOX/repo.wt/stale-unmerged"
OUT="$(./scripts/create-worktree.sh stale-unmerged 2>/tmp/cw-err3-$$.log)"; RC=$?
if [ -z "$OUT" ] && [ "$RC" -ne 0 ]; then pass "refused, empty stdout, nonzero exit"; else fail "unmerged stale not refused: out=[$OUT] rc=$RC"; fi
if git rev-parse --verify --quiet refs/heads/worktree-stale-unmerged >/dev/null; then pass "unmerged branch preserved (not destroyed)"; else fail "unmerged branch was destroyed"; fi
rm -f "/tmp/cw-err3-$$.log"

# --- 6. Pre-commit guard: blocks a direct commit to main in the PRIMARY ----
#        checkout, allows the escape hatch, and never touches worktrees ----
note "6. pre-commit guard"
HOOKS_PATH="$(git config --get core.hooksPath || echo '')"
if [ "$HOOKS_PATH" = ".githooks" ]; then pass "core.hooksPath wired by install-git-hooks.sh"; else fail "core.hooksPath is [$HOOKS_PATH], expected .githooks"; fi

echo x > mainfile.txt
git add mainfile.txt
if git -c user.email=t@t.com -c user.name=t commit -q -m "direct main commit" 2>/tmp/cw-hook-$$.log; then
  fail "commit to main in primary checkout was NOT blocked"
  git reset -q --hard HEAD~1
else
  pass "commit to main in primary checkout BLOCKED"
fi
if grep -q "REFUSED" "/tmp/cw-hook-$$.log"; then pass "refusal message printed"; else fail "no refusal message"; fi
git reset -q HEAD mainfile.txt 2>/dev/null; rm -f mainfile.txt "/tmp/cw-hook-$$.log"

echo x > mainfile2.txt
git add mainfile2.txt
if DONMAI_ALLOW_MAIN_COMMIT=1 git -c user.email=t@t.com -c user.name=t commit -q -m "escape hatch commit" 2>/dev/null; then
  pass "escape hatch (DONMAI_ALLOW_MAIN_COMMIT=1) allows the commit"
  git reset -q --hard HEAD~1
else
  fail "escape hatch did not allow the commit"
fi

if (
  cd "$SANDBOX/repo.wt/auth" || exit 1
  echo y > wtfile.txt
  git add wtfile.txt
  git -c user.email=t@t.com -c user.name=t commit -q -m "worktree commit" 2>/tmp/cw-wt-$$.log
); then
  pass "commit on a worktree branch is unaffected"
else
  fail "worktree commit was incorrectly blocked"
fi
rm -f "/tmp/cw-wt-$$.log"

note ""
note "=== $PASS_COUNT passed, $FAILURES failed ==="
[ "$FAILURES" -eq 0 ]
