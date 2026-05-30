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

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git curl \
    && rm -rf /var/lib/apt/lists/*

# Prebuilt linux/amd64 donmai v0.10.0 binary (see worker/e2b/README.md).
COPY donmai /usr/local/bin/donmai
RUN chmod +x /usr/local/bin/donmai
