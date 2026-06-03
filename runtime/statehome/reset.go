package statehome

// ResetForTest restores the process-global seam to its zero configuration:
// the default brand, no base-home override, and an un-fired
// resolve-before-set warning. It exists so tests that exercise the
// process-global state can isolate from one another and from the order in
// which other tests ran.
//
// It is exported (rather than living behind an internal export_test.go seam)
// so downstream embedders' own test suites can reset the shared state too.
// Production code must not call it.
func ResetForTest() {
	mu.Lock()
	brand = DefaultBrand
	baseHome = ""
	brandSet = false
	warnedResolveBeforeSet = false
	mu.Unlock()
}
