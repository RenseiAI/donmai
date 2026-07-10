---
name: ruby-debugging
description: Diagnose Ruby load, Bundler resolution, RuboCop, and RSpec failures. Use when gem installation, requires, linting, or Ruby tests fail or hang.
license: MIT
metadata:
  author: donmai
  version: "1.0.0"
---

# Ruby Debugging

## Overview

Debugging Ruby load errors, Bundler resolution problems, rubocop offenses, and
spec failures. Systematic diagnosis for the most common Ruby/Bundler failure
modes.

## Triggers

Use this skill when:
- `bundle install` fails to resolve gems
- `bundle exec rspec` fails, errors on load, or hangs
- `bundle exec rubocop` reports offenses that block validate
- `LoadError` / `cannot load such file` at require time

## Debugging Workflow

### 1. Bundler / Gem Resolution

```
bundle install 2>&1 | head -40
```

**Common root causes:**
- Version conflicts in Gemfile.lock — `bundle update <gem>` for the conflicting
  dependency, or relax the constraint in the Gemfile
- Native-extension build failure — a missing system library/header
- Wrong Ruby version — check `.ruby-version` vs the active rbenv version
  (`rbenv version`)

### 2. Load Errors

```
ruby -e "require './lib/<file>'" 2>&1 | head -20
```

**Common root causes:**
- `require` vs `require_relative` mismatch
- File not on the load path — run via `bundle exec` so the gem's lib/ is loaded
- Missing gem — add it to the Gemfile and `bundle install`

### 3. Rubocop (validate gate)

```
bundle exec rubocop 2>&1 | tail -30
```

Most offenses are auto-correctable: `bundle exec rubocop -A` (safe) /
`-a` (autocorrect). Prefer fixing over inline `# rubocop:disable`.

### 4. Spec Failures

```
bundle exec rspec 2>&1 | grep -E "Failure|Error|examples" | head -30
```

**Common root causes:**
- Shared mutable state across examples — reset in `before`/`after` hooks
- Stubbed/mocked methods drifting from the real API
- Flaky tests depending on example ordering (`--order random` surfaces these)

## Notes

- rbenv shims live in `~/.rbenv/shims`. If `ruby`/`bundle` are not found, run
  `eval "$(rbenv init - bash)"` first (the installer set rbenv up there).
- Always invoke tools through `bundle exec` to use the locked gem versions.
