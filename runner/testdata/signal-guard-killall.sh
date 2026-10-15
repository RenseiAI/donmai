#!/bin/sh
# donmai session-scoped killall guard. Same contract as the sibling pkill
# guard (see signal-guard-pkill.sh): only processes whose working directory
# is inside the session's worktree (or workarea root), or that belong to the
# session's recorded process group, may be signalled. The rest are refused
# and reported. Kill by PID is unaffected. Tool lookup is PATH-independent
# the same way the pkill guard's is (fixed directory list).
set -u

TOOL_DIRS="/usr/bin /bin /usr/sbin /sbin"

resolve_tool() {
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

real_killall() {
  for dir in /usr/bin /bin; do
    if [ -x "$dir/killall" ] && [ "$dir/killall" != "$0" ]; then
      exec "$dir/killall" "$@"
    fi
  done
  echo "donmai-signal-guard: no system killall found" >&2
  exit 127
}

if [ -z "$WORKTREE" ]; then
  real_killall "$@"
fi

if [ -z "$PGREP" ] || [ -z "$LSOF" ]; then
  echo "donmai-signal-guard: cannot scope kill by name (missing pgrep/lsof); kill by PID instead." >&2
  if [ -n "$EVENT_LOG" ]; then
    printf '%s\n' "refused killall $*: scope inspection unavailable" >>"$EVENT_LOG" 2>/dev/null || true
  fi
  exit 1
fi

canon() {
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
  _pid="$1"
  [ -n "$SESSION_PGID" ] || return 1
  [ -n "$PS" ] || return 1
  _pgid="$("$PS" -p "$_pid" -o pgid= 2>/dev/null | "$TR" -d ' ')" || return 1
  [ -n "$_pgid" ] && [ "$_pgid" = "$SESSION_PGID" ]
}

log_refusal() {
  [ -n "$EVENT_LOG" ] || return 0
  printf '%s\n' "$1" >>"$EVENT_LOG" 2>/dev/null || true
}

# The process-name operand is the last non-flag argument; flags (signals,
# -u, -q, ...) pass through to the real killall for the in-scope pids.
name=""
signal_args=""
for arg in "$@"; do
  case "$arg" in
    -*) signal_args="$signal_args $arg" ;;
    *) name="$arg" ;;
  esac
done

if [ -z "$name" ]; then
  real_killall "$@"
fi

matched="$("$PGREP" -x -- "$name" 2>/dev/null)" || {
  real_killall "$@"
}

# killall matches by process name; the guard's own ancestry can still
# appear under a shared name (sh), so drop it the same way the pkill
# guard does: init plus every pid on the path from here upward.
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
  log_refusal "refused killall '$name': pid(s)$refused outside session scope (worktree $WORKTREE)"
  printf 'donmai-signal-guard: refusing to signal pid(s)%s outside your session worktree (name %s). Kill by PID instead.\n' "$refused" "$name" >&2
fi

if [ -z "$allowed" ]; then
  exit 1
fi

# Signal the in-scope pids directly with kill: re-applying the name to
# the real killall would re-match the out-of-scope processes just
# refused. The signal flag (default TERM) is extracted from the original
# arguments.
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
