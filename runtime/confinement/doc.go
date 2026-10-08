// Package confinement is the executor's OS confinement of a harness process
// that has no sandbox of its own, per
// ADR-2026-10-03-executor-os-confinement.md.
//
// The boundary wraps the harness process itself, at its own spawn and in both
// session modes; the worker and the session shim stay outside it. A session's
// Spec names the closed writable set (D2): its mutable repository leaves with
// their own .git directories, the harness's per-session state, an
// executor-owned per-session temporary directory and per-session caches, plus
// the device nodes a process needs. Everything else is read-only to the
// harness, and the read-only leaves, the workarea root and its metadata, and
// any protected path inside the set are denied after the writable allows, so
// they win.
//
// A Spec can also confine reads (ReadScope, the fileRead level). Under the
// workarea read scope, file contents and directory listings are refused
// everywhere except the read allowlist: the writable set, the read-only
// leaves, the declared ReadPaths and the runtime and toolchain paths the
// backend names. Metadata stays readable, so path lookups keep working,
// and a search over the whole disk is refused at the top instead of
// walking it. home-minus-secrets is not implemented and refuses.
//
// A Confiner is the production spawn binding:
//
//	c, _ := confinement.New(confinement.Options{
//		Backend:    confinement.DefaultBackend(),
//		ProfileDir: profileDir, // under the host state home
//		Home:       home,
//		StateHome:  stateHome,
//		ExtraRules: composerDenies, // optional, deny-only
//	})
//	record, err := c.SelfTest(ctx, confinement.SelfTestOptions{
//		ProbeCommand: []string{selfExecutable},
//		ScratchDir:   scratch,
//	})
//	plan, err := c.Prepare(spec)     // typed *Error when unavailable
//	argv, err := plan.Command(harnessArgv)
//	env := plan.Environment()        // TMPDIR/TMP/TEMP and cache bindings
//	evidence := plan.Record().Digest()
//
// Prepare refuses, never degrades: with no backend (backend_absent), from a
// process already inside a profile (nested_sandbox), without a passing and
// current self-test for the session mode (self_test_failed, self_test_stale),
// for a writable set it cannot represent (writable_set_unrepresentable), or
// for a composer rule the backend cannot render (rule_unrenderable).
//
// The self-test drives a probe process — an executable that calls
// RunLandlockStageFromEnv first in main, then RunProbeFromEnv (the stage
// marker names the re-executed stage invocation only, and the probe marker
// the probe invocation; an executable that never runs the stage entrypoint
// fails the self-test closed instead of running unconfined) — through the
// same Prepare and Command, once
// through the headless spawn path and once through the PTY host, and judges
// every probe by what changed on disk. A second pass per mode renders the
// same world under the workarea read scope and judges reads inside and
// outside the allowlist. Replacing the backend with nothing, or with
// same-identity permission bits, turns it red; so does dropping only the
// read rules, for exactly the read-scope probes.
//
// A spawn site that wraps a harness must not hand it a descriptor open on a
// file outside the writable set: the boundary judges an open, not a write
// through a descriptor the process inherited.
//
// The macOS backend renders a sandbox profile and runs the harness under
// /usr/bin/sandbox-exec. Under the read scope it denies file contents and
// extended attributes together (a transparently compressed file keeps its
// contents in attributes) and its read allowlist names the OS userland, the
// frameworks (the system volumes, a second spelling of the data volume,
// re-denied), the command line tools and the active developer application,
// Homebrew's binaries, libraries and configuration, the system
// configuration, the Xcode licence record and xcrun's lookup cache that the
// /usr/bin developer shims read, and the device nodes a process needs, by
// name: never a terminal. Package data (Homebrew's var and /usr/local/var,
// where database directories and keys live) is re-denied after the runtime
// allows. dyld reads the root directory itself at every exec, so the root
// stays listable and nothing under it does unless named.
//
// The Linux backend renders the same contract as a bubblewrap mount tree:
// a tmpfs root hides everything, read-only binds carry the OS userland,
// the operator home and the host state home (so file metadata stays
// readable) and the declared sockets, the writable set is bound read-write
// over them, deny overlays win last only where host content would otherwise
// show through a writable bind, and a Landlock stage the harness process
// itself executes confines reads outside the allowlist. TCP connects are
// deliberately unhandled by the stage — Landlock port rules carry no
// address, so handling CONNECT_TCP would deny every undeclared port on any
// address, including the remote endpoints the contract leaves open — and
// loopback egress outside the declared ports is a documented gap the
// self-test observes rather than denies. Three granularity differences from
// the profile backend follow from the mechanism, and the self-test proves
// the rest: renames and hard links whose both parents are writable cannot
// be denied (the permission lives on the parents — both endpoints stay
// inside the set and the Landlock rules follow inodes across the rename),
// extended-attribute reads are not mediated on paths left visible for
// metadata, and file metadata itself is refused where nothing is bound.
// Other operating systems have no backend yet.
package confinement
