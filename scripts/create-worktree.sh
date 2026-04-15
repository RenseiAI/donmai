#!/usr/bin/env bash
# Create a git worktree at ../rensei-tui.wt/<name> for isolated Claude sessions.
#
# Usage:
#   ./scripts/create-worktree.sh <name>
#
# Creates the worktree if it doesn't exist, copies .env.local, and prints the path.

set -euo pipefail

WT_NAME="${1:?Usage: create-worktree.sh <name>}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO_NAME="$(basename "$REPO_ROOT")"
WT_ROOT="$(dirname "$REPO_ROOT")/${REPO_NAME}.wt"
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
