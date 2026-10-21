#!/usr/bin/env bash
# Admit only the issues explicitly selected for this demonstration.
set -euo pipefail
if (( $# < 2 )) || [[ ! $1 =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo 'Usage: bash label.sh OWNER/REPO ISSUE [ISSUE ...]' >&2
  exit 2
fi
repo=$1
shift
# Validate the entire batch before publishing any labels.
for issue in "$@"; do
  if [[ ! $issue =~ ^[1-9][0-9]*$ ]]; then
    echo 'Issue numbers must be positive integers.' >&2
    exit 2
  fi
done
for issue in "$@"; do
  donmai github add-labels --repo "$repo" --number "$issue" --labels donmai
done
