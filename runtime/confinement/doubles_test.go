package confinement

import "path/filepath"

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

// renderingBackend renders the real macOS profile without applying it, so
// the rendering and its refusals are exercised on every OS.
type renderingBackend struct {
	noopBackend
	version string
}

func (r renderingBackend) Version() (string, error) {
	if r.version == "" {
		return "rendering-v1", nil
	}
	return r.version, nil
}

func (r renderingBackend) Apply(req ApplyRequest) (Applied, error) {
	text, err := renderSeatbelt(req.Resolved, seatbeltHost{shared: []string{"/tmp"}}, req.Rules, r.Canonical)
	if err != nil {
		return Applied{}, err
	}
	return Applied{Rendered: []byte(text), Wrap: func(argv []string) []string { return append([]string{"/wrapped"}, argv...) }}, nil
}
