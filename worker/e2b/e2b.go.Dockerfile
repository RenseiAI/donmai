# e2b sandbox template for Go repositories (`donmai-worker-go`).
#
# Same runner-in-box base as e2b.Dockerfile (debian:bookworm-slim with
# ca-certificates, git, curl, gh, Node 22 + provider CLIs, and the prebuilt
# linux/amd64 donmai binary at /usr/local/bin/donmai) plus a pinned Go
# toolchain, golangci-lint and gofumpt on PATH, sized for `go test -race ./...`.
#
# The donmai binary is built OUTSIDE this Dockerfile (see build.sh / README.md)
# and copied in, exactly like e2b.Dockerfile. Do NOT commit the binary.
#
# Single-stage only: the E2B template build SDK rejects multi-stage
# Dockerfiles, so this file must keep exactly one FROM and no COPY --from.
#
# Pins:
# - Go 1.26.10 tracks go.mod (`go 1.26.10`), via the official Go tarball
#   below. Bump the URL together with go.mod.
# - golangci-lint v2.11.4 tracks CI's lint action
#   (.github/workflows/ci.yml `golangci-lint-action` version).
# - gofumpt v0.12.0 is the first line supporting the Go 1.26 floor
#   (`go list -m -f {{.GoVersion}} mvdan.cc/gofumpt@v0.12.0` reports 1.26.0).
# Bump the three together with those sources.
#
# Build resources are set at build time (4 vCPU / 8 GiB) via E2B_CPU_COUNT /
# E2B_MEMORY_MB in e2b-template.yml — E2B's defaults (2 vCPU / 1 GiB) are too
# small for `go test -race ./...`. The base `donmai-worker` template keeps
# E2B's defaults.
FROM debian:bookworm-slim

# Base tools + GitHub CLI (`gh`) + C toolchain for `go test -race`. Same as
# e2b.Dockerfile: `gh` is not in Debian's default repos, so the official
# GitHub CLI apt repo is added. arch=amd64 matches the e2b sandbox arch and
# the prebuilt linux/amd64 donmai binary. gcc + libc6-dev satisfy the Linux
# race detector's cgo requirement (a C compiler must be installed).
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git curl gcc libc6-dev \
    && mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
         -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
         > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

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
# the same binary blob between the base and Go template builds.
COPY donmai /usr/local/bin/donmai
RUN chmod +x /usr/local/bin/donmai

# Go toolchain for Go seats (matches go.mod's 1.26.10 floor). Single-stage
# tarball install keeps this Dockerfile to one FROM for the E2B build SDK.
RUN curl -fsSL https://go.dev/dl/go1.26.10.linux-amd64.tar.gz -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz \
    && /usr/local/go/bin/go version
ENV PATH=/usr/local/go/bin:/usr/local/bin:${PATH}

# Go lint + format tools, version-pinned (see header). GOBIN keeps the
# binaries on the default PATH without an extra GOPATH entry.
RUN go version \
    && GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 \
    && GOBIN=/usr/local/bin go install mvdan.cc/gofumpt@v0.12.0 \
    && go version \
    && golangci-lint --version \
    && gofumpt --version
