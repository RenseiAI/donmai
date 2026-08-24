# Session-shim installed-artifact acceptance mutator

This command is a trusted fault mutator for the real installed-service
session-shim acceptance. It is not an oracle: its stdout and stderr are never
evidence, and every mutation must be proved again through independent daemon,
heartbeat, process, and viewer-wire observations.

The first delivered target is Linux/systemd-user, matching the fresh hosted
release runner. Other service managers are refused by `check` until they have
their own installed-artifact proof.

The external command surface is closed:

- `check`
- `prepare`
- `force-gap <session-id>`
- `quarantine-arm <session-id>`
- `quarantine-clear <session-id>`
- `fence-refuse-arm <session-id>`
- `fence-refuse-clear <session-id>`
- `cleanup [session-id]`

`prepare` creates a private bearer under the exact configured acceptance-state
directory and supplies only its path to the user service manager. The daemon
control route remains indistinguishable from absent unless that private file is
explicitly configured. Requests are then bound to an exact lifecycle already
adopted by the daemon.

The incompatible-shim leg starts a real, separately correlated live process and
Unix socket with a non-overlapping protocol range. The daemon charges and
reports it only after validating the actual registry and process identity. The
clear leg succeeds only after that exact process and record are gone. The gap
leg drives real resize/redraw traffic through the adopted shim ring. The fence
leg refuses one exact planned-restart acknowledgement, then becomes inert until
explicit cleanup.

Required environment:

- `DONMAI_SESSION_SHIM_ACCEPTANCE_STATE_DIR`
- `DONMAI_SESSION_SHIM_ACCEPTANCE_REGISTRY_DIR`
- `DONMAI_SESSION_SHIM_ACCEPTANCE_CANDIDATE`
- `DONMAI_SESSION_SHIM_ACCEPTANCE_TOKEN_FILE`
- `DONMAI_SESSION_SHIM_ACCEPTANCE_DAEMON_URL` (optional; defaults to loopback
  port 7734)

All configured paths must be absolute. The token file must be directly inside
the dedicated acceptance-state directory.
