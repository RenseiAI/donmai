---
name: typescript-debugging
description: Diagnose TypeScript compile, module-resolution, declaration, isolated-module, build, and type-check failures. Use when npm or TypeScript gates fail in a Node project.
license: MIT
metadata:
  author: donmai
  version: "1.0.0"
---

# TypeScript Debugging

## Overview

Debugging TypeScript compile and type errors in any Node/TypeScript project.
This skill provides systematic diagnosis workflows for the most common failure
modes that block `npm run build` / `npm run typecheck`.

## Triggers

Use this skill when:
- TypeScript type errors block `npm run build` or `npm run typecheck`
- The compiler reports module-resolution failures (`Cannot find module ...`)
- Re-exported types fail under `verbatimModuleSyntax` / `isolatedModules`
- Declaration emit (`.d.ts`) breaks downstream consumers

## Debugging Workflow

### 1. Type Errors

```
npm run typecheck 2>&1 | head -50
```

Read the FIRST error. TypeScript errors cascade — fix the root cause first.

**Common root causes:**
- Missing `export type` on re-exported types (fix: `export type { Foo }`)
- Incorrect return-type annotations (fix: infer from the implementation)
- Missing `@types/*` packages for untyped dependencies
- Circular imports (fix: break the cycle with an interface or a barrel file)
- `strictNullChecks` violations — narrow with a guard before use

### 2. Module Resolution

```
npm run build 2>&1 | grep -E "Cannot find module|TS2307|TS2305" | head -20
```

**Common root causes:**
- `paths` in tsconfig.json not mirrored by the bundler/runtime
- ESM/CJS mismatch — `"type": "module"` requires explicit `.js` import suffixes
- Workspace dependency not built — build the dependency package first

### 3. Test Failures

```
npm test 2>&1 | grep -E "FAIL|Error" | head -30
```

**Common root causes:**
- Missing test-environment config (vitest/jest config not picked up)
- Module not found — check `moduleNameMapper` / `resolve.alias`
- Async test timeouts — raise the per-test timeout

## Notes

- This kit is the FOUNDATION layer; a framework kit (e.g. the Next.js kit)
  composes on top and adds framework-specific commands and conventions.
