---
name: java-debugging
description: Diagnose Java compile, Maven, Gradle, dependency, JDK-version, and test failures. Use when JVM project builds fail, hang, or resolve incompatible artifacts.
license: MIT
metadata:
  author: donmai
  version: "1.0.0"
---

# Java Debugging

## Overview

Debugging Java compile failures, Maven/Gradle build problems, and test failures.
Systematic diagnosis for the most common JVM build-tool failure modes.

## Triggers

Use this skill when:
- `./mvnw compile` / `./gradlew compileJava` fails
- Maven/Gradle reports dependency resolution errors
- `./mvnw test` fails or hangs
- Version/JDK mismatch errors (`class file has wrong version`, `release 17`)

## Debugging Workflow

### 1. Compile Failures

```
./mvnw -B compile 2>&1 | grep -E "ERROR|error:" | head -40
```

**Common root causes:**
- Missing import / unresolved symbol — check the dependency is declared
- `release`/`source`/`target` mismatch — JDK version vs `<maven.compiler.release>`
- Generics / type-inference errors — annotate explicitly to localize the fix

### 2. Dependency Resolution

```
./mvnw -B dependency:tree 2>&1 | grep -iE "conflict|omitted|error" | head -30
```

**Common root causes:**
- Version conflicts across transitive deps — pin in `<dependencyManagement>`
- Wrong/missing repository — check `~/.m2/settings.xml` and `<repositories>`
- Offline mode without a primed cache — run `dependency:go-offline` first

### 3. Test Failures

```
./mvnw -B test 2>&1 | grep -E "FAIL|ERROR|Tests run" | head -30
```

**Common root causes:**
- `NullPointerException` — trace the null source; prefer `Optional`
- Flaky tests depending on ordering or shared static state
- Surefire/JUnit version mismatch — align the plugin and the JUnit dependency

## Notes

- This kit assumes a committed Maven wrapper (`./mvnw`). For Gradle projects,
  override the build/test/validate commands via a local `.kit.toml` manifest.
- The JDK is provisioned by the toolchain_install scripts (Temurin 17).
