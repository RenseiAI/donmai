#!/usr/bin/env bash
# ============================================================================
# VENDORED — do not hand-edit below this header.
#
# Source:         RenseiAI/donmai-architecture scripts/guard-b-lint-selftest.sh
# Pinned commit:  0e601b3c6b5ff0dcecca6a7512786548726b6d6d (2026-08-08, "fix(guard-b):
#                 close nine round-2 holes..." #58) — round 3, 59/59 on its own
#                 selftest at that commit.
#
# This is a byte-identical copy of the upstream engine, not a fork: the parsing,
# the allowlist grammar and its refusal rules, the commit/stdin scan modes, and
# the rule table all come from here unmodified. Per-repo differences belong in
# THIS repo's .guard-allowlist (narrow, identifier-scoped exemptions), never in
# a hand-edit of the rules or engine below — three hand-edited forks is exactly
# the drift this vendoring avoids.
#
# scripts/check-guard-b-vendor-drift.sh (run in CI) fetches the pinned commit's
# copy of this file from GitHub and diffs it (this header excluded) against
# what's on disk. A mismatch means either a hand-edit slipped in here, or
# upstream has moved on and this repo's pin has gone stale — either way it is
# reported, not silently absorbed.
#
# To pick up an upstream change: copy the new file verbatim from
# donmai-architecture, update the pinned commit above, and update the matching
# SHA in scripts/check-guard-b-vendor-drift.sh.
# ============================================================================
# guard-b-lint-selftest.sh — prove guard-b actually fires.
#
# A leak guard that has never been shown to fail is not evidence of anything.
# This runs guard-b-lint.sh against synthetic input: every BAD sample must be
# flagged, every GOOD sample must pass. Each BAD sample carries the rule it is
# meant to trip, and the test asserts that rule is the one named in the output —
# so a rule cannot silently stop working while a neighbouring rule keeps the
# suite green.
#
# It also exercises the four engine behaviours that carried silent holes and
# that no file scan can reach: merge-commit messages, the squash-merge message
# composed from a pull-request title and body, files containing a NUL byte, and
# the refusal of blanket allowlist entries.
#
# ── Why the samples are assembled from fragments ─────────────────────────────
# This file is a tracked file: guard-b scans it. Written literally, every BAD
# sample would be a real violation, and the only way to keep the tree green was
# an allowlist entry covering every rule on every sample line — a whole-file
# escape by another name, and the exact hole this file's own header warns about.
# Assembling each sample at runtime (`"${U}EN-9990"` is not a tracker ID until
# the shell joins it) keeps every banned literal out of the source, so this file
# needs no allowlist entry at all. Do not "simplify" the fragments away.
#
# Usage: scripts/guard-b-lint-selftest.sh
# Exit 0 = every sample and engine behaviour is as specified; exit 1 otherwise.

set -eo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUARD="$REPO_ROOT/scripts/guard-b-lint.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
N=0

fail() {
  echo "self-test FAIL: $1" >&2
  FAIL=$((FAIL + 1))
}

# run_guard <arg>... — run guard-b from the repo root.
run_guard() {
  (cd "$REPO_ROOT" && "$GUARD" "$@" 2>&1)
}

# expect_flagged <RULE_ID> <sample-text>
expect_flagged() {
  local want_rule="$1" sample="$2" f out
  N=$((N + 1))
  f="$TMP/bad-$N.txt"
  printf '%s\n' "$sample" > "$f"
  if out="$(run_guard "$f")"; then
    fail "not flagged at all [$want_rule]: $sample"
    return
  fi
  if ! printf '%s\n' "$out" | grep -q "rule: $want_rule "; then
    fail "flagged, but not by $want_rule: $sample"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  PASS=$((PASS + 1))
}

# expect_clean <sample-text>
expect_clean() {
  local sample="$1" f out
  N=$((N + 1))
  f="$TMP/good-$N.txt"
  printf '%s\n' "$sample" > "$f"
  if out="$(run_guard "$f")"; then
    PASS=$((PASS + 1))
  else
    fail "false positive: $sample"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
  fi
}

# ---- Sample fragments (see header) ------------------------------------------
U='R'; L='r'; S='S'; M='M'; LS='s'; LM='m'  # tracker-prefix initials
BR='Rens'; BE='ei'                        # closed brand, split
PL='platform'                             # internal monorepo root
LN='linear.app'                           # tracker host
CLI='cli'                                 # closed control-plane route segment
UD='U'                                    # home-directory root initial
NUM='902'                                 # tail of a synthetic count

# ---- BAD: every rule must fire ----------------------------------------------
expect_flagged BRAND_NAME          "built by the ${BR}${BE} team"
expect_flagged CLOSED_TUI_REPO     "see ${L}ensei-tui for the composed binary"
expect_flagged CLOSED_PLATFORM     "the ${L}ensei-platform owns dispatch"
expect_flagged PARENT_DOMAIN       "POST https://app.${L}ensei.ai/api/workers"
expect_flagged PLATFORM_PATH       "writer lives in ${PL}/src/lib/dispatch.ts"
expect_flagged DEV_ABS_PATH        "cloned at /${UD}sers/someone/Developer/org/repo"
expect_flagged CLOSED_ENV_VAR      "export ${U}ENSEI_DAEMON_JWT=secret"

# App-Router paths under the closed monorepo, which the old src-only rule missed.
expect_flagged PLATFORM_PATH       "route handler at ${PL}/app/api/workers/route.ts"
expect_flagged PLATFORM_PATH       "migration in ${PL}/drizzle/0042_add_column.sql"

# The boundary violation AGENTS.md names most explicitly, and which no rule
# covered until 2026-08-07.
expect_flagged CLOSED_CLI_ENDPOINT "the daemon calls /api/${CLI}/capacity on boot"
expect_flagged CLOSED_CLI_ENDPOINT "POST /api/${CLI}/dispatch returns a receipt"

# ---- BAD: every tracker prefix the org issues, in all three casings ----------
# The pre-2026-08-07 rule was one prefix, uppercase only.
expect_flagged TRACKER_ID           "fixes ${U}EN-9990 regression"
expect_flagged TRACKER_ID           "ported from ${U}EN2-9991"
expect_flagged TRACKER_ID           "see ${S}UP-9992 for the credential surface"
expect_flagged TRACKER_ID           "raised as ${M}AR-9993"
expect_flagged TRACKER_ID           "ops escalation ${U}ENOPS-9994"
expect_flagged TRACKER_ID_SLUG      "branch agent/${L}en-9990-wire-fix carries it"
expect_flagged TRACKER_ID_SLUG      "worktree ${L}en2-9991-node-matrix"
expect_flagged TRACKER_ID_SLUG      "taskListId ${L}enops-9994-ops"
expect_flagged TRACKER_ID_SLUG      "cloned into ${LS}up-9992-cred-surface"
expect_flagged TRACKER_ID_SLUG      "worktree ${LM}ar-9993-launch-copy"
expect_flagged TRACKER_ID_TITLECASE "Fixes ${U}en-9990 in the composer"
expect_flagged TRACKER_ID_TITLECASE "Ported from ${U}en2-9991"
expect_flagged TRACKER_ID_TITLECASE "See ${S}up-9992 for the surface"
expect_flagged TRACKER_ID_TITLECASE "Ops escalation ${U}enops-9994"
expect_flagged TRACKER_ID_TITLECASE "Branch ${M}ar-9993-launch-copy"

# ---- BAD: tracker deep links, any workspace, team key or path ---------------
expect_flagged TRACKER_URL "tracked at https://${LN}/acme-workspace/issue/ABC-9/some-slug"
expect_flagged TRACKER_URL "board https://${LN}/acme-workspace/team/ABC/active"
# Paths outside the five the rule used to enumerate still expose the workspace key.
expect_flagged TRACKER_URL "members at https://${LN}/acme-workspace/settings/members"
expect_flagged TRACKER_URL "search https://${LN}/acme-workspace/search?q=pool"

# ---- BAD: production measurements of the closed control plane ---------------
# A seven-figure row count reached this corpus on 2026-08-07 and no rule saw it.
expect_flagged PROD_METRIC "zero fall-back events across 4,318,${NUM} ledger records"
expect_flagged PROD_METRIC "8,421,${NUM} dispatches observed"
expect_flagged PROD_METRIC "in production we counted 42,15${NUM:0:1} sessions"

# ---- GOOD: must not fire ----------------------------------------------------
expect_clean 'fixes the ENG-1234 fixture'
expect_clean 'CVE-2026-1234 hardening, RFC-7519 tokens, SHA-256 digests'
expect_clean 'the ADR-2026-06-07 ruling and ISO-8601 timestamps'
expect_clean 'import "github.com/RenseiAI/donmai/agent"'
expect_clean 'consumed by github.com/RenseiAI/tui-components'
expect_clean 'export DONMAI_DAEMON_JWT=secret'
expect_clean 'docs at donmai-architecture/002-provider-base-contract.md'
expect_clean 'the daemon serves /api/daemon/workarea/list'
expect_clean 'the GraphQL endpoint is api.linear.app/graphql'
expect_clean 'planning window Mar-2026, review on mar-15'
expect_clean 'renders a summary; supports up to 40 rows'
expect_clean 'the cache holds 1,024 entries by default'
expect_clean 'a synthetic benchmark of 2,500,000 iterations'
expect_clean 'the boundary between platform and OSS execution'

# ---- ENGINE: merge-commit messages ------------------------------------------
# `git rev-list --no-merges` used to be on the commit scan, and a merge commit
# is the one place a branch slug lands in published history.
check_merge_commits() {
  local d="$TMP/merge-repo" out
  N=$((N + 1))
  mkdir -p "$d"
  (
    cd "$d"
    export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
    export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
    export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
    git init -q .
    echo a > a.txt && git add a.txt && git commit -qm 'base'
    git tag base
    base_branch="$(git symbolic-ref --short HEAD)"
    git checkout -q -b side
    echo b > b.txt && git add b.txt && git commit -qm 'side work'
    git checkout -q "$base_branch"
    git merge -q --no-ff side -m "Merge branch 'agent/${L}en-9990-wire-fix'"
  ) >/dev/null 2>&1
  out="$(cd "$d" && "$GUARD" --commits base..HEAD 2>&1)" && {
    fail "merge-commit message not scanned (--commits reported clean)"
    return
  }
  if ! printf '%s\n' "$out" | grep -q 'rule: TRACKER_ID_SLUG '; then
    fail "merge-commit message scanned, but the slug rule did not name it"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  PASS=$((PASS + 1))
}
check_merge_commits

# ---- ENGINE: the squash-merge message GitHub composes -----------------------
# The commit gate only ever saw branch commits. GitHub then builds a squash
# message from the PR title and body that exists in no branch commit and that
# no run had ever scanned.
check_stdin_squash_message() {
  local out
  N=$((N + 1))
  if out="$(printf 'docs(adr): drift fixes (%sEN-9990) (#39)\n' "$U" | (cd "$REPO_ROOT" && "$GUARD" --stdin pr-squash-message) 2>&1)"; then
    fail "squash-merge message not scanned (--stdin reported clean)"
    return
  fi
  if ! printf '%s\n' "$out" | grep -q '^  pr-squash-message:1:.*rule: TRACKER_ID '; then
    fail "--stdin scanned, but did not report under its label"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  PASS=$((PASS + 1))
}
check_stdin_squash_message

# ---- ENGINE: the location prefix must not participate in matching -----------
# Locations are attached from a parallel index, not prefixed onto the content.
# Prefixing would make a tracker slug in a branch name flag every line of that
# commit's body, burying the real hits under thousands of phantom ones.
check_location_not_matched() {
  local out
  N=$((N + 1))
  if ! out="$(printf 'a perfectly clean line of prose\n' \
      | (cd "$REPO_ROOT" && "$GUARD" --stdin "added-by:agent/${L}en-9990-wire-fix") 2>&1)"; then
    fail "the location label matched a rule and flagged clean content"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  PASS=$((PASS + 1))
}
check_location_not_matched

# ---- ENGINE: a NUL byte must not suppress a file ----------------------------
# `grep -I` used to be in the flag set, so one NUL byte made an entire file
# invisible to every rule, with no report of the skip.
check_binary_not_skipped() {
  local f="$TMP/binary-sample.bin" out
  N=$((N + 1))
  {
    printf 'harmless first line\n'
    head -c 1 /dev/zero
    printf '\nfixes %sEN-9990 in the fixture\n' "$U"
  } > "$f"
  if out="$(run_guard "$f")"; then
    fail "NUL byte suppressed the whole file (guard reported clean)"
    return
  fi
  if ! printf '%s\n' "$out" | grep -q 'NOTICE — 1 binary file'; then
    fail "binary file was scanned but not reported as such"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  PASS=$((PASS + 1))
}
check_binary_not_skipped

# ---- ENGINE: blanket allowlist entries are refused --------------------------
# Both the guard's header and the allowlist's header declared the
# no-whole-file rule; nothing enforced it, and a whole-file-shaped entry
# survived three review rounds.
check_allowlist_refusal() {
  local d="$TMP/allow-$1" entry="$2" want="$3" out rc
  N=$((N + 1))
  mkdir -p "$d"
  printf '%s\n' "$entry" > "$d/.guard-allowlist"
  printf 'a harmless line\n' > "$d/sample.md"
  set +e
  out="$(cd "$d" && "$GUARD" sample.md 2>&1)"
  rc=$?
  set -e
  if [[ "$want" == "refuse" ]]; then
    if [[ $rc -ne 2 ]] || ! printf '%s\n' "$out" | grep -q 'REFUSED'; then
      fail "blanket allowlist entry accepted (rc=$rc): $entry"
      printf '%s\n' "$out" | sed 's/^/    /' >&2
      return
    fi
  else
    if [[ $rc -ne 0 ]]; then
      fail "well-formed allowlist entry rejected (rc=$rc): $entry"
      printf '%s\n' "$out" | sed 's/^/    /' >&2
      return
    fi
  fi
  PASS=$((PASS + 1))
}
check_allowlist_refusal wholefile  'BRAND_NAME :: ^sample\.md:[0-9]+ :: .*'    refuse
check_allowlist_refusal unanchored 'BRAND_NAME :: sample\.md:[0-9]+ :: Acme'   refuse
check_allowlist_refusal wildcard   'BRAND_NAME :: ^.*:[0-9]+ :: Acme'          refuse
check_allowlist_refusal twofield   'BRAND_NAME :: ^sample\.md:[0-9]+'          refuse
check_allowlist_refusal unknownid  'BRAND_NAMES :: ^sample\.md:[0-9]+ :: Acme' refuse
check_allowlist_refusal scoped     'BRAND_NAME :: ^sample\.md:[0-9]+ :: Acme'  accept

# ---- ENGINE: an allowlist entry exempts one OCCURRENCE, not a whole line ----
# The pre-round-3 grammar matched one regex against the composed display string
# "<location>:<line>:<content> [rule: …]". Because <content> is the whole line,
# an exemption keyed on one identifier silently covered every other violation
# sharing that line. Two identifiers, one exempted: the other must still fire.
check_allowlist_per_occurrence() {
  local d="$TMP/allow-per-occurrence" out rc
  N=$((N + 1))
  mkdir -p "$d"
  printf '%s\n' \
    "TRACKER_ID :: ^sample\.md:[0-9]+ :: ${U}EN-9990" > "$d/.guard-allowlist"
  printf 'fixes %sEN-9990 and also %sEN-9991 in one line\n' "$U" "$U" > "$d/sample.md"
  set +e
  out="$(cd "$d" && "$GUARD" sample.md 2>&1)"
  rc=$?
  set -e
  if [[ $rc -eq 0 ]]; then
    fail "exempting one identifier suppressed a second, unlisted one on the same line"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  # Assert on the [match: …] annotation, not the whole output: the reported
  # line's CONTENT necessarily contains both identifiers, so grepping the
  # output for the exempted one always hits and proves nothing.
  if printf '%s\n' "$out" | grep -q "\[match: .*${U}EN-9990"; then
    fail "the exempted occurrence was still reported"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  if ! printf '%s\n' "$out" | grep -q "\[match: .*${U}EN-9991"; then
    fail "the unlisted occurrence was not reported"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  PASS=$((PASS + 1))
}
check_allowlist_per_occurrence

# ---- ENGINE: a rule scope is a whole string, not a prefix -------------------
# `TRACKER_ID` must NOT cover `TRACKER_ID_SLUG`. The old grammar compared the
# scope as a bare prefix of the composed string, so the narrower-looking scope
# silently exempted every rule whose ID began with it.
check_allowlist_scope_not_prefix() {
  local d="$TMP/allow-scope-prefix" out rc
  N=$((N + 1))
  mkdir -p "$d"
  # NB: pass the whole prefix as ONE argument. Splitting it so that a '%s'
  # conversion abuts the rest of the prefix reassembles the banned literal in
  # this file's own source — the exact violation the fragment convention in the
  # header exists to prevent, and this guard flags it on its own selftest.
  printf '%s\n' \
    "TRACKER_ID :: ^sample\.md:[0-9]+ :: ${LS}up-9990" > "$d/.guard-allowlist"
  printf 'branch %s-9990-wire carries the slug form\n' "${LS}up" > "$d/sample.md"
  set +e
  out="$(cd "$d" && "$GUARD" sample.md 2>&1)"
  rc=$?
  set -e
  if [[ $rc -eq 0 ]]; then
    fail "a TRACKER_ID scope exempted a TRACKER_ID_SLUG violation (prefix match)"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  if ! printf '%s\n' "$out" | grep -q 'rule: TRACKER_ID_SLUG '; then
    fail "the slug violation was not reported under TRACKER_ID_SLUG"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    return
  fi
  PASS=$((PASS + 1))
}
check_allowlist_scope_not_prefix

# ---- Report -----------------------------------------------------------------
echo ""
if [[ $FAIL -eq 0 ]]; then
  echo "guard-b self-test: OK — $PASS/$N checks behaved as specified."
  exit 0
fi
echo "guard-b self-test: FAILED — $FAIL failing, $PASS passing (of $N)." >&2
exit 1
