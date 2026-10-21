#!/usr/bin/env bash
# Publish completed strict assessments, including gated results, never errors.
set -euo pipefail
if (( $# != 1 )) || [[ ! $1 =~ ^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[1-9][0-9]*$ ]]; then
  echo 'Usage: bash comment.sh https://github.com/OWNER/REPO/pull/NUMBER' >&2
  exit 2
fi
summary=$(mktemp)
trap 'rm -f "$summary"' EXIT
status=0
donmai arch assess "$1" --require-diff --summary --gate-policy no-severity-high > "$summary" || status=$?
if (( status != 0 && status != 1 )); then
  echo 'Assessment failed; no comment was posted.' >&2
  exit "$status"
fi
if [[ ! -s $summary ]]; then
  echo 'Assessment produced no summary; no comment was posted.' >&2
  exit 2
fi
gh pr comment "$1" --body-file "$summary"
exit "$status"
