# Owned parser continuation state

The state machine derives from `github.com/charmbracelet/x/ansi` v0.11.7.
LICENSE preserves the upstream MIT terms; UPSTREAM.json records original
source hashes. The original ANSI Handler, Cmd, Param and Params remain aliases,
so callbacks and values keep their existing type identity. Parser transition
constants/table continue to use the pinned public ANSI parser package.

Local changes add versioned export/restore with explicit bounded validation,
owned buffers, exact finite/unlimited collection semantics and reachable
partial UTF-8 state. Safe parameter conversion and packed-byte extraction
replace the upstream unsafe slice/layout reads. An impossible invalid rune
state produces a replacement rune rather than panicking. No upstream private
layout is accessed through reflection or unsafe.

Keep this package module-private. An upstream update must refresh provenance,
retain every continuation field, preserve reachable malformed-byte behavior,
and run split-input equivalence, atomic-refusal and literal state-removal
controls together with the engine tests. Never infer missing state.
