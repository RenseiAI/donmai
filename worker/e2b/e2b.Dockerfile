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

# Node 20 + the Claude Code CLI. The in-box agent-runtime provider 'claude' (the
# resolved model is Claude Opus 4.8) must be on PATH, else `donmai agent run`'s
# registry can't register it and the runner fails "no provider registered for
# name claude" BEFORE the clone. This is only the agent CLI — repo language
# toolchains are still installed in-box by the kit after clone (the runner's
# shellExecer). The org's ANTHROPIC_API_KEY arrives at runtime via the
# credential snapshot (byok).
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm i -g @anthropic-ai/claude-code \
    && npm cache clean --force \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Prebuilt linux/amd64 donmai v0.10.0 binary (see worker/e2b/README.md).
COPY donmai /usr/local/bin/donmai
RUN chmod +x /usr/local/bin/donmai
