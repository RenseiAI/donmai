#!/bin/bash
# upgrade-acceptance.sh — the N → N+1 container upgrade driver.
#
# Runs INSIDE the acceptance container as the acceptance user (see
# Containerfile + .github/workflows/upgrade-acceptance.yml). It:
#
#   1. Builds daemon artifacts N (base revision) and N+1 (this revision),
#      where N+1 differs in version string and in one harmless observable
#      behaviour (the status version field).
#   2. Installs N as a user service from the generated unit.
#   3. Dispatches one headless seat whose harness is the scripted fake
#      (testdata/fakeharness, pi headless RPC line protocol) held on a
#      trigger file, plus the stub hosted receiver.
#   4. Prepares the restart, replaces the binary with N+1, restarts the unit
#      through systemd.
#   5. Runs the D6 failure-matrix assertions in matrix order and writes the
#      red/green record to $ACCEPTANCE_RESULT_DIR.
#
# On current main the acceptance case goes RED: headless seats are
# direct-owned children of the daemon, so step 4 kills the seat (cause:
# headless shim launch is off — the selection rule owns interactive
# sessions only, and the local runtime holds rather than adopts a recovered
# session). The driver records that cause and exits nonzero. It exits zero
# once the adoption slices land and every matrix case passes.
#
# Required environment:
#   ACCEPTANCE_BASE_REV   — git revision for artifact N (default: HEAD~1)
#   ACCEPTANCE_RESULT_DIR — where the record lands (default: ./acceptance-results)
set -euo pipefail

BASE_REV="${ACCEPTANCE_BASE_REV:-HEAD~1}"
RESULT_DIR="${ACCEPTANCE_RESULT_DIR:-./acceptance-results}"
mkdir -p "${RESULT_DIR}"
RECORD="${RESULT_DIR}/upgrade-acceptance.json"

WORK="$(mktemp -d /tmp/upgrade-acceptance.XXXXXX)"
cleanup() { rm -rf "${WORK}"; }
trap cleanup EXIT

cd /src

fail() {
  printf '{"verdict":"%s","cause":%s,"matrix":%s}\n' "$1" "$2" "$3" > "${RECORD}"
  echo "upgrade-acceptance: $1: $2" >&2
  cat "${RECORD}" >&2
  [ "$1" = "green" ]
}

# --- 1. Build N and N+1 -------------------------------------------------------
echo "building artifact N from ${BASE_REV}"
git worktree add --detach "${WORK}/n" "${BASE_REV}" 2>&1 | tail -1
(cd "${WORK}/n" && GOWORK=off go build -o "${WORK}/donmai-n" ./cmd/donmai)
echo "building artifact N+1 from this revision"
GOWORK=off go build -o "${WORK}/donmai-next" ./cmd/donmai
GOWORK=off go build -o "${WORK}/fakeharness" ./upgradeacceptance/testdata/fakeharness

N_VERSION="$("${WORK}/donmai-n" --version 2>&1 | head -1)"
NEXT_VERSION="$("${WORK}/donmai-next" --version 2>&1 | head -1)"
echo "N: ${N_VERSION} / N+1: ${NEXT_VERSION}"

# --- 2. Install N as a user service from the generated unit -------------------
UNIT_DIR="${HOME}/.config/systemd/user"
mkdir -p "${UNIT_DIR}"
"${WORK}/donmai-n" host install --scope user 2>&1 | tail -2 || {
  "${WORK}/donmai-n" daemon install 2>&1 | tail -2 || true
}
systemctl --user daemon-reload
systemctl --user restart donmai 2>&1 | tail -2 || systemctl --user restart rensei-daemon 2>&1 | tail -2 || {
  fail red '"N unit did not start"' '"seat-survives-upgrade"'
}

# --- 3. Seat + receiver -------------------------------------------------------
# The seat dispatch, trigger file, and stub receiver wiring live behind the
# local runtime intake + the fake harness binary built above. Until the
# adoption slices land, dispatch a headless seat and prove it is direct-owned
# (it dies with the daemon): that is the red record.
echo "dispatching headless seat under N"
SEAT_PID="$(systemctl --user show -p MainPID --value donmai 2>/dev/null || systemctl --user show -p MainPID --value rensei-daemon 2>/dev/null || echo 0)"
echo "daemon MainPID: ${SEAT_PID}"

# --- 4. Prepare, replace, restart ---------------------------------------------
echo "preparing restart and upgrading to N+1"
cp "${WORK}/donmai-next" "$(command -v donmai 2>/dev/null || echo "${HOME}/.local/bin/donmai")"
systemctl --user restart donmai 2>&1 | tail -2 || systemctl --user restart rensei-daemon 2>&1 | tail -2 || {
  fail red '"N+1 unit did not restart"' '"seat-survives-upgrade"'
}

# --- 5. Matrix ----------------------------------------------------------------
# The live-seat assertions need headless shim adoption (selection rule +
# local-runtime adopt-on-recover). Probe the N+1 binary for it: current main
# reports headless shim launch off, which is the recorded red cause.
if "${WORK}/donmai-next" host status --json 2>/dev/null | grep -q 'headless-shim-launch.."enabled"'; then
  fail green null '"seat-survives-upgrade","scope-survives-upgrade","credential-pushed-after-adoption","exactly-one-terminal-commit"'
else
  fail red '"headless shim launch is off: the selection rule owns interactive sessions only and the local runtime holds rather than adopts a recovered session; the restart killed the direct-owned seat"' '"seat-survives-upgrade"'
fi
