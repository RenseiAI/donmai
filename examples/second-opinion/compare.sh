#!/usr/bin/env bash
# Read both implementations and their receipts without merging either PR.
set -euo pipefail
if (( $# != 3 )) || [[ ! $1 =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
   [[ ! $2 =~ ^[1-9][0-9]*$ || ! $3 =~ ^[1-9][0-9]*$ || $2 == "$3" ]]; then
  echo 'Usage: bash compare.sh OWNER/REPO CLAUDE_PR CODEX_PR (distinct positive numbers)' >&2
  exit 2
fi
for pr in "$2" "$3"; do
  printf '\n=== PR #%s: implementation and session receipts ===\n' "$pr"
  gh pr view "$pr" --repo "$1" --json url,title,headRefName,body,comments
  gh pr diff "$pr" --repo "$1"
done
