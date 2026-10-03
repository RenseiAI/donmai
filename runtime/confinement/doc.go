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
// RunProbeFromEnv first in main — through the same Prepare and Command, once
// through the headless spawn path and once through the PTY host, and judges
// every probe by what changed on disk. Replacing the backend with nothing, or
// with same-identity permission bits, turns it red.
//
// The macOS backend renders a sandbox profile and runs the harness under
// /usr/bin/sandbox-exec. Other operating systems have no backend yet.
package confinement
