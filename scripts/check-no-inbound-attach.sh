#!/usr/bin/env bash
# check-no-inbound-attach.sh — CI assert for the interactive-attach
# outbound-only mandate (donmai-architecture/protocol/interactive-attach-v1.md
# §12; ADR-2026-07-12-interactive-pty-session-host.md, outbound-stream
# mandate rule 2).
#
# The attach path NEVER opens an inbound listener on the host: everything the
# relay needs (snapshots, resize, kill) rides Control frames on the
# connection the host dialed OUT. This script fails if listener-creating
# calls appear in the attach-path packages.
#
# Scope (per spec §12 R5, the mandate governs the relay attach path):
#   - attachwire/      : pure codec — no sockets at all, tests included.
#   - attachclient/    : outbound dialer — no listeners in non-test code.
#     attachclient/attachtest/ is the sanctioned carve-out: a loopback-only
#     stub relay used exclusively by tests (W5 builds the real relay,
#     closed). Its listener is the TEST SERVER the client dials.
#   - ptyhost/         : in-process host — no listeners anywhere, tests
#     included (local attach is an in-process API, not a socket).
# The pre-existing daemon loopback control API (127.0.0.1:7734) is outside
# the attach path and outside this script's scope by design.

set -euo pipefail
cd "$(dirname "$0")/.."

pattern='net\.Listen|http\.ListenAndServe|tls\.Listen|net\.ListenTCP|net\.ListenUnix|http\.Serve\('

fail=0

check() {
  local desc="$1"; shift
  local hits
  hits=$(grep -rnE "$pattern" "$@" 2>/dev/null || true)
  if [[ -n "$hits" ]]; then
    echo "FAIL: inbound-listener call in ${desc}:" >&2
    echo "$hits" >&2
    fail=1
  fi
}

# attachwire + ptyhost: no listeners anywhere (tests included).
[[ -d attachwire ]] && check "attachwire (incl. tests)" attachwire --include='*.go'
[[ -d ptyhost ]] && check "ptyhost (incl. tests)" ptyhost --include='*.go'

# attachclient: no listeners in non-test code outside attachtest.
if [[ -d attachclient ]]; then
  files=$(find attachclient -name '*.go' ! -name '*_test.go' ! -path 'attachclient/attachtest/*')
  if [[ -n "$files" ]]; then
    # shellcheck disable=SC2086
    check "attachclient non-test code (attachtest carve-out excluded)" $files
  fi
fi

if [[ "$fail" -ne 0 ]]; then
  echo "no-inbound-attach: FAILED — the attach path must stay outbound-only." >&2
  exit 1
fi
echo "no-inbound-attach: OK"
