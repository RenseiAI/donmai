---
name: go-debugging
description: Diagnose Go build, vet, test, race, and module-resolution failures. Use when Go commands fail, hang, or report dependency and checksum drift.
license: MIT
metadata:
  author: donmai
  version: "1.0.0"
---

# Go Debugging

## Overview

Debugging Go build, vet, and test failures. Systematic diagnosis for the most
common failure modes in Go modules.

## Triggers

Use this skill when:
- `go build ./...` fails to compile
- `go vet ./...` reports issues
- `go test ./...` fails, hangs, or flakes under `-race`
- Module resolution / `go.sum` mismatch errors appear

## Debugging Workflow

### 1. Build Failures

```
go build ./... 2>&1 | head -40
```

**Common root causes:**
- Unused imports / variables (Go treats these as hard errors)
- Type mismatch — read the package + line; Go errors are precise
- Missing `go mod tidy` after adding an import (`missing go.sum entry`)
- Mismatched module path vs import path

### 2. Vet / Static Analysis

```
go vet ./... 2>&1 | head -40
```

**Common root causes:**
- `Printf` format/arg mismatches
- Lock copied by value (`copylocks`)
- Unreachable code, shadowed errors

### 3. Test Failures (run with -race for concurrency code)

```
go test -race ./... 2>&1 | grep -E "FAIL|panic|DATA RACE|--- FAIL" | head -30
```

**Common root causes:**
- Data races — the `-race` detector points at both goroutines; guard with a
  mutex or restructure ownership
- Goroutine leaks / deadlocks — a hanging test usually waits on an unbuffered
  channel that never receives
- Flaky tests that pass under `-p 1` but fail in parallel — shared global state

## Notes

- Always run tests with `-race` for any code touching goroutines/channels; CI
  exposes flakes a serial local run hides.
