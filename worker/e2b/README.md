# donmai e2b sandbox template (`donmai-worker`)

An [e2b](https://e2b.dev) sandbox template that bakes the donmai `v0.10.0`
binary plus the minimal toolchain dependencies needed for the **runner-in-box**
model: the in-box `donmai agent run` process clones the target repo and installs
kit toolchains *after* clone, so the image only needs `donmai` + git + curl +
ca-certificates + apt + a POSIX shell.

## What's in the image

Mirrors the runtime stage of [`worker/Dockerfile`](../Dockerfile):

- Base: `debian:bookworm-slim` (provides `apt`, `sh`/`bash` for kit toolchain
  installs that the in-box runner performs after cloning a repo).
- `ca-certificates git curl` (via apt).
- Prebuilt **linux/amd64** `donmai` binary at `/usr/local/bin/donmai`
  (e2b sandboxes are x86_64 linux).

The compiled `donmai` binary is **not** committed (see `.gitignore`); it is
produced on demand by `build.sh` and copied into the e2b build context.

## Start command

`sleep infinity` — a long-lived keep-alive (set in `e2b.toml`).

The platform's e2b execution provider
(`platform/src/lib/providers/sandbox/e2b/index.ts`) supports two ways to launch
the in-box runner; this template is authored for the **explicit / inline** one:

1. **Inline launch (this template's model).** Keep the start command as a
   keep-alive (`sleep infinity`) and set the pool config
   `launchRunnerInline = true`. After `provision()` creates the sandbox the
   provider execs `nohup donmai agent run >/tmp/donmai-runner.log 2>&1 &`
   (`RUNNER_LAUNCH_CMD`) over the envd exec surface. The keep-alive start
   command exists only to keep the microVM alive so that backgrounded runner
   persists.
2. **Baked start command.** Alternatively the template's start command itself
   could be `donmai agent run` and the pool would leave `launchRunnerInline`
   unset. We did **not** do this here so the same image is reusable for the K2
   bare-exec path (the provider's `execCommand` runs kit shell commands in a
   freshly provisioned box) without an already-running runner racing it.

Either way the runner owns clone + kit install + agent loop in-box; there is no
remote Execer.

## Status: NOT yet pushed to e2b

The image was authored and the binary cross-compiles cleanly, but it was **not**
built/pushed to e2b because of two environment blockers:

1. **No CLI access token.** `e2b template create|list` authenticate with
   `E2B_ACCESS_TOKEN` (the CLI/CD management token from
   <https://e2b.dev/dashboard?tab=personal>), **not** the runtime `E2B_API_KEY`
   the SDK uses to spawn sandboxes. The only key available
   (`platform/.env.local` `E2B_API_KEY`) leaves the CLI reporting "Not logged
   in". Run `e2b auth login` (browser) or export `E2B_ACCESS_TOKEN`.
2. **Docker not installed / not running.** The current e2b CLI (2.10.2)
   `template create` builds the image with a local Docker daemon before pushing
   (`docker not found` on this host). Install + start Docker first.

Once both are resolved, `./build.sh` writes the resulting `template_id` into
`e2b.toml`; update the table below and the wiring section with that ID.

| Field        | Value                                |
| ------------ | ------------------------------------ |
| Name         | `donmai-worker`                      |
| Template ID  | _(pending first build — see above)_  |
| Start cmd    | `sleep infinity`                     |
| Arch         | linux/amd64                          |
| donmai ver   | v0.10.0                              |

## Rebuild

```sh
# requires: go, Docker running, e2b CLI (>=2.x) authenticated
#   (E2B_ACCESS_TOKEN in env or `e2b auth login`)
./build.sh
```

`build.sh` cross-compiles `donmai` for linux/amd64 into this directory, then runs
the e2b template build against `e2b.Dockerfile` / `e2b.toml`. The resulting
template ID is written back to `e2b.toml`.

> NOTE on CLI invocation: e2b CLI 2.10.2 deprecated the `e2b template build`
> (v1) subcommand in favour of `e2b template create <name>` (and a `migrate`
> path to the SDK build system). `build.sh` uses the build command that matches
> the installed CLI; adjust the final `e2b template ...` line if your CLI
> version differs.

## Wiring into the platform

Set the e2b execution provider pool's `config` JSON
(`execution_provider_pools.config` for the `providerId = 'e2b'` pool):

```json
{
  "templateId": "<TEMPLATE_ID_FROM_BUILD>",
  "launchRunnerInline": true
}
```

- `templateId` selects this image. The provider reads
  `spec.config.templateId` (falls back to `spec.config.template`, then the
  `DEFAULT_TEMPLATE` `"base"` — which has NO donmai binary and cannot host the
  runner, so this MUST be set).
- `launchRunnerInline = true` makes the provider exec `donmai agent run` inside
  the sandbox after create (this template's model — start cmd is just a
  keep-alive). Without it, with this `sleep infinity` start command, the sandbox
  boots but no runner starts.
