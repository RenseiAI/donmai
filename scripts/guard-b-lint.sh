#!/usr/bin/env bash
# ============================================================================
# VENDORED — do not hand-edit below this header.
#
# Source:         RenseiAI/donmai-architecture scripts/guard-b-lint.sh
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
# guard-b-lint.sh — closed-source content linter for this public corpus.
#
# Usage: ./scripts/guard-b-lint.sh [MODE...] [<file>...]
#   --staged            scan git-staged files (pre-commit mode)
#   --all               scan every tracked file (CI mode)
#   --commits <range>   scan the COMMIT MESSAGES of a rev-range, merge commits
#                       included. Commit messages are published with the repo
#                       and a file scan cannot see them.
#   --stdin <label>     scan text arriving on stdin, reported under <label>.
#                       CI uses this for the squash-merge message GitHub
#                       composes from the PR title and body — that text becomes
#                       a published commit and exists in no branch commit — and
#                       for the pull request's own head branch name, which
#                       `git ls-remote --heads` publishes to anyone.
#   --punch-list        also write violations to GUARD-B-VIOLATIONS.txt
#   <file>...           scan an explicit file list
#
# Exit codes: 0 clean, 1 violations found, 2 usage error or refused allowlist.
#
# Self-test: scripts/guard-b-lint-selftest.sh — proves every rule fires on a
# known-bad sample, stays quiet on a known-good one, and that the engine's
# non-obvious behaviours (merge commits, stdin, binary files, per-occurrence
# allowlisting, blanket-allowlist refusal) actually work. A guard nobody has
# watched fail is not evidence.
#
# ── Allowlist: .guard-allowlist, three or four fields per entry ──────────────
#
#     <RULE_ID>[,<RULE_ID>...] :: <location-regex> :: <match-regex>[ :: <line-regex>]
#
# An entry exempts a violation only when ALL of these hold:
#   * the firing rule's ID is in the comma-separated list, compared as a whole
#     string — a `TRACKER_ID` scope does NOT cover `TRACKER_ID_SLUG`;
#   * <location-regex> matches "<location>:<line-number>";
#   * <match-regex> matches THE TEXT THE RULE ACTUALLY MATCHED, not the line it
#     sits on — so a second, unlisted identifier sharing a line with an exempted
#     one is still reported;
#   * <line-regex>, if given, matches the whole line (optional extra narrowing).
#
# This shape replaced a single regex matched against the composed display
# string. That design had two holes, both of which shipped: an exemption keyed
# on one identifier silently covered every other violation on the same line, and
# a rule scope written as a bare prefix covered every rule whose ID started with
# it. Every field is validated at load time and a malformed or blanket entry is
# REFUSED with exit 2 — precedent: scripts/retired-claim-lint.sh. Declaring a
# discipline in a comment is not enforcing it; that is how a gate stops being
# evidence.

set -eo pipefail
# nounset (-u) intentionally omitted: bash 3 treats empty arrays as unbound,
# which fires before we can check ${#ARR[@]}. Portability over strictness.

ALLOWLIST=".guard-allowlist"
PUNCH_LIST_MODE=false

# ---- Rule definitions: "ID|FLAGS|regex|description" ----
# FLAGS: "-" = case-sensitive, "i" = case-insensitive.
# The regex field may itself contain '|' alternation; the description may not.
#
# ── Why the patterns are written with single-character [brackets] ────────────
# Several rules would otherwise match their own definition line, and the only
# way to keep this file green was an allowlist entry covering the rule table —
# which is a whole-file escape wearing a disguise. Bracketing one letter
# (`Rens[e]i`, `/Us[e]rs/`, `/api/cl[i]/`) changes nothing about what the rule
# matches at scan time and makes the definition line itself unmatchable, so
# this script needs no exemption at all. Do not "tidy" the brackets away.
#
# ── Rule IDs are deliberately brand-neutral ─────────────────────────────────
# They used to embed the closed brand and the closed env-var prefix, so every
# place that merely *named* a rule — this file, the CI workflow, the allowlist —
# tripped the rules and had to be exempted. Those exemptions are where the
# whole-file escapes came from.
#
# ── Word boundaries are spelled out, because \b does not mean what it looks
#    like it means ─────────────────────────────────────────────────────────────
# `_` is a word character, so `\br[e]n-2034\b` does NOT match the branch name
# `feature_r[e]n-2034-wire` and `\bs[u]p-1840\b` does NOT match the worktree
# `wt_s[u]p-1840-cred`. Both are shapes ordinary branch and worktree naming
# produces, and both scanned clean under a `\b` boundary. (The bracketed letters
# are the same convention as the rule table below: they keep this comment from
# being a violation of the very rules it documents.)
# The tracker rules use explicit `(^|[^0-9A-Za-z])` / `([^0-9A-Za-z]|$)`
# boundaries instead, which treat `_` as the separator it visually is. The cost
# is that the matched text includes the boundary character; allowlist
# <match-regex>es are matched unanchored, so that is invisible to them.
#
# ── Tracker IDs are covered in all three casings that occur in practice ─────
# The three rules are disjoint by construction (upper / lower / title), so a
# single leak is reported once, by the rule that names its shape:
#   TRACKER_ID            the prose form, all upper case
#   TRACKER_ID_SLUG       the all-lower-case form a branch, worktree directory
#                         or task-list id carries, with its trailing slug
#   TRACKER_ID_TITLECASE  the form a title-cased sentence or a UI label makes
# The separator between prefix and number is OPTIONAL (`-?`): live branches in
# a sibling public repo carry the un-hyphenated spelling of the same ID, and it
# scanned clean while the hyphenated spelling failed. The marketing team's key
# collides with calendar strings, so it keeps its hyphen in the prose form and
# its lower- and title-case forms require the trailing slug segment that a
# branch name always carries. A bare month-and-day and a bare month-and-year
# stay clean; the same string followed by a hyphen and a slug word does not.
RULES=(
  'BRAND_NAME|-|\bRens[e]i\b|closed product brand (use Donmai, or allowlist parent-brand attribution)'
  'TRACKER_ID|-|(^|[^0-9A-Za-z])((R[E]N2|R[E]NOPS|R[E]N|S[U]P)-?[0-9]+|M[A]R-[0-9]+)([^0-9A-Za-z]|$)|internal tracker issue ID'
  'TRACKER_ID_SLUG|-|(^|[^0-9A-Za-z])((r[e]n2|r[e]nops|r[e]n|s[u]p)-?[0-9]+([^0-9A-Za-z]|$)|m[a]r-?[0-9]+-)|internal tracker ID in a branch / worktree / task-list slug'
  'TRACKER_ID_TITLECASE|-|(^|[^0-9A-Za-z])((R[e]n2|R[e]nOps|R[e]nops|R[e]n|S[u]p)-?[0-9]+([^0-9A-Za-z]|$)|M[a]r-?[0-9]+-)|internal tracker ID in title case'
  'TRACKER_URL|-|(^|[^.[:alnum:]-])linear\.app/[A-Za-z0-9_-]+|internal tracker deep link (any workspace, team key or path)'
  'CLOSED_CLI_ENDPOINT|-|/api/cl[i]/|closed control-plane CLI endpoint (only /api/daemon/* is OSS-shipped)'
  'CLOSED_TUI_REPO|-|rens[e]i-tui|closed-source TUI repo name'
  'CLOSED_PLATFORM|-|rens[e]i-platform|closed-source platform moniker'
  'PARENT_DOMAIN|-|rens[e]i\.ai|parent-company domain (allowlist legitimate parent-brand URLs)'
  'PLATFORM_PATH|-|(^|[^0-9A-Za-z])platform/(src|app|api|lib|components|drizzle|migrations|e2e|sdk|scripts|contracts|types|clickhouse|public|docs|tests)/|internal monorepo path prefix'
  'DEV_ABS_PATH|-|/Us[e]rs/[^/[:space:]]+/|developer absolute path'
  'CLOSED_ENV_VAR|-|RENS[E]I_[A-Z_]+|closed-source environment variable name'
  'PROD_METRIC|-|(([0-9]{1,3}(,[0-9]{3}){2,}|[0-9]{7,})[[:space:]]+([a-z][a-z-]*[[:space:]]+){0,2}(record|row|event|session|run|job|org|organi[sz]ation|tenant|user|deliver(y|ies)|span|dispatch|message|request|invocation|entr(y|ies))(e?s)?\b|\b(in|on|across) production\b[^.]{0,60}[0-9]{1,3}(,[0-9]{3})+)|production measurement of the closed control plane (operational data, not architecture)'
)

RULE_IDS=""
for _r in "${RULES[@]}"; do
  RULE_IDS="${RULE_IDS},${_r%%|*}"
done
RULE_IDS="${RULE_IDS},"

# ---- Parse args ----
FILES=()
COMMIT_RANGE=""
STDIN_LABEL=""
WANT=""
for arg in "$@"; do
  if [[ -n "$WANT" ]]; then
    case "$WANT" in
      range) COMMIT_RANGE="$arg" ;;
      label) STDIN_LABEL="$arg" ;;
    esac
    WANT=""
    continue
  fi
  case "$arg" in
    --staged)
      while IFS= read -r f; do
        [[ -n "$f" ]] && FILES+=("$f")
      done < <(git diff --cached --name-only --diff-filter=ACMR)
      ;;
    --all)
      while IFS= read -r f; do
        [[ -n "$f" ]] && FILES+=("$f")
      done < <(git ls-files)
      ;;
    --commits) WANT=range ;;
    --stdin) WANT=label ;;
    --punch-list) PUNCH_LIST_MODE=true ;;
    -*)
      echo "guard-b: unknown flag: $arg" >&2
      exit 2
      ;;
    *) FILES+=("$arg") ;;
  esac
done

if [[ -n "$WANT" ]]; then
  echo "guard-b: --${WANT/range/commits} requires an argument." >&2
  exit 2
fi

if [[ ${#FILES[@]} -eq 0 && -z "$COMMIT_RANGE" && -z "$STDIN_LABEL" ]]; then
  echo "No files to scan. Pass --staged, --all, --commits <range>, --stdin <label>, or file paths." >&2
  exit 0
fi

STDIN_TEXT=""
if [[ -n "$STDIN_LABEL" ]]; then
  STDIN_TEXT="$(cat)"
fi

# ---- Load allowlist, refusing malformed and blanket entries ----
refuse_allowlist() {
  local lineno="$1" why="$2" entry="$3"
  echo "guard-b: $ALLOWLIST:$lineno — REFUSED: $why" >&2
  echo "  entry: $entry" >&2
  echo "" >&2
  echo "  Entry grammar:" >&2
  echo "    <RULE_ID>[,<RULE_ID>...] :: <location-regex> :: <match-regex>[ :: <line-regex>]" >&2
  echo "" >&2
  echo "  Every rule ID must be one this guard defines, spelled in full." >&2
  echo "  <location-regex> must be '^'-anchored and must start on a literal" >&2
  echo "  path character, so it names a file rather than matching every" >&2
  echo "  location. <match-regex> must bind the identifier being exempted, so" >&2
  echo "  a second, unlisted one on the same line is still reported. No field" >&2
  echo "  may be a wildcard: a blanket entry disables the guard silently, and" >&2
  echo "  that is how a gate stops being evidence. Narrow it, or fix the line." >&2
  exit 2
}

# is_blanket <regex> — 0 when the regex carries no real literal to bind on.
is_blanket() {
  local stripped
  stripped="$(printf '%s' "$1" | tr -d '.*+?^$()|[]{}\\ ')"
  [[ ${#stripped} -lt 2 ]]
}

ALLOW_RULES=()
ALLOW_LOC=()
ALLOW_MATCH=()
ALLOW_LINE=()
if [[ -f "$ALLOWLIST" ]]; then
  al_lineno=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    al_lineno=$((al_lineno + 1))
    [[ "$line" =~ ^[[:space:]]*# || -z "$line" ]] && continue

    case "$line" in
      *' :: '*) ;;
      *) refuse_allowlist "$al_lineno" "entry has no ' :: ' field separators" "$line" ;;
    esac

    al_rules="${line%% :: *}"
    al_rest="${line#* :: }"
    al_loc="${al_rest%% :: *}"
    al_match=""
    al_line=""
    if [[ "$al_rest" == *' :: '* ]]; then
      al_rest2="${al_rest#* :: }"
      al_match="${al_rest2%% :: *}"
      if [[ "$al_rest2" == *' :: '* ]]; then
        al_line="${al_rest2#* :: }"
        [[ "$al_line" == *' :: '* ]] && \
          refuse_allowlist "$al_lineno" "entry has more than four fields" "$line"
      fi
    else
      refuse_allowlist "$al_lineno" "entry has fewer than three fields" "$line"
    fi

    # (a) every rule ID must exist, spelled in full.
    [[ -n "$al_rules" ]] || refuse_allowlist "$al_lineno" "entry names no rule" "$line"
    al_norm=""
    IFS=',' read -r -a al_rule_arr <<< "$al_rules"
    for al_rid in "${al_rule_arr[@]}"; do
      al_rid="${al_rid//[[:space:]]/}"
      [[ -n "$al_rid" ]] || refuse_allowlist "$al_lineno" "empty rule ID in the scope list" "$line"
      case "$RULE_IDS" in
        *",$al_rid,"*) ;;
        *) refuse_allowlist "$al_lineno" "unknown rule ID '$al_rid' (a typo exempts nothing, or everything)" "$line" ;;
      esac
      al_norm="${al_norm},${al_rid}"
    done
    al_norm="${al_norm},"

    # (b) the location regex must name a file, not match every location.
    [[ "$al_loc" == '^'* ]] || refuse_allowlist "$al_lineno" "location regex is not anchored with '^'" "$line"
    al_head="${al_loc#^}"
    al_head="${al_head#(}"
    [[ "$al_head" =~ ^[A-Za-z0-9_/-] ]] || \
      refuse_allowlist "$al_lineno" "location regex must start on a literal path character, not a wildcard" "$line"
    is_blanket "$al_loc" && refuse_allowlist "$al_lineno" "location regex is blanket" "$line"

    # (c) the match regex must bind the identifier being exempted.
    is_blanket "$al_match" && \
      refuse_allowlist "$al_lineno" "match regex is blanket — it would exempt every hit of these rules in that file" "$line"

    # (d) the optional line regex, if present, must also be concrete.
    if [[ -n "$al_line" ]]; then
      is_blanket "$al_line" && refuse_allowlist "$al_lineno" "line regex is blanket" "$line"
    fi

    ALLOW_RULES+=("$al_norm")
    ALLOW_LOC+=("$al_loc")
    ALLOW_MATCH+=("$al_match")
    ALLOW_LINE+=("$al_line")
  done < "$ALLOWLIST"
fi

# is_allowed <rule-id> <location> <line-number> <matched-text> <line-content>
is_allowed() {
  local rid="$1" loc="$2" lno="$3" occ="$4" content="$5" i
  for ((i = 0; i < ${#ALLOW_RULES[@]}; i++)); do
    case "${ALLOW_RULES[$i]}" in
      *",$rid,"*) ;;
      *) continue ;;
    esac
    printf '%s' "$loc:$lno" | grep -qE -- "${ALLOW_LOC[$i]}" || continue
    printf '%s' "$occ" | grep -qE -- "${ALLOW_MATCH[$i]}" || continue
    if [[ -n "${ALLOW_LINE[$i]}" ]]; then
      printf '%s' "$content" | grep -qE -- "${ALLOW_LINE[$i]}" || continue
    fi
    return 0
  done
  return 1
}

# ---- Binary files: scanned as text, and named ----
# `grep -I` used to be in the flag set, so a single NUL byte anywhere in a file
# made grep report no matches for the whole file and the guard said nothing
# about the skip — a silent, file-sized hole. We scan with -a instead and list
# every binary file in scope so a reviewer can see what was text-scanned.
BINARY_FILES=()
for file in "${FILES[@]}"; do
  [[ -f "$file" && -s "$file" ]] || continue
  LC_ALL=C grep -qI -- '' "$file" 2>/dev/null || BINARY_FILES+=("$file")
done
if [[ ${#BINARY_FILES[@]} -gt 0 ]]; then
  echo "guard-b: NOTICE — ${#BINARY_FILES[@]} binary file(s) in scope, scanned as text (-a):"
  printf '  %s\n' "${BINARY_FILES[@]}"
fi

# ---- Non-file sources, flattened once ----
# Commit messages, the composed squash message and the head branch name are
# published text that is not a file, so a file scan cannot see them. Flattening
# once here rather than re-reading inside the rule loop keeps the cost at one
# `git log` per commit instead of one per commit per rule.
#
# Content and location go to two PARALLEL files, line for line, and the rules
# are matched against the content only. Prefixing the content with its location
# would let the location itself match — a branch name in a `Merge branch ...`
# subject would flag every line of that commit's body.
NONFILE_SRC="$(mktemp)"
NONFILE_LOC="$(mktemp)"
trap 'rm -f "$NONFILE_SRC" "$NONFILE_LOC" "$NONFILE_SRC.one"' EXIT

# Merge commits are NOT excluded. `--no-merges` used to be on this rev-list, and
# a merge commit is the one place a branch slug lands in published history —
# precisely the shape TRACKER_ID_SLUG exists to catch.
if [[ -n "$COMMIT_RANGE" ]]; then
  while IFS= read -r sha; do
    [[ -n "$sha" ]] || continue
    git log -1 --format='%s%n%b' "$sha" > "$NONFILE_SRC.one"
    cat "$NONFILE_SRC.one" >> "$NONFILE_SRC"
    awk -v p="commit-message:${sha:0:12}" '{ print p "\t" NR }' "$NONFILE_SRC.one" >> "$NONFILE_LOC"
  done < <(git rev-list "$COMMIT_RANGE" 2>/dev/null || true)
  rm -f "$NONFILE_SRC.one"
fi

if [[ -n "$STDIN_LABEL" ]]; then
  printf '%s\n' "$STDIN_TEXT" >> "$NONFILE_SRC"
  printf '%s\n' "$STDIN_TEXT" | awk -v p="$STDIN_LABEL" '{ print p "\t" NR }' >> "$NONFILE_LOC"
fi

# ---- Scan ----
VIOLATIONS=()

# record_line <rule-id> <pattern> <flags> <description> <location> <lineno> <content>
#
# One violation per DISTINCT MATCHED OCCURRENCE, not one per matching line. The
# allowlist is consulted per occurrence, so exempting one identifier cannot
# exempt a different one that happens to share its line.
record_line() {
  local rule_id="$1" pattern="$2" flags="$3" description="$4"
  local loc="$5" lineno="$6" content="$7"
  local occ found=0
  local oflags=(-o -E -a)
  [[ "$flags" == "i" ]] && oflags+=(-i)
  while IFS= read -r occ; do
    [[ -n "$occ" ]] || continue
    found=1
    is_allowed "$rule_id" "$loc" "$lineno" "$occ" "$content" && continue
    VIOLATIONS+=("$loc:$lineno:$content  [rule: $rule_id — $description]  [match: $occ]")
  done < <(printf '%s\n' "$content" | grep "${oflags[@]}" -- "$pattern" 2>/dev/null | awk '!seen[$0]++')
  # The line matched but no occurrence could be re-extracted — possible when a
  # NUL byte mangles the content on its way through the shell. Report it rather
  # than dropping it: an unreportable match is still a match.
  if [[ $found -eq 0 ]]; then
    VIOLATIONS+=("$loc:$lineno:$content  [rule: $rule_id — $description]  [match: <unresolved>]")
  fi
}

for rule in "${RULES[@]}"; do
  rule_id="${rule%%|*}"
  rest="${rule#*|}"
  flags="${rest%%|*}"
  rest="${rest#*|}"
  description="${rest##*|}"
  pattern="${rest%|*}"

  line_flags=(-n -E -a)
  if [[ "$flags" == "i" ]]; then
    line_flags+=(-i)
  fi

  # --- files ---
  for file in "${FILES[@]}"; do
    [[ -f "$file" ]] || continue
    while IFS= read -r match; do
      [[ -n "$match" ]] || continue
      record_line "$rule_id" "$pattern" "$flags" "$description" \
        "$file" "${match%%:*}" "${match#*:}"
    done < <(grep "${line_flags[@]}" -- "$pattern" "$file" 2>/dev/null || true)
  done

  # --- commit messages, the composed squash message, the head branch name ---
  if [[ -s "$NONFILE_SRC" ]]; then
    while IFS= read -r match; do
      [[ -n "$match" ]] || continue
      nf_n="${match%%:*}"
      nf_content="${match#*:}"
      nf_loc_rec="$(awk -v n="$nf_n" 'NR == n { print; exit }' "$NONFILE_LOC")"
      record_line "$rule_id" "$pattern" "$flags" "$description" \
        "${nf_loc_rec%%$'\t'*}" "${nf_loc_rec##*$'\t'}" "$nf_content"
    done < <(grep "${line_flags[@]}" -- "$pattern" "$NONFILE_SRC" || true)
  fi
done

# ---- Output ----
if [[ ${#VIOLATIONS[@]} -eq 0 ]]; then
  echo "guard-b: OK — no closed-source content violations found."
  exit 0
fi

echo ""
echo "guard-b: VIOLATIONS FOUND (${#VIOLATIONS[@]})"
echo "------------------------------------------------------------"
for v in "${VIOLATIONS[@]}"; do
  echo "  $v"
done
echo "------------------------------------------------------------"
echo ""
echo "Rewrite the content to describe the behaviour instead of citing internal"
echo "trackers, repos, endpoints, hosts or production measurements. To allowlist"
echo "one specific identifier on one specific file, add an entry to"
echo ".guard-allowlist:"
echo "  <RULE_ID>[,<RULE_ID>...] :: <location-regex> :: <match-regex>[ :: <line-regex>]"
echo "Spell each rule ID in full, anchor the location on a literal filename, and"
echo "bind the match regex to the identifier itself — never to the whole line."
echo "Malformed or blanket entries are refused outright."
echo ""

if $PUNCH_LIST_MODE; then
  printf '%s\n' "${VIOLATIONS[@]}" > GUARD-B-VIOLATIONS.txt
  echo "Punch list written to GUARD-B-VIOLATIONS.txt"
fi

exit 1
