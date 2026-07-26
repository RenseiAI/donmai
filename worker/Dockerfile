# Worker image for cloud sandbox providers (docker, kubernetes; also the default
# image for modal/daytona). It bakes the donmai binary in and sets
# `donmai agent run` as its ENTRYPOINT so the box self-registers, polls for
# work, clones the repo, installs kit toolchains IN-BOX via the runner's local
# shellExecer, and runs the agent loop — all in-box, on one filesystem, with no
# remote Execer. This is the runner-in-box model.
# See runs/2026-05-28-kits-delivery/01-cloud-execer-research.md.
#
# The platform references this image as
#   ghcr.io/renseiai/donmai-worker:latest
# from src/lib/providers/sandbox/{docker,kubernetes,modal,daytona}/index.ts.
#
# BUILD CONTEXT: the donmai repo ROOT (this file lives in worker/ but builds
# from the module root so `go build ./cmd/donmai` can see the whole module).
#
#   # from the donmai repo root:
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f worker/Dockerfile \
#     --build-arg DONMAI_VERSION=v0.11.0 \
#     -t ghcr.io/renseiai/donmai-worker:v0.11.0 \
#     -t ghcr.io/renseiai/donmai-worker:latest \
#     --push .
#
# Requires ghcr.io push creds (a PAT / GITHUB_TOKEN with write:packages on the
# renseiai org). For kubernetes, the cluster must also be able to pull the image
# (imagePullSecrets if the package is private).

# --- build stage: compile donmai from source (matches .goreleaser build) ---
# Pinned to $BUILDPLATFORM and cross-compiled to $TARGETPLATFORM rather than
# run under QEMU. CGO_ENABLED=0 makes this a pure Go cross-compile — the same
# thing .goreleaser does to produce every published arch from one host — so the
# emitted binary is byte-for-byte the arch we asked for, with no emulator in
# the loop. Emulating this stage cost ~178s for the linux/arm64 leg versus
# ~22s native (measured, worker-image-pr run 30196904421).
FROM --platform=$BUILDPLATFORM golang:1.25.12-bookworm AS build
ARG DONMAI_VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${DONMAI_VERSION}" \
    -o /out/donmai ./cmd/donmai

# --- go toolchain stage: the TARGET-arch Go toolchain for the runtime image ---
# The runtime image ships a real Go toolchain for RW4 Go reachability, and that
# toolchain must be native to the image's own arch — it cannot come from the
# cross-compiling `build` stage above, which is always $BUILDPLATFORM. This
# stage runs no commands; buildkit only pulls layers and COPYs out of it, so
# naming it for $TARGETPLATFORM costs no emulation.
FROM golang:1.25.12-bookworm AS gotoolchain

# --- ts-morph deps stage: pre-install the code-survival TS reachability deps ---
# RW4 reachability runs a baked ts-morph node script as a subprocess. Pre-install
# its node_modules here so there is NO npm latency at scan time. The whole dir
# (script + node_modules + package.json) is copied into the runtime image.
FROM node:22-bookworm-slim AS ts-deps
WORKDIR /ts-morph
COPY codesurvival/reachability/ts-morph/package.json ./package.json
# Install ts-morph (and its TypeScript dep) into node_modules. npm install (not
# `ci`) because the script ships no lockfile; deps are pinned by ^range in
# package.json. --omit=dev keeps the layer lean (the node test is dev-only).
RUN npm install --omit=dev --no-audit --no-fund
COPY codesurvival/reachability/ts-morph/reachability.js ./reachability.js

# --- runtime stage: box with git + a shell for kits + Go & Node for RW4 ---
# Kit toolchain installs run in-box via `sh -c`, and the runner clones repos, so
# the runtime image needs git, ca-certificates, and a shell. Debian slim keeps
# `sh`/`bash` + apt available for kits that install OS packages.
#
# RW4 (code-survival reachability) additionally needs:
#   - the Go toolchain (golang.org/x/tools/go/packages shells out to `go list`
#     over the cloned repo to build the SSA call graph), and
#   - node + the baked ts-morph script (TS/JS reachability subprocess).
# Both are toolchains the scan executor probes for; when absent it degrades to
# status:partial rather than failing, so the image stays buildable even if a
# future base drops one.
FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates git curl \
    && rm -rf /var/lib/apt/lists/*

# Go toolchain for RW4 Go reachability (matches the build stage's 1.25).
# Sourced from the target-arch `gotoolchain` stage, NOT from `build`: `build`
# cross-compiles from $BUILDPLATFORM and its /usr/local/go is the builder's
# arch, which would be unrunnable inside a foreign-arch runtime image.
COPY --from=gotoolchain /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
# GOFLAGS/GOTOOLCHAIN: do not auto-download a newer toolchain at scan time (the
# box is network-restricted to the clone remote); use the baked one as-is.
ENV GOTOOLCHAIN=local GOFLAGS=-mod=mod

# Node runtime for RW4 TS reachability AND the provider CLIs.
# Copying from the node base image gives us node + npm; after the copy we
# install the provider CLIs:
#
#   @anthropic-ai/claude-code  — the 'claude' provider shells out to `claude`
#                                on PATH; without it the claude probe fails
#                                ErrProviderUnavailable before any clone starts.
#
#   @openai/codex              — the 'codex' provider spawns `codex app-server`
#                                on PATH; without it any session dispatched with
#                                providerId='codex' fails the probe.
#
# Gemini: NO CLI install needed. The donmai binary (built above in the 'build'
# stage) ships a native HTTP provider (provider/gemini) compiled in — it calls
# the Gemini REST API directly with no external subprocess. The build stage
# always compiles from source, so the latest provider/gemini code ships with
# every image rebuild triggered by a release tag.
COPY --from=node:22-bookworm-slim /usr/local/bin/node /usr/local/bin/node
COPY --from=node:22-bookworm-slim /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -sf /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && npm i -g @anthropic-ai/claude-code @openai/codex \
    && npm cache clean --force

# Baked code-survival TS reachability script + its pre-installed node_modules.
# reachability_ts.go resolves this default path relative to the donmai binary
# (/usr/local/bin/donmai → ../lib/code-survival/ts-morph/reachability.js);
# CODE_SURVIVAL_TS_REACHABILITY_SCRIPT can override it.
COPY --from=ts-deps /ts-morph /usr/local/lib/code-survival/ts-morph
ENV CODE_SURVIVAL_TS_REACHABILITY_SCRIPT=/usr/local/lib/code-survival/ts-morph/reachability.js

COPY --from=build /out/donmai /usr/local/bin/donmai

# The box's reason to exist: run the in-box donmai runner. It reads its session
# context from the env injected by the platform (DONMAI_SESSION_ID, registration
# token, etc.), self-registers, and owns clone + kit install + agent loop.
ENTRYPOINT ["donmai", "agent", "run"]
