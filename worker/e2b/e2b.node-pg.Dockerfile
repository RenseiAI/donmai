# e2b sandbox template for large Node.js/TypeScript repositories with
# Postgres-backed tests (`donmai-worker-node-pg`).
#
# Same runner-in-box base as e2b.Dockerfile (debian:bookworm-slim with
# ca-certificates, git, curl, gh, Node 22 + provider CLIs, and the prebuilt
# linux/amd64 donmai binary at /usr/local/bin/donmai) plus:
#
# - pnpm via corepack, pinned by ARG PNPM_VERSION below and additionally
#   installed globally with npm at the same pin. The npm-global copy is the
#   PATH-resolved fallback: E2B executes sandbox commands as a non-root user
#   whose login shell cannot read root's corepack download cache, while the
#   npm-global binary in /usr/bin serves every user. Both copies report the
#   same `pnpm --version`; the verifier asserts it.
# - A warm pnpm content-addressable store. The release build always warms the
#   store from the pinned inline seed below (exact versions only, so template
#   rebuilds stay deterministic) via `pnpm fetch`; the seed directory is
#   removed again in the same layer, keeping only the store blobs. A warm
#   store only accelerates installs whose lockfile resolves the same exact
#   versions, so seats whose lockfile is available at build time should extend
#   the seed before building. Pass --build-arg PNPM_WARM_STORE=0 for a cold
#   store. No lockfile from any private repository is ever baked in: the seed
#   is authored in this file, and only store blobs ship in the image.
# - Postgres 17 with the pgvector extension (from the official PGDG apt
#   repository; Debian bookworm itself ships only Postgres 15). The cluster
#   is created at build time with trust authentication for local socket and
#   loopback TCP connections, so in-box test suites can connect without a
#   password: this template is an ephemeral single-tenant sandbox, and no
#   credential is baked in (there is no password to bake). `pg-start`
#   (in /usr/local/bin, on the login default PATH) brings the cluster up on
#   first use; the build-time smoke below proves the whole path before the
#   template ships.
#
# Seats running tsc on large repositories should export
# NODE_OPTIONS=--max-old-space-size=12288 (12 GiB of the 16 GiB box). It is
# intentionally NOT set here so the provider CLIs keep their default heaps.
#
# The donmai binary is built OUTSIDE this Dockerfile (see build.sh / README.md)
# and copied in, exactly like e2b.Dockerfile. Do NOT commit the binary.
#
# Single-stage only: the E2B template build SDK rejects multi-stage
# Dockerfiles, so this file must keep exactly one FROM and no COPY --from.
#
# Build resources are set at build time (8 vCPU / 16 GiB) via E2B_CPU_COUNT /
# E2B_MEMORY_MB in e2b-template.yml — E2B's defaults (2 vCPU / 1 GiB) are too
# small for `vitest` plus a type check with a 12 GiB heap on a large
# TypeScript repository. The base `donmai-worker` template keeps E2B's
# defaults; the Go template builds at 4 vCPU / 8 GiB.
FROM debian:bookworm-slim

# Base tools + GitHub CLI (`gh`) + gnupg for the PGDG keyring. Same as
# e2b.Dockerfile: `gh` is not in Debian's default repos, so the official
# GitHub CLI apt repo is added. arch=amd64 matches the e2b sandbox arch and
# the prebuilt linux/amd64 donmai binary. gnupg de-armors the PGDG release
# key below (bookworm-slim ships no gpg). Native-addon builds (node-gyp)
# are left to the in-box install: apt stays available at runtime for seats
# that need a compiler.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git curl gnupg \
    && mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
         -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
         > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Postgres 17 + pgvector from the official PGDG repository. bookworm-pgdg is
# the PGDG suite tracking Debian bookworm; the postgresql-17 and
# postgresql-17-pgvector names pin the major version (the verifier asserts
# major 17 and a working `CREATE EXTENSION vector`), while patch updates
# float with the repository so security fixes are picked up at build time.
RUN curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
         | gpg --dearmor -o /etc/apt/keyrings/postgresql.gpg \
    && chmod go+r /etc/apt/keyrings/postgresql.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/postgresql.gpg] http://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
         > /etc/apt/sources.list.d/pgdg.list \
    && apt-get update && apt-get install -y --no-install-recommends \
         postgresql-17 postgresql-17-pgvector \
    && rm -rf /var/lib/apt/lists/*

# Trust authentication for local connections. The PGDG postinst creates the
# `main` cluster with peer/scram-sha-256 auth, which rejects passwordless
# `psql -U postgres` from any uid but the postgres user. This template is an
# ephemeral single-tenant sandbox with no password anywhere, so local socket
# and loopback TCP connections trust: in-box `*.pg.test.ts` suites connect
# with no credential setup. Remote TCP is untouched (no listen_addresses
# change here; the default is localhost-only).
RUN sed -i -E \
      -e 's/^(local\s+all\s+(all|postgres)\s+)peer$/\1trust/' \
      -e 's/^(host\s+all\s+all\s+(127\.0\.0\.1\/32|::1\/128)\s+)scram-sha-256$/\1trust/' \
      /etc/postgresql/17/main/pg_hba.conf \
    && grep -E '^(local|host)' /etc/postgresql/17/main/pg_hba.conf

# pg-start: bring the local Postgres 17 cluster up on first use. Idempotent
# (a second call is a no-op) so seats and the template verifier can call it
# unconditionally, and it waits for readiness instead of returning while the
# server is still starting. Lives in /usr/local/bin so it resolves under
# `bash -l -c`, the shell E2B uses for build steps and SDK version checks.
RUN cat > /usr/local/bin/pg-start <<'EOF'
#!/bin/sh
# Bring up the template's local Postgres 17 cluster on first use.
set -eu
if pg_lsclusters 17 main 2>/dev/null | grep -q ' online'; then
  echo "postgres 17/main already online"
else
  pg_ctlcluster 17 main start
fi
for _ in $(seq 1 30); do
  if pg_isready -h /var/run/postgresql -U postgres >/dev/null 2>&1; then
    echo "postgres 17/main ready"
    exit 0
  fi
  sleep 1
done
echo "pg-start: postgres 17/main did not become ready" >&2
exit 1
EOF \
    && chmod +x /usr/local/bin/pg-start

# Node 22 + provider CLIs. Same pins as e2b.Dockerfile and worker/Dockerfile:
# an unpinned `npm i -g pkg` lets a stale cached layer quietly stand in for a
# newer/patched package forever (see worker/Dockerfile's CREEP-class cache
# poisoning note). pi stays EQUAL to PinnedVersion in
# provider/harness/pi/probe.go — bump both together.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm i -g @anthropic-ai/claude-code@2.1.283 @openai/codex@0.157.1 \
         @earendil-works/pi-coding-agent@0.80.10 \
    && npm cache clean --force \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Prebuilt linux/amd64 donmai binary (see worker/e2b/README.md and build.sh).
# Cross-compiled outside this Dockerfile (GOOS=linux GOARCH=amd64
# CGO_ENABLED=0 go build ./cmd/donmai) and placed at worker/e2b/donmai before
# the build runs. The CI workflow (e2b-template.yml) does this once and shares
# the same binary blob between the base, Go, and node-pg template builds.
COPY donmai /usr/local/bin/donmai
RUN chmod +x /usr/local/bin/donmai

# pnpm, pinned once via ARG and installed both ways at that pin (see the
# header for why both are needed). PNPM_VERSION is the single source of truth
# the template verifier parses; keep the corepack prepare and the npm install
# on ${PNPM_VERSION} so they can never drift apart.
ARG PNPM_VERSION=12.16.1
RUN corepack enable \
    && corepack prepare pnpm@${PNPM_VERSION} --activate \
    && npm i -g pnpm@${PNPM_VERSION} \
    && npm cache clean --force \
    && bash -l -c 'pnpm --version'

# Warm pnpm store from a pinned inline seed. Exact versions only: the store is
# content-addressed, so only installs resolving these exact versions hit the
# cache. The seed below covers the template's workload (vitest suites, tsc
# type checks, node-postgres suites). `pnpm install --lockfile-only` pins the
# seed's lockfile deterministically; `pnpm fetch` populates the store without
# running install scripts; the seed directory is removed in the same layer.
# Pass --build-arg PNPM_WARM_STORE=0 to skip warming (cold store).
ARG PNPM_WARM_STORE=1
RUN if [ "${PNPM_WARM_STORE}" = "1" ]; then \
      mkdir -p /tmp/pnpm-seed && cd /tmp/pnpm-seed \
      && printf '%s\n' '{' \
        '  "name": "node-pg-seed",' \
        '  "private": true,' \
        '  "version": "0.0.0",' \
        '  "dependencies": {' \
        '    "pg": "8.23.1",' \
        '    "typescript": "7.0.2",' \
        '    "vitest": "5.0.5"' \
        '  }' \
        '}' > package.json \
      && pnpm install --lockfile-only \
      && pnpm fetch \
      && cd / && rm -rf /tmp/pnpm-seed; \
    else \
      echo "PNPM_WARM_STORE=${PNPM_WARM_STORE}: pnpm store starts cold"; \
    fi \
    && bash -l -c 'pnpm --version'

# Build-time smoke: prove the whole Postgres path before the template ships.
# Starts the cluster via pg-start, creates a scratch database, enables the
# vector extension in it, then cleans up and stops the cluster so the first
# in-box pg-start exercises the start path. Any failure here fails the
# template build rather than shipping a template whose verify step fails at
# its first `CREATE EXTENSION vector`.
RUN pg-start \
    && psql -U postgres -d postgres -c "DROP DATABASE IF EXISTS build_probe" \
    && createdb -U postgres build_probe \
    && psql -U postgres -d build_probe -c 'CREATE EXTENSION vector' \
    && psql -U postgres -d postgres -c 'DROP DATABASE build_probe' \
    && pg_ctlcluster 17 main stop
