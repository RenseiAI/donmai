package depprep

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// systemPath follows the toolchain directories on every child's PATH.
var systemPath = []string{"/usr/bin", "/bin"}

// proxyEnvNames are the only names Request.ProxyEnv may carry, in either
// case.
var proxyEnvNames = map[string]struct{}{
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {}, "ALL_PROXY": {},
}

// runDirs are the fresh per-run directories under Scratch.
type runDirs struct {
	root    string
	home    string
	tmp     string
	xdg     map[string]string
	storeOf func(manager string, index int, name string) string
}

func newRunDirs(scratch string) (*runDirs, error) {
	root, err := os.MkdirTemp(scratch, "depprep-")
	if err != nil {
		return nil, err
	}
	d := &runDirs{
		root: root,
		home: filepath.Join(root, "home"),
		tmp:  filepath.Join(root, "tmp"),
		xdg: map[string]string{
			"XDG_CONFIG_HOME": filepath.Join(root, "xdg", "config"),
			"XDG_CACHE_HOME":  filepath.Join(root, "xdg", "cache"),
			"XDG_DATA_HOME":   filepath.Join(root, "xdg", "data"),
			"XDG_STATE_HOME":  filepath.Join(root, "xdg", "state"),
			"XDG_RUNTIME_DIR": filepath.Join(root, "xdg", "runtime"),
		},
	}
	d.storeOf = func(manager string, index int, name string) string {
		return filepath.Join(root, "stores", fmt.Sprintf("%d-%s", index, manager), name)
	}
	dirs := []string{d.home, d.tmp}
	for _, p := range d.xdg {
		dirs = append(dirs, p)
	}
	for _, p := range dirs {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// envSet is a constructed environment, built up in layers.
type envSet map[string]string

// baseEnv is ADR D4.1's fixed part: PATH, HOME, TMPDIR, the XDG bases,
// LANG and CI, then the caller's explicit proxy settings. Nothing is read
// from the calling process's environment.
func baseEnv(req Request, d *runDirs) envSet {
	env := envSet{
		"PATH":   strings.Join(pathDirs(req.Toolchain), string(os.PathListSeparator)),
		"HOME":   d.home,
		"TMPDIR": d.tmp,
		"LANG":   defaultLang(),
		"CI":     "1",
	}
	for k, v := range d.xdg {
		env[k] = v
	}
	for k, v := range req.ProxyEnv {
		env[k] = v
	}
	return env
}

func pathDirs(toolchain []string) []string {
	return append(append([]string{}, toolchain...), systemPath...)
}

func defaultLang() string {
	if runtime.GOOS == "darwin" {
		return "en_US.UTF-8"
	}
	return "C.UTF-8"
}

// with returns a copy of e overlaid with each layer in order.
func (e envSet) with(layers ...map[string]string) envSet {
	out := make(envSet, len(e))
	for k, v := range e {
		out[k] = v
	}
	for _, layer := range layers {
		for k, v := range layer {
			out[k] = v
		}
	}
	return out
}

// list renders KEY=VALUE pairs in a stable order.
func (e envSet) list() []string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+e[k])
	}
	return out
}

// storeEnv binds each store variable to an empty per-run directory: the
// session-only exposure (ADR D4.1, D10).
func storeEnv(e Entry, index int, d *runDirs) (map[string]string, error) {
	out := make(map[string]string, len(e.Store.Env))
	for _, name := range e.Store.Env {
		dir := d.storeOf(e.Manager, index, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		out[name] = dir
	}
	return out, nil
}

// validateRequest enforces the request contract (code invalid_request).
func validateRequest(req Request) error {
	switch req.Subject.Kind {
	case SubjectSession, SubjectInvocation:
	default:
		return fmt.Errorf("subject kind %q is not session or invocation", req.Subject.Kind)
	}
	switch req.Network {
	case NetworkAllow, NetworkDeny:
	default:
		return fmt.Errorf("network policy %q is not allow or deny", req.Network)
	}
	root, err := absDir("root", req.Root)
	if err != nil {
		return err
	}
	if req.Scratch == "" {
		return fmt.Errorf("scratch is required")
	}
	scratch, err := absDir("scratch", req.Scratch)
	if err != nil {
		return err
	}
	if within(scratch, root) {
		return fmt.Errorf("scratch must be outside the leaf")
	}
	for _, dir := range req.Toolchain {
		if strings.ContainsRune(dir, os.PathListSeparator) {
			return fmt.Errorf("toolchain directory %q contains the path list separator", dir)
		}
		resolved, err := absDir("toolchain directory", dir)
		if err != nil {
			return err
		}
		if within(resolved, root) {
			return fmt.Errorf("toolchain directory %q is inside the leaf", dir)
		}
	}
	for name, value := range req.ProxyEnv {
		if _, ok := proxyEnvNames[strings.ToUpper(name)]; !ok || !validEnvName(name) {
			return fmt.Errorf("proxy setting %q is not a proxy variable", name)
		}
		if strings.ContainsAny(value, "\x00\n") {
			return fmt.Errorf("proxy setting %q has a control character", name)
		}
	}
	return nil
}

// absDir requires an absolute, clean path to an existing directory and
// returns it with symlinks resolved.
func absDir(what, p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return "", fmt.Errorf("%s %q is not an absolute clean path", what, p)
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s %q is not a directory", what, p)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	return resolved, nil
}

// within reports whether p is base or inside it.
func within(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
