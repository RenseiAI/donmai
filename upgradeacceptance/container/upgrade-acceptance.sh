#!/bin/bash
# upgrade-acceptance.sh — the N → N+1 container upgrade driver.
#
# Runs INSIDE the booted acceptance container (systemd as PID 1) as the
# acceptance user. run-lane.sh dispatches it with `exec`; it is never passed
# to the container as arguments. It:
#
#   1. Checks the prerequisites the flow stands on: systemd is PID 1, the
#      acceptance user manager answers, and its cgroup v2 subtree is
#      delegated with the controllers a seat scope needs.
#   2. Builds daemon artifacts N (base revision) and N+1 (this revision). N+1
#      differs from N in its version string, which the status route reports:
#      the harmless observable behaviour that proves the upgrade happened.
#   3. Installs N as a user service from the generated unit and waits for it
#      to serve status as N.
#   4. Calls the restart preflight, replaces the binary the unit's ExecStart
#      names with N+1, restarts the unit through systemd, and waits for it to
#      serve status as N+1 from a new main PID.
#   5. Reads the N+1 build's own selection rule (the record probe, built from
#      this revision's acceptance suite) and writes the record.
#
# Outcome — exit code and the record's verdict, never conflated:
#
#   0 green  every live case was asserted and passed. Unreachable until the
#            driver carries the live-seat assertions (see 3 below).
#   1 red    the acceptance case is red for the recorded cause and every
#            infrastructure checkpoint passed. On this build the cause is
#            headless_shim_launch_off: the N+1 selection rule launches only
#            interactive seats under shim ownership, so a headless seat is a
#            direct-owned child of the daemon unit and the restart above kills
#            it. No standalone path admits a long-lived headless seat yet (the
#            local runtime refuses the shim; the control-API accept path
#            carries no session detail), so the seat itself is not dispatched.
#   2 infra  a prerequisite failed: boot, user manager, toolchain, build,
#            install, status, preflight, upgrade, or the probe. The record
#            names the failed checkpoint; an infrastructure failure is never
#            recorded as the acceptance red.
#   3 stale  the N+1 selection rule owns headless seats, so the red record no
#            longer describes this build: replace it with the live-seat
#            assertions instead of letting the lane go green unasserted.
#
# Environment:
#   ACCEPTANCE_BASE_REV   git revision for artifact N (default: HEAD~1)
#   ACCEPTANCE_RESULT_DIR where the record lands (default: ~/acceptance-results)
set -euo pipefail

export GOWORK=off
BASE_REV="${ACCEPTANCE_BASE_REV:-HEAD~1}"
RESULT_DIR="${ACCEPTANCE_RESULT_DIR:-${HOME}/acceptance-results}"
RECORD="${RESULT_DIR}/upgrade-acceptance.json"
# Deliberately off PATH and off ~/.local/bin: the upgrade must find the
# binary through the unit's own ExecStart, so a driver that guessed the path
# would replace nothing and fail upgrade_not_applied.
INSTALL_PATH="${HOME}/.local/opt/donmai-acceptance/donmai"
mkdir -p "${RESULT_DIR}" "$(dirname "${INSTALL_PATH}")"

WORK="$(mktemp -d /tmp/upgrade-acceptance.XXXXXX)"
cleanup() { rm -rf "${WORK}"; }
trap cleanup EXIT

CHECKPOINTS='{}'
PROBE='null'
CASES='[]'

checkpoint() {
  CHECKPOINTS="$(jq -c --arg k "$1" --arg v "$2" '. + {($k): $v}' <<<"${CHECKPOINTS}")"
  echo "checkpoint ${1}: ${2}"
}

write_record() {
  jq -n --arg verdict "$1" --arg cause "$2" --arg detail "$3" \
    --argjson checkpoints "${CHECKPOINTS}" --argjson probe "${PROBE}" --argjson cases "${CASES}" \
    '{verdict: $verdict, cause: $cause, detail: $detail, checkpoints: $checkpoints, probe: $probe, cases: $cases}' \
    >"${RECORD}"
  cat "${RECORD}"
}

infra() {
  write_record infra "$1" "$2"
  echo "upgrade-acceptance: infra: $1: $2" >&2
  exit 2
}

unit_diagnostics() {
  systemctl --user status "${UNIT:-none}" --no-pager -n 40 2>&1 | tail -40 || true
}

# status_json prints the daemon's status projection, or fails when no daemon
# answers. The caller decides what an unreachable status means; it is never
# folded into an empty projection.
status_json() {
  "${WORK}/donmai-next" host status --json 2>/dev/null
}

# wait_serving waits until the daemon reports version $1 from a main PID other
# than $2, and prints that status. wait_serving_timeout is the same probe
# with an explicit deadline in seconds, for the fresh-refusal check where
# serving must NOT happen and a 90s wait would burn the lane.
wait_serving_timeout() {
  local want="$1" not_pid="$2" timeout="$3" deadline=$((SECONDS + timeout)) out
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if out="$(status_json)" \
      && [ "$(jq -r '.version // empty' <<<"${out}")" = "${want}" ] \
      && [ "$(jq -r '.pid // 0' <<<"${out}")" != "${not_pid}" ]; then
      printf '%s' "${out}"
      return 0
    fi
    sleep 1
  done
  return 1
}
wait_serving() {
  wait_serving_timeout "$1" "$2" 90
}

# --- 1. Prerequisites ----------------------------------------------------------
[ "$(cat /proc/1/comm)" = "systemd" ] || infra pid1_not_systemd "PID 1 is $(cat /proc/1/comm)"
[ "$(stat -fc %T /sys/fs/cgroup)" = "cgroup2fs" ] || infra not_cgroup_v2 "/sys/fs/cgroup is $(stat -fc %T /sys/fs/cgroup)"
MANAGER_STATE="$(systemctl --user is-system-running 2>&1 || true)"
case "${MANAGER_STATE}" in
  running | degraded) checkpoint user_manager "${MANAGER_STATE}" ;;
  *) infra user_manager_unreachable "systemctl --user is-system-running: ${MANAGER_STATE}" ;;
esac
MANAGER_CGROUP="$(systemctl show -p ControlGroup --value "user@$(id -u).service")"
CONTROLLERS="$(cat "/sys/fs/cgroup${MANAGER_CGROUP}/cgroup.controllers" 2>/dev/null || true)"
for want in memory pids; do
  case " ${CONTROLLERS} " in
    *" ${want} "*) ;;
    *) infra cgroup_not_delegated "user manager cgroup ${MANAGER_CGROUP} controllers: '${CONTROLLERS}' (want ${want})" ;;
  esac
done
checkpoint delegated_controllers "${CONTROLLERS}"

# --- 2. Build N and N+1 --------------------------------------------------------
cd /src
BASE_SHA="$(git rev-parse --verify --quiet "${BASE_REV}^{commit}")" || infra base_rev_unresolved "${BASE_REV}"
HEAD_SHA="$(git rev-parse HEAD)"
N_VERSION="0.0.0-acceptance.n.${BASE_SHA:0:12}"
NEXT_VERSION="0.0.1-acceptance.next.${HEAD_SHA:0:12}"
echo "building artifact N (${N_VERSION}) from ${BASE_REV}"
mkdir -p "${WORK}/n"
git archive --format=tar "${BASE_SHA}" | tar -x -C "${WORK}/n" || infra base_tree_failed "${BASE_SHA}"
(cd "${WORK}/n" && go build -ldflags "-X main.version=${N_VERSION}" -o "${WORK}/donmai-n" ./cmd/donmai) \
  || infra build_n_failed "${BASE_SHA}"
echo "building artifact N+1 (${NEXT_VERSION}) from this revision"
go build -ldflags "-X main.version=${NEXT_VERSION}" -o "${WORK}/donmai-next" ./cmd/donmai || infra build_next_failed "${HEAD_SHA}"
go test -c -o "${WORK}/acceptance.test" ./upgradeacceptance || infra build_probe_failed "${HEAD_SHA}"
for pair in "donmai-n:${N_VERSION}" "donmai-next:${NEXT_VERSION}"; do
  "${WORK}/${pair%%:*}" --version 2>&1 | grep -qF "${pair#*:}" || infra version_not_injected "${pair%%:*} --version does not report ${pair#*:}"
done
checkpoint artifacts "N=${N_VERSION} N+1=${NEXT_VERSION}"

# --- 3. Fresh-install N+1 as a user service, then run ----------------------
# Fresh-install-then-run: no config exists when install runs, so install
# seeds the fresh-host config and the unit starts without the interactive
# first-run wizard (the unit's stdin is /dev/null, a character device that
# is not a terminal). The seed names the local file queue with an explicit
# execution policy but no harness or repository profile, so the fresh unit
# refuses with the setup action instead of crash-looping on a missing
# answer; the driver then writes the standalone config the upgrade flow
# stands on and restarts into it. Every checkpoint below stays live: the
# fresh refusal, the seed's presence, and the configured serving state are
# each asserted, never assumed. The fresh install runs artifact N+1 (this
# revision): N predates the seed/refusal and cannot assert what it never
# implemented — N still proves the upgrade half of the lane below.
rm -f "${HOME}/.donmai/daemon.yaml"
install -m 0755 "${WORK}/donmai-next" "${INSTALL_PATH}"
INSTALL_OUT="$("${INSTALL_PATH}" host install --user 2>&1)" || infra install_failed "${INSTALL_OUT}"
SEED_PATH="${HOME}/.donmai/daemon.yaml"
[ -f "${SEED_PATH}" ] || infra fresh_seed_missing "host install wrote no config at ${SEED_PATH}"
grep -q '^apiVersion: donmai.dev/v2' "${SEED_PATH}" || infra fresh_seed_not_v2 "seed config is not the fresh-host file-queue seed: $(head -3 "${SEED_PATH}")"
checkpoint fresh_seed "host install seeded ${SEED_PATH}"
UNIT_PATH="$(sed -n 's/^Service registered: //p' <<<"${INSTALL_OUT}" | head -1)"
[ -n "${UNIT_PATH}" ] || infra install_failed "no unit path in: ${INSTALL_OUT}"
UNIT="$(basename "${UNIT_PATH}")"
# The unit's own ExecStart is the binary the upgrade must replace: never a
# guess from PATH. A unit that names a binary which is not N is not the
# install under test.
EXEC_PATH="$(systemctl --user show -p ExecStart --value "${UNIT}" | sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -1)"
[ -n "${EXEC_PATH}" ] && [ -x "${EXEC_PATH}" ] || infra unit_exec_unresolved "${UNIT}: ExecStart path '${EXEC_PATH}'"
"${EXEC_PATH}" --version 2>&1 | grep -qF "${NEXT_VERSION}" || infra unit_exec_not_next "${EXEC_PATH} is not artifact N+1"
checkpoint unit "${UNIT} ExecStart=${EXEC_PATH}"
# The fresh unit must refuse with the setup action, not crash-loop on a
# missing answer: it never reaches serving (wait_serving times out), it
# never restarts in a storm, and the journal names the fix. The refusal
# exits with the setup status the unit holds failed
# (RestartPreventExitStatus), so Restart=on-failure does not re-fire on
# it — the start count stays flat while the unit waits for its config.
systemctl --user show "${UNIT}" | grep -qF 'RestartPreventExitStatus=4' \
  || infra fresh_refusal_restartable "unit does not hold the setup refusal failed (want RestartPreventExitStatus=4)"
STARTS_BEFORE="$(systemctl --user show -p NRestarts --value "${UNIT}")"
if wait_serving_timeout "${NEXT_VERSION}" 0 12 >/dev/null 2>&1; then
  infra fresh_unit_served "the fresh-host unit served without a harness or repository profile; it must refuse until setup completes"
fi
STARTS_AFTER="$(systemctl --user show -p NRestarts --value "${UNIT}")"
[ "${STARTS_AFTER}" = "${STARTS_BEFORE}" ] || infra fresh_unit_restart_storm "fresh unit restarts ${STARTS_BEFORE} -> ${STARTS_AFTER} in 12s; the setup refusal must not crash-loop"
journalctl --user -u "${UNIT}" --no-pager -n 30 2>/dev/null | grep -qF 'host setup' \
  || infra fresh_refusal_unactionable "$(unit_diagnostics)"
checkpoint fresh_refusal "unit refuses without setup and does not restart (restarts=${STARTS_AFTER})"
# The standalone config the upgrade flow stands on: no control plane (the
# orchestrator URL is a closed loopback port and no registration token is
# configured, so registration takes the local stub path).
cat >"${HOME}/.donmai/daemon.yaml" <<'YAML'
apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: upgrade-acceptance
capacity:
  maxConcurrentSessions: 2
orchestrator:
  url: http://127.0.0.1:1
YAML
# The installed N+1 binary is refreshed from N+1 (same artifact the fresh
# install laid down) before the configured restart, so the serving state
# below proves the standalone config — never a stale ExecStart copy.
cp "${WORK}/donmai-next" "${EXEC_PATH}.next"
mv -f "${EXEC_PATH}.next" "${EXEC_PATH}"
systemctl --user restart "${UNIT}" || infra restart_failed "$(unit_diagnostics)"
N_STATUS="$(wait_serving "${NEXT_VERSION}" 0)" || infra n_not_serving "$(unit_diagnostics)"
N_PID="$(jq -r '.pid' <<<"${N_STATUS}")"
checkpoint n_serving "version=${NEXT_VERSION} pid=${N_PID}"

# --- 4. Preflight, downgrade to N, upgrade to N+1 ---------------------------
# The N/N+1 upgrade half of the lane: replace the installed N+1 with N,
# restart into N, then replace with N+1 and restart into N+1 from a new
# PID. The N+1 selection-rule record below is read from the final N+1.
cp "${WORK}/donmai-n" "${EXEC_PATH}.next"
mv -f "${EXEC_PATH}.next" "${EXEC_PATH}"
systemctl --user restart "${UNIT}" || infra restart_failed "$(unit_diagnostics)"
DOWNGRADED_STATUS="$(wait_serving "${N_VERSION}" "${N_PID}")" || infra downgrade_not_applied "$(unit_diagnostics)"
checkpoint downgraded "version=${N_VERSION} pid=$(jq -r '.pid' <<<"${DOWNGRADED_STATUS}")"
TOKEN_FILE="${HOME}/.donmai/control-token"
AUTH=()
[ -r "${TOKEN_FILE}" ] && AUTH=(-H "Authorization: Bearer $(cat "${TOKEN_FILE}")")
PREPARE="$(curl -sS -X POST "${AUTH[@]}" -w '\n%{http_code}' http://127.0.0.1:7734/api/daemon/restart/prepare 2>&1)" \
  || infra preflight_unreachable "${PREPARE}"
PREPARE_CODE="$(tail -1 <<<"${PREPARE}")"
PREPARE_STATE="$(sed '$d' <<<"${PREPARE}" | jq -r '.state // empty' 2>/dev/null || true)"
case "${PREPARE_CODE}:${PREPARE_STATE}" in
  200:prepared | 200:not_required) checkpoint preflight "${PREPARE_STATE}" ;;
  *) infra preflight_refused "HTTP ${PREPARE}" ;;
esac
cp "${WORK}/donmai-next" "${EXEC_PATH}.next"
mv -f "${EXEC_PATH}.next" "${EXEC_PATH}"
systemctl --user restart "${UNIT}" || infra restart_failed "$(unit_diagnostics)"
DOWNGRADED_PID="$(jq -r '.pid' <<<"${DOWNGRADED_STATUS}")"
NEXT_STATUS="$(wait_serving "${NEXT_VERSION}" "${DOWNGRADED_PID}")" || infra upgrade_not_applied "$(unit_diagnostics)"
NEXT_PID="$(jq -r '.pid' <<<"${NEXT_STATUS}")"
checkpoint upgraded "version=${NEXT_VERSION} pid=${NEXT_PID}"
# Context only: the configured ownership mode. It is derived from the two
# config flags, not from the selection rule, so it never decides the verdict.
checkpoint ownership_mode "$(jq -r '.sessionShim.ownershipMode // "absent"' <<<"${NEXT_STATUS}")"

# --- 5. Record -----------------------------------------------------------------
(cd /src/upgradeacceptance && "${WORK}/acceptance.test" -test.run '^TestRecordProbe$' -test.count=1 \
  -acceptance.record-probe="${WORK}/probe.json" >"${WORK}/probe.log" 2>&1) \
  || infra probe_failed "$(tail -20 "${WORK}/probe.log")"
PROBE="$(jq -c . "${WORK}/probe.json")" || infra probe_failed "unreadable probe output"
CASES="$(jq -c '[.liveCases[] | {name, status: (if .name == "seat-survives-upgrade" then "red" else "blocked" end)}]' <<<"${PROBE}")"
[ "$(jq -r '.interactiveOwned' <<<"${PROBE}")" = "true" ] \
  || infra probe_not_reading_rule "the N+1 selection rule does not own the interactive control spec"
if [ "$(jq -r '.headlessOwned' <<<"${PROBE}")" = "true" ]; then
  write_record stale headless_rule_flipped "the N+1 selection rule owns headless seats; replace this red record with the live-seat assertions"
  exit 3
fi
write_record red headless_shim_launch_off \
  "the N+1 selection rule launches only interactive seats under shim ownership, so a headless seat is a direct-owned child of ${UNIT} and the restart kills it"
exit 1
