# Standalone configuration

Donmai has separate configuration for a repository, its local daemon, and
optional network services. These settings are not interchangeable.

## Repository configuration: `.donmai/config.yaml`

The local `donmai orchestrator` reads this optional file from the enclosing git
repository root. A missing file is allowed; invalid YAML or an invalid `kind`
fails startup. The file contains repository and project settings, not secrets.

This minimal example selects one Linear project and checks the git remote:

```yaml
apiVersion: v1
kind: RepositoryConfig
repository: github.com/example/my-app # Replace with your origin repository.
allowedProjects:
  - My Project # Exact Linear project name.
```

From that repository, with `LINEAR_API_KEY` configured:

```bash
donmai orchestrator --project "My Project" --dry-run
```

The dry run queries Linear and reports dispatch candidates without launching
agents. It still requires the Linear credential. `donmai orchestrator` currently
uses Linear; the `donmai github` commands do not select a different tracker for
this orchestrator.

### Every repository key

| Key | Omitted value | Meaning |
| --- | --- | --- |
| `apiVersion` | Empty string | Use `v1`. The current parser reads this value but does not validate the version. |
| `kind` | Invalid | Must be exactly `RepositoryConfig`. |
| `repository` | Empty string | Expected git `origin`; accepts HTTPS, SSH, or `github.com/owner/repo` notation. `--repo` overrides it. Empty means no remote-match check. |
| `allowedProjects` | Empty list | Linear project names this repository permits. Mutually exclusive with nonempty `projectPaths`. |
| `projectPaths` | Empty map | Project-name-to-path/settings map; its keys become the allowed project names. See below. |
| `sharedPaths` | Empty list | Shared directory metadata. The local orchestrator does not enforce it as a filesystem boundary. |
| `packageManager` | Empty string | Repository package-manager metadata, inherited by project entries. |
| `buildCommand` | Empty string | Build-command metadata, inherited by project entries. |
| `testCommand` | Empty string | Test-command metadata, inherited by project entries. |
| `validateCommand` | Empty string | Validation-command metadata, inherited by project entries. |
| `linearCli` | Empty string | CLI-command metadata. It does not replace the local orchestrator's native Linear client. |

The current local orchestrator consumes the repository match and project-name
selection. It parses the path and command metadata above but does not use it to
change the agent working directory, enforce path isolation, or run build/test
commands. Setting those keys alone does not add those behaviors.

Unknown YAML keys are currently ignored. A file loading successfully does not
prove that an additional setting is implemented. There are no implicit build,
test, validation, or package-manager defaults in this parser.

### Monorepo project entries

Use `projectPaths` instead of `allowedProjects`:

```yaml
apiVersion: v1
kind: RepositoryConfig
repository: github.com/example/my-app
packageManager: pnpm
buildCommand: pnpm build
testCommand: pnpm test
validateCommand: pnpm lint
sharedPaths: [packages/shared]
linearCli: donmai linear
projectPaths:
  Web:
    path: apps/web
    testCommand: pnpm test:web # Overrides the repository value.
  API: apps/api # String shorthand for an entry containing only path.
```

Each object entry accepts `path`, `packageManager`, `buildCommand`, `testCommand`,
and `validateCommand`. An omitted `path` is an empty string. Empty or omitted
command/package-manager values inherit the repository value. The parser does
not check that these paths exist or that these commands succeed.

`--project` selects one permitted project; without it, the orchestrator scans
the configured project names. If neither project setting is present, any
explicit `--project` is permitted, but omitting `--project` fails with “no project
specified.” An empty allowlist does not automatically discover every project.

## Queue choices and `REDIS_URL`

`donmai admin queue` and `donmai admin merge-queue` operate on a Redis-backed
queue. They require `REDIS_URL`, for example `redis://127.0.0.1:6379/0` for a
Redis instance you operate locally. They neither start Redis nor select a local
file queue. Use the URL for the queue you intend to inspect; mutating commands
can remove or requeue work.

```bash
REDIS_URL=redis://127.0.0.1:6379/0 donmai admin queue list
```

The setup wizard's “Local file queue (single-user)” choice writes a `file://`
orchestrator URL in the daemon configuration. **The current daemon does not
consume work from that directory:** this URL selects stub registration, and the
work poller starts only for a real remote registration. Do not rely on this
option for an unattended issue-to-agent workflow yet.

The local `donmai orchestrator` separately launches its chosen harness directly
from Linear issues; it does not enqueue those dispatches in Redis or the
wizard's directory. Local `code` commands need neither Redis nor a daemon.

## What the global `--url` means

`--url` selects the server base URL for API-backed commands. Precedence is an
explicit `--url`, then `WORKER_API_URL`, then `http://localhost:3000`. The default
is a client setting: the binary does not start a server on port 3000 for you.
Use a compatible server you operate when invoking commands that need that API.

This setting is distinct from the daemon's localhost control API (port 7734 by
default) and from the daemon's configured `orchestrator.url`, which is the
assignment source selected during host setup. Changing the global `--url` does
not configure either service.

The six native `donmai code` commands run against the local checkout and do not
need `--url`, credentials, or a running daemon. Optional semantic-search API keys
enable network-backed enhancements; without them the native lexical search
remains available. See the [code-intelligence MCP contract](CODE-INTELLIGENCE-MCP-CONTRACT.md)
for the local agent-facing server.

`donmai arch assess` also does not use `--url`. Its native diff retrieval uses
the GitHub CLI, so install `gh` and authenticate it for PR access. In the current
default mode, fetch failures warn on stderr and can return an empty assessment
with exit code 0. An empty result after such a warning is not proof that a PR
was checked.

For automated checks, use strict native assessment:

```bash
donmai arch assess https://github.com/example/my-app/pull/123 --require-diff
```

Strict mode returns exit code 2 when metadata or patches cannot be fetched, a
changed file has no matching patch section, or a legacy arch shim is selected.
It keeps exit code 1 for a triggered drift gate and 0 for a completed ungated
assessment. It requires GitHub access, but no other service or account.

For provider credentials, see [standalone credentials](agents/CREDENTIALS-STANDALONE.md).
For daemon configuration and its control API, see the [daemon manual](../daemon/README.md).
