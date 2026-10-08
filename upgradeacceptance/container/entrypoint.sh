#!/bin/bash
# Container entrypoint: bring up systemd as PID 1 with a delegated cgroup v2
# hierarchy, enable lingering for the acceptance user so its user manager
# runs, then hand over to the commanded run (the CI job execs
# upgrade-acceptance.sh as the acceptance user).
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

exec /lib/systemd/systemd "$@"
