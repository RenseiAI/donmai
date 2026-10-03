package confinement

import (
	"io/fs"
	"os"
	"path/filepath"
)

// noopBackend applies nothing: the discriminating control a self-test must
// turn red against.
type noopBackend struct{}

func (noopBackend) Name() BackendName { return BackendMacOSSeatbelt }

func (noopBackend) Version() (string, error) { return "noop-v1", nil }

func (noopBackend) Check() error { return nil }

func (noopBackend) Canonical(path string) (string, error) { return filepath.EvalSymlinks(path) }

func (noopBackend) Apply(ApplyRequest) (Applied, error) {
	return Applied{Rendered: []byte("noop"), Wrap: func(argv []string) []string { return argv }}, nil
}

// chmodBackend makes the read-only leaves and protected paths read-only by
// permission bits under the same identity, which ADR-2026-08-22 D6.7 rules
// out: the harness can undo it. The self-test must turn red against it too.
type chmodBackend struct{ noopBackend }

func (chmodBackend) Apply(req ApplyRequest) (Applied, error) {
	paths := append(append([]string{}, req.Resolved.ReadOnly...), req.Resolved.Protected...)
	for _, path := range paths {
		_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			return os.Chmod(p, 0o444) //nolint:gosec // G302: the double makes files read-only by permission bits.
		})
		_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o555) //nolint:gosec // G302: the double makes directories read-only by permission bits.
			}
			return nil
		})
	}
	return Applied{Rendered: []byte("chmod"), Wrap: func(argv []string) []string { return argv }}, nil
}
