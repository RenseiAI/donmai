#!/usr/bin/env bash
# check-oss-boundary.sh — Prevents closed-source references from leaking
# into this open-source repository. Run as a pre-commit hook or CI check.

set -euo pipefail

RED='\033[0;31m'
NC='\033[0m'

ERRORS=0

if [[ "${1:-}" == "--all" ]]; then
  FILES=$(find . -type f \( -name '*.go' -o -name '*.md' -o -name '*.yaml' -o -name '*.yml' -o -name '*.json' -o -name '*.sh' \) -not -path './.git/*' -not -path './.claude/*' -not -path './vendor/*' -not -path './scripts/check-oss-boundary.sh')
else
  FILES=$(git diff --cached --name-only --diff-filter=ACMR 2>/dev/null || echo "")
fi

if [[ -z "$FILES" ]]; then
  exit 0
fi

# Hard violations — closed-source project names must never appear
for pattern in 'rensei-tui' 'rensei\.ai' 'rensei_tui'; do
  matches=$(echo "$FILES" | xargs grep -ilE "$pattern" 2>/dev/null || true)
  if [[ -n "$matches" ]]; then
    echo -e "${RED}OSS BOUNDARY VIOLATION:${NC} Closed-source reference '$pattern' found in:"
    echo "$matches" | sed 's/^/  /'
    ERRORS=$((ERRORS + 1))
  fi
done

# Check branch name
BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")
if echo "$BRANCH" | grep -qiE 'rensei'; then
  echo -e "${RED}OSS BOUNDARY VIOLATION:${NC} Branch name '$BRANCH' contains closed-source reference"
  ERRORS=$((ERRORS + 1))
fi

if [[ $ERRORS -gt 0 ]]; then
  echo ""
  echo "This is an open-source repository. Closed-source project names"
  echo "must not appear in code, comments, commits, or PR descriptions."
  echo "Use generic language: 'downstream consumers', 'importing CLIs', etc."
  exit 1
fi
