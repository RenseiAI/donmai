# Local runtime threat model

This model describes the local daemon, workarea archives and installed kits.
It states the supported trust boundary; it is not an exhaustive security audit
or a guarantee about an embedder's filesystem, network or sandbox configuration.

## Overview

Donmai runs a CLI and daemon under an operator's operating-system account.
The daemon maintains account-owned state, restores local archives and installs
kits. Host-native agents and kit commands execute with that account's authority.

| Component | Resources and controls | Source |
| --- | --- | --- |
| Native daemon control | Loopback listener; independent per-install bearer for mutations; required-but-unavailable bearer fails closed | `daemon/control_bind.go`, `daemon/control_auth.go`, `afcli/daemon_run.go` |
| Workarea archives | Validated archive IDs; private registry root; rooted copying and lifecycle identity checks | `daemon/workarea_archive.go`, `runtime/workarea/acquisition.go` |
| Installed kits | Configured scan paths and package generations; applicable installation verification | `daemon/kit_registry.go`, `daemon/kit_package.go`, `daemon/kit_trust.go` |
| Kit execution | Local shell at the selected worktree, with a composed environment | `runner/kit_execer.go` |

The default state location is the effective state base plus `.donmai`.
Configured archive roots and kit scan paths can differ. A legacy restore's
destination and metadata are in a sibling of the archive root; a session-root
restore instead uses its retained acquisition identity and final root. These
locations must not be described as one interchangeable protected directory.
See `internal/statepath/statepath.go`, `daemon/workarea_archive.go` and
`runtime/workarea/acquisition.go`.

## Trust boundaries and assumptions

### The operator account is trusted

The operator's Unix account, including host-native agents and other processes
running with the same user identity, is a trusted principal. Such a process
already has the account's filesystem and process authority. Reading its token
file, changing its installed kits or modifying its local archives does not by
itself cross a new security boundary.

A claim that requires this same-account authority, without an additional
boundary or capability gain, is accepted same-user risk. This does not waive
remote-service authorization, trust checks on external inputs, or a real
sandbox boundary. It is not a blanket exclusion of local security issues.

This is the maintainer-approved trust policy. The source also documents the
same-account limitation in `afclient/control_token.go` and
`runtime/env/composer.go`.

### Sandboxes require actual isolation

Use a process, filesystem and network isolation boundary for untrusted agent
workloads. Establish which mounts, credentials, sockets and privileges a
workload actually receives. A sandbox label, different working directory,
alternate state path or filtered environment alone does not prove isolation.

Configure state, archive and installed-kit paths under trusted account control.
If a deployment gives another principal access to these paths or their parent
directories, assess that deployment's permissions and boundary explicitly;
do not assume that changing a child directory's mode protects every ancestor.
Embedders own their effective resource and isolation configuration.

### Other principals and external content remain distinct

Another OS user, a browser-originated request, a remote publisher and a
separately isolated workload do not automatically gain the operator account's
authority. Loopback binding is not caller authentication. Native daemon
mutations require the control bearer; read-only GET endpoints remain available
without it. An embedder can explicitly construct the separate legacy-open
control mode, so its choice must not be confused with the native default.

Fresh Git kit content is an external input. Requested identity, applicable
signer policy, inventory, file-type, size and digest checks remain meaningful.
HTTP installation's trust policy and per-session discovery of already installed
kits are separate consumers with separate defaults. A signature is not a
sandbox, and trusting an installed directory does not make every remote
publisher trusted. See `daemon/handle_kit.go`, `daemon/kit_package.go` and
`afcli/agent_run.go`.

## Attack surfaces and maintained hygiene

The following are review scenarios, not findings asserted by this document.

| Scenario | Boundary and prerequisite | Required controls or investigation |
| --- | --- | --- |
| A caller attempts a daemon mutation | Caller lacks the configured control bearer | Preserve independent bearer enforcement and fail-closed startup; loopback alone is insufficient |
| External kit bytes become accepted executable material | Publisher lacks operator or approved signer authority | Preserve identity, trust, containment and content verification; identify deliberate operator overrides separately |
| An isolated workload reaches host state or credentials | Actual deployment exposes a protected mount, socket or credential | Verify the concrete isolation and resource mapping; a label or environment filter is not evidence |
| A path is redirected during archive or kit operations | Identify a writer outside the trusted operator account and the protected consequence | Trace the real source, topology, operation and consumer; do not infer an attacker from a dynamic path alone |
| A restored symbolic link is followed later | Establish less-trusted content and a supported downstream consumer | Preserving a link object is different from following it for a read or write; retain the consumer-specific question |

Keep secret tokens and private state files owner-only, normally mode `0600`,
and private directories owner-only, normally `0700`. These protect against
other OS principals; they do not isolate two processes sharing the same UID.

Keep the requirement that writes must not follow symbolic links into unintended
targets. Retain bounded regular-file reads, rooted/exclusive destination
creation, exact lifecycle identity and no-replace publication where required.
The trusted-account policy is not permission to weaken any of these checks.
Individual writers and ancestor directories still require review; this model
does not assert a universal no-follow property merely because an operation uses
a temporary filename or rename.

## Severity calibration

Severity depends on an established boundary and consequence, not the presence
of a path operation or a local deployment alone.

- **Critical:** a supported untrusted caller can acquire broad privileged
  execution or credential authority across an established isolation boundary.
- **High:** a supported lower-trust actor can read, overwrite or execute a
  protected resource through an identified input and consumer.
- **Medium:** a supported boundary crossing has a narrower integrity,
  availability or disclosure consequence with established prerequisites.
- **Low:** bounded hardening or hygiene concerns with limited demonstrated
  impact. Do not promote an unestablished actor or consequence into a finding.

Account-owned changes requiring authority the trusted operator already has are
accepted same-user risk. A remote input, different OS principal or actual
sandbox escape requires its own source-backed assessment. Missing deployment
or consumer evidence remains an explicit question, not a proof of safety.
