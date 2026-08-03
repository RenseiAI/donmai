#!/usr/bin/env bash
# Create a git worktree at ../donmai.wt/<name> for isolated Claude sessions.
#
# Usage:
#   ./scripts/create-worktree.sh <name>
#
# Creates the worktree if it doesn't exist, copies .env.local, and prints the path.

set -euo pipefail

WT_NAME="${1:?Usage: create-worktree.sh <name>}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Resolve the PRIMARY repo root, not whatever checkout this script copy lives in.
#
# `--git-common-dir` points at the main repo's .git even when invoked from a
# worktree. Deriving from $0 nests: running a copy at donmai.wt/foo/scripts/
# made REPO_NAME "foo" and WT_ROOT "donmai.wt/foo.wt".
GIT_COMMON="$(cd "$REPO_ROOT" && git rev-parse --git-common-dir 2>/dev/null || echo "")"
case "$GIT_COMMON" in
  "") PRIMARY_ROOT="$REPO_ROOT" ;;
  /*) PRIMARY_ROOT="$(cd "$GIT_COMMON/.." && pwd)" ;;
  *)  PRIMARY_ROOT="$(cd "$REPO_ROOT/$GIT_COMMON/.." && pwd)" ;;
esac

REPO_NAME="$(basename "$PRIMARY_ROOT")"
WT_ROOT="$(dirname "$PRIMARY_ROOT")/${REPO_NAME}.wt"

# Refuse to create a worktree inside the repo. Nested checkouts get swept into
# build-tool globs; on 2026-08-03 that turned a sibling repo's type-check into
# an OOM SIGABRT.
case "$WT_ROOT/" in
  "$PRIMARY_ROOT"/*)
    echo "refusing: worktree root ($WT_ROOT) is inside the repo ($PRIMARY_ROOT)." >&2
    echo "         Run this from the primary checkout, not a worktree copy." >&2
    exit 1 ;;
esac
WT_PATH="$WT_ROOT/$WT_NAME"
BRANCH="worktree-${WT_NAME}"

mkdir -p "$WT_ROOT"

# If the worktree directory already exists AND git knows about it, reuse it
if [ -d "$WT_PATH" ] && git -C "$REPO_ROOT" worktree list 2>/dev/null | grep -q "$WT_PATH"; then
  echo "$WT_PATH"
  exit 0
fi

# Clean up stale state
if [ -d "$WT_PATH" ]; then
  git -C "$REPO_ROOT" worktree remove "$WT_PATH" --force >/dev/null 2>&1 || rm -rf "$WT_PATH"
fi
git -C "$REPO_ROOT" worktree prune >/dev/null 2>&1 || true
git -C "$REPO_ROOT" branch -D "$BRANCH" >/dev/null 2>&1 || true

# Fetch latest from origin
git -C "$REPO_ROOT" fetch origin --quiet 2>/dev/null || true

# Create the worktree with a new branch from origin/HEAD
git -C "$REPO_ROOT" worktree add "$WT_PATH" -b "$BRANCH" origin/HEAD >/dev/null 2>&1 || {
  echo "Failed to create worktree at $WT_PATH" >&2
  exit 1
}

# Copy dev config files into the worktree
for f in .env.local .env; do
  if [ -f "$REPO_ROOT/$f" ]; then
    cp "$REPO_ROOT/$f" "$WT_PATH/$f"
  fi
done

echo "$WT_PATH"
