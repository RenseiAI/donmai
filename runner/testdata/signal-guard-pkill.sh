#!/bin/sh
# donmai session-scoped pkill guard. Ships with the runner (go:embed) and is
# installed per session ahead of the session's PATH: a pattern kill from
# inside the session signals only processes whose working directory is inside
# the session's worktree (or workarea root), or that belong to the session's
# recorded process group. Every other match is refused and reported.
#
# Scope: DONMAI_SESSION_WORKTREE (required), DONMAI_SESSION_WORKAREA_ROOT
# (optional), DONMAI_SESSION_PGID (optional), DONMAI_SIGNAL_GUARD_EVENT_LOG
# (optional refusal log the runner drains into typed activity events).
#
# Refused targets are never signalled: when nothing is in scope the guard
# exits 1 (no process matched) without signalling anyone. Kill by PID is
# unaffected — this guard only narrows pattern selection; "kill <pid>"
# still addresses any process the session may signal.
#
# Path comparison canonicalises symlinks (macOS mounts /tmp and /var under
# /private; lsof reports the canonical cwd while the worktree variable may
# carry the symlinked spelling). realpath is preferred; readlink -f is the
# fallback; when neither resolves, the raw strings are compared.
#
# Tool lookup is PATH-independent: the guard resolves pgrep/ps/lsof/tr/sed/
# head/cut/realpath/readlink/kill against a fixed directory list, because a
# session may invoke the guard with a minimal PATH that omits /usr/sbin
# (where lsof lives on macOS) or other tool directories.
set -u

TOOL_DIRS="/usr/bin /bin /usr/sbin /sbin"

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
SED="$(resolve_tool sed)" || SED=""
HEAD="$(resolve_tool head)" || HEAD=""
CUT="$(resolve_tool cut)" || CUT=""
REALPATH="$(resolve_tool realpath)" || REALPATH=""
READLINK="$(resolve_tool readlink)" || READLINK=""

WORKTREE="${DONMAI_SESSION_WORKTREE:-}"
EVENT_LOG="${DONMAI_SIGNAL_GUARD_EVENT_LOG:-}"
SESSION_PGID="${DONMAI_SESSION_PGID:-}"

real_pkill() {
  for dir in /usr/bin /bin; do
    if [ -x "$dir/pkill" ] && [ "$dir/pkill" != "$0" ]; then
      exec "$dir/pkill" "$@"
    fi
  done
  echo "donmai-signal-guard: no system pkill found" >&2
  exit 127
}

# Outside a session worktree there is no scope to enforce: behave like pkill.
if [ -z "$WORKTREE" ]; then
  real_pkill "$@"
fi

# Without the inspection tools the guard cannot scope anything: refuse
# closed rather than signalling blindly.
if [ -z "$PGREP" ] || [ -z "$LSOF" ]; then
  echo "donmai-signal-guard: cannot scope pattern kill (missing pgrep/lsof); kill by PID instead." >&2
  if [ -n "$EVENT_LOG" ]; then
    printf '%s\n' "refused pkill $*: scope inspection unavailable" >>"$EVENT_LOG" 2>/dev/null || true
  fi
  exit 1
fi

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
  # $1 = candidate cwd. True when it is the worktree, the workarea root, or
  # a descendant of either. String-prefix with a separator so a sibling
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
  [ -n "$PS" ] || return 1
  _pgid="$("$PS" -p "$_pid" -o pgid= 2>/dev/null | "$TR" -d ' ')" || return 1
  [ -n "$_pgid" ] && [ "$_pgid" = "$SESSION_PGID" ]
}

log_refusal() {
  # $1 = human-readable refusal line. Best-effort append to the event log.
  [ -n "$EVENT_LOG" ] || return 0
  printf '%s\n' "$1" >>"$EVENT_LOG" 2>/dev/null || true
}

# Collect the pids pkill would signal. "-f pattern" semantics are matched by
# pgrep -f over the same trailing arguments; an explicit pid operand list
# (no -f flag) is an in-scope PID kill and passes through untouched.
pattern=""
signal_args=""
pids_from_args=""
for arg in "$@"; do
  case "$arg" in
    -f) want_pattern=1 ;;
    -[0-9]*|-[A-Z]*|--*) signal_args="$signal_args $arg" ;;
    -*) signal_args="$signal_args $arg" ;;
    *)
      if [ "${want_pattern:-0}" = "1" ] && [ -z "$pattern" ]; then
        pattern="$arg"
      else
        pids_from_args="$pids_from_args $arg"
      fi
      ;;
  esac
done

if [ -z "$pattern" ]; then
  # No pattern: explicit pids, or a flag-only invocation pkill itself will
  # reject. Either way there is no cross-session blast radius to narrow.
  real_pkill "$@"
fi

matched="$("$PGREP" -f -- "$pattern" 2>/dev/null)" || {
  # pgrep found nothing: keep pkill's own exit code and output.
  real_pkill "$@"
}

# pgrep -f matches the full command line, which includes the pgrep
# invocation itself when the pattern names the shell running this guard
# (e.g. a test harness whose own command line contains the marker), and
# the wrapper shells that spawned the session's sleepers (whose command
# lines embed the marker while their cwd is the harness's, not the
# session's). Neither is a kill target on its own: the wrappers are
# skipped by identity — their child, which shares their command-line
# marker but runs with the session's cwd, is still scoped normally — so
# drop this guard's own ancestry (every pid on the path from here to
# init) plus pid 1 before scoping. Ancestor pids are collected from
# $PPID upward via ps; the loop is bounded so a wedged ps cannot hang
# the guard.
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
for pid in $matched; do
  # Never signal init, and never scope-check the guard's own ancestry:
  # pgrep -f matches full command lines, so the shells that spawned the
  # session's processes (and the pattern-bearing harness above them)
  # match the pattern too. They are infrastructure, not targets.
  if [ "$pid" = "1" ] || is_ancestor "$pid"; then
    continue
  fi
  cwd="$("$LSOF" -p "$pid" -a -d cwd -F n 2>/dev/null | "$SED" -n 's/^n//p' | "$HEAD" -n 1)"
  if [ -n "$cwd" ] && in_scope_dir "$cwd"; then
    allowed="$allowed $pid"
  elif in_scope_group "$pid"; then
    allowed="$allowed $pid"
  else
    refused="$refused $pid"
  fi
done

if [ -n "$refused" ]; then
  log_refusal "refused pkill -f '$pattern': pid(s)$refused outside session scope (worktree $WORKTREE)"
  printf 'donmai-signal-guard: refusing to signal pid(s)%s outside your session worktree (pattern %s). Kill by PID instead.\n' "$refused" "$pattern" >&2
fi

if [ -z "$allowed" ]; then
  # Nothing in scope: report pkill's no-match status without signalling.
  exit 1
fi

# Signal the in-scope pids directly with kill: re-applying the pattern to
# the real pkill would re-match the out-of-scope processes just refused,
# and pkill's pid-operand form does not signal by pid on every platform.
# The pattern is preserved in the refusal log line. The signal flag
# (default TERM) is extracted from the original arguments.
_sig="TERM"
for _a in $signal_args; do
  case "$_a" in
    -9) _sig="KILL" ;;
    -[A-Z]*) _sig="$("$CUT" -c2- <<EOF
$_a
EOF
)" ;;
  esac
done
# shellcheck disable=SC2086
kill -s "$_sig" $allowed
