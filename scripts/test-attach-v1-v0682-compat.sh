#!/usr/bin/env bash
set -euo pipefail

released_sha=65d80580fabdb6a469436965e9e17990cda23716
actual_sha=$(git rev-parse 'v0.68.2^{}')
if [[ "$actual_sha" != "$released_sha" ]]; then
  echo "v0.68.2 resolved to $actual_sha, want $released_sha" >&2
  exit 1
fi

compat_tmp=$(mktemp -d "/tmp/donmai-attach-v1-compat.XXXXXX")
cleanup() {
  rm -rf "$compat_tmp"
}
trap cleanup EXIT

old_tree="$compat_tmp/old"
old_binary="$compat_tmp/old-attach-v1"
new_binary="$compat_tmp/new-attach-v1"
old_bytes="$compat_tmp/old.bytes"
new_bytes="$compat_tmp/new.bytes"

mkdir -p "$old_tree/cmd/attach-v1-compat"
git archive "$released_sha" | tar -x -C "$old_tree"
cp scripts/testdata/attach-v1-compat/main.go "$old_tree/cmd/attach-v1-compat/main.go"

(cd "$old_tree" && GOWORK=off go build -o "$old_binary" ./cmd/attach-v1-compat)
GOWORK=off go build -o "$new_binary" ./scripts/testdata/attach-v1-compat

"$old_binary" > "$old_bytes"
"$new_binary" > "$new_bytes"
cmp "$old_bytes" "$new_bytes"
echo "attach-v1 v0.68.2 artifact bytes: identical"
