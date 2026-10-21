#!/bin/sh
# donmai session-scoped signal guard. Ships with the runner (go:embed) and is
# installed per session ahead of the session's PATH, once under the name
# "pkill" and once under the name "killall": the basename of argv[0]
# selects the pkill or the killall matching semantics below. Any other name
# is refused closed.
#
# Contract: a pattern kill from inside the session signals only processes
# whose working directory is inside the session's worktree (or workarea
# root), or that belong to the session's recorded process group. Every other
# match is refused and reported. Kill by PID is unaffected: this guard only
# narrows pattern selection.
#
# Fail-closed rules (the whole point of this file):
# - pkill takes no PID operands: EVERY non-flag operand is a pattern, with
#   or without -f. `pkill node`, `pkill -9 vitest` and combined shorts such
#   as -9f are scoped exactly like `pkill -f`. More than one pattern
#   operand is refused (the real pkill accepts only one).
# - Only flags the guard can model exactly are accepted. For pkill: -f and
#   -x (passed through to the guard's own pgrep call) and signal specs
#   (-9, -TERM, ...). For killall: signal specs only. Anything else
#   (-i, -v, -n, -o, -u, -t, -m, -s, -q, -e, -I, -l, -d, -F, --help, ...)
#   is refused without signalling, because passing it through would either
#   widen the match set (negations, user/tty selectors) or mean different
#   things per platform. In particular killall -s is a dry-run flag on
#   macOS/BSD but takes a signal argument on Linux, so it cannot be
#   modelled safely anywhere and is always refused.
# - When the guard's own match comes back empty, the guard exits 1 (the
#   no-match status) WITHOUT re-execing the real tool: re-execing would
#   signal out-of-scope processes through a match the guard just declined
#   to model (for example the case-insensitive -i form above).
# - Without the inspection tools (pgrep, ps, lsof, tr) the guard cannot
#   scope anything and refuses closed rather than signalling blindly.
# - An unknown argv[0] basename and an empty pattern or name are refused
#   closed: an empty pgrep pattern would match every process.
#
# Scope: DONMAI_SESSION_WORKTREE (required; when empty the guard is outside
# a session and defers to the real tool), DONMAI_SESSION_WORKAREA_ROOT
# (optional), DONMAI_SESSION_PGID (optional; otherwise read from
# $DONMAI_SIGNAL_GUARD_DIR/session.pgid, which the runner writes when the
# harness pid is observed), DONMAI_SIGNAL_GUARD_DIR (the runner-owned
# directory holding this guard and the pgid file), DONMAI_SIGNAL_GUARD_EVENT_LOG
# (optional refusal log the runner drains into typed activity events).
# DONMAI_SIGNAL_GUARD_TOOL_DIRS overrides the fixed tool search path
# (testing seam for the missing-tool fail-closed cases; production uses
# the fixed list because a session may invoke the guard with a minimal
# PATH that omits directories such as /usr/sbin).
#
# Path comparison canonicalises symlinks (macOS mounts /tmp and /var under
# /private; lsof reports the canonical cwd while the worktree variable may
# carry the symlinked spelling). realpath is preferred; readlink -f is the
# fallback; when neither resolves, the raw strings are compared. The
# separator check ("$W"|"$W"/*) keeps a sibling directory ("sess-evil" vs
# "sess") out of scope.
#
# Ancestry: like the real pkill/pgrep, the guard never signals itself, pid
# 1, or its own ancestry. pgrep -f matches full command lines, so the guard
# invocation itself (whose arguments carry the pattern) and any wrapper
# shell that spawned the session's processes would otherwise match.
set -u

# The original invocation for refusal messages (functions below must not
# use $* directly: inside a function that would be the function's args).
ORIG_ARGS="$*"

MODE="${0##*/}"
case "$MODE" in
  pkill|killall) ;;
  *)
    echo "donmai-signal-guard: refusing to run under unexpected name '$MODE'; kill by PID instead." >&2
    exit 1
    ;;
esac

TOOL_DIRS="${DONMAI_SIGNAL_GUARD_TOOL_DIRS:-/usr/bin /bin /usr/sbin /sbin}"

resolve_tool() {
  # $1 = tool name. Print its absolute path or nothing.
  _t="$1"
  case "$_t" in
    */*) [ -x "$_t" ] && printf '%s' "$_t" && return 0 || return 1 ;;
  esac
  for _d in $TOOL_DIRS; do
    if [ -x "$_d/$_t" ]; then
      printf '%s' "$_d/$_t"
      return 0
    fi
  done
  return 1
}

PGREP="$(resolve_tool pgrep)" || PGREP=""
PS="$(resolve_tool ps)" || PS=""
LSOF="$(resolve_tool lsof)" || LSOF=""
TR="$(resolve_tool tr)" || TR=""
REALPATH="$(resolve_tool realpath)" || REALPATH=""
READLINK="$(resolve_tool readlink)" || READLINK=""

WORKTREE="${DONMAI_SESSION_WORKTREE:-}"
EVENT_LOG="${DONMAI_SIGNAL_GUARD_EVENT_LOG:-}"
SESSION_PGID="${DONMAI_SESSION_PGID:-}"
GUARD_DIR="${DONMAI_SIGNAL_GUARD_DIR:-}"

real_tool() {
  for dir in /usr/bin /bin; do
    if [ -x "$dir/$MODE" ] && [ "$dir/$MODE" != "$0" ]; then
      exec "$dir/$MODE" "$@"
    fi
  done
  echo "donmai-signal-guard: no system $MODE found" >&2
  exit 127
}

# Outside a session worktree there is no scope to enforce: behave like the
# real tool. (The runner always sets the worktree inside a session.)
if [ -z "$WORKTREE" ]; then
  real_tool "$@"
fi

# Without the inspection tools the guard cannot scope anything: refuse
# closed rather than signalling blindly.
if [ -z "$PGREP" ] || [ -z "$PS" ] || [ -z "$LSOF" ] || [ -z "$TR" ]; then
  echo "donmai-signal-guard: cannot scope $MODE selection (missing inspection tools); kill by PID instead." >&2
  if [ -n "$EVENT_LOG" ]; then
    printf '%s\n' "refused $MODE $*: scope inspection unavailable" >>"$EVENT_LOG" 2>/dev/null || true
  fi
  exit 1
fi

# The runner-owned guard directory also carries the recorded process-group
# id. When argv[0] holds a path, its directory is that guard directory; a
# bare name (PATH lookup) cannot name it, so the runner also stamps it via
# the environment.
case "$0" in
  */*) GUARD_DIR="$(dirname "$0")" ;;
esac
if [ -z "$SESSION_PGID" ] && [ -n "$GUARD_DIR" ] && [ -f "$GUARD_DIR/session.pgid" ]; then
  if read -r _pgid_line <"$GUARD_DIR/session.pgid" 2>/dev/null; then
    SESSION_PGID="$_pgid_line"
  fi
fi
case "$SESSION_PGID" in
  ''|*[!0-9]*|"0") SESSION_PGID="" ;;
esac

canon() {
  # $1 = path. Print the canonical spelling (symlinks resolved).
  _c=""
  if [ -n "$REALPATH" ]; then
    _c="$("$REALPATH" "$1" 2>/dev/null)"
  fi
  if [ -z "$_c" ] && [ -n "$READLINK" ]; then
    _c="$("$READLINK" -f "$1" 2>/dev/null)"
  fi
  if [ -n "$_c" ]; then
    printf '%s' "$_c"
  else
    printf '%s' "$1"
  fi
}

WORKTREE_CANON="$(canon "$WORKTREE")"
if [ -n "${DONMAI_SESSION_WORKAREA_ROOT:-}" ]; then
  WORKAREA_CANON="$(canon "$DONMAI_SESSION_WORKAREA_ROOT")"
else
  WORKAREA_CANON=""
fi

in_scope_dir() {
  # $1 = candidate cwd. True when it is the worktree, the workarea root,
  # or a descendant of either. String-prefix with a separator so a sibling
  # ("sess-evil" vs "sess") never matches.
  _p="$(canon "$1")"
  case "$_p" in
    "$WORKTREE_CANON"|"$WORKTREE_CANON"/*) return 0 ;;
  esac
  if [ -n "$WORKAREA_CANON" ]; then
    case "$_p" in
      "$WORKAREA_CANON"|"$WORKAREA_CANON"/*) return 0 ;;
    esac
  fi
  return 1
}

in_scope_group() {
  # $1 = pid. True when the session recorded a process group and ps shows
  # the pid is a member of it.
  _pid="$1"
  [ -n "$SESSION_PGID" ] || return 1
  _pgid="$("$PS" -p "$_pid" -o pgid= 2>/dev/null | "$TR" -d ' ')" || return 1
  [ -n "$_pgid" ] && [ "$_pgid" = "$SESSION_PGID" ]
}

log_refusal() {
  # $1 = human-readable refusal line. Best-effort append to the event log.
  [ -n "$EVENT_LOG" ] || return 0
  printf '%s\n' "$1" >>"$EVENT_LOG" 2>/dev/null || true
}

refuse_closed() {
  # $1 = reason. Log, warn, exit 1 without signalling anything.
  log_refusal "refused $MODE $ORIG_ARGS: $1"
  printf 'donmai-signal-guard: refusing %s %s: %s Kill by PID instead.\n' "$MODE" "$ORIG_ARGS" "$1" >&2
  exit 1
}

# Argument grammar. pkill takes no PID operands: every non-flag operand is
# a pattern (exactly one is allowed); killall takes one or more process
# names. Signal specs (-9, -TERM) select the signal for the in-scope pids
# and never widen matching. -f and -x (pkill only) are passed through to
# the guard's own pgrep call so matching stays identical to the real tool.
# Every other flag is refused closed (see the header). Signal names are
# uppercase (-TERM, -HUP, -USR1); a lone letter or lowercase word is a
# combined-short cluster the guard does not model (-fi, -F, ...) and is
# refused rather than misread as a signal.
full=0
exact=0
sig="-TERM"
operands=""
noperands=0
ended=0
for arg in "$@"; do
  if [ "$ended" = "1" ]; then
    operands="$operands $arg"
    noperands=$((noperands + 1))
    continue
  fi
  case "$arg" in
    --) ended=1 ;;
    -)
      # A lone dash is not a flag: the real tool reads it as an operand.
      operands="$operands $arg"
      noperands=$((noperands + 1))
      ;;
    -f)
      if [ "$MODE" = "killall" ]; then
        refuse_closed "unsupported flag -f."
      fi
      full=1
      ;;
    -x)
      if [ "$MODE" = "killall" ]; then
        refuse_closed "unsupported flag -x."
      fi
      exact=1
      ;;
    -*)
      _tok="${arg#-}"
      case "$_tok" in
        # A combined-short cluster of digits and f (e.g. -9f, -f9)
        # means signal <digits> with full matching. A repeated -f
        # (-ff) is full matching with no signal change. Anything else
        # in the cluster (-fi, -if, -F, ...) is unmodelled: refuse.
        *[!0-9f]*)
          # Not a digit/f cluster: a signal name or an unknown flag.
          # Signal names are uppercase words of length 2+ (-TERM,
          # -HUP, -USR1, -IO); a lone letter (-F pidfile, -I confirm,
          # -V, ...) or a lowercase word is a flag cluster the guard
          # does not model: refuse rather than misread it as a signal.
          case "$_tok" in
            [A-Z]) refuse_closed "unsupported flag $arg." ;;
            [A-Z]*)
              case "$_tok" in
                *[!A-Z0-9]*) refuse_closed "unsupported flag $arg." ;;
                *) sig="-$_tok" ;;
              esac
              ;;
            *) refuse_closed "unsupported flag $arg." ;;
          esac
          ;;
        *)
          case "$_tok" in
            *f*)
              if [ "$MODE" = "killall" ]; then
                refuse_closed "unsupported flag $arg."
              fi
              full=1
              ;;
          esac
          _digits=""
          _rest="$_tok"
          while [ -n "$_rest" ]; do
            _ch="${_rest%"${_rest#?}"}"
            _rest="${_rest#?}"
            case "$_ch" in
              [0-9]) _digits="$_digits$_ch" ;;
            esac
          done
          if [ -n "$_digits" ]; then
            sig="-$_digits"
          fi
          ;;
      esac
      ;;
    *)
      operands="$operands $arg"
      noperands=$((noperands + 1))
      ;;
  esac
done

if [ "$MODE" = "pkill" ]; then
  if [ "$noperands" -eq 0 ]; then
    # No pattern: a flag-only invocation the real pkill itself rejects.
    # Nothing to narrow, so defer to it.
    real_tool "$@"
  fi
  if [ "$noperands" -gt 1 ]; then
    refuse_closed "only one pattern operand is supported ($noperands given)."
  fi
  # The single pattern is $operands trimmed of its leading blank.
  pattern="${operands# }"
  if [ -z "$pattern" ]; then
    refuse_closed "an empty pattern would match every process."
  fi
else
  if [ "$noperands" -eq 0 ]; then
    # No name: a flag-only invocation the real killall itself rejects.
    real_tool "$@"
  fi
fi

# pgrep with the caller's own matching semantics: -f matches the full
# command line, -x requires an exact name, otherwise the process name.
# A -f pattern carrying -x (pkill -fx) is exact on the full command line,
# exactly like the real tool.
pgrep_flags=""
[ "$full" = "1" ] && pgrep_flags="$pgrep_flags -f"
[ "$exact" = "1" ] && pgrep_flags="$pgrep_flags -x"

pgrep_failed=0
if [ "$MODE" = "pkill" ]; then
  # shellcheck disable=SC2086
  matched="$("$PGREP" $pgrep_flags -- "$pattern" 2>/dev/null)"
  _pgrep_rc=$?
  if [ "$_pgrep_rc" -ne 0 ]; then
    matched=""
    if [ "$_pgrep_rc" -gt 1 ]; then
      pgrep_failed=1
    fi
  fi
else
  # Every name operand is scoped, not just the last: a trailing unknown
  # name must not route the invocation to the unscoped real tool.
  # Names are re-derived from "$@" quoted (one iteration per operand,
  # empty preserved) instead of the space-joined $operands string, which
  # would split a quoted "a b" into two lookups and glob-expand "*".
  matched=""
  _ended2=0
  for _a in "$@"; do
    if [ "$_ended2" = "1" ]; then
      _name="$_a"
      if [ -z "$_name" ]; then
        refuse_closed "an empty process name would match every process."
      fi
      _m="$("$PGREP" -x -- "$_name" 2>/dev/null)"
      _rc=$?
      if [ "$_rc" -ne 0 ]; then
        _m=""
        if [ "$_rc" -gt 1 ]; then
          pgrep_failed=1
        fi
      fi
      [ -n "$_m" ] && matched="$matched $_m"
      continue
    fi
    case "$_a" in
      --) _ended2=1 ;;
      -)
        _m="$("$PGREP" -x -- "-" 2>/dev/null)"
        _rc=$?
        if [ "$_rc" -ne 0 ]; then
          _m=""
          if [ "$_rc" -gt 1 ]; then
            pgrep_failed=1
          fi
        fi
        [ -n "$_m" ] && matched="$matched $_m"
        ;;
      -*) ;; # Signal spec (unmodelled flags already refused above): skip.
      *)
        _name="$_a"
        _m="$("$PGREP" -x -- "$_name" 2>/dev/null)"
        _rc=$?
        if [ "$_rc" -ne 0 ]; then
          _m=""
          if [ "$_rc" -gt 1 ]; then
            pgrep_failed=1
          fi
        fi
        [ -n "$_m" ] && matched="$matched $_m"
        ;;
    esac
  done
fi

if [ -z "$matched" ]; then
  if [ "$pgrep_failed" = "1" ]; then
    # The lookup itself failed (not merely no match): fail closed with a
    # refusal rather than silently reporting no-match. Re-execing the real
    # tool here would signal out-of-scope processes through an unscopable
    # match.
    refuse_closed "process match lookup failed."
  fi
  # Nothing matched: the real tool's own no-match status, without
  # signalling anyone. Never re-exec the real tool here (see header).
  exit 1
fi

# Never signal init, and never scope-check the guard's own ancestry:
# pgrep -f matches full command lines, so the shells that spawned the
# session's processes (and the pattern-bearing harness above them)
# match the pattern too. They are infrastructure, not targets.
# Ancestor pids are collected from $PPID upward via ps; the loop is
# bounded so a wedged ps cannot hang the guard.
ancestors=" $$ $PPID"
_ancestor_pid="$PPID"
_ancestor_steps=0
while [ "$_ancestor_pid" != "" ] && [ "$_ancestor_pid" != "0" ] && [ "$_ancestor_pid" != "1" ] && [ "$_ancestor_steps" -lt 32 ]; do
  _ancestor_ppid="$("$PS" -p "$_ancestor_pid" -o ppid= 2>/dev/null | "$TR" -d ' ')"
  [ -n "$_ancestor_ppid" ] || break
  ancestors="$ancestors $_ancestor_ppid"
  _ancestor_pid="$_ancestor_ppid"
  _ancestor_steps=$((_ancestor_steps + 1))
done

is_ancestor() {
  _q="$1"
  for _a in $ancestors; do
    [ "$_q" = "$_a" ] && return 0
  done
  return 1
}

allowed=""
refused=""
# A literal newline for splitting lsof output with parameter expansion
# only (no sed/head pipe, and no heredoc whose expansion pass would
# mangle cwd paths containing $ or backticks).
_nl='
'
deduped=""
for pid in $matched; do
  if [ "$pid" = "1" ] || is_ancestor "$pid"; then
    continue
  fi
  # Dedupe: repeated killall names list each pid once per name, so the
  # same pid can appear several times in matched. Signalling it twice
  # must not turn a success into an ESRCH failure below.
  case " $deduped " in
    *" $pid "*) continue ;;
  esac
  deduped="$deduped $pid"
  _lsof_out="$("$LSOF" -p "$pid" -a -d cwd -F n 2>/dev/null)"
  # Reset per pid: an empty or failed lsof lookup must not inherit the
  # previous pid's cwd (fail-open). An uninspectable pid stays out of
  # scope unless the process-group leg proves membership.
  cwd=""
  _nline=""
  case "$_lsof_out" in
    *"$_nl"n*) _nline="${_lsof_out##*"$_nl"n}" ;;
    n*) _nline="${_lsof_out#n}" ;;
  esac
  if [ -n "${_nline:-}" ]; then
    cwd="${_nline%%$_nl*}"
  fi
  if [ -n "$cwd" ] && in_scope_dir "$cwd"; then
    allowed="$allowed $pid"
  elif in_scope_group "$pid"; then
    allowed="$allowed $pid"
  else
    refused="$refused $pid"
  fi
done

if [ "$MODE" = "pkill" ]; then
  _flags=""
  [ "$full" = "1" ] && _flags="$_flags -f"
  [ "$exact" = "1" ] && _flags="$_flags -x"
  _sel="pkill$_flags '$pattern'"
else
  _sel="killall '${operands# }'"
fi

if [ -n "$refused" ]; then
  log_refusal "refused $_sel: pid(s)$refused outside session scope (worktree $WORKTREE)"
  printf 'donmai-signal-guard: refusing to signal pid(s)%s outside your session worktree (%s). Kill by PID instead.\n' "$refused" "$_sel" >&2
fi

if [ -z "$allowed" ]; then
  # Nothing in scope: report the no-match status without signalling.
  exit 1
fi

# Signal the in-scope pids directly with kill: re-applying the pattern to
# the real tool would re-match the out-of-scope processes just refused.
# The signal keeps its dash form (-9 stays numeric so -2 means INT rather
# than silently becoming TERM; names pass through as -NAME and kill
# itself rejects the unknown). Each pid is signalled separately so a pid
# that exits between the scope check and the signal (a reaped sleeper, a
# finished daemon child) cannot turn the whole invocation into a failure
# when other in-scope targets were signalled: exit 0 when at least one
# pid was signalled, matching pkill/killall semantics.
# shellcheck disable=SC2086
signalled=0
seen=""
for _kp in $allowed; do
  case " $seen " in
    *" $_kp "*) continue ;;
  esac
  seen="$seen $_kp"
  if kill "$sig" "$_kp" 2>/dev/null; then
    signalled=1
  fi
done
if [ "$signalled" = "1" ]; then
  exit 0
fi
exit 1
