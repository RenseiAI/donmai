# Worker image for cloud sandbox providers (docker, kubernetes; also the default
# image for modal/daytona). It bakes the donmai binary in and sets
# `donmai agent run` as its ENTRYPOINT so the box self-registers, polls for
# work, clones the repo, installs kit toolchains IN-BOX via the runner's local
# shellExecer, and runs the agent loop — all in-box, on one filesystem, with no
# remote Execer. This is the runner-in-box model.
# See runs/2026-05-28-kits-delivery/01-cloud-execer-research.md.
#
# The platform references this image as
#   ghcr.io/renseiai/agentfactory-worker:latest
# from src/lib/providers/sandbox/{docker,kubernetes,modal,daytona}/index.ts.
#
# BUILD CONTEXT: the donmai repo ROOT (this file lives in worker/ but builds
# from the module root so `go build ./cmd/donmai` can see the whole module).
#
#   # from the donmai repo root:
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f worker/Dockerfile \
#     --build-arg DONMAI_VERSION=v0.11.0 \
#     -t ghcr.io/renseiai/agentfactory-worker:v0.11.0 \
#     -t ghcr.io/renseiai/agentfactory-worker:latest \
#     --push .
#
# Requires ghcr.io push creds (a PAT / GITHUB_TOKEN with write:packages on the
# renseiai org). For kubernetes, the cluster must also be able to pull the image
# (imagePullSecrets if the package is private).

# --- build stage: compile donmai from source (matches .goreleaser build) ---
FROM golang:1.24-bookworm AS build
ARG DONMAI_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w -X main.version=${DONMAI_VERSION}" \
    -o /out/donmai ./cmd/donmai

# --- runtime stage: minimal box with git + a shell for kit installs ---
# Kit toolchain installs run in-box via `sh -c`, and the runner clones repos, so
# the runtime image needs git, ca-certificates, and a shell. Debian slim keeps
# `sh`/`bash` + apt available for kits that install OS packages.
FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates git curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/donmai /usr/local/bin/donmai

# The box's reason to exist: run the in-box donmai runner. It reads its session
# context from the env injected by the platform (RENSEI_SESSION_ID, registration
# token, etc.), self-registers, and owns clone + kit install + agent loop.
ENTRYPOINT ["donmai", "agent", "run"]
