#!/usr/bin/env bash
# leak-guard.sh — closed-source content linter for the donmai OSS repo.
#
# Blocks content that must never ship in this repository: internal
# tracker issue IDs, private repo references, internal platform
# hostnames, closed-source environment variable names, and developer
# workspace paths.
#
# Usage: scripts/leak-guard.sh [--staged | --all | --self-test | <file>...]
#   --staged     scan only git-staged files (pre-commit mode)
#   --all        scan the entire tracked tree (CI mode)
#   --self-test  verify the guard detects known-bad content (CI sanity)
#   <file>...    scan an explicit file list
#
# Allowlist: .leak-guard-allowlist in the repo root — one grep -E pattern
# per line (comments with #), matched against the full "file:line:content"
# violation string.
#
# Always-excluded paths (see EXCLUDED_RE below):
#   - CHANGELOG.md            append-only historical record
#   - scripts/leak-guard.sh   contains the rule patterns themselves
#   - .leak-guard-allowlist   contains violation-shaped patterns

set -eo pipefail
# nounset (-u) intentionally omitted: bash 3 treats empty arrays as
# unbound, which fires before ${#ARR[@]} can be checked.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ALLOWLIST="$REPO_ROOT/.leak-guard-allowlist"
EXCLUDED_RE='^(CHANGELOG\.md|scripts/leak-guard\.sh|\.leak-guard-allowlist)$'

# ---- Rule definitions: "ID|regex|description" -------------------------------
# The first '|' separates the rule ID; the last '|' separates the
# description; everything between is the pattern (which may itself
# contain '|' alternation). The script file is excluded from scanning,
# so the rule lines cannot self-flag.
RULES=(
  'TRACKER_ID|\b(REN2|REN|SUP)-[0-9]+\b|internal tracker issue ID'
  'PRIVATE_REPO|rensei-architecture|rensei-ops|RenseiAI/(platform|rensei-tui)|private repo reference'
  'PLATFORM_HOST|app\.rensei\.ai|rensei\.dev|internal platform hostname'
  'CLOSED_ENV|RENSEI_[A-Z_]+|closed-source environment variable (use DONMAI_*)'
  'DEV_WORKSPACE|/Users/[^/[:space:]]+/Developer/|developer workspace absolute path'
)

# ---- Scan --------------------------------------------------------------------
# scan_files <file>... — prints violations, returns 1 if any found.
scan_files() {
  local violations=() rule rule_id pattern description match allowed ap

  local allow_patterns=()
  if [[ -f "$ALLOWLIST" ]]; then
    while IFS= read -r line; do
      [[ "$line" =~ ^#.*$ || -z "$line" ]] && continue
      allow_patterns+=("$line")
    done < "$ALLOWLIST"
  fi

  for rule in "${RULES[@]}"; do
    rule_id="${rule%%|*}"
    pattern="${rule#*|}"
    pattern="${pattern%|*}"
    description="${rule##*|}"
    while IFS= read -r match; do
      [[ -n "$match" ]] || continue
      allowed=false
      for ap in "${allow_patterns[@]}"; do
        if printf '%s\n' "$match" | grep -qE "$ap"; then
          allowed=true
          break
        fi
      done
      $allowed && continue
      violations+=("$match  [rule: $rule_id — $description]")
    done < <(grep -nIHE "$pattern" "$@" 2>/dev/null || true)
  done

  if [[ ${#violations[@]} -eq 0 ]]; then
    echo "leak-guard: OK — no closed-source content found ($# files)."
    return 0
  fi

  echo ""
  echo "leak-guard: VIOLATIONS FOUND (${#violations[@]})"
  echo "------------------------------------------------------------"
  printf '  %s\n' "${violations[@]}"
  echo "------------------------------------------------------------"
  echo "Rewrite the content to describe the behavior instead of citing"
  echo "internal trackers/repos. To allowlist a specific line, add a"
  echo "grep -E pattern matching the full 'file:line:content' string to"
  echo ".leak-guard-allowlist."
  return 1
}

# ---- Self-test ---------------------------------------------------------------
self_test() {
  local tmp pass=0 fail=0 i=0 f s
  tmp="$(mktemp -d)"

  # Each bad sample must be flagged.
  local bad=(
    'fixes REN-1234 regression'
    'see SUP-99 for details'
    'ported from REN2-7'
    'documented in rensei-architecture/002.md'
    'tracked in rensei-ops'
    'see RenseiAI/platform for the server half'
    'forked from RenseiAI/rensei-tui'
    'POST https://app.rensei.ai/api/workers'
    'defaults to agent.rensei.dev'
    'export RENSEI_DAEMON_JWT=secret'
    'cloned at /Users/someone/Developer/private-org/repo'
  )
  # Each good sample must pass.
  local good=(
    'fixes the ENG-1234 fixture'
    'import "github.com/RenseiAI/donmai/agent"'
    'consumed by github.com/RenseiAI/tui-components'
    'export DONMAI_DAEMON_JWT=secret'
    'HOME=/Users/x and Read(/Users/**/.env*)'
    'docs at donmai-architecture/002-provider-base-contract.md'
  )

  for s in "${bad[@]}"; do
    f="$tmp/bad-$i.txt"
    printf '%s\n' "$s" > "$f"
    if scan_files "$f" >/dev/null 2>&1; then
      echo "self-test FAIL: not flagged: $s" >&2
      fail=$((fail + 1))
    else
      pass=$((pass + 1))
    fi
    i=$((i + 1))
  done
  for s in "${good[@]}"; do
    f="$tmp/good-$i.txt"
    printf '%s\n' "$s" > "$f"
    if scan_files "$f" >/dev/null 2>&1; then
      pass=$((pass + 1))
    else
      echo "self-test FAIL: false positive: $s" >&2
      fail=$((fail + 1))
    fi
    i=$((i + 1))
  done

  rm -rf "$tmp"
  echo "leak-guard self-test: $pass passed, $fail failed"
  [[ $fail -eq 0 ]]
}

# ---- Parse args --------------------------------------------------------------
FILES=()
case "${1:-}" in
  --self-test)
    self_test
    exit $?
    ;;
  --staged)
    while IFS= read -r f; do
      [[ -n "$f" ]] && ! [[ "$f" =~ $EXCLUDED_RE ]] && FILES+=("$REPO_ROOT/$f")
    done < <(git -C "$REPO_ROOT" diff --cached --name-only --diff-filter=ACMR)
    ;;
  --all)
    while IFS= read -r f; do
      [[ -n "$f" ]] && ! [[ "$f" =~ $EXCLUDED_RE ]] && FILES+=("$REPO_ROOT/$f")
    done < <(git -C "$REPO_ROOT" ls-files)
    ;;
  "")
    echo "usage: scripts/leak-guard.sh [--staged | --all | --self-test | <file>...]" >&2
    exit 2
    ;;
  *)
    FILES=("$@")
    ;;
esac

if [[ ${#FILES[@]} -eq 0 ]]; then
  echo "leak-guard: no files to scan."
  exit 0
fi

scan_files "${FILES[@]}"
