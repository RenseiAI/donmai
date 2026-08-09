#!/usr/bin/env bash
# install-git-hooks.sh — point this repo's core.hooksPath at .githooks and
# make sure the tracked hooks are executable.
#
# Idempotent and safe to call from anywhere: scripts/create-worktree.sh,
# scripts/refresh-worktree.sh (the SessionStart hook), a package-manager
# "prepare"/"hooks" lifecycle step, or by hand.
#
# core.hooksPath is repo-wide git config — shared by the primary checkout
# AND every linked worktree (they share one .git/config unless
# extensions.worktreeConfig is in play, which this repo does not use). So
# running this once, from any worktree, wires the guard everywhere,
# including the primary checkout that never runs create-worktree.sh itself.
#
# Never fails the caller: hook installation is best-effort. A failure here
# must not block dependency installs or worktree creation.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"
HOOKS_DIR=".githooks"

if [ ! -f "$REPO_ROOT/$HOOKS_DIR/pre-commit" ]; then
  echo "[install-git-hooks] $HOOKS_DIR/pre-commit not found — skipping" >&2
  exit 0
fi

chmod +x "$REPO_ROOT/$HOOKS_DIR/pre-commit" 2>/dev/null || true

CURRENT="$(git -C "$REPO_ROOT" config --get core.hooksPath 2>/dev/null || echo '')"
if [ "$CURRENT" != "$HOOKS_DIR" ]; then
  if git -C "$REPO_ROOT" config core.hooksPath "$HOOKS_DIR" 2>/dev/null; then
    echo "[install-git-hooks] core.hooksPath set to $HOOKS_DIR (was: ${CURRENT:-<unset>})" >&2
  else
    echo "[install-git-hooks] WARNING: failed to set core.hooksPath — the main-commit guard is NOT installed" >&2
  fi
fi

exit 0
