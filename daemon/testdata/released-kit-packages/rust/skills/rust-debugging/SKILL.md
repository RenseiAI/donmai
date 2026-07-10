---
name: rust-debugging
description: Diagnose Rust compile, borrow-checker, trait, Clippy, formatting, and test failures. Use when Cargo commands fail or report lifetime, bound, lint, or test errors.
license: MIT
metadata:
  author: donmai
  version: "1.0.0"
---

# Rust Debugging

## Overview

Debugging Rust compile errors, borrow-checker failures, clippy lints, and test
failures. Systematic diagnosis for the most common Cargo failure modes.

## Triggers

Use this skill when:
- `cargo build` fails to compile (borrow checker, trait bounds, lifetimes)
- `cargo clippy -- -D warnings` blocks the validate gate
- `cargo test` fails, panics, or doctests break
- `cargo fmt --check` reports formatting drift

## Debugging Workflow

### 1. Compile / Borrow-Checker Errors

```
cargo build 2>&1 | head -60
```

Read the FIRST error and its `help:` / `note:` lines — rustc's diagnostics
usually point at the exact fix.

**Common root causes:**
- Borrow conflicts — restructure ownership, clone deliberately, or scope the
  borrow with a block
- Lifetime mismatches — name the lifetime explicitly or return an owned value
- Missing trait bounds — add `where T: Trait` or derive the trait
- Moved value used after move — borrow (`&`) instead, or clone

### 2. Clippy Lints (validate gate)

```
cargo clippy -- -D warnings 2>&1 | head -40
```

Fix the lint rather than `#[allow(...)]`-ing it; clippy's suggestion is usually
the idiomatic form.

### 3. Test Failures

```
cargo test 2>&1 | grep -E "FAILED|panicked|error\[" | head -30
```

**Common root causes:**
- `unwrap()`/`expect()` panics on `None`/`Err` — handle the variant
- Doctest failures — the example in `///` comments must compile and run
- Integration tests in `tests/` need the crate's public API only

## Notes

- cargo/rustc live in `~/.cargo/bin`. If a command reports `cargo: not found`,
  source `~/.cargo/env` first (the toolchain installer wrote it there).
