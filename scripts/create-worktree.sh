#!/usr/bin/env bash
# Create a git worktree at ../donmai.wt/<name> for isolated Claude sessions.
#
# Usage:
#   ./scripts/create-worktree.sh <name>
#
# Reuses an EXISTING worktree only when git has an exact-path record for it
# AND its directory is actually present. Otherwise creates a fresh worktree,
# copies .env.local, and prints the path — the ONLY line on stdout, on every
# success path. Every failure path prints to stderr only and exits nonzero,
# so `cd "$(scripts/create-worktree.sh name)"` fails loudly instead of
# silently cd-ing into an empty string or a stray info line.
#
# Hardening (2026-08-09) — three fatal flaws closed:
#   1. Reuse used to be `grep -q "$WT_PATH"` over `git worktree list`, a
#      SUBSTRING match: requesting "auth" would false-positive-reuse an
#      existing "auth-fix" worktree (its path contains "auth"). Reuse is now
#      an exact `worktree <path>` line match parsed from
#      `git worktree list --porcelain`, and a match is only trusted if the
#      directory still exists on disk.
#   2. Any name collision used to be "cleaned up" unconditionally:
#      `worktree remove --force || rm -rf`, `git worktree prune`, and
#      `branch -D` — all of it destructive, none of it asked first. All of
#      that is gone. The ONLY automatic cleanup left is narrow and safe:
#      metadata-only staleness (git still lists the worktree, its directory
#      is gone, and its branch is fully merged into origin/main) is cleared
#      with a non-forced `git worktree remove` + `git branch -d` (git's own
#      merge-safety gate, not just this script's). Anything else — a real
#      name collision, an unmerged stale branch — REFUSES loudly and tells
#      you how to resolve it by hand. Nothing is ever force-deleted.
#   3. Fetch failures used to be `2>/dev/null || true`, silently swallowing
#      both the error text AND the exit code, so a failed fetch let the
#      worktree branch from a stale on-disk `origin/HEAD` with no signal.
#      A fetch failure is now non-fatal but LOUD (see below).
#
# This script also wires this repo's main-commit guard on every run (see
# scripts/install-git-hooks.sh) — best-effort, never blocks worktree
# creation.

set -euo pipefail

WT_NAME="${1:?Usage: create-worktree.sh <name>}"
# `pwd -P` (physical, symlinks resolved), not `pwd`: `git worktree add` records
# a realpath internally, so `git worktree list --porcelain` reports paths with
# symlinks (e.g. macOS /tmp -> /private/tmp, /var -> /private/var) already
# resolved. Comparing that against an un-resolved REPO_ROOT would make the
# exact-match check below spuriously miss real matches whenever this repo (or
# a tmp sandbox exercising this script) sits under a symlinked path.
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"

# Derive the worktree root from the PRIMARY checkout, not from whatever checkout
# this copy of the script happens to live in.
#
# Every worktree gets a full copy of scripts/, so running
# ../donmai.wt/foo/scripts/create-worktree.sh bar made REPO_NAME "foo" and
# WT_ROOT "donmai.wt/foo.wt" — a worktree nested inside a worktree, one level
# deeper on each hop. `--git-common-dir` resolves to the main repo's .git even
# when invoked from a worktree, so PRIMARY_ROOT is the real checkout either way
# and every worktree lands as a sibling in ONE donmai.wt/.
GIT_COMMON="$(git -C "$REPO_ROOT" rev-parse --git-common-dir 2>/dev/null || echo "")"
case "$GIT_COMMON" in
  "") PRIMARY_ROOT="$REPO_ROOT" ;;
  /*) PRIMARY_ROOT="$(cd "$GIT_COMMON/.." && pwd -P)" ;;
  *)  PRIMARY_ROOT="$(cd "$REPO_ROOT/$GIT_COMMON/.." && pwd -P)" ;;
esac

REPO_NAME="$(basename "$PRIMARY_ROOT")"
WT_ROOT="$(dirname "$PRIMARY_ROOT")/${REPO_NAME}.wt"
WT_PATH="$WT_ROOT/$WT_NAME"
BRANCH="worktree-${WT_NAME}"

err() { printf '%s\n' "$*" >&2; }

# Best-effort: wire the repo-local pre-commit main-commit guard. Never blocks
# worktree creation — install-git-hooks.sh fails open.
if [ -x "$REPO_ROOT/scripts/install-git-hooks.sh" ]; then
  "$REPO_ROOT/scripts/install-git-hooks.sh" || true
fi

# Belt-and-braces on the resolution above: never place a worktree inside the
# repo. Nested checkouts get swept into build-tool globs — on 2026-08-03 that
# turned a sibling repo's type-check into an OOM SIGABRT. With PRIMARY_ROOT
# resolved this cannot trigger on a normal run, so if it ever fires the
# resolution itself is wrong.
case "$WT_ROOT/" in
  "$PRIMARY_ROOT"/*)
    err "REFUSED: worktree root ($WT_ROOT) resolves INSIDE the repo ($PRIMARY_ROOT)."
    err "  Nested worktrees break build tooling. This indicates the primary-root"
    err "  resolution failed — report it rather than working around it."
    exit 1 ;;
esac

mkdir -p "$WT_ROOT"

# --- Exact-match reuse check -------------------------------------------------
# Prints the branch ref of the worktree whose path equals $1 BYTE-FOR-BYTE
# ("(detached)" if it has no branch), or nothing if git has no such record.
find_worktree_branch() {
  git -C "$REPO_ROOT" worktree list --porcelain | awk -v target="$1" '
    /^worktree / { path = substr($0, 10); in_match = (path == target); branch = ""; next }
    /^branch /   { if (in_match) branch = substr($0, 8); next }
    /^$/         { if (in_match) print (branch == "" ? "(detached)" : branch); in_match = 0; next }
    END          { if (in_match) print (branch == "" ? "(detached)" : branch) }
  '
}

MATCH_BRANCH_REF="$(find_worktree_branch "$WT_PATH")"

if [ -n "$MATCH_BRANCH_REF" ]; then
  # Git tracks this exact path. Reuse only if the directory is really there.
  if [ -d "$WT_PATH" ]; then
    echo "$WT_PATH"
    exit 0
  fi

  # Metadata-only stale: git still lists it, the directory is gone. The ONLY
  # narrow auto-cleanup this script performs: remove the stale metadata
  # (no --force — nothing to force, the directory is already absent) and the
  # now-pointless branch, but ONLY when the branch is fully merged into
  # origin/main. Everything else refuses loudly.
  git -C "$REPO_ROOT" fetch origin --quiet 2>/dev/null || true

  ELIGIBLE=0
  if [ "$MATCH_BRANCH_REF" != "(detached)" ] \
     && git -C "$REPO_ROOT" rev-parse --verify --quiet origin/main >/dev/null \
     && git -C "$REPO_ROOT" merge-base --is-ancestor "${MATCH_BRANCH_REF#refs/heads/}" origin/main 2>/dev/null; then
    ELIGIBLE=1
  fi

  STALE_BRANCH="${MATCH_BRANCH_REF#refs/heads/}"

  if [ "$ELIGIBLE" != "1" ]; then
    err "REFUSED: worktree '$WT_NAME' has metadata pointing at a missing"
    err "  directory ($WT_PATH), but its branch ('$STALE_BRANCH') is not"
    err "  verifiably merged into origin/main — it will NOT be auto-removed."
    err ""
    err "  Inspect it, then resolve by hand:"
    err "    git -C '$REPO_ROOT' log origin/main..$STALE_BRANCH   # unmerged commits, if any"
    err "    git -C '$REPO_ROOT' worktree remove '$WT_PATH' --force   # only if you're sure"
    err "  ...or pick a different worktree name."
    exit 1
  fi

  if ! REMOVE_ERR=$(git -C "$REPO_ROOT" worktree remove "$WT_PATH" 2>&1); then
    err "REFUSED: worktree '$WT_NAME' has metadata pointing at a missing"
    err "  directory ($WT_PATH), and automatic cleanup failed:"
    err "  $REMOVE_ERR"
    err "  Investigate manually: git -C '$REPO_ROOT' worktree list --porcelain"
    exit 1
  fi
  err "create-worktree: removed stale metadata for '$WT_NAME' (directory was missing; branch '$STALE_BRANCH' is fully merged into origin/main)."

  if git -C "$REPO_ROOT" branch -d "$STALE_BRANCH" >/dev/null 2>&1; then
    err "create-worktree: also removed the now-stale branch '$STALE_BRANCH' (git confirmed it's safe to delete)."
  elif git -C "$REPO_ROOT" show-ref --verify --quiet "refs/heads/$STALE_BRANCH"; then
    err "REFUSED: stale worktree metadata was cleared, but branch '$STALE_BRANCH' could"
    err "  not be safely removed (git branch -d refused it) — this script never"
    err "  force-deletes a branch. Resolve manually, e.g.:"
    err "    git -C '$REPO_ROOT' branch -d '$STALE_BRANCH'   # after confirming it's safe"
    err "  ...or pick a different worktree name."
    exit 1
  fi
  # Fall through to creation below.
else
  # --- Collision refusal -----------------------------------------------------
  # Git has no record of this exact path, but something else may already
  # occupy the name. Refuse loudly and list every collision found — never
  # auto-remove a directory or force-delete a branch to make room.
  COLLISIONS=()
  if [ -e "$WT_PATH" ]; then
    COLLISIONS+=("a directory already exists at $WT_PATH (not a git worktree git knows about)")
  fi
  if git -C "$REPO_ROOT" show-ref --verify --quiet "refs/heads/$BRANCH"; then
    COLLISIONS+=("branch '$BRANCH' already exists")
  fi
  if [ "${#COLLISIONS[@]}" -gt 0 ]; then
    err "REFUSED: worktree name '$WT_NAME' collides with existing state:"
    for c in "${COLLISIONS[@]}"; do err "  - $c"; done
    err ""
    err "  This script never force-removes a directory or deletes a branch to"
    err "  make room. Pick a different name (e.g. '${WT_NAME}-2'), or resolve"
    err "  the collision yourself and re-run."
    exit 1
  fi
fi

# Fetch latest from origin.
#
# This used to be `2>/dev/null || true`, which swallowed BOTH the error text
# and the exit code — so when the fetch failed the script carried on and
# branched from whatever stale `origin/HEAD` happened to be on disk,
# silently. See the platform sibling of this script for the incident this
# was ported from (two agent lanes cut hundreds of commits behind main).
#
# A fetch failure is now non-fatal but LOUD, and the base is always reported
# so a stale branch point is visible at creation time rather than at CI time.
FETCH_ERR=$(git -C "$REPO_ROOT" fetch origin 2>&1) || {
  err "WARNING: 'git fetch origin' FAILED — this worktree will be branched from a"
  err "         possibly STALE origin/HEAD. Fix the fetch before trusting this base."
  err "         git said: ${FETCH_ERR}"
  err "         On a host that injects credentials via GIT_CONFIG_* env vars, retry with:"
  err "           env -u GIT_CONFIG_COUNT -u GIT_CONFIG_KEY_0 -u GIT_CONFIG_VALUE_0 $0 $*"
}

# Create the worktree with a new branch from origin/HEAD
git -C "$REPO_ROOT" worktree add "$WT_PATH" -b "$BRANCH" origin/HEAD >/dev/null 2>&1 || {
  err "Failed to create worktree at $WT_PATH"
  exit 1
}

# Always state the base. Cheap, and it makes "am I current?" answerable
# without a second command. (stderr, not stdout — see stdout-discipline note
# above; nothing but the final path may reach stdout on success.)
BASE_SHA=$(git -C "$REPO_ROOT" rev-parse --short origin/HEAD 2>/dev/null || echo '?')
BASE_SUBJECT=$(git -C "$REPO_ROOT" log -1 --format='%s' origin/HEAD 2>/dev/null || echo '?')
err "Branched from origin/HEAD @ ${BASE_SHA} — ${BASE_SUBJECT}"

# Copy dev config files into the worktree
for f in .env.local .env; do
  if [ -f "$REPO_ROOT/$f" ]; then
    cp "$REPO_ROOT/$f" "$WT_PATH/$f"
  fi
done

echo "$WT_PATH"
