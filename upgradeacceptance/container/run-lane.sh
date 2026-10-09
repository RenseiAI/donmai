#!/bin/bash
# run-lane.sh — boot the acceptance image and drive the N → N+1 flow.
#
# Runs on the HOST (the CI runner or an operator's workstation) from the repo
# root, after the image is built:
#
#   docker build -f upgradeacceptance/container/Containerfile -t donmai-upgrade-acceptance .
#   ACCEPTANCE_EXPECT=red:headless_shim_launch_off upgradeacceptance/container/run-lane.sh
#
# It starts the image with systemd as PID 1, waits for the system manager and
# the acceptance user manager, proves the toolchain the driver will use is the
# one go.mod requires, then runs upgrade-acceptance.sh inside the container
# with `exec` as the acceptance user and copies its record out.
#
# The lane passes only when the record's "<verdict>:<cause>" equals
# ACCEPTANCE_EXPECT. On this build that is the red record
# (red:headless_shim_launch_off), the same red TestUpgradeAcceptanceRed pins
# in-repo: an infrastructure failure, a different cause, or a stale record
# all fail the lane, each under its own name.
#
# Environment:
#   CONTAINER_ENGINE      docker (default) or podman
#   ACCEPTANCE_EXPECT     required record, "<verdict>:<cause>"
#   ACCEPTANCE_BASE_REV   git revision for artifact N (default: HEAD~1)
#   ACCEPTANCE_RESULT_DIR host directory for the record (default: ./acceptance-results)
#   ACCEPTANCE_RUN_ARGS   extra engine run flags, e.g. resource caps
set -euo pipefail

ENGINE="${CONTAINER_ENGINE:-docker}"
IMAGE="${1:-donmai-upgrade-acceptance}"
EXPECT="${ACCEPTANCE_EXPECT:?ACCEPTANCE_EXPECT must name the required record, e.g. red:headless_shim_launch_off}"
BASE_REV="${ACCEPTANCE_BASE_REV:-HEAD~1}"
OUT="${ACCEPTANCE_RESULT_DIR:-./acceptance-results}"
ACCEPTANCE_UID=1000
GO_WANT="go$(awk '$1 == "go" { print $2; exit }' go.mod)"
mkdir -p "${OUT}"

lane_fail() {
  echo "upgrade-acceptance lane: $1" >&2
  exit 1
}

# Extra engine run flags, word-split on purpose (an operator's resource caps,
# e.g. "--memory 6g --cpus 4" on a shared host). The guarded expansion keeps
# an empty list from tripping `set -u` on bash before 4.4 (macOS /bin/bash).
read -r -a extra_run_args <<<"${ACCEPTANCE_RUN_ARGS:-}"
cid="$("${ENGINE}" run -d --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
  ${extra_run_args[@]+"${extra_run_args[@]}"} "${IMAGE}")"
diagnostics() {
  echo "--- failed units ---"
  "${ENGINE}" exec "${cid}" systemctl --failed --no-pager 2>&1 || true
  echo "--- journal (tail) ---"
  "${ENGINE}" exec "${cid}" journalctl --no-pager -n 200 2>&1 || true
}
cleanup() {
  "${ENGINE}" rm -f "${cid}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Boot: the system manager reaches running (or degraded: a unit a container
# cannot run failed, which the driver tolerates) and PID 1 is systemd.
state=""
for _ in $(seq 1 120); do
  state="$("${ENGINE}" exec "${cid}" systemctl is-system-running 2>/dev/null || true)"
  case "${state}" in running | degraded) break ;; esac
  sleep 1
done
case "${state}" in
  running | degraded) echo "system manager: ${state}" ;;
  *) diagnostics; lane_fail "infra: boot_timeout: system manager state '${state}'" ;;
esac
pid1="$("${ENGINE}" exec "${cid}" cat /proc/1/comm)"
[ "${pid1}" = "systemd" ] || lane_fail "infra: pid1_not_systemd: PID 1 is ${pid1}"

# The lingering acceptance user's manager, started by logind at boot.
manager=""
for _ in $(seq 1 60); do
  manager="$("${ENGINE}" exec "${cid}" systemctl is-active "user@${ACCEPTANCE_UID}.service" 2>/dev/null || true)"
  [ "${manager}" = "active" ] && break
  sleep 1
done
[ "${manager}" = "active" ] || { diagnostics; lane_fail "infra: user_manager_unavailable: user@${ACCEPTANCE_UID}.service is '${manager}'"; }

as_acceptance=("${ENGINE}" exec -u acceptance -w /src
  -e HOME=/home/acceptance
  -e "XDG_RUNTIME_DIR=/run/user/${ACCEPTANCE_UID}"
  -e "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/${ACCEPTANCE_UID}/bus"
  -e "ACCEPTANCE_BASE_REV=${BASE_REV}"
  "${cid}")

# The toolchain the driver builds N and N+1 with, read through the same exec
# shape the driver runs in.
toolchain="$("${as_acceptance[@]}" go version)"
echo "${toolchain}"
case "${toolchain}" in
  "go version ${GO_WANT} linux/"*) ;;
  *) lane_fail "infra: toolchain_mismatch: driver toolchain '${toolchain}', go.mod requires ${GO_WANT}" ;;
esac

status=0
"${as_acceptance[@]}" bash upgradeacceptance/container/upgrade-acceptance.sh || status=$?
"${ENGINE}" cp "${cid}:/home/acceptance/acceptance-results/upgrade-acceptance.json" "${OUT}/upgrade-acceptance.json" \
  || { diagnostics; lane_fail "infra: no_record: driver exited ${status} without writing a record"; }

verdict="$(jq -r '.verdict' "${OUT}/upgrade-acceptance.json")"
got="${verdict}:$(jq -r '.cause' "${OUT}/upgrade-acceptance.json")"
echo "driver exit ${status}; record ${got}; lane requires ${EXPECT}"
# The exit code and the record must tell the same story (see the outcome
# table in upgrade-acceptance.sh): a record the driver did not finish with
# is not evidence.
case "${verdict}:${status}" in
  green:0 | red:1 | infra:2 | stale:3) ;;
  *) diagnostics; lane_fail "driver exit ${status} disagrees with record verdict ${verdict}" ;;
esac
if [ "${got}" != "${EXPECT}" ]; then
  diagnostics
  lane_fail "record ${got} does not match the required ${EXPECT}"
fi
echo "upgrade-acceptance lane: record matches ${EXPECT}"
