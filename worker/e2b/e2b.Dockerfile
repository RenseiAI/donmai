# e2b sandbox template for the donmai runner-in-box model.
#
# Mirrors the runtime stage of worker/Dockerfile: a debian-slim base with
# ca-certificates, git, and curl, plus the prebuilt linux/amd64 donmai binary
# at /usr/local/bin/donmai.
#
# The donmai binary is built OUTSIDE this Dockerfile (see build.sh / README.md)
# and copied in. It is intentionally NOT committed to git (.gitignore) and NOT
# built inside the image, to keep the e2b build a single fast layer copy.
#
# Runner-in-box: the platform's e2b provider boots a sandbox from this template
# and then execs `donmai agent run` itself. The in-box runner clones the target
# repo and installs kit toolchains after clone, which is why apt + a POSIX shell
# (both present in debian:bookworm-slim) must remain available at runtime.
FROM debian:bookworm-slim

# Base tools + GitHub CLI (`gh`). `gh` is not in Debian's default repos, so we
# add the official GitHub CLI apt repo (REN-1554 / GAP 3). Development work
# types open PRs via `gh pr create`; `gh` auto-authenticates from the GH_TOKEN
# the platform threads in-box (the short-lived git clone token). arch=amd64
# matches the e2b sandbox arch and the prebuilt linux/amd64 donmai binary.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git curl \
    && mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
         -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
         > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Node 20 + provider CLIs.
#
# Claude Code CLI: the 'claude' provider shells out to `claude` on PATH.
# Without it donmai agent run fails "no provider registered for name claude"
# before the clone even starts. The org's ANTHROPIC_API_KEY arrives at runtime
# via the credential snapshot (byok).
#
# Codex CLI (@openai/codex): the 'codex' provider spawns `codex app-server`
# on PATH. Without it the codex probe returns ErrProviderUnavailable and any
# session dispatched to this sandbox with providerId='codex' fails the probe.
# The org's OPENAI_API_KEY arrives at runtime via the credential snapshot.
#
# Gemini: NO CLI install needed. The donmai binary ships a native HTTP provider
# (provider/gemini) that calls the Gemini REST API directly — no external
# subprocess. The org's GEMINI_API_KEY / GOOGLE_API_KEY arrives at runtime via
# the credential snapshot.
#
# This is only the agent CLI layer — repo language toolchains are still
# installed in-box by the kit after clone (the runner's shellExecer).
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm i -g @anthropic-ai/claude-code @openai/codex \
    && npm cache clean --force \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Prebuilt linux/amd64 donmai binary (see worker/e2b/README.md and build.sh).
# The binary is cross-compiled outside this Dockerfile (GOOS=linux GOARCH=amd64
# CGO_ENABLED=0 go build ./cmd/donmai) and placed at worker/e2b/donmai before
# `e2b template create` runs. The CI workflow (e2b-template.yml) does this in
# the "Cross-compile donmai binary" step. The compiled binary INCLUDES the
# native Gemini provider (provider/gemini) — no gemini CLI is required.
COPY donmai /usr/local/bin/donmai
RUN chmod +x /usr/local/bin/donmai
