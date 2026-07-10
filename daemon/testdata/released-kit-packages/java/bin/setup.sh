#!/usr/bin/env bash
# Kit post-acquire hook: Java
# Runs once after the workarea is acquired and ready.
# Resolves dependencies offline-ready (NOT the base toolchain).

set -euo pipefail

WORKAREA_ROOT="${1:-$(pwd)}"
cd "$WORKAREA_ROOT"

echo "[java kit] post_acquire: resolving dependencies..."

if [ -f "pom.xml" ]; then
  if [ -x "./mvnw" ]; then
    ./mvnw -B -q dependency:go-offline || echo "[java kit] dependency:go-offline non-fatal"
  elif command -v mvn &>/dev/null; then
    mvn -B -q dependency:go-offline || echo "[java kit] dependency:go-offline non-fatal"
  else
    echo "[java kit] WARNING: no Maven wrapper or mvn found. Skipping."
  fi
elif [ -f "build.gradle" ] || [ -f "build.gradle.kts" ]; then
  if [ -x "./gradlew" ]; then
    ./gradlew --no-daemon dependencies || echo "[java kit] gradle dependencies non-fatal"
  else
    echo "[java kit] WARNING: no Gradle wrapper found. Skipping."
  fi
fi

echo "[java kit] post_acquire: done."
