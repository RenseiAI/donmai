#!/usr/bin/env bash
# Exercise the real container test runner through its Podman process boundary.
set -euo pipefail
root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
subject="${root_dir}/scripts/podman-go-test.sh"
fixture_dir=$(mktemp -d)
runner_pid=''
cleanup_fixture() {
  if [[ -n "${runner_pid}" ]]; then
    kill -TERM "${runner_pid}" 2>/dev/null || true
    touch "${PODMAN_FIXTURE}/finish-create" "${PODMAN_FIXTURE}/stop"
    wait "${runner_pid}" 2>/dev/null || true
  fi
  rm -rf -- "${fixture_dir}"
}
trap cleanup_fixture EXIT
mkdir -p "${fixture_dir}/bin" "${fixture_dir}/repo"
git -C "${fixture_dir}/repo" init -q
printf 'module example.test/fixture\ngo 1.26.6\n' >"${fixture_dir}/repo/go.mod"
cat >"${fixture_dir}/bin/podman" <<'PODMAN'
#!/usr/bin/env python3
import json
import os
import pathlib
import sys
import time

state = pathlib.Path(os.environ["PODMAN_FIXTURE"])
args = sys.argv[1:]
with (state / "calls").open("a") as output:
    output.write(json.dumps(args) + "\n")
if args[:2] == ["image", "exists"]:
    sys.exit(0)
if args[0] == "rm":
    (state / "removed").write_text(args[-1])
    (state / "stop").touch()
    sys.exit(int(os.environ.get("PODMAN_CLEANUP_EXIT", "0")))
if args[0] == "run":
    sys.exit(int(os.environ.get("PODMAN_TEST_EXIT", "0")))
if args[0] == "create":
    if os.environ.get("PODMAN_NO_CREATE"):
        sys.exit(125)
    if os.environ.get("PODMAN_CREATE_WAIT"):
        (state / "creating").touch()
        deadline = time.monotonic() + 5
        while not (state / "finish-create").exists():
            if time.monotonic() > deadline:
                sys.exit(124)
            time.sleep(0.01)
    pathlib.Path(args[args.index("--cidfile") + 1]).write_text("a" * 64)
    sys.exit(0)
if args[0] != "start":
    sys.exit(99)
(state / "started").touch()
if os.environ.get("PODMAN_WAIT"):
    deadline = time.monotonic() + 5
    while not (state / "stop").exists():
        if time.monotonic() > deadline:
            sys.exit(124)
        time.sleep(0.01)
sys.exit(int(os.environ.get("PODMAN_TEST_EXIT", "0")))
PODMAN
chmod +x "${fixture_dir}/bin/podman"
export PATH="${fixture_dir}/bin:${PATH}"

run_case() {
  local name=$1 expected=$2 status=0
  shift 2
  export PODMAN_FIXTURE="${fixture_dir}/${name}"
  mkdir -p "${PODMAN_FIXTURE}"
  (cd "${fixture_dir}/repo" && env "$@" bash "${subject}" -race ./daemon -run '^TestArchive$' -count=1) >"${PODMAN_FIXTURE}/output" 2>&1 || status=$?
  if [[ "${status}" -ne "${expected}" ]]; then
    cat "${PODMAN_FIXTURE}/output" >&2
    printf 'FAIL: %s exit %s, expected %s\n' "${name}" "${status}" "${expected}" >&2
    exit 1
  fi
}

run_case success 0
python3 - "${PODMAN_FIXTURE}/calls" <<'PY_CHECK'
import json
import pathlib
import sys
calls = [json.loads(line) for line in pathlib.Path(sys.argv[1]).read_text().splitlines()]
create = next(call for call in calls if call[0] in ["create", "run"])
assert "--rm" not in create, "--rm makes the test filesystem volatile and omits fsync"
PY_CHECK
run_case test-failure 23 PODMAN_TEST_EXIT=23
run_case killed-test 137 PODMAN_TEST_EXIT=137
run_case create-failure 125 PODMAN_NO_CREATE=1
run_case cleanup-failure 1 PODMAN_CLEANUP_EXIT=17
run_case both-failures 23 PODMAN_TEST_EXIT=23 PODMAN_CLEANUP_EXIT=17
for name in cleanup-failure both-failures; do
  grep -q 'cleanup failed for container' "${fixture_dir}/${name}/output" || { printf 'FAIL: cleanup failure was hidden\n' >&2; exit 1; }
done

export PODMAN_FIXTURE="${fixture_dir}/signal"
mkdir -p "${PODMAN_FIXTURE}"
(cd "${fixture_dir}/repo" && exec env PODMAN_WAIT=1 bash "${subject}" -race ./daemon) >"${PODMAN_FIXTURE}/output" 2>&1 &
runner_pid=$!
for ((attempt=0; attempt<200; attempt++)); do
  [[ ! -f "${PODMAN_FIXTURE}/started" ]] || break
  sleep 0.01
done
[[ -f "${PODMAN_FIXTURE}/started" ]] || { printf 'FAIL: signal fixture never started\n' >&2; exit 1; }
kill -TERM "${runner_pid}"
status=0
wait "${runner_pid}" || status=$?
runner_pid=''
[[ "${status}" -eq 143 ]] || { printf 'FAIL: signal exit %s, expected 143\n' "${status}" >&2; exit 1; }

export PODMAN_FIXTURE="${fixture_dir}/early-signal"
mkdir -p "${PODMAN_FIXTURE}"
(cd "${fixture_dir}/repo" && exec env PODMAN_CREATE_WAIT=1 bash "${subject}" -race ./daemon) >"${PODMAN_FIXTURE}/output" 2>&1 &
runner_pid=$!
for ((attempt=0; attempt<200; attempt++)); do
  [[ ! -f "${PODMAN_FIXTURE}/creating" ]] || break
  sleep 0.01
done
[[ -f "${PODMAN_FIXTURE}/creating" ]] || { printf 'FAIL: early signal fixture never reached creation\n' >&2; exit 1; }
kill -TERM "${runner_pid}"
# The runner must wait for creation and then remove its recorded ID.
touch "${PODMAN_FIXTURE}/finish-create"
status=0
wait "${runner_pid}" || status=$?
runner_pid=''
[[ "${status}" -eq 143 ]] || { printf 'FAIL: early signal exit %s, expected 143\n' "${status}" >&2; exit 1; }

python3 - "${fixture_dir}" <<'PY'
import json
import pathlib
import sys
root = pathlib.Path(sys.argv[1])
for name in ["success", "test-failure", "killed-test", "create-failure", "cleanup-failure", "both-failures", "signal", "early-signal"]:
    calls = [json.loads(line) for line in (root / name / "calls").read_text().splitlines()]
    run = next(call for call in calls if call[0] == "create")
    assert "--rm" not in run, f"{name}: --rm makes the test filesystem volatile"
    starts = [call for call in calls if call[0] == "start"]
    if name in ["create-failure", "early-signal"]:
        assert not starts, f"{name}: started a workload after creation failure or cancellation"
    else:
        assert starts == [["start", "--attach", "a" * 64]], f"{name}: started an unowned container"
    assert "--cidfile" in run, f"{name}: no owned container receipt"
    assert "--init" in run, f"{name}: no child reaper"
    receipt = pathlib.Path(run[run.index("--cidfile") + 1])
    assert not receipt.parent.exists(), f"{name}: receipt directory leaked"
    removal = [call for call in calls if call[0] == "rm"]
    if name == "create-failure":
        assert not removal, "creation failure removed an unowned container"
    else:
        assert removal == [["rm", "--force", "a" * 64]], f"{name}: cleanup did not target only the owned ID: {removal}"
    if name == "success":
        assert run[-6:] == ["go-test", "-race", "./daemon", "-run", "^TestArchive$", "-count=1"], "Go test arguments changed"
print("podman test runner contracts: PASS (8 cases)")
PY
