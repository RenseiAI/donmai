//go:build race

package afcli

// raceEnabled reports whether the test binary was built with -race.
// Used by tests that need to skip work that exercises a known data
// race in another package (REN-1460 — codex provider startup race).
func raceEnabled() bool { return true }
