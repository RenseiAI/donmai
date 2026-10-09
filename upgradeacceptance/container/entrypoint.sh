#!/bin/bash
# Container entrypoint: bring up systemd as PID 1 with a delegated cgroup v2
# hierarchy, enable lingering for the acceptance user so its user manager
# runs, then wait for the manager and hand over to the commanded run.
#
# PID 1 MUST stay systemd: passing the commanded run as systemd arguments
# would make systemd parse it as kernel-cmdline options (exit 255 before
# the script's first line). Instead the entrypoint runs systemd in the
# background, waits for the acceptance user manager, and execs the command
# as the acceptance user inside the running unit — the same shape `machinectl
# shell` would give, without needing the machine bus inside the container.
set -euo pipefail

if [ -d /sys/fs/cgroup/systemd ]; then
  echo "entrypoint: cgroupfs already mounted" >&2
else
  mount -t cgroup2 none /sys/fs/cgroup 2>/dev/null || true
fi

# Delegate the hierarchy to the acceptance user manager: without this the
# user instance cannot create scopes for shim-owned seats.
mkdir -p /sys/fs/cgroup/user.slice
chown -R acceptance:acceptance /sys/fs/cgroup/user.slice 2>/dev/null || true

mkdir -p /run/dbus
if [ ! -e /run/dbus/system_bus_socket ]; then
  dbus-daemon --system --fork 2>/dev/null || true
fi

loginctl enable-linger acceptance 2>/dev/null || true

if [ "$#" -eq 0 ]; then
  # No command: stay as PID 1 so `docker exec` / detached runs can drive
  # the unit. A bare `docker run` with no command used to exec systemd
  # with an empty argv tail; making the wait explicit keeps that shape
  # working instead of exiting.
  exec /lib/systemd/systemd
fi

/lib/systemd/systemd &
SYSTEMD_PID=$!

# Wait for the acceptance user manager: the driver needs `systemctl --user`
# and `loginctl` to answer as acceptance before it installs the N unit.
for _ in $(seq 1 60); do
  if nsenter -t "${SYSTEMD_PID}" -m -p su acceptance -c 'systemctl --user is-system-running' 2>/dev/null | grep -qE 'running|degraded'; then
    break
  fi
  sleep 1
done

# Dispatch the commanded run as the acceptance user inside the running
# unit (systemd stays PID 1). The CI job passes the driver this way:
# `upgrade-acceptance.sh` with ACCEPTANCE_BASE_REV in the environment.
nsenter -t "${SYSTEMD_PID}" -m -p su acceptance -c "$*"
status=$?
kill "${SYSTEMD_PID}" 2>/dev/null || true
wait "${SYSTEMD_PID}" 2>/dev/null || true
exit "${status}"
